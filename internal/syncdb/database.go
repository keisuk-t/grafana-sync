package syncdb

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

type Database struct {
	DB     *sql.DB
	Engine string
}

func Open(ctx context.Context, engine, dsn string) (*Database, error) {
	var db *sql.DB
	switch engine {
	case "mysql":
		cfg, err := mysql.ParseDSN(dsn)
		if err != nil {
			return nil, errors.New("invalid MySQL DSN")
		}
		cfg.ParseTime = true
		cfg.Loc = time.UTC
		cfg.MultiStatements = false
		cfg.ColumnsWithAlias = false
		if cfg.Params == nil {
			cfg.Params = make(map[string]string)
		}
		cfg.Params["sql_mode"] = "'STRICT_ALL_TABLES,NO_ZERO_DATE,NO_ZERO_IN_DATE,NO_ENGINE_SUBSTITUTION'"
		connector, err := mysql.NewConnector(cfg)
		if err != nil {
			return nil, errors.New("invalid MySQL connection configuration")
		}
		db = sql.OpenDB(connector)
	case "postgres":
		cfg, err := pgx.ParseConfig(dsn)
		if err != nil {
			return nil, errors.New("invalid PostgreSQL DSN")
		}
		db = stdlib.OpenDB(*cfg)
	default:
		return nil, errors.New("db must be mysql or postgres")
	}
	db.SetMaxOpenConns(1)
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, errors.New("database connection failed; check DSN, TLS, permissions and connectivity")
	}
	return &Database{DB: db, Engine: engine}, nil
}

func (d *Database) quote(name string) string {
	if d.Engine == "mysql" {
		return "`" + strings.ReplaceAll(name, "`", "``") + "`"
	}
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

func (d *Database) checkStorage(ctx context.Context, targets Targets) error {
	if d.Engine != "mysql" {
		return nil
	}
	for _, entry := range (&Snapshot{Version: 4, Targets: targets.Names()}).entries() {
		name := entry.name
		var engine string
		if err := d.DB.QueryRowContext(ctx, "SELECT ENGINE FROM information_schema.TABLES WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ?", name).Scan(&engine); err != nil {
			return fmt.Errorf("cannot inspect %s storage engine", name)
		}
		if !strings.EqualFold(engine, "InnoDB") {
			return fmt.Errorf("%s must use InnoDB for transactional transfer", name)
		}
	}
	if targets.Has(TargetDashboard) {
		for _, name := range []string{"role", "builtin_role", "permission", "user_role", "team_role", "dashboard_acl", "user", "org_user", "team", "team_member"} {
			var engine string
			err := d.DB.QueryRowContext(ctx, "SELECT ENGINE FROM information_schema.TABLES WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ?", name).Scan(&engine)
			if err == sql.ErrNoRows && name != "role" && name != "builtin_role" && name != "permission" {
				continue
			}
			if err != nil || !strings.EqualFold(engine, "InnoDB") {
				return fmt.Errorf("%s must exist and use InnoDB for transactional dashboard permissions", name)
			}
		}
	}
	return nil
}

func (d *Database) Export(ctx context.Context) (*Snapshot, error) {
	return d.ExportTargets(ctx, DefaultTargets)
}

func (d *Database) ExportTargets(ctx context.Context, targets Targets) (*Snapshot, error) {
	if err := targets.Validate(); err != nil {
		return nil, err
	}
	if err := d.checkStorage(ctx, targets); err != nil {
		return nil, err
	}
	tx, err := d.DB.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return nil, errors.New("cannot begin export transaction")
	}
	defer tx.Rollback()
	s := &Snapshot{Version: 5, Targets: targets.Names(), Scope: Scope{OrgIDs: []string{}}}
	org, err := d.readTable(ctx, tx, (&Snapshot{Version: 2}).entries()[0])
	if err != nil {
		return nil, err
	}
	for _, row := range org.Rows {
		s.Scope.OrgIDs = append(s.Scope.OrgIDs, cell(org, row, "id"))
	}
	for _, entry := range s.entries() {
		*entry.table, err = d.readTable(ctx, tx, entry)
		if err != nil {
			return nil, err
		}
	}
	if targets.Has(TargetDashboard) {
		if public := columnIndex(s.Dashboard.Columns, "is_public"); public >= 0 {
			for _, row := range s.Dashboard.Rows {
				if cell(s.Dashboard, row, "is_public") == "1" {
					value := "0"
					row[public] = &value
				}
			}
		}
		if err := d.checkDashboardStorage(ctx, tx); err != nil {
			return nil, err
		}
		if err := d.guardDashboardDependenciesWithRBAC(ctx, tx, s.Dashboard, false, true); err != nil {
			return nil, err
		}
		s.RBAC, err = d.readRBAC(ctx, tx, permissionResources(s.Dashboard, s.LibraryElement))
		if err != nil {
			return nil, err
		}
		aclRows, err := d.readLegacyACL(ctx, tx, s.Dashboard, s.ScopeIDs(), s.RBAC, true)
		if err != nil {
			return nil, err
		}
		for _, row := range aclRows {
			s.RBAC.LegacyACL = append(s.RBAC.LegacyACL, row.RBACACL)
		}
		canonicalizeManagedRBAC(s.RBAC)
	}
	if err := s.Validate(); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, errors.New("cannot commit export transaction")
	}
	return s, nil
}

