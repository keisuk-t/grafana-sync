package syncdb

import (
	"context"
	"database/sql"
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

func (d *Database) checkStorage(ctx context.Context) error {
	if d.Engine != "mysql" {
		return nil
	}
	for _, name := range []string{"org", "data_source"} {
		var engine string
		if err := d.DB.QueryRowContext(ctx, "SELECT ENGINE FROM information_schema.TABLES WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ?", name).Scan(&engine); err != nil {
			return fmt.Errorf("cannot inspect %s storage engine", name)
		}
		if !strings.EqualFold(engine, "InnoDB") {
			return fmt.Errorf("%s must use InnoDB for transactional transfer", name)
		}
	}
	return nil
}

func (d *Database) Export(ctx context.Context) (*Snapshot, error) {
	if err := d.checkStorage(ctx); err != nil {
		return nil, err
	}
	tx, err := d.DB.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return nil, errors.New("cannot begin export transaction")
	}
	defer tx.Rollback()
	s := &Snapshot{Version: 1}
	s.Org, err = d.readTable(ctx, tx, "org")
	if err != nil {
		return nil, err
	}
	s.DataSource, err = d.readTable(ctx, tx, "data_source")
	if err != nil {
		return nil, err
	}
	if err := s.Validate(); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, errors.New("cannot commit export transaction")
	}
	return s, nil
}

func (d *Database) readTable(ctx context.Context, tx *sql.Tx, name string) (Table, error) {
	rows, err := tx.QueryContext(ctx, "SELECT * FROM "+d.quote(name)+" ORDER BY "+d.quote("id"))
	if err != nil {
		return Table{}, fmt.Errorf("cannot read %s", name)
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		return Table{}, err
	}
	table := Table{Columns: columns, Rows: make([][]*string, 0)}
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
			row[i], err = encodeCell(value)
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
