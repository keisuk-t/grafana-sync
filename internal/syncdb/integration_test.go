//go:build integration

package syncdb

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

// These tests require disposable databases with no pre-existing target tables.
func TestDatabaseTransfer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	databases := make(map[string]*Database)
	snapshots := make(map[string]*Snapshot)
	for _, engine := range []string{"mysql", "postgres"} {
		dsn := os.Getenv(strings.ToUpper(engine) + "_TEST_DSN")
		if dsn == "" {
			t.Fatalf("%s_TEST_DSN must point to a disposable database", strings.ToUpper(engine))
		}
		db, err := Open(ctx, engine, dsn)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { db.DB.Close() })
		databases[engine] = db
		createFixtures(t, ctx, db)
		seedFixtures(t, ctx, db)
		snapshots[engine], err = db.Export(ctx)
		if err != nil {
			t.Fatal(err)
		}
	}
	if !reflect.DeepEqual(snapshots["mysql"], snapshots["postgres"]) {
		t.Fatal("exports differ between engines")
	}
	for _, source := range []string{"mysql", "postgres"} {
		for _, target := range []string{"mysql", "postgres"} {
			t.Run(source+"_to_"+target, func(t *testing.T) {
				db := databases[target]
				clearFixtures(t, ctx, db)
				encoded, err := json.Marshal(snapshots[source])
				if err != nil {
					t.Fatal(err)
				}
				snapshot, err := Decode(strings.NewReader(string(encoded)))
				if err != nil {
					t.Fatal(err)
				}
				if err := db.Import(ctx, snapshot); err != nil {
					t.Fatal(err)
				}
				got, err := db.Export(ctx)
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(snapshot, got) {
					t.Fatalf("round trip mismatch: %s -> %s", source, target)
				}
				if err := db.Import(ctx, snapshot); err == nil {
					t.Fatal("import into populated tables succeeded")
				}
				assertSentinel(t, ctx, db)
				assertNextID(t, ctx, db)
			})
		}
	}
	for engine, db := range databases {
		t.Run(engine+"_rollback", func(t *testing.T) {
			clearFixtures(t, ctx, db)
			data, err := json.Marshal(snapshots[engine])
			if err != nil {
				t.Fatal(err)
			}
			bad, err := Decode(strings.NewReader(string(data)))
			if err != nil {
				t.Fatal(err)
			}
			// Fail in data_source after org has already been inserted.
			bad.DataSource.Rows[0][columnIndex(bad.DataSource.Columns, "name")] = nil
			if err := db.Import(ctx, bad); err == nil {
				t.Fatal("expected constraint failure")
			}
			assertEmpty(t, ctx, db)
			bad, err = Decode(strings.NewReader(string(data)))
			if err != nil {
				t.Fatal(err)
			}
			bad.DataSource.Columns[columnIndex(bad.DataSource.Columns, "name")] = "nonexistent"
			if err := db.Import(ctx, bad); err == nil {
				t.Fatal("expected schema mismatch")
			}
			assertEmpty(t, ctx, db)
			bad, err = Decode(strings.NewReader(string(data)))
			if err != nil {
				t.Fatal(err)
			}
			bad.DataSource.Rows[0][columnIndex(bad.DataSource.Columns, "org_id")] = str("77")
			if err := db.Import(ctx, bad); err == nil {
				t.Fatal("expected orphan rejection")
			}
			assertEmpty(t, ctx, db)
			assertSentinel(t, ctx, db)
		})
	}
}

func createFixtures(t *testing.T, ctx context.Context, db *Database) {
	t.Helper()
	id, boolType, stamp, suffix := "BIGSERIAL PRIMARY KEY", "BOOLEAN", "TIMESTAMP(6)", ""
	if db.Engine == "mysql" {
		id, boolType, stamp, suffix = "BIGINT PRIMARY KEY AUTO_INCREMENT", "TINYINT(1)", "DATETIME(6)", " ENGINE=InnoDB DEFAULT CHARSET=utf8mb4"
	}
	statements := map[string]string{
		"org":         "CREATE TABLE " + db.quote("org") + " (id " + id + ", version INT NOT NULL, name VARCHAR(190) NOT NULL UNIQUE, created " + stamp + " NOT NULL, updated " + stamp + " NOT NULL, note TEXT NULL)" + suffix,
		"data_source": "CREATE TABLE " + db.quote("data_source") + " (id " + id + ", org_id BIGINT NOT NULL, version INT NOT NULL, name VARCHAR(190) NOT NULL, " + db.quote("user") + " VARCHAR(255), " + db.quote("database") + " VARCHAR(255), basic_auth " + boolType + " NOT NULL, is_default " + boolType + " NOT NULL, read_only " + boolType + " NULL, json_data TEXT, secure_json_data TEXT, created " + stamp + " NOT NULL, updated " + stamp + " NOT NULL, UNIQUE(org_id,name))" + suffix,
		"dashboard":   "CREATE TABLE " + db.quote("dashboard") + " (id BIGINT PRIMARY KEY, title VARCHAR(255))" + suffix,
	}
	for _, name := range []string{"org", "data_source", "dashboard"} {
		if _, err := db.DB.ExecContext(ctx, statements[name]); err != nil {
			t.Fatalf("create %s (do not use an existing Grafana DB): %v", name, err)
		}
		t.Cleanup(func() {
			if _, err := db.DB.Exec("DROP TABLE " + db.quote(name)); err != nil {
				t.Error(err)
			}
		})
	}
	if _, err := db.DB.ExecContext(ctx, "INSERT INTO "+db.quote("dashboard")+" VALUES (1, 'must not change')"); err != nil {
		t.Fatal(err)
	}
}

