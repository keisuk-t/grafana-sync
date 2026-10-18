package syncdb

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

func (d *Database) lockTargets(ctx context.Context, tx *sql.Tx, s *Snapshot) error {
	if d.Engine != "postgres" {
		return nil
	}
	names := []string{}
	for _, entry := range s.entries() {
		names = append(names, d.quote(entry.name))
	}
	if s.Selection().Has(TargetDashboard) {
		for _, name := range []string{"role", "builtin_role", "permission"} {
			names = append(names, d.quote(name))
		}
		if s.Version < 4 {
			names = append(names, d.quote("library_element"), d.quote("library_element_connection"))
		}
	}
	if _, err := tx.ExecContext(ctx, "LOCK TABLE "+strings.Join(names, ", ")+" IN EXCLUSIVE MODE"); err != nil {
		return errors.New("cannot lock target tables")
	}
	return nil
}

func (d *Database) checkOrganizations(ctx context.Context, tx *sql.Tx, s *Snapshot) error {
	if s.Selection().Has(TargetOrg) {
		return nil
	}
	for _, id := range s.ScopeIDs() {
		exists, err := d.targetHasKey(ctx, tx, "org", "id", id)
		if err != nil {
			return err
		}
		if !exists {
			return errors.New("snapshot scope requires an organization missing from the target; import org first")
		}
	}
	return nil
}

func (d *Database) prepareImport(ctx context.Context, tx *sql.Tx, s *Snapshot, mode ImportMode) (*Snapshot, int, error) {
	if err := d.checkOrganizations(ctx, tx, s); err != nil {
		return nil, 0, err
	}
	if !s.Selection().Has(TargetDashboard) {
		return s, 0, nil
	}
	if err := d.checkDashboardStorage(ctx, tx); err != nil {
		return nil, 0, err
	}
	prepared := *s
	targets := make(map[string]Table)
	for _, entry := range s.entries() {
		target, kinds, err := d.readSyncTarget(ctx, tx, entry)
		if err != nil {
			return nil, 0, err
		}
		if err := checkSyncColumns(entry.table, target, kinds); err != nil {
			return nil, 0, fmt.Errorf("%s: %w", entry.name, err)
		}
		targets[entry.name] = target
	}
	skipped := 0
	if mode == ImportInsertOnly {
		existing := syncRowsByID(targets["dashboard"], "id")
		for _, table := range []*Table{&prepared.DashboardTag, &prepared.DashboardVersion, &prepared.LibraryConnection} {
			rows := [][]*string{}
			for _, row := range table.Rows {
				parent := "dashboard_id"
				if table == &prepared.LibraryConnection {
					parent = "connection_id"
				}
				id, _ := syncID(row, table.Columns, parent)
				if existing[id] != nil {
					skipped++
					continue
				}
				rows = append(rows, row)
			}
			table.Rows = rows
		}
	}
	final := make(map[string]Table)
	for _, entry := range prepared.entries() {
		table := targets[entry.name]
		if mode == ImportRequireEmpty {
			final[entry.name] = *entry.table
			continue
		}
		keys := make(map[string]bool)
		for _, row := range table.Rows {
			keys[cell(table, row, entry.key)] = true
		}
		rows := append([][]*string{}, table.Rows...)
		for _, row := range entry.table.Rows {
			if !keys[cell(*entry.table, row, entry.key)] {
				rows = append(rows, reorderRow(row, entry.table.Columns, table.Columns))
			}
		}
		table.Rows = rows
		final[entry.name] = table
	}
	if err := d.validateDashboardOutcome(ctx, tx, &prepared, final); err != nil {
		return nil, 0, err
	}
	return &prepared, skipped, nil
}

func reorderRow(row []*string, source, target []string) []*string {
	values := make([]*string, len(target))
	for i, name := range target {
		values[i] = row[columnIndex(source, name)]
	}
	return values
}

func (d *Database) validateDashboardOutcome(ctx context.Context, tx *sql.Tx, s *Snapshot, final map[string]Table) error {
	if !s.Selection().Has(TargetDashboard) {
		return nil
	}
	if err := d.preserveLegacyLibraries(ctx, tx, s, final); err != nil {
		return err
	}
	effective := Snapshot{Version: 4, Targets: []string{"dashboard"}, Scope: s.Scope}
	for _, entry := range effective.entries() {
		*entry.table = final[entry.name]
	}
	scope := s.scopeIDs()
	parents := syncRowsByID(effective.Dashboard, "id")
	for _, entry := range effective.entries() {
		rows := [][]*string{}
		for _, row := range entry.table.Rows {
			if dashboardRowInScope(entry, row, parents, effective.Dashboard.Columns, scope) {
				rows = append(rows, row)
			}
		}
		entry.table.Rows = rows
	}
	if err := effective.validateDashboardState(scope, false); err != nil {
		return err
	}
	if err := effective.validateLibraryState(scope); err != nil {
		return err
	}
	sources, ok := final["data_source"]
	documents := make(map[int64][]string)
	for _, row := range effective.Dashboard.Rows {
		org := rowOrg(effective.Dashboard, row)
		documents[org] = append(documents[org], cell(effective.Dashboard, row, "data"))
	}
	for _, row := range effective.DashboardVersion.Rows {
		id, _ := syncID(row, effective.DashboardVersion.Columns, "dashboard_id")
		org := rowOrg(effective.Dashboard, parents[id])
		documents[org] = append(documents[org], cell(effective.DashboardVersion, row, "data"))
	}
	for _, row := range effective.LibraryElement.Rows {
		org := rowOrg(effective.LibraryElement, row)
		documents[org] = append(documents[org], cell(effective.LibraryElement, row, "model"))
	}
	for org, payloads := range documents {
		for _, payload := range payloads {
			refs, _, err := dashboardReferences(payload)
			if err != nil {
				return err
			}
			for _, ref := range refs {
				if !ok {
					sources, err = d.readTable(ctx, tx, tableEntry{name: "data_source", key: "id"})
					if err != nil {
						return err
					}
					ok = true
				}
				column := "name"
				if ref.uid {
					column = "uid"
				}
				found := false
				for _, source := range sources.Rows {
					if rowOrg(sources, source) == org && cell(sources, source, column) == ref.value {
						found = true
						break
					}
				}
				if !found {
					return errors.New("dashboard has an unresolved static datasource reference in the resulting target state")
				}
			}
		}
	}
	return nil
}

func dashboardRowInScope(entry tableEntry, row []*string, parents map[int64][]*string, columns []string, scope map[int64]bool) bool {
	if entry.name == "dashboard_tag" || entry.name == "dashboard_version" || entry.name == "library_element_connection" {
		key := "dashboard_id"
		if entry.name == "library_element_connection" {
			key = "connection_id"
		}
		id, err := syncID(row, entry.table.Columns, key)
		if err != nil || parents[id] == nil {
			return false
		}
		org, _ := syncID(parents[id], columns, "org_id")
		return scope[org]
	}
	return scope[rowOrg(*entry.table, row)]
}
