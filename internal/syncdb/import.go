package syncdb

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

func (d *Database) Import(ctx context.Context, s *Snapshot) error {
	if err := s.Validate(); err != nil {
		return err
	}
	if err := d.checkStorage(ctx); err != nil {
		return err
	}
	tx, err := d.DB.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return errors.New("cannot begin import transaction")
	}
	defer tx.Rollback()
	if d.Engine == "postgres" {
		if _, err := tx.ExecContext(ctx, `LOCK TABLE "org", "data_source" IN EXCLUSIVE MODE`); err != nil {
			return errors.New("cannot lock target tables")
		}
	}
	for _, name := range []string{"org", "data_source"} {
		if err := d.requireEmpty(ctx, tx, name); err != nil {
			return err
		}
	}
	for _, entry := range []struct {
		name  string
		table Table
	}{{"org", s.Org}, {"data_source", s.DataSource}} {
		if err := d.insertTable(ctx, tx, entry.name, entry.table); err != nil {
			return err
		}
	}
	if d.Engine == "postgres" {
		for _, name := range []string{"org", "data_source"} {
			if err := d.restartSequence(ctx, tx, name); err != nil {
				return err
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return errors.New("import commit failed; verify the target before retrying")
	}
	return nil
}

func (d *Database) requireEmpty(ctx context.Context, tx *sql.Tx, name string) error {
	query := "SELECT " + d.quote("id") + " FROM " + d.quote(name)
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

func (d *Database) insertTable(ctx context.Context, tx *sql.Tx, name string, table Table) error {
	rows, err := tx.QueryContext(ctx, "SELECT * FROM "+d.quote(name)+" WHERE 1 = 0")
	if err != nil {
		return fmt.Errorf("cannot inspect %s schema", name)
	}
	types, err := rows.ColumnTypes()
	rows.Close()
	if err != nil {
		return fmt.Errorf("cannot inspect %s column types", name)
	}
	if len(types) != len(table.Columns) {
		return fmt.Errorf("%s column set differs from the snapshot", name)
	}
	targetTypes := make(map[string]string, len(types))
	for _, col := range types {
		targetTypes[col.Name()] = col.DatabaseTypeName()
	}
	columns := make([]string, len(table.Columns))
	placeholders := make([]string, len(columns))
	for i, column := range table.Columns {
		if _, ok := targetTypes[column]; !ok {
			return fmt.Errorf("%s column set differs from the snapshot", name)
		}
		columns[i] = d.quote(column)
		placeholders[i] = "?"
		if d.Engine == "postgres" {
			placeholders[i] = fmt.Sprintf("$%d", i+1)
		}
	}
	stmt, err := tx.PrepareContext(ctx, "INSERT INTO "+d.quote(name)+" ("+strings.Join(columns, ",")+") VALUES ("+strings.Join(placeholders, ",")+")")
	if err != nil {
		return fmt.Errorf("cannot prepare %s insert", name)
	}
	defer stmt.Close()
	for rowIndex, row := range table.Rows {
		args := make([]any, len(row))
		for i, value := range row {
			args[i], err = decodeCell(value, targetTypes[table.Columns[i]])
			if err != nil {
				return fmt.Errorf("%s row %d column %s has an invalid or unsupported value", name, rowIndex+1, table.Columns[i])
			}
		}
		if _, err := stmt.ExecContext(ctx, args...); err != nil {
			// Driver errors can include passwords or secure_json_data in constraint details.
			return fmt.Errorf("cannot insert %s row %d; check constraints and permissions", name, rowIndex+1)
		}
	}
	return nil
}

func (d *Database) restartSequence(ctx context.Context, tx *sql.Tx, name string) error {
	var maxID sql.NullInt64
	if err := tx.QueryRowContext(ctx, "SELECT MAX("+d.quote("id")+") FROM "+d.quote(name)).Scan(&maxID); err != nil {
		return fmt.Errorf("cannot inspect %s ids", name)
	}
	if !maxID.Valid {
		return nil
	}
	var schema, sequence string
	err := tx.QueryRowContext(ctx, `SELECT n.nspname, c.relname FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE c.oid = pg_get_serial_sequence($1, 'id')::regclass`, d.quote(name)).Scan(&schema, &sequence)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return fmt.Errorf("cannot inspect %s id sequence", name)
	}
	// Unlike setval, ALTER SEQUENCE RESTART is transactional.
	query := "ALTER SEQUENCE " + d.quote(schema) + "." + d.quote(sequence) + " RESTART WITH " + strconv.FormatInt(maxID.Int64+1, 10)
	if _, err := tx.ExecContext(ctx, query); err != nil {
		return fmt.Errorf("cannot restart %s id sequence; sequence ownership is required", name)
	}
	return nil
}
