package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/keisuk-t/grafana-sync/internal/syncdb"
)

func TestHelpAndValidation(t *testing.T) {
	for _, args := range [][]string{nil, {"help"}, {"--help"}, {"export", "-h"}, {"import", "-h"}} {
		var out, errOut bytes.Buffer
		if err := Run(context.Background(), args, &out, &errOut); err != nil {
			t.Fatal(err)
		}
		if out.Len()+errOut.Len() == 0 {
			t.Fatal("missing help")
		}
	}
	t.Setenv("GRAFANA_SYNC_DSN", "")
	for _, args := range [][]string{{"other"}, {"export"}, {"import", "-db", "sqlite", "-file", "x"}, {"export", "-db", "mysql", "-file", "x", "-timeout", "0s"}, {"export", "-db", "mysql", "-file", "x"}, {"export", "-unknown"}} {
		if err := Run(context.Background(), args, &bytes.Buffer{}, &bytes.Buffer{}); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
}

func TestWriteSnapshot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "snapshot.json")
	s := &syncdb.Snapshot{Version: 2,
		Org:        syncdb.Table{Columns: []string{"id"}, Rows: [][]*string{}},
		DataSource: syncdb.Table{Columns: []string{"id", "org_id", "name"}, Rows: [][]*string{}},
		DataKeys:   syncdb.Table{Columns: []string{"name", "encrypted_data"}, BinaryColumns: []string{"encrypted_data"}, Rows: [][]*string{}},
		Secrets:    syncdb.Table{Columns: []string{"id", "org_id", "namespace", "type", "value"}, Rows: [][]*string{}},
	}
	if err := writeSnapshot(path, s); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("permissions: %v %v", info, err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeSnapshot(path, s); err == nil {
		t.Fatal("overwrote existing dump")
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("modified existing file")
	}
	if _, err := syncdb.Decode(bytes.NewReader(after)); err != nil {
		t.Fatal(err)
	}
}

func TestImportValidatesBeforeConnecting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "invalid.json")
	if err := os.WriteFile(path, []byte(`{"version":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GRAFANA_SYNC_DSN", "invalid-dsn")
	err := Run(context.Background(), []string{"import", "-db", "mysql", "-file", path}, &bytes.Buffer{}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "version") {
		t.Fatalf("expected validation first: %v", err)
	}
}

func TestImportModeFlag(t *testing.T) {
	t.Setenv("GRAFANA_SYNC_DSN", "")
	for _, mode := range []string{"require-empty", "insert-only"} {
		err := Run(context.Background(), []string{"import", "-db", "mysql", "-file", "missing.json", "--mode", mode}, &bytes.Buffer{}, &bytes.Buffer{})
		if err == nil || err.Error() != "DSN environment variable is unset or empty" {
			t.Fatalf("mode %s rejected: %v", mode, err)
		}
	}
	for _, mode := range []string{"append", "upsert", ""} {
		err := Run(context.Background(), []string{"import", "-db", "mysql", "-file", "missing.json", "--mode", mode}, &bytes.Buffer{}, &bytes.Buffer{})
		if err == nil || !strings.Contains(err.Error(), "import mode must be") {
			t.Fatalf("mode %s accepted: %v", mode, err)
		}
	}
	if err := Run(context.Background(), []string{"export", "--mode", "insert-only"}, &bytes.Buffer{}, &bytes.Buffer{}); err == nil {
		t.Fatal("export accepted import-only mode")
	}
	var out bytes.Buffer
	if err := Run(context.Background(), []string{"import", "--help"}, &bytes.Buffer{}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "require-empty or insert-only") {
		t.Fatal("help omits import modes")
	}
}
