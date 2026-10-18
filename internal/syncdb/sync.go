package syncdb

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type SyncOptions struct {
	DryRun       bool
	AllowDelete  bool
	PrincipalMap *PrincipalMap
}

type syncTablePlan struct {
	entry         tableEntry
	targetColumns []string
	insert        [][]*string
	update        [][]*string
	previous      map[int64][]*string
	remove        []int64
	count         TableResult
}

func (d *Database) Sync(ctx context.Context, s *Snapshot, options SyncOptions) (ImportResult, error) {
	if s != nil {
		copy := *s
		copy.principalMap = options.PrincipalMap
		s = &copy
	}
	if !options.DryRun && !options.AllowDelete {
		return ImportResult{}, errors.New("sync requires --allow-delete or --dry-run")
	}
	if err := s.Validate(); err != nil {
		return ImportResult{}, err
	}
	if len(s.ScopeIDs()) == 0 {
		return ImportResult{}, errors.New("sync requires at least one organization in the snapshot")
	}
	if s.Selection().Has(TargetOrg) && columnIndex(s.Org.Columns, "name") < 0 {
		return ImportResult{}, errors.New("sync requires org.name")
	}
	if s.Selection().Has(TargetDatasource) && columnIndex(s.DataKeys.Columns, "active") < 0 {
		return ImportResult{}, errors.New("sync requires data_keys.active to preserve the target encryption key selection")
	}
	if err := d.checkStorage(ctx, s.Selection()); err != nil {
		return ImportResult{}, err
	}
	tx, err := d.DB.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return ImportResult{}, errors.New("cannot begin sync transaction")
	}
	defer tx.Rollback()
	if err := d.lockTargets(ctx, tx, s); err != nil {
		return ImportResult{}, err
	}
	if err := d.checkOrganizations(ctx, tx, s); err != nil {
		return ImportResult{}, err
	}
	if s.Selection().Has(TargetDashboard) {
		if err := d.checkDashboardStorage(ctx, tx); err != nil {
			return ImportResult{}, err
		}
	}
	targets := make(map[string]Table)
	types := make(map[string]map[string]string)
	for _, entry := range s.entries() {
		target, kinds, err := d.readSyncTarget(ctx, tx, entry)
		if err != nil {
			return ImportResult{}, err
		}
		if err := checkSyncColumns(entry.table, target, kinds); err != nil {
			return ImportResult{}, fmt.Errorf("%s: %w", entry.name, err)
		}
		targets[entry.name], types[entry.name] = target, kinds
	}
	plans, err := d.planSync(ctx, tx, s, targets, types)
	if err != nil {
		return ImportResult{}, err
	}
	if err := d.prepareDashboardSync(ctx, tx, s, plans, targets); err != nil {
		return ImportResult{}, err
	}
	permissions, err := d.planDashboardPermissions(ctx, tx, s, ImportSync)
	if err != nil {
		return ImportResult{}, err
	}
	var result ImportResult
	for _, plan := range plans {
		switch plan.entry.name {
		case "org":
			result.Org = plan.count
		case "data_keys":
			result.DataKeys = plan.count
		case "data_source":
			result.DataSource = plan.count
		case "secrets":
			result.Secrets = plan.count
		case "folder":
			result.Folder = plan.count
		case "dashboard":
			result.Dashboard = plan.count
		case "dashboard_tag":
			result.DashboardTag = plan.count
		case "dashboard_version":
			result.DashboardVersion = plan.count
		case "library_element":
			result.LibraryElement = plan.count
		case "library_element_connection":
			result.LibraryConnection = plan.count
		}
		result.Inserted += plan.count.Inserted
		result.Updated += plan.count.Updated
		result.Deleted += plan.count.Deleted
		result.Unchanged += plan.count.Unchanged
		result.Skipped += plan.count.Skipped
	}
	addPermissionCounts(&result, permissions.count)
	addRBACCounts(&result, permissions.rbac)
	if options.DryRun {
		return result, nil
	}
	if err := d.applyRBACDeletes(ctx, tx, permissions.rbac); err != nil {
		return ImportResult{}, err
	}
	byName := make(map[string]*syncTablePlan)
	for _, plan := range plans {
		byName[plan.entry.name] = plan
	}
	for _, name := range []string{"org", "data_keys"} {
		if byName[name] == nil {
			continue
		}
		if err := d.syncInsert(ctx, tx, byName[name]); err != nil {
			return ImportResult{}, err
		}
	}
	for _, name := range []string{"secrets", "data_source"} {
		if byName[name] == nil {
			continue
		}
		if err := d.syncDelete(ctx, tx, byName[name]); err != nil {
			return ImportResult{}, err
		}
	}
	for _, name := range []string{"data_source", "secrets"} {
		plan := byName[name]
		if plan == nil {
			continue
		}
		if err := d.syncUpdate(ctx, tx, plan, types[name]); err != nil {
			return ImportResult{}, err
		}
		if err := d.syncInsert(ctx, tx, plan); err != nil {
			return ImportResult{}, err
		}
	}
	if err := d.applyDashboardSync(ctx, tx, byName, types); err != nil {
		return ImportResult{}, err
	}
	if err := d.applyDashboardPermissions(ctx, tx, permissions); err != nil {
		return ImportResult{}, err
	}
	if d.Engine == "postgres" {
		for _, plan := range plans {
			name := plan.entry.name
			if plan.entry.numeric && plan.count.Inserted > 0 {
				if err := d.restartSequence(ctx, tx, name, true); err != nil {
					return ImportResult{}, err
				}
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return ImportResult{}, errors.New("sync commit failed; verify the target before retrying")
	}
	return result, nil
}

func (d *Database) readSyncTarget(ctx context.Context, tx *sql.Tx, entry tableEntry) (Table, map[string]string, error) {
	query := "SELECT * FROM " + d.quote(entry.name) + " ORDER BY " + d.quote(entry.key)
	if d.Engine == "mysql" {
		query += " FOR UPDATE"
	}
	rows, err := tx.QueryContext(ctx, query)
	if err != nil {
		return Table{}, nil, fmt.Errorf("cannot lock target %s", entry.name)
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		return Table{}, nil, fmt.Errorf("cannot inspect %s columns", entry.name)
	}
	columnTypes, err := rows.ColumnTypes()
	if err != nil {
		return Table{}, nil, fmt.Errorf("cannot inspect %s column types", entry.name)
	}
	table := Table{Columns: columns, Rows: make([][]*string, 0)}
	kinds := make(map[string]string, len(columns))
	for i, col := range columnTypes {
		kinds[columns[i]] = col.DatabaseTypeName()
		if binaryType(col.DatabaseTypeName()) {
			table.BinaryColumns = append(table.BinaryColumns, columns[i])
		}
	}
	for rows.Next() {
		values := make([]any, len(columns))
		ptrs := make([]any, len(columns))
		for i := range values {
			ptrs[i] = &values[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return Table{}, nil, fmt.Errorf("cannot scan target %s", entry.name)
		}
		row := make([]*string, len(columns))
		for i, value := range values {
			if binaryType(columnTypes[i].DatabaseTypeName()) {
				row[i], err = encodeBinary(value)
			} else {
				row[i], err = encodeCell(value)
			}
			if err != nil {
				return Table{}, nil, fmt.Errorf("cannot encode target %s row", entry.name)
			}
		}
		table.Rows = append(table.Rows, row)
	}
	if err := rows.Err(); err != nil {
		return Table{}, nil, fmt.Errorf("cannot finish reading target %s", entry.name)
	}
	return table, kinds, nil
}

func checkSyncColumns(source *Table, target Table, kinds map[string]string) error {
	if len(source.Columns) != len(target.Columns) {
		return errors.New("column set differs from the snapshot")
	}
	for _, col := range source.Columns {
		kind, ok := kinds[col]
		if !ok {
			return errors.New("column set differs from the snapshot")
		}
		if binaryType(kind) != (columnIndex(source.BinaryColumns, col) >= 0) {
			return errors.New("binary columns differ from the snapshot")
		}
	}
	return nil
}

func syncID(row []*string, columns []string, key string) (int64, error) {
	i := columnIndex(columns, key)
	if i < 0 || i >= len(row) {
		return 0, errors.New("missing numeric key")
	}
	return positiveID(row[i])
}

func syncRowsByID(table Table, key string) map[int64][]*string {
	indexed := make(map[int64][]*string, len(table.Rows))
	for _, row := range table.Rows {
		id, _ := syncID(row, table.Columns, key)
		indexed[id] = row
	}
	return indexed
}

func syncEqual(a, b *string, kind string) (bool, error) {
	left, err := decodeCell(a, kind)
	if err != nil {
		return false, errors.New("invalid snapshot cell")
	}
	right, err := decodeCell(b, kind)
	if err != nil {
		return false, errors.New("invalid target cell")
	}
	switch x := left.(type) {
	case []byte:
		y, ok := right.([]byte)
		return ok && bytes.Equal(x, y), nil
	case time.Time:
		y, ok := right.(time.Time)
		return ok && x.Truncate(time.Microsecond).Equal(y.Truncate(time.Microsecond)), nil
	default:
		return reflect.DeepEqual(left, right), nil
	}
}

func syncRowEqual(source, target []*string, sourceColumns, targetColumns []string, kinds map[string]string) (bool, error) {
	for i, name := range sourceColumns {
		equal, err := syncEqual(source[i], target[columnIndex(targetColumns, name)], kinds[name])
		if err != nil || !equal {
			return false, err
		}
	}
	return true, nil
}

func (d *Database) planSync(ctx context.Context, tx *sql.Tx, s *Snapshot, targets map[string]Table, kinds map[string]map[string]string) ([]*syncTablePlan, error) {
	if err := d.checkSyncUnique(ctx, tx, s, targets["org"]); err != nil {
		return nil, err
	}
	orgIDs := s.scopeIDs()
	plans := make([]*syncTablePlan, 0, 4)
	for _, entry := range s.entries() {
		target := targets[entry.name]
		plan := &syncTablePlan{entry: entry, targetColumns: target.Columns, previous: make(map[int64][]*string)}
		for rowIndex, row := range entry.table.Rows {
			for i, cell := range row {
				if _, err := decodeCell(cell, kinds[entry.name][entry.table.Columns[i]]); err != nil {
					return nil, fmt.Errorf("%s row %d has an invalid or unsupported value in %s", entry.name, rowIndex+1, entry.table.Columns[i])
				}
			}
		}
		if entry.name == "data_keys" {
			byName := make(map[string][]*string)
			for _, row := range target.Rows {
				byName[*row[columnIndex(target.Columns, "name")]] = row
			}
			for _, row := range entry.table.Rows {
				name := *row[columnIndex(entry.table.Columns, "name")]
				existing, ok := byName[name]
				if !ok {
					var stored string
					err := tx.QueryRowContext(ctx, "SELECT "+d.quote("name")+" FROM "+d.quote("data_keys")+" WHERE "+d.quote("name")+" = "+d.bind(1), name).Scan(&stored)
					if err != nil && err != sql.ErrNoRows {
						return nil, errors.New("cannot inspect target data key names")
					}
					if err == nil {
						if stored != name {
							return nil, fmt.Errorf("data_keys name %s differs from the target's colliding key name", name)
						}
						existing, ok = byName[stored]
					}
				}
				if ok {
					for _, col := range []string{"encrypted_data", "provider", "scope", "label"} {
						if columnIndex(entry.table.Columns, col) < 0 {
							continue
						}
						equal, err := syncEqual(row[columnIndex(entry.table.Columns, col)], existing[columnIndex(target.Columns, col)], kinds[entry.name][col])
						if err != nil || !equal {
							return nil, fmt.Errorf("data_keys %s has incompatible key material or metadata", name)
						}
					}
					plan.count.Skipped++
					continue
				}
				inactive := append([]*string(nil), row...)
				inactive[columnIndex(entry.table.Columns, "active")] = stringPtr("0")
				plan.insert = append(plan.insert, inactive)
				plan.count.Inserted++
			}
			plans = append(plans, plan)
			continue
		}
		byID := syncRowsByID(target, "id")
		incomingIDs := make(map[int64]bool)
		for _, row := range entry.table.Rows {
			id, _ := syncID(row, entry.table.Columns, "id")
			incomingIDs[id] = true
			old, ok := byID[id]
			if ok && entry.name != "org" {
				oldOrg, _ := syncID(old, target.Columns, "org_id")
				inScope := orgIDs[oldOrg]
				if entry.group == TargetDashboard {
					e := entry
					e.table = &target
					inScope = dashboardRowInScope(e, old, syncRowsByID(targets["dashboard"], "id"), targets["dashboard"].Columns, orgIDs)
				}
				if !inScope || (entry.name == "secrets" && (old[columnIndex(target.Columns, "type")] == nil || *old[columnIndex(target.Columns, "type")] != "datasource")) {
					return nil, fmt.Errorf("%s id %d belongs to a protected row outside the sync scope", entry.name, id)
				}
			}
			if !ok {
				plan.insert = append(plan.insert, row)
				plan.count.Inserted++
				continue
			}
			if entry.name == "org" {
				plan.count.Skipped++
				continue
			}
			equal, err := syncRowEqual(row, old, entry.table.Columns, target.Columns, kinds[entry.name])
			if err != nil {
				return nil, fmt.Errorf("%s id %d has an invalid value", entry.name, id)
			}
			if equal {
				plan.count.Unchanged++
			} else {
				plan.update = append(plan.update, row)
				plan.previous[id] = old
				plan.count.Updated++
			}
		}
		if entry.name == "org" {
			for _, row := range plan.insert {
				id, _ := syncID(row, entry.table.Columns, "id")
				name := row[columnIndex(entry.table.Columns, "name")]
				if name == nil {
					return nil, errors.New("org has a missing name")
				}
				placeholder := "?"
				if d.Engine == "postgres" {
					placeholder = "$1"
				}
				var existingID int64
				err := tx.QueryRowContext(ctx, "SELECT "+d.quote("id")+" FROM "+d.quote("org")+" WHERE "+d.quote("name")+" = "+placeholder, *name).Scan(&existingID)
				if err != nil && err != sql.ErrNoRows {
					return nil, errors.New("cannot inspect target organization names")
				}
				if err == nil && existingID != id {
					return nil, fmt.Errorf("org id %d conflicts with an existing organization name", id)
				}
			}
		}
		if entry.name == "data_source" || entry.name == "secrets" || entry.group == TargetDashboard {
			for _, row := range target.Rows {
				orgID, _ := syncID(row, target.Columns, "org_id")
				inScope := orgIDs[orgID]
				if entry.group == TargetDashboard {
					e := entry
					e.table = &target
					inScope = dashboardRowInScope(e, row, syncRowsByID(targets["dashboard"], "id"), targets["dashboard"].Columns, orgIDs)
				}
				if !inScope {
					continue
				}
				if entry.name == "secrets" && (row[columnIndex(target.Columns, "type")] == nil || *row[columnIndex(target.Columns, "type")] != "datasource") {
					continue
				}
				id, _ := syncID(row, target.Columns, "id")
				if !incomingIDs[id] {
					plan.remove = append(plan.remove, id)
					plan.count.Deleted++
				}
			}
		}
		plans = append(plans, plan)
	}
	return plans, nil
}

func stringPtr(s string) *string { return &s }

var syncSQLIdentifier = regexp.MustCompile(`^[a-zA-Z0-9_]+$`)

func (d *Database) checkSyncUnique(ctx context.Context, tx *sql.Tx, s *Snapshot, targetOrg Table) error {
	if s.Selection().Has(TargetDashboard) {
		if err := d.checkDashboardUnique(ctx, tx, s); err != nil {
			return err
		}
	}
	org := s.Org
	org.Rows = nil
	storedOrgs := syncRowsByID(targetOrg, "id")
	for _, row := range s.Org.Rows {
		id, _ := syncID(row, s.Org.Columns, "id")
		if _, exists := storedOrgs[id]; !exists {
			org.Rows = append(org.Rows, row)
		}
	}
	for _, spec := range []struct {
		name, column string
		table        *Table
		byOrg        bool
	}{
		{"org", "name", &org, false},
		{"data_source", "name", &s.DataSource, true},
		{"data_source", "uid", &s.DataSource, true},
		{"secrets", "namespace", &s.Secrets, true},
		{"data_keys", "name", &s.DataKeys, false},
	} {
		index := columnIndex(spec.table.Columns, spec.column)
		if index < 0 || len(spec.table.Rows) < 2 {
			continue
		}
		query, err := d.syncUniqueQuery(ctx, tx, spec.name, spec.column)
		if err != nil {
			return fmt.Errorf("cannot inspect %s.%s collation", spec.name, spec.column)
		}
		type logicalKey struct {
			OrgID int64  `json:"org_id"`
			Value string `json:"value"`
		}
		var values []logicalKey
		for _, row := range spec.table.Rows {
			if row[index] == nil {
				continue
			}
			orgID := int64(0)
			if spec.byOrg {
				orgID, _ = syncID(row, spec.table.Columns, "org_id")
			}
			values = append(values, logicalKey{orgID, *row[index]})
		}
		if len(values) < 2 {
			continue
		}
		payload, err := json.Marshal(values)
		if err != nil {
			return errors.New("cannot encode logical keys")
		}
		var duplicate int
		err = tx.QueryRowContext(ctx, query, string(payload)).Scan(&duplicate)
		if err == nil {
			return fmt.Errorf("%s contains duplicate %s in the sync scope", spec.name, spec.column)
		}
		if err != sql.ErrNoRows {
			return fmt.Errorf("cannot compare %s.%s logical keys", spec.name, spec.column)
		}
	}
	return nil
}

func (d *Database) checkUniquePayload(ctx context.Context, tx *sql.Tx, query string, values any) error {
	payload, err := json.Marshal(values)
	if err != nil {
		return errors.New("cannot encode logical keys")
	}
	var duplicate int
	err = tx.QueryRowContext(ctx, query, string(payload)).Scan(&duplicate)
	if err == nil {
		return errors.New("dashboard group contains colliding logical keys")
	}
	if err != sql.ErrNoRows {
		return errors.New("cannot compare dashboard logical keys")
	}
	return nil
}

func (d *Database) syncUniqueQuery(ctx context.Context, tx *sql.Tx, table, column string) (string, error) {
	if d.Engine == "mysql" {
		var charset, collation string
		if err := tx.QueryRowContext(ctx, `SELECT CHARACTER_SET_NAME, COLLATION_NAME FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ? AND COLUMN_NAME = ?`, table, column).Scan(&charset, &collation); err != nil {
			return "", err
		}
		if !syncSQLIdentifier.MatchString(charset) || !syncSQLIdentifier.MatchString(collation) {
			return "", errors.New("unsupported collation")
		}
		return "SELECT 1 FROM JSON_TABLE(?, '$[*]' COLUMNS (org_id BIGINT PATH '$.org_id', value LONGTEXT CHARACTER SET " + charset + " COLLATE " + d.quote(collation) + " PATH '$.value')) AS incoming GROUP BY org_id, value HAVING COUNT(*) > 1 LIMIT 1", nil
	}
	var schema, collation string
	if err := tx.QueryRowContext(ctx, `SELECT n.nspname, c.collname FROM pg_attribute a JOIN pg_collation c ON c.oid = a.attcollation JOIN pg_namespace n ON n.oid = c.collnamespace WHERE a.attrelid = to_regclass($1) AND a.attname = $2`, table, column).Scan(&schema, &collation); err != nil {
		return "", err
	}
	qualified := d.quote(schema) + "." + d.quote(collation)
	return "SELECT 1 FROM jsonb_to_recordset($1::jsonb) AS incoming(org_id bigint, value text) GROUP BY org_id, value COLLATE " + qualified + " HAVING COUNT(*) > 1 LIMIT 1", nil
}

func (d *Database) syncDelete(ctx context.Context, tx *sql.Tx, plan *syncTablePlan) error {
	placeholder := "?"
	if d.Engine == "postgres" {
		placeholder = "$1"
	}
	query := "DELETE FROM " + d.quote(plan.entry.name) + " WHERE " + d.quote("id") + " = " + placeholder
	for _, id := range plan.remove {
		if _, err := tx.ExecContext(ctx, query, id); err != nil {
			return fmt.Errorf("cannot delete %s id %d; check references and permissions", plan.entry.name, id)
		}
	}
	return nil
}

func (d *Database) syncInsert(ctx context.Context, tx *sql.Tx, plan *syncTablePlan) error {
	if len(plan.insert) == 0 {
		return nil
	}
	table := *plan.entry.table
	table.Rows = plan.insert
	entry := plan.entry
	entry.table = &table
	_, err := d.insertTable(ctx, tx, entry, ImportRequireEmpty)
	return err
}

func (d *Database) syncUpdate(ctx context.Context, tx *sql.Tx, plan *syncTablePlan, kinds map[string]string) error {
	if len(plan.update) == 0 {
		return nil
	}
	entry := plan.entry
	cols := entry.table.Columns
	keyIndex := columnIndex(cols, "id")
	assignments := make([]string, 0, len(cols)-1)
	for i, col := range cols {
		if i != keyIndex {
			assignments = append(assignments, d.quote(col)+" = "+d.bind(len(assignments)+1))
		}
	}
	query := "UPDATE " + d.quote(entry.name) + " SET " + strings.Join(assignments, ", ") + " WHERE " + d.quote("id") + " = " + d.bind(len(cols))
	for _, row := range plan.update {
		id, _ := syncID(row, cols, "id")
		old := plan.previous[id]
		orgID, _ := syncID(row, cols, "org_id")
		oldOrgID, _ := syncID(old, plan.targetColumns, "org_id")
		for _, col := range []string{"name", "uid", "namespace", "title", "slug"} {
			index := columnIndex(cols, col)
			if index < 0 {
				continue
			}
			equal, err := syncEqual(row[index], old[columnIndex(plan.targetColumns, col)], kinds[col])
			if err != nil {
				return fmt.Errorf("%s id %d has an invalid logical key", entry.name, id)
			}
			if col == "title" {
				parent := "folder_uid"
				if entry.name == "folder" {
					parent = "parent_uid"
				}
				if cell(*entry.table, row, parent) != cell(Table{Columns: plan.targetColumns}, old, parent) {
					equal = false
				}
			}
			if col == "name" && entry.name == "library_element" && cell(*entry.table, row, "folder_uid") != cell(Table{Columns: plan.targetColumns}, old, "folder_uid") {
				equal = false
			}
			if !equal || orgID != oldOrgID {
				temp, err := temporaryKey()
				if err != nil {
					return errors.New("cannot generate temporary sync key")
				}
				if _, err := tx.ExecContext(ctx, "UPDATE "+d.quote(entry.name)+" SET "+d.quote(col)+" = "+d.bind(1)+" WHERE "+d.quote("id")+" = "+d.bind(2), temp, id); err != nil {
					return fmt.Errorf("cannot stage %s id %d logical key", entry.name, id)
				}
			}
		}
	}
	for _, row := range plan.update {
		id, _ := syncID(row, cols, "id")
		args := make([]any, 0, len(cols))
		for i, cell := range row {
			if i == keyIndex {
				continue
			}
			value, err := decodeCell(cell, kinds[cols[i]])
			if err != nil {
				return fmt.Errorf("%s id %d has an invalid value in %s", entry.name, id, cols[i])
			}
			args = append(args, value)
		}
		args = append(args, id)
		if _, err := tx.ExecContext(ctx, query, args...); err != nil {
			return fmt.Errorf("cannot update %s id %d; check constraints and permissions", entry.name, id)
		}
	}
	return nil
}

func (d *Database) bind(index int) string {
	if d.Engine == "postgres" {
		return "$" + strconv.Itoa(index)
	}
	return "?"
}

func temporaryKey() (string, error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	return "gs-" + hex.EncodeToString(random[:]), nil
}
