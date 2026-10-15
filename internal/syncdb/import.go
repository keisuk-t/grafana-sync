package syncdb

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

type ImportMode string

const (
	ImportRequireEmpty ImportMode = "require-empty"
	ImportInsertOnly   ImportMode = "insert-only"
)

func (mode ImportMode) Validate() error {
	if mode != ImportRequireEmpty && mode != ImportInsertOnly {
		return errors.New("import mode must be require-empty or insert-only")
	}
	return nil
}

type ImportResult struct {
	Inserted int
	Skipped  int
}

func (d *Database) Import(ctx context.Context, s *Snapshot) error {
	_, err := d.ImportWithMode(ctx, s, ImportRequireEmpty)
	return err
}

func (d *Database) ImportWithMode(ctx context.Context, s *Snapshot, mode ImportMode) (ImportResult, error) {
	if err := mode.Validate(); err != nil {
		return ImportResult{}, err
	}
	if err := s.Validate(); err != nil {
		return ImportResult{}, err
	}
	if err := d.checkStorage(ctx); err != nil {
		return ImportResult{}, err
	}
	tx, err := d.DB.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return ImportResult{}, errors.New("cannot begin import transaction")
	}
	defer tx.Rollback()
	if d.Engine == "postgres" {
		if _, err := tx.ExecContext(ctx, `LOCK TABLE "org", "data_keys", "data_source", "secrets" IN EXCLUSIVE MODE`); err != nil {
			return ImportResult{}, errors.New("cannot lock target tables")
		}
	}
	if mode == ImportRequireEmpty {
		for _, entry := range s.entries() {
			if err := d.requireEmpty(ctx, tx, entry); err != nil {
				return ImportResult{}, err
			}
		}
	}
	var result ImportResult
	inserted := make(map[string]int)
	for _, entry := range s.entries() {
		counts, err := d.insertTable(ctx, tx, entry, mode)
		if err != nil {
			return ImportResult{}, err
		}
		result.Inserted += counts.Inserted
		result.Skipped += counts.Skipped
		inserted[entry.name] = counts.Inserted
	}
	if d.Engine == "postgres" {
		for _, entry := range s.entries() {
			if !entry.numeric || (mode == ImportInsertOnly && inserted[entry.name] == 0) {
				continue
			}
			if err := d.restartSequence(ctx, tx, entry.name, mode == ImportInsertOnly); err != nil {
				return ImportResult{}, err
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return ImportResult{}, errors.New("import commit failed; verify the target before retrying")
	}
	return result, nil
}

func (d *Database) requireEmpty(ctx context.Context, tx *sql.Tx, entry tableEntry) error {
	name := entry.name
	query := "SELECT " + d.quote(entry.key) + " FROM " + d.quote(name)
	if d.Engine == "mysql" {
		// Lock the empty primary-key range as well as existing rows in InnoDB.
		query += " FOR UPDATE"
	}
	rows, err := tx.QueryContext(ctx, query)
	if err != nil {
		return fmt.Errorf("cannot inspect target %s", name)
	}
	defer rows.Close()
	if rows.Next() {
		return fmt.Errorf("target %s is not empty; import never deletes or overwrites existing rows", name)
	}
	if rows.Err() != nil {
		return fmt.Errorf("cannot inspect target %s", name)
	}
	return nil
}

func (d *Database) insertTable(ctx context.Context, tx *sql.Tx, entry tableEntry, mode ImportMode) (ImportResult, error) {
	name, table := entry.name, entry.table
	rows, err := tx.QueryContext(ctx, "SELECT * FROM "+d.quote(name)+" WHERE 1 = 0")
	if err != nil {
		return ImportResult{}, fmt.Errorf("cannot inspect %s schema", name)
	}
	types, err := rows.ColumnTypes()
	rows.Close()
	if err != nil {
		return ImportResult{}, fmt.Errorf("cannot inspect %s column types", name)
	}
	if len(types) != len(table.Columns) {
		return ImportResult{}, fmt.Errorf("%s column set differs from the snapshot", name)
	}
	targetTypes := make(map[string]string, len(types))
	for _, col := range types {
		targetTypes[col.Name()] = col.DatabaseTypeName()
	}
	columns := make([]string, len(table.Columns))
	placeholders := make([]string, len(columns))
	for i, column := range table.Columns {
		kind, ok := targetTypes[column]
		if !ok {
			return ImportResult{}, fmt.Errorf("%s column set differs from the snapshot", name)
		}
		if binaryType(kind) != (columnIndex(table.BinaryColumns, column) >= 0) {
			return ImportResult{}, fmt.Errorf("%s binary columns differ from the snapshot", name)
		}
		columns[i] = d.quote(column)
		placeholders[i] = "?"
		if d.Engine == "postgres" {
			placeholders[i] = fmt.Sprintf("$%d", i+1)
		}
	}
	stmt, err := tx.PrepareContext(ctx, "INSERT INTO "+d.quote(name)+" ("+strings.Join(columns, ",")+") VALUES ("+strings.Join(placeholders, ",")+")")
	if err != nil {
		return ImportResult{}, fmt.Errorf("cannot prepare %s insert", name)
	}
	defer stmt.Close()
	var result ImportResult
	for rowIndex, row := range table.Rows {
		if mode == ImportInsertOnly {
			keyIndex := columnIndex(table.Columns, entry.key)
			key, err := decodeCell(row[keyIndex], targetTypes[entry.key])
			if err != nil {
				return ImportResult{}, fmt.Errorf("%s row %d has an invalid key", name, rowIndex+1)
			}
			exists, err := d.targetHasKey(ctx, tx, name, entry.key, key)
			if err != nil {
				return ImportResult{}, err
			}
			if exists {
				result.Skipped++
				continue
			}
			if err := d.checkTargetReference(ctx, tx, entry, row); err != nil {
				return ImportResult{}, fmt.Errorf("%s row %d: %w", name, rowIndex+1, err)
			}
		}
		args := make([]any, len(row))
		for i, value := range row {
			args[i], err = decodeCell(value, targetTypes[table.Columns[i]])
			if err != nil {
				return ImportResult{}, fmt.Errorf("%s row %d column %s has an invalid or unsupported value", name, rowIndex+1, table.Columns[i])
			}
		}
		if _, err := stmt.ExecContext(ctx, args...); err != nil {
			// Driver errors can include passwords or secure_json_data in constraint details.
			return ImportResult{}, fmt.Errorf("cannot insert %s row %d; check constraints and permissions", name, rowIndex+1)
		}
		result.Inserted++
	}
	return result, nil
}

func (d *Database) targetHasKey(ctx context.Context, tx *sql.Tx, name, key string, value any) (bool, error) {
	placeholder := "?"
	if d.Engine == "postgres" {
		placeholder = "$1"
	}
	var one int
	err := tx.QueryRowContext(ctx, "SELECT 1 FROM "+d.quote(name)+" WHERE "+d.quote(key)+" = "+placeholder+" FOR UPDATE", value).Scan(&one)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("cannot inspect target %s key", name)
	}
	return true, nil
}