func seedFixtures(t *testing.T, ctx context.Context, db *Database) {
	t.Helper()
	stamp := time.Date(2025, 1, 2, 3, 4, 5, 123456000, time.UTC).Format(time.RFC3339Nano)
	s := &Snapshot{Version: 1,
		Org: Table{Columns: []string{"id", "version", "name", "created", "updated", "note"}, Rows: [][]*string{{str("9007199254740993"), str("1"), str("組織 'A'"), str(stamp), str(stamp), nil}}},
		DataSource: Table{Columns: []string{"id", "org_id", "version", "name", "user", "database", "basic_auth", "is_default", "read_only", "json_data", "secure_json_data", "created", "updated"}, Rows: [][]*string{
			{str("42"), str("9007199254740993"), str("1"), str("metrics"), str(""), nil, str("1"), str("1"), nil, str(`{"text":"日本語","quote":"'"}`), str("opaque-encrypted-value"), str(stamp), str(stamp)},
			{str("88"), str("9007199254740993"), str("2"), str("logs"), nil, str(""), str("0"), str("0"), str("0"), str("{}"), str(""), str(stamp), str(stamp)},
		}},
	}
	if err := db.Import(ctx, s); err != nil {
		t.Fatal(err)
	}
}

func clearFixtures(t *testing.T, ctx context.Context, db *Database) {
	t.Helper()
	for _, name := range []string{"data_source", "org"} {
		if _, err := db.DB.ExecContext(ctx, "DELETE FROM "+db.quote(name)); err != nil {
			t.Fatal(err)
		}
	}
}

func assertEmpty(t *testing.T, ctx context.Context, db *Database) {
	t.Helper()
	for _, name := range []string{"org", "data_source"} {
		var count int
		if err := db.DB.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+db.quote(name)).Scan(&count); err != nil || count != 0 {
			t.Fatalf("%s not rolled back: count=%d err=%v", name, count, err)
		}
	}
}

func assertSentinel(t *testing.T, ctx context.Context, db *Database) {
	t.Helper()
	var title string
	if err := db.DB.QueryRowContext(ctx, "SELECT title FROM "+db.quote("dashboard")+" WHERE id = 1").Scan(&title); err != nil || title != "must not change" {
		t.Fatal("unrelated table changed", err)
	}
}

func assertNextID(t *testing.T, ctx context.Context, db *Database) {
	t.Helper()
	stamp := time.Now().UTC()
	query := "INSERT INTO " + db.quote("org") + " (version,name,created,updated) VALUES (1,?,?,?)"
	if db.Engine == "postgres" {
		query = `INSERT INTO "org" (version,name,created,updated) VALUES (1,$1,$2,$3) RETURNING id`
	}
	var id int64
	if db.Engine == "postgres" {
		if err := db.DB.QueryRowContext(ctx, query, "next", stamp, stamp).Scan(&id); err != nil {
			t.Fatal(err)
		}
	} else {
		result, err := db.DB.ExecContext(ctx, query, "next", stamp, stamp)
		if err != nil {
			t.Fatal(err)
		}
		id, err = result.LastInsertId()
		if err != nil {
			t.Fatal(err)
		}
	}
	if id <= 9007199254740993 {
		t.Fatal("org sequence did not advance")
	}
	// The data_source sequence must advance independently, too.
	var next sql.NullInt64
	if db.Engine == "postgres" {
		if err := db.DB.QueryRowContext(ctx, `SELECT nextval(pg_get_serial_sequence('data_source','id'))`).Scan(&next); err != nil {
			t.Fatal(err)
		}
	} else {
		if err := db.DB.QueryRowContext(ctx, `SELECT AUTO_INCREMENT FROM information_schema.TABLES WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME='data_source'`).Scan(&next); err != nil {
			t.Fatal(err)
		}
	}
	if !next.Valid || next.Int64 <= 88 {
		t.Fatal("data_source sequence did not advance")
	}
}
