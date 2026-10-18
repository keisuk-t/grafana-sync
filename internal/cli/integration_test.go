//go:build integration

package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/keisuk-t/grafana-sync/internal/syncdb"
)

func TestImportModesWithDatabase(t *testing.T) {
	for _, engine := range []string{"mysql", "postgres"} {
		t.Run(engine, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			dsn := os.Getenv(strings.ToUpper(engine) + "_TEST_DSN")
			if dsn == "" {
				t.Fatal("a disposable test DB is required")
			}
			db, err := syncdb.Open(ctx, engine, dsn)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { db.DB.Close() })
			blob, suffix := "BYTEA", ""
			if engine == "mysql" {
				blob, suffix = "BLOB", " ENGINE=InnoDB"
			}
			for name, columns := range map[string]string{
				"org":         "id BIGINT PRIMARY KEY, name VARCHAR(100)",
				"data_source": "id BIGINT PRIMARY KEY, org_id BIGINT, name VARCHAR(100)",
				"data_keys":   "name VARCHAR(100) PRIMARY KEY, active SMALLINT NOT NULL, encrypted_data " + blob,
				"secrets":     "id BIGINT PRIMARY KEY, org_id BIGINT, namespace VARCHAR(100), type VARCHAR(100), value TEXT",
			} {
				if _, err := db.DB.ExecContext(ctx, "CREATE TABLE "+name+" ("+columns+")"+suffix); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if _, err := db.DB.Exec("DROP TABLE " + name); err != nil {
						t.Error(err)
					}
				})
			}
			ptr := func(s string) *string { return &s }
			s := &syncdb.Snapshot{Version: 2,
				Org:        syncdb.Table{Columns: []string{"id", "name"}, Rows: [][]*string{{ptr("1"), ptr("original")}}},
				DataSource: syncdb.Table{Columns: []string{"id", "org_id", "name"}, Rows: [][]*string{{ptr("1"), ptr("1"), ptr("metrics")}}},
				DataKeys:   syncdb.Table{Columns: []string{"name", "active", "encrypted_data"}, BinaryColumns: []string{"encrypted_data"}, Rows: [][]*string{{ptr("key"), ptr("1"), ptr("AP+A")}}},
				Secrets:    syncdb.Table{Columns: []string{"id", "org_id", "namespace", "type", "value"}, Rows: [][]*string{{ptr("1"), ptr("1"), ptr("metrics"), ptr("datasource"), ptr("opaque")}}},
			}
			path := filepath.Join(t.TempDir(), "initial.json")
			if err := writeSnapshot(path, s); err != nil {
				t.Fatal(err)
			}
			t.Setenv("GRAFANA_SYNC_DSN", dsn)
			var out bytes.Buffer
			run := func(path, mode, want string) {
				t.Helper()
				out.Reset()
				args := []string{"import", "-db", engine, "-file", path}
				if mode != "" {
					args = append(args, "--mode", mode)
				}
				if err := Run(ctx, args, &out, &bytes.Buffer{}); err != nil {
					t.Fatal(err)
				}
				orgIDs := make([]string, len(s.Org.Rows))
				for i, row := range s.Org.Rows {
					orgIDs[i] = *row[0]
				}
				if out.String() != "targets=org,datasource scope.org_ids="+strings.Join(orgIDs, ",")+"\n"+want+"\n" {
					t.Fatalf("unexpected output: %q", out.String())
				}
			}
			run(path, "", "import completed: mode=require-empty inserted=4 skipped=0")
			s.Org.Rows[0][1] = ptr("incoming-different-name")
			s.Org.Rows = append(s.Org.Rows, []*string{ptr("2"), ptr("new-org")})
			path = filepath.Join(t.TempDir(), "incremental.json")
			if err := writeSnapshot(path, s); err != nil {
				t.Fatal(err)
			}
			run(path, "insert-only", "import completed: mode=insert-only inserted=1 skipped=4")
			run(path, "insert-only", "import completed: mode=insert-only inserted=0 skipped=5")
			var name string
			if err := db.DB.QueryRowContext(ctx, "SELECT name FROM org WHERE id = 1").Scan(&name); err != nil || name != "original" {
				t.Fatal("existing row changed", err)
			}
			if err := Run(ctx, []string{"import", "-db", engine, "-file", path}, &out, &bytes.Buffer{}); err == nil {
				t.Fatal("default mode accepted populated tables")
			}
			s.DataSource.Rows[0][0] = ptr("2")
			s.Secrets.Rows[0][0] = ptr("2")
			path = filepath.Join(t.TempDir(), "sync.json")
			if err := writeSnapshot(path, s); err != nil {
				t.Fatal(err)
			}
			if err := Run(ctx, []string{"import", "-db", engine, "-file", path, "--mode", "sync"}, &out, &bytes.Buffer{}); err == nil {
				t.Fatal("sync accepted missing deletion approval")
			}
			runSync := func(option string) string {
				t.Helper()
				out.Reset()
				if err := Run(ctx, []string{"import", "-db", engine, "-file", path, "--mode", "sync", option}, &out, &bytes.Buffer{}); err != nil {
					t.Fatal(err)
				}
				return out.String()
			}
			if got := runSync("--dry-run"); !strings.Contains(got, "sync planned (no changes applied): inserted=2 updated=0 deleted=2") || !strings.Contains(got, "data_source: inserted=1 updated=0 deleted=1") {
				t.Fatalf("unexpected sync preview: %q", got)
			}
			var count int
			if err := db.DB.QueryRowContext(ctx, "SELECT COUNT(*) FROM data_source WHERE id = 1").Scan(&count); err != nil || count != 1 {
				t.Fatal("dry-run changed target", err)
			}
			if got := runSync("--allow-delete"); !strings.Contains(got, "sync completed: inserted=2 updated=0 deleted=2") {
				t.Fatalf("unexpected sync result: %q", got)
			}
			if err := db.DB.QueryRowContext(ctx, "SELECT COUNT(*) FROM data_source WHERE id = 1").Scan(&count); err != nil || count != 0 {
				t.Fatal("old data source remains", err)
			}
			if got := runSync("--allow-delete"); !strings.Contains(got, "sync completed: inserted=0 updated=0 deleted=0") {
				t.Fatalf("retry was not idempotent: %q", got)
			}
		})
	}
}
