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

func TestPrincipalMapFlag(t *testing.T) {
	path := filepath.Join(t.TempDir(), "snapshot.json")
	if err := os.WriteFile(path, []byte(`{"version":5,"targets":["org"],"scope":{"org_ids":[]},"tables":{"org":{"columns":["id","name"],"rows":[]}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	mapping := filepath.Join(t.TempDir(), "principals.json")
	t.Setenv("GRAFANA_SYNC_DSN", "invalid-dsn")
	for _, data := range []string{`{"version":2,"mappings":[]}`, `{"version":1,"mappings":[{"org_id":"1","kind":"user","source_id":1,"target_id":"2","target_uid":"uid"}]}`, `{"password":"must-not-be-logged"}`} {
		if err := os.WriteFile(mapping, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
		err := Run(context.Background(), []string{"import", "-db", "mysql", "-file", path, "--targets", "org", "--principal-map", mapping}, &bytes.Buffer{}, &bytes.Buffer{})
		if err == nil || !strings.Contains(err.Error(), "principal map") || strings.Contains(err.Error(), "must-not-be-logged") {
			t.Fatal("invalid principal map was not safely rejected before connecting", err)
		}
	}
}

func TestRehomeOrphanLibraryPanelsFlag(t *testing.T) {
	t.Setenv("GRAFANA_SYNC_DSN", "")
	for targets := syncdb.Targets(1); targets <= syncdb.AllTargets; targets++ {
		err := Run(context.Background(), []string{"export", "-db", "mysql", "-file", "missing.json", "--targets", targets.String(), "--rehome-orphan-library-panels"}, &bytes.Buffer{}, &bytes.Buffer{})
		want := "--rehome-orphan-library-panels requires --targets including dashboard"
		if targets.Has(syncdb.TargetDashboard) {
			want = "DSN environment variable is unset or empty"
		}
		if err == nil || err.Error() != want {
			t.Fatal("unexpected flag validation", targets, err)
		}
	}
	if err := Run(context.Background(), []string{"import", "--rehome-orphan-library-panels"}, &bytes.Buffer{}, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "flag provided but not defined") {
		t.Fatal("import accepted export-only repair option", err)
	}
	var help bytes.Buffer
	if err := Run(context.Background(), []string{"export", "--help"}, &bytes.Buffer{}, &help); err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"rehome-orphan-library-panels", "General", "access may change"} {
		if !strings.Contains(help.String(), text) {
			t.Fatal("help omits rehome restriction or risk", text)
		}
	}
}

func TestRenameCollidingLibraryPanelsFlag(t *testing.T) {
	t.Setenv("GRAFANA_SYNC_DSN", "")
	for _, rehome := range []bool{false, true} {
		for targets := syncdb.Targets(1); targets <= syncdb.AllTargets; targets++ {
			args := []string{"export", "-db", "mysql", "-file", "missing.json", "--targets", targets.String(), "--rename-colliding-library-panels"}
			want := "--rename-colliding-library-panels requires --rehome-orphan-library-panels"
			if rehome {
				args = append(args, "--rehome-orphan-library-panels")
				want = "--rehome-orphan-library-panels requires --targets including dashboard"
				if targets.Has(syncdb.TargetDashboard) {
					want = "DSN environment variable is unset or empty"
				}
			}
			if err := Run(context.Background(), args, &bytes.Buffer{}, &bytes.Buffer{}); err == nil || err.Error() != want {
				t.Fatal("rename flag accepted without required options or failed before DSN check", targets, err)
			}
		}
	}
	if err := Run(context.Background(), []string{"import", "--rename-colliding-library-panels"}, &bytes.Buffer{}, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "flag provided but not defined") {
		t.Fatal("import accepted export-only name repair", err)
	}
	var help bytes.Buffer
	if err := Run(context.Background(), []string{"export", "--help"}, &bytes.Buffer{}, &help); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(help.String(), "rename-colliding-library-panels") || !strings.Contains(help.String(), "requires --rehome-orphan-library-panels") {
		t.Fatal("help omitted rename restrictions")
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
	if !strings.Contains(out.String(), "require-empty, insert-only or sync") {
		t.Fatal("help omits import modes")
	}
	for _, args := range [][]string{
		{"import", "-db", "mysql", "-file", "missing.json", "--mode", "sync"},
		{"import", "-db", "mysql", "-file", "missing.json", "--dry-run"},
		{"import", "-db", "mysql", "-file", "missing.json", "--allow-delete"},
	} {
		if err := Run(context.Background(), args, &bytes.Buffer{}, &bytes.Buffer{}); err == nil || strings.Contains(err.Error(), "DSN") {
			t.Fatalf("accepted unapproved sync/invalid option: %v %v", args, err)
		}
	}
	for _, option := range []string{"--dry-run", "--allow-delete"} {
		if err := Run(context.Background(), []string{"import", "-db", "mysql", "-file", "missing.json", "--mode", "sync", option}, &bytes.Buffer{}, &bytes.Buffer{}); err == nil || err.Error() != "DSN environment variable is unset or empty" {
			t.Fatalf("rejected approved sync: %v", err)
		}
	}
}

func TestTargetsFlag(t *testing.T) {
	t.Setenv("GRAFANA_SYNC_DSN", "")
	for _, command := range []string{"export", "import"} {
		for targets := syncdb.Targets(1); targets <= syncdb.AllTargets; targets++ {
			err := Run(context.Background(), []string{command, "-db", "mysql", "-file", "missing.json", "--targets", targets.String()}, &bytes.Buffer{}, &bytes.Buffer{})
			if err == nil || err.Error() != "DSN environment variable is unset or empty" {
				t.Fatalf("valid targets rejected: %v", err)
			}
		}
		for _, value := range []string{"", "org,", "data_source", "dashboard,dashboard", "unknown"} {
			err := Run(context.Background(), []string{command, "-db", "mysql", "-file", "missing.json", "--targets", value}, &bytes.Buffer{}, &bytes.Buffer{})
			if err == nil || strings.Contains(err.Error(), "DSN") {
				t.Fatalf("invalid targets accepted: %v", err)
			}
		}
	}
	path := filepath.Join(t.TempDir(), "org.json")
	s := &syncdb.Snapshot{Version: 3, Targets: []string{"org"}, Scope: syncdb.Scope{OrgIDs: []string{}}, Org: syncdb.Table{Columns: []string{"id"}, Rows: [][]*string{}}}
	if err := writeSnapshot(path, s); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GRAFANA_SYNC_DSN", "invalid-dsn")
	if err := Run(context.Background(), []string{"import", "-db", "mysql", "-file", path}, &bytes.Buffer{}, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "absent") {
		t.Fatalf("default inferred absent targets: %v", err)
	}
}

func TestSelectionExplainsPublicSharingExclusion(t *testing.T) {
	var out bytes.Buffer
	s := &syncdb.Snapshot{Version: 3, Targets: []string{"dashboard"}}
	if err := printSelection(&out, s); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "public sharing settings excluded (existing target sharing unchanged)") {
		t.Fatal("selection output does not explain public sharing exclusion")
	}
}
