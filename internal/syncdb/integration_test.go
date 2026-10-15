//go:build integration

package syncdb

import (
	"context"
	"database/sql"
	"encoding/base64"
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
		if len(snapshots[engine].DataKeys.Rows) != 2 || len(snapshots[engine].Secrets.Rows) != 2 {
			t.Fatal("export omitted a key or included unrelated secrets")
		}
		var count int
		if err := db.DB.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+db.quote("secrets")).Scan(&count); err != nil || count != 5 {
			t.Fatal("export modified secrets", err)
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
		for source, snapshot := range snapshots {
			t.Run(source+"_to_"+engine+"_insert_only", func(t *testing.T) { testInsertOnly(t, ctx, db, snapshot) })
		}
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
			// Fail in the last table after org, data_keys, data_source and one secret.
			bad.Secrets.Rows[1][columnIndex(bad.Secrets.Columns, "namespace")] = str("metrics")
			if err := bad.Validate(); err != nil {
				t.Fatal("fixture must reach the database", err)
			}
			if err := db.Import(ctx, bad); err == nil {
				t.Fatal("expected constraint failure")
			}
			assertEmpty(t, ctx, db)
			bad, err = Decode(strings.NewReader(string(data)))
			if err != nil {
				t.Fatal(err)
			}
			bad.DataSource.Columns[columnIndex(bad.DataSource.Columns, "read_only")] = "nonexistent"
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
			bad, err = Decode(strings.NewReader(string(data)))
			if err != nil {
				t.Fatal(err)
			}
			bad.DataSource.Rows[0][columnIndex(bad.DataSource.Columns, "user")] = str(strings.Repeat("x", 256))
			if err := bad.Validate(); err != nil {
				t.Fatal("fixture must reach the database", err)
			}
			if err := db.Import(ctx, bad); err == nil {
				t.Fatal("expected oversized text rejection, not silent truncation")
			}
			assertEmpty(t, ctx, db)
			assertSentinel(t, ctx, db)
		})
		for _, name := range []string{"data_keys", "secrets"} {
			t.Run(engine+"_populated_"+name, func(t *testing.T) {
				clearFixtures(t, ctx, db)
				tx, err := db.DB.BeginTx(ctx, nil)
				if err != nil {
					t.Fatal(err)
				}
				defer tx.Rollback()
				for _, entry := range snapshots[engine].entries() {
					if entry.name == name {
						if _, err := db.insertTable(ctx, tx, entry, ImportRequireEmpty); err != nil {
							t.Fatal(err)
						}
					}
				}
				if err := tx.Commit(); err != nil {
					t.Fatal(err)
				}
				err = db.Import(ctx, snapshots[engine])
				if err == nil || !strings.Contains(err.Error(), name+" is not empty") {
					t.Fatalf("expected populated %s rejection: %v", name, err)
				}
				for _, entry := range snapshots[engine].entries() {
					var count int
					want := 0
					if entry.name == name {
						want = len(entry.table.Rows)
					}
					if err := db.DB.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+db.quote(entry.name)).Scan(&count); err != nil || count != want {
						t.Fatalf("%s modified", entry.name)
					}
				}
				assertSentinel(t, ctx, db)
			})
		}
		t.Run(engine+"_binary_schema", func(t *testing.T) {
			clearFixtures(t, ctx, db)
			data, _ := json.Marshal(snapshots[engine])
			bad, err := Decode(strings.NewReader(string(data)))
			if err != nil {
				t.Fatal(err)
			}
			bad.Org.BinaryColumns = []string{"note"}
			if err := db.Import(ctx, bad); err == nil {
				t.Fatal("accepted binary metadata for text")
			}
			assertEmpty(t, ctx, db)
		})
	}
}