func (d *Database) checkTargetReference(ctx context.Context, tx *sql.Tx, entry tableEntry, row []*string) error {
	switch entry.name {
	case "data_source":
		orgID, _ := positiveID(row[columnIndex(entry.table.Columns, "org_id")])
		exists, err := d.targetHasKey(ctx, tx, "org", "id", orgID)
		if err != nil {
			return err
		}
		if !exists {
			return errors.New("organization is missing from the target")
		}
	case "secrets":
		orgID, _ := positiveID(row[columnIndex(entry.table.Columns, "org_id")])
		namespace := row[columnIndex(entry.table.Columns, "namespace")]
		first, second := "?", "?"
		if d.Engine == "postgres" {
			first, second = "$1", "$2"
		}
		query := "SELECT 1 FROM " + d.quote("data_source") + " WHERE " + d.quote("org_id") + " = " + first + " AND " + d.quote("name") + " = " + second + " FOR UPDATE"
		var one int
		err := tx.QueryRowContext(ctx, query, orgID, *namespace).Scan(&one)
		if err == sql.ErrNoRows {
			return errors.New("data source is missing from the target; an existing ID may have a different name or organization")
		}
		if err != nil {
			return errors.New("cannot inspect target data source reference")
		}
	}
	return nil
}

func (d *Database) restartSequence(ctx context.Context, tx *sql.Tx, name string, preserveNext bool) error {
	var maxID sql.NullInt64
	if err := tx.QueryRowContext(ctx, "SELECT MAX("+d.quote("id")+") FROM "+d.quote(name)).Scan(&maxID); err != nil {
		return fmt.Errorf("cannot inspect %s ids", name)
	}
	if !maxID.Valid {
		return nil
	}
	if maxID.Int64 == math.MaxInt64 {
		return fmt.Errorf("%s id sequence is exhausted", name)
	}
	var schema, sequence string
	err := tx.QueryRowContext(ctx, `SELECT n.nspname, c.relname FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE c.oid = pg_get_serial_sequence($1, 'id')::regclass`, d.quote(name)).Scan(&schema, &sequence)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return fmt.Errorf("cannot inspect %s id sequence", name)
	}
	next := maxID.Int64 + 1
	qualified := d.quote(schema) + "." + d.quote(sequence)
	if preserveNext {
		var last int64
		var called bool
		if err := tx.QueryRowContext(ctx, "SELECT last_value, is_called FROM "+qualified).Scan(&last, &called); err != nil {
			return fmt.Errorf("cannot inspect %s sequence position", name)
		}
		if called {
			if last == math.MaxInt64 {
				return fmt.Errorf("%s id sequence is exhausted", name)
			}
			last++
		}
		if last > next {
			next = last
		}
	}
	// Unlike setval, ALTER SEQUENCE RESTART is transactional.
	query := "ALTER SEQUENCE " + qualified + " RESTART WITH " + strconv.FormatInt(next, 10)
	if _, err := tx.ExecContext(ctx, query); err != nil {
		return fmt.Errorf("cannot restart %s id sequence; sequence ownership is required", name)
	}
	return nil
}