func (d *Database) readTable(ctx context.Context, tx *sql.Tx, entry tableEntry) (Table, error) {
	name := entry.name
	query := "SELECT * FROM " + d.quote(name)
	if name == "secrets" {
		query += fmt.Sprintf(` WHERE %[1]s = 'datasource' AND EXISTS (
			SELECT 1 FROM %[2]s
			WHERE %[2]s.%[3]s = %[4]s.%[3]s AND %[2]s.%[5]s = %[4]s.%[6]s
		)`, d.quote("type"), d.quote("data_source"), d.quote("org_id"), d.quote("secrets"), d.quote("name"), d.quote("namespace"))
	}
	if name == "dashboard_tag" || name == "dashboard_version" {
		query += " WHERE EXISTS (SELECT 1 FROM " + d.quote("dashboard") + " WHERE " + d.quote("dashboard") + "." + d.quote("id") + " = " + d.quote(name) + "." + d.quote("dashboard_id") + ")"
	}
	rows, err := tx.QueryContext(ctx, query+" ORDER BY "+d.quote(entry.key))
	if err != nil {
		return Table{}, fmt.Errorf("cannot read %s", name)
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		return Table{}, err
	}
	table := Table{Columns: columns, Rows: make([][]*string, 0)}
	types, err := rows.ColumnTypes()
	if err != nil {
		return Table{}, fmt.Errorf("cannot inspect %s column types", name)
	}
	for i, column := range types {
		if binaryType(column.DatabaseTypeName()) {
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
			return Table{}, fmt.Errorf("cannot scan %s", name)
		}
		row := make([]*string, len(columns))
		for i, value := range values {
			if binaryType(types[i].DatabaseTypeName()) {
				row[i], err = encodeBinary(value)
			} else {
				row[i], err = encodeCell(value)
			}
			if err != nil {
				return Table{}, fmt.Errorf("%s.%s: %w", name, columns[i], err)
			}
		}
		table.Rows = append(table.Rows, row)
	}
	if err := rows.Err(); err != nil {
		return Table{}, fmt.Errorf("cannot finish reading %s", name)
	}
	return table, nil
}

func encodeCell(value any) (*string, error) {
	var text string
	switch v := value.(type) {
	case nil:
		return nil, nil
	case string:
		text = v
	case []byte:
		text = string(v)
	case int64:
		text = strconv.FormatInt(v, 10)
	case bool:
		text = "0"
		if v {
			text = "1"
		}
	case time.Time:
		if v.IsZero() {
			return nil, errors.New("zero database timestamp cannot be transferred losslessly")
		}
		text = v.Format(time.RFC3339Nano)
	default:
		return nil, errors.New("unsupported database value type")
	}
	if !utf8.ValidString(text) {
		return nil, errors.New("non-UTF-8 text cannot be represented losslessly")
	}
	return &text, nil
}

func decodeCell(value *string, kind string) (any, error) {
	if value == nil {
		return nil, nil
	}
	if binaryType(kind) {
		return base64.StdEncoding.Strict().DecodeString(*value)
	}
	switch strings.ToUpper(kind) {
	case "INT", "INTEGER", "INT2", "INT4", "INT8", "BIGINT", "SMALLINT", "TINYINT", "MEDIUMINT":
		return strconv.ParseInt(*value, 10, 64)
	case "BOOL", "BOOLEAN":
		if *value == "0" || *value == "1" {
			return *value == "1", nil
		}
		return nil, errors.New("expected boolean encoded as 0 or 1")
	case "TIMESTAMP", "TIMESTAMPTZ", "DATETIME":
		return time.Parse(time.RFC3339Nano, *value)
	case "VARCHAR", "CHAR", "TEXT", "TINYTEXT", "MEDIUMTEXT", "LONGTEXT", "BPCHAR":
		return *value, nil
	default:
		return nil, errors.New("unsupported target column type")
	}
}

func binaryType(kind string) bool {
	switch strings.ToUpper(kind) {
	case "BYTEA", "BLOB", "TINYBLOB", "MEDIUMBLOB", "LONGBLOB", "BINARY", "VARBINARY":
		return true
	default:
		return false
	}
}

func encodeBinary(value any) (*string, error) {
	if value == nil {
		return nil, nil
	}
	data, ok := value.([]byte)
	if !ok {
		return nil, errors.New("unexpected binary database value")
	}
	encoded := base64.StdEncoding.EncodeToString(data)
	return &encoded, nil
}