func cloneSnapshot(t *testing.T, s *Snapshot) *Snapshot {
	t.Helper()
	data, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	cloned, err := Decode(strings.NewReader(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	return cloned
}

func insertOnlyTarget(t *testing.T, ctx context.Context, db *Database, source *Snapshot) *Snapshot {
	t.Helper()
	clearFixtures(t, ctx, db)
	s := cloneSnapshot(t, source)
	for _, entry := range s.entries() {
		entry.table.Rows = entry.table.Rows[:1]
	}
	s.Org.Rows[0][columnIndex(s.Org.Columns, "name")] = str("target-org")
	s.Org.Rows[0][columnIndex(s.Org.Columns, "version")] = str("99")
	s.Org.Rows[0][columnIndex(s.Org.Columns, "note")] = str("target-note")
	s.DataSource.Rows[0][columnIndex(s.DataSource.Columns, "user")] = str("target-user")
	s.DataSource.Rows[0][columnIndex(s.DataSource.Columns, "secure_json_data")] = str("target-ciphertext")
	s.DataKeys.Rows[0][columnIndex(s.DataKeys.Columns, "active")] = str("0")
	s.DataKeys.Rows[0][columnIndex(s.DataKeys.Columns, "encrypted_data")] = str(base64.StdEncoding.EncodeToString([]byte("target-key")))
	s.Secrets.Rows[0][columnIndex(s.Secrets.Columns, "value")] = str("target-secret")
	extra := append([]*string(nil), s.Org.Rows[0]...)
	extra[columnIndex(s.Org.Columns, "id")] = str("9007199254741093")
	extra[columnIndex(s.Org.Columns, "name")] = str("target-only-org")
	s.Org.Rows = append(s.Org.Rows, extra)
	if err := db.Import(ctx, s); err != nil {
		t.Fatal(err)
	}
	return s
}

func assertSnapshot(t *testing.T, ctx context.Context, db *Database, want *Snapshot) {
	t.Helper()
	got, err := db.Export(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatal("target rows changed unexpectedly")
	}
	assertSentinel(t, ctx, db)
}

func testInsertOnly(t *testing.T, ctx context.Context, db *Database, source *Snapshot) {
	t.Run("preserve_existing_and_retry", func(t *testing.T) {
		existing := insertOnlyTarget(t, ctx, db, source)
		if err := db.Import(ctx, source); err == nil {
			t.Fatal("default mode accepted populated tables")
		}
		assertSnapshot(t, ctx, db, existing)
		result, err := db.ImportWithMode(ctx, source, ImportInsertOnly)
		if err != nil || result != (ImportResult{Inserted: 3, Skipped: 4}) {
			t.Fatalf("insert-only result: %+v %v", result, err)
		}
		want := cloneSnapshot(t, source)
		for i, entry := range want.entries() {
			entry.table.Rows[0] = existing.entries()[i].table.Rows[0]
		}
		want.Org.Rows = append(want.Org.Rows, existing.Org.Rows[1])
		assertSnapshot(t, ctx, db, want)
		result, err = db.ImportWithMode(ctx, source, ImportInsertOnly)
		if err != nil || result != (ImportResult{Skipped: 7}) {
			t.Fatalf("retry result: %+v %v", result, err)
		}
		assertSnapshot(t, ctx, db, want)
		assertNextID(t, ctx, db)
	})
	for _, name := range []string{"data_source_unique", "secrets_unique", "oversized_text", "target_reference", "schema"} {
		t.Run(name, func(t *testing.T) {
			existing := insertOnlyTarget(t, ctx, db, source)
			incoming := cloneSnapshot(t, source)
			extra := append([]*string(nil), incoming.Org.Rows[0]...)
			extra[columnIndex(incoming.Org.Columns, "id")] = str("2")
			extra[columnIndex(incoming.Org.Columns, "name")] = str("new-org")
			incoming.Org.Rows = append(incoming.Org.Rows, extra)
			switch name {
			case "data_source_unique":
				extra := append([]*string(nil), incoming.DataSource.Rows[0]...)
				extra[columnIndex(incoming.DataSource.Columns, "id")] = str("999")
				incoming.DataSource.Rows = append(incoming.DataSource.Rows, extra)
			case "secrets_unique":
				extra := append([]*string(nil), incoming.Secrets.Rows[0]...)
				extra[columnIndex(incoming.Secrets.Columns, "id")] = str("999")
				incoming.Secrets.Rows = append(incoming.Secrets.Rows, extra)
			case "oversized_text":
				incoming.DataSource.Rows[1][columnIndex(incoming.DataSource.Columns, "user")] = str(strings.Repeat("x", 256))
			case "target_reference":
				incoming.DataSource.Rows[0][columnIndex(incoming.DataSource.Columns, "name")] = str("source-renamed")
				incoming.Secrets.Rows[0][columnIndex(incoming.Secrets.Columns, "namespace")] = str("source-renamed")
				incoming.Secrets.Rows[0][columnIndex(incoming.Secrets.Columns, "id")] = str("999")
			case "schema":
				incoming.Secrets.Columns[columnIndex(incoming.Secrets.Columns, "updated")] = "nonexistent"
			}
			if err := incoming.Validate(); err != nil {
				t.Fatal("fixture must reach the database", err)
			}
			result, err := db.ImportWithMode(ctx, incoming, ImportInsertOnly)
			if err == nil || result != (ImportResult{}) {
				t.Fatalf("expected atomic failure: %+v %v", result, err)
			}
			if strings.Contains(err.Error(), "target-secret") || strings.Contains(err.Error(), "source-renamed") {
				t.Fatal("error exposed data")
			}
			assertSnapshot(t, ctx, db, existing)
		})
	}
	if db.Engine == "mysql" {
		t.Run("key_collation", func(t *testing.T) {
			insertOnlyTarget(t, ctx, db, source)
			incoming := cloneSnapshot(t, source)
			incoming.DataKeys.Rows[0][columnIndex(incoming.DataKeys.Columns, "name")] = str("KEY-CURRENT")
			result, err := db.ImportWithMode(ctx, incoming, ImportInsertOnly)
			if err != nil || result != (ImportResult{Inserted: 3, Skipped: 4}) {
				t.Fatalf("database collation ignored: %+v %v", result, err)
			}
		})
	}
	if db.Engine == "postgres" {
		t.Run("sequence_never_rewinds", func(t *testing.T) {
			for _, called := range []bool{false, true} {
				insertOnlyTarget(t, ctx, db, source)
				if _, err := db.DB.ExecContext(ctx, `SELECT setval('data_source_id_seq',1000,$1)`, called); err != nil {
					t.Fatal(err)
				}
				if _, err := db.ImportWithMode(ctx, source, ImportInsertOnly); err != nil {
					t.Fatal(err)
				}
				var next int64
				want := int64(1000)
				if called {
					want++
				}
				if err := db.DB.QueryRowContext(ctx, `SELECT nextval(pg_get_serial_sequence('data_source','id'))`).Scan(&next); err != nil || next != want {
					t.Fatalf("sequence rewound: %d %v", next, err)
				}
				if _, err := db.ImportWithMode(ctx, source, ImportInsertOnly); err != nil {
					t.Fatal(err)
				}
				if err := db.DB.QueryRowContext(ctx, `SELECT nextval(pg_get_serial_sequence('data_source','id'))`).Scan(&next); err != nil || next != want+1 {
					t.Fatalf("retry changed sequence: %d %v", next, err)
				}
			}
		})
	}
}

func createFixtures(t *testing.T, ctx context.Context, db *Database) {
	t.Helper()
	id, boolType, stamp, blob, suffix := "BIGSERIAL PRIMARY KEY", "BOOLEAN", "TIMESTAMP(6)", "BYTEA", ""
	if db.Engine == "mysql" {
		id, boolType, stamp, blob, suffix = "BIGINT PRIMARY KEY AUTO_INCREMENT", "TINYINT(1)", "DATETIME(6)", "BLOB", " ENGINE=InnoDB DEFAULT CHARSET=utf8mb4"
	}
	statements := map[string]string{
		"org":         "CREATE TABLE " + db.quote("org") + " (id " + id + ", version INT NOT NULL, name VARCHAR(190) NOT NULL UNIQUE, created " + stamp + " NOT NULL, updated " + stamp + " NOT NULL, note TEXT NULL)" + suffix,
		"data_source": "CREATE TABLE " + db.quote("data_source") + " (id " + id + ", org_id BIGINT NOT NULL, version INT NOT NULL, name VARCHAR(190) NOT NULL, " + db.quote("user") + " VARCHAR(255), " + db.quote("database") + " VARCHAR(255), basic_auth " + boolType + " NOT NULL, is_default " + boolType + " NOT NULL, read_only " + boolType + " NULL, json_data TEXT, secure_json_data TEXT, created " + stamp + " NOT NULL, updated " + stamp + " NOT NULL, UNIQUE(org_id,name))" + suffix,
		"dashboard":   "CREATE TABLE " + db.quote("dashboard") + " (id BIGINT PRIMARY KEY, title VARCHAR(255))" + suffix,
		"data_keys":   "CREATE TABLE " + db.quote("data_keys") + " (name VARCHAR(100) PRIMARY KEY, active " + boolType + ", scope VARCHAR(30) NOT NULL, provider VARCHAR(50) NOT NULL, encrypted_data " + blob + " NOT NULL, created " + stamp + " NOT NULL, updated " + stamp + " NOT NULL, label VARCHAR(100) NOT NULL)" + suffix,
		"secrets":     "CREATE TABLE " + db.quote("secrets") + " (id " + id + ", org_id BIGINT NOT NULL, namespace VARCHAR(255) NOT NULL, " + db.quote("type") + " VARCHAR(255) NOT NULL, value TEXT NULL, created " + stamp + " NOT NULL, updated " + stamp + " NOT NULL, UNIQUE(org_id,namespace," + db.quote("type") + "))" + suffix,
	}
	for _, name := range []string{"org", "data_keys", "data_source", "secrets", "dashboard"} {
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
	s := &Snapshot{Version: 2,
		Org: Table{Columns: []string{"id", "version", "name", "created", "updated", "note"}, Rows: [][]*string{{str("9007199254740993"), str("1"), str("組織 'A'"), str(stamp), str(stamp), nil}}},
		DataSource: Table{Columns: []string{"id", "org_id", "version", "name", "user", "database", "basic_auth", "is_default", "read_only", "json_data", "secure_json_data", "created", "updated"}, Rows: [][]*string{
			{str("42"), str("9007199254740993"), str("1"), str("metrics"), str(""), nil, str("1"), str("1"), nil, str(`{"text":"日本語","quote":"'"}`), str("opaque-encrypted-value"), str(stamp), str(stamp)},
			{str("88"), str("9007199254740993"), str("2"), str("logs"), nil, str(""), str("0"), str("0"), str("0"), str("{}"), str(""), str(stamp), str(stamp)},
		}},
		DataKeys: Table{Columns: []string{"name", "active", "scope", "provider", "encrypted_data", "created", "updated", "label"}, BinaryColumns: []string{"encrypted_data"}, Rows: [][]*string{
			{str("key-current"), str("1"), str("global"), str("secretKey.v1"), str(base64.StdEncoding.EncodeToString([]byte{0, 255, 128, 0, 10, 13})), str(stamp), str(stamp), str("current-label")},
			{str("key-old"), str("0"), str("global"), str("secretKey.v1"), str(base64.StdEncoding.EncodeToString([]byte("valid UTF-8 bytes"))), str(stamp), str(stamp), str("previous-label")},
		}},
		Secrets: Table{Columns: []string{"id", "org_id", "namespace", "type", "value", "created", "updated"}, Rows: [][]*string{
			{str("111"), str("9007199254740993"), str("metrics"), str("datasource"), str("opaque-base64-current-key"), str(stamp), str(stamp)},
			{str("222"), str("9007199254740993"), str("logs"), str("datasource"), str("opaque-base64-old-key"), str(stamp), str(stamp)},
		}},
	}
	if err := db.Import(ctx, s); err != nil {
		t.Fatal(err)
	}
	// These rows must never be exported or deleted by export.
	tx, err := db.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	unrelated := Table{Columns: s.Secrets.Columns, Rows: [][]*string{
		{str("333"), str("9007199254740993"), str("metrics"), str("alerting"), str("unrelated-secret"), str(stamp), str(stamp)},
		{str("444"), str("9007199254740993"), str("missing"), str("datasource"), nil, str(stamp), str(stamp)},
		{str("555"), str("77"), str("metrics"), str("datasource"), str("unrelated-org"), str(stamp), str(stamp)},
	}}
	if _, err := db.insertTable(ctx, tx, tableEntry{name: "secrets", key: "id", numeric: true, table: &unrelated}, ImportRequireEmpty); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func clearFixtures(t *testing.T, ctx context.Context, db *Database) {
	t.Helper()
	for _, name := range []string{"secrets", "data_source", "data_keys", "org"} {
		if _, err := db.DB.ExecContext(ctx, "DELETE FROM "+db.quote(name)); err != nil {
			t.Fatal(err)
		}
	}
}

func assertEmpty(t *testing.T, ctx context.Context, db *Database) {
	t.Helper()
	for _, name := range []string{"org", "data_keys", "data_source", "secrets"} {
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
	var maxID int64
	if err := db.DB.QueryRowContext(ctx, "SELECT MAX(id) FROM "+db.quote("org")).Scan(&maxID); err != nil {
		t.Fatal(err)
	}
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
	if id <= maxID {
		t.Fatal("org sequence did not advance")
	}
	for _, entry := range []struct {
		name  string
		maxID int64
	}{{"data_source", 88}, {"secrets", 222}} {
		var next sql.NullInt64
		if db.Engine == "postgres" {
			if err := db.DB.QueryRowContext(ctx, `SELECT nextval(pg_get_serial_sequence($1,'id'))`, entry.name).Scan(&next); err != nil {
				t.Fatal(err)
			}
		} else {
			if err := db.DB.QueryRowContext(ctx, `SELECT AUTO_INCREMENT FROM information_schema.TABLES WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME=?`, entry.name).Scan(&next); err != nil {
				t.Fatal(err)
			}
		}
		if !next.Valid || next.Int64 <= entry.maxID {
			t.Fatalf("%s sequence did not advance", entry.name)
		}
	}
}
