//go:build integration

package syncdb

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestOrphanLibraryFolders(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	binary := filepath.Join(t.TempDir(), "grafana-sync")
	if out, err := exec.CommandContext(ctx, "go", "build", "-o", binary, "../../cmd/grafana-sync").CombinedOutput(); err != nil {
		t.Fatalf("build CLI: %v %s", err, out)
	}
	databases, snapshots := map[string]*Database{}, map[string]*Snapshot{}
	for _, engine := range []string{"mysql", "postgres"} {
		db, err := Open(ctx, engine, os.Getenv(strings.ToUpper(engine)+"_TEST_DSN"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { db.DB.Close() })
		createRBACFixtures(t, ctx, db)
		t.Cleanup(func() {
			for _, name := range []string{"user_role", "team_role", "dashboard_acl", "team_member", "team", "org_user", "user"} {
				if _, err := db.DB.Exec("DROP TABLE " + db.quote(name)); err != nil {
					t.Error(err)
				}
			}
		})
		seedFixtures(t, ctx, db)
		s := librarySnapshot()
		for i := 1; i < 8; i++ {
			row := append([]*string{}, s.LibraryElement.Rows[0]...)
			row[0], row[4], row[5] = str(strconv.Itoa(40+i)), str("shared-"+strconv.Itoa(i)), str("共有パネル"+strconv.Itoa(i))
			row[8] = str(`{"uid":"shared-` + strconv.Itoa(i) + `","type":"timeseries"}`)
			s.LibraryElement.Rows = append(s.LibraryElement.Rows, row)
		}
		s, err = s.Select(TargetDashboard)
		if err != nil {
			t.Fatal(err)
		}
		if err := db.Import(ctx, s); err != nil {
			t.Fatal(err)
		}
		seedRBACPrincipals(t, ctx, db, false)
		for _, spec := range []struct{ kind, id, action string }{{"user", "101", "library.panels:read"}, {"team", "201", "library.panels:write"}} {
			seedRBACRole(t, ctx, db, RBACRole{OrgID: s.Scope.OrgIDs[0], Kind: spec.kind, Name: "managed:" + spec.kind + "s:" + spec.id + ":permissions", UID: "orphan-" + spec.kind, Permissions: []RBACPermission{{Action: spec.action, Scope: "library.panels:uid:shared", Kind: "library.panels", Attribute: "uid", Identifier: "shared"}}, Bindings: []RBACBinding{{OrgID: s.Scope.OrgIDs[0], Kind: spec.kind, ID: spec.id}}})
		}
		if _, err := db.DB.ExecContext(ctx, "INSERT INTO "+db.quote("org")+" (id, version, name, created, updated) VALUES (77, 1, 'other-org', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)"); err != nil {
			t.Fatal(err)
		}
		if _, err := db.DB.ExecContext(ctx, "INSERT INTO dashboard (id, org_id, uid, title, data, version, is_folder, folder_id, has_acl, is_public) VALUES (77, 77, 'other-board', 'Other board', '{}', 1, FALSE, 0, FALSE, FALSE)"); err != nil {
			t.Fatal(err)
		}
		want, err := db.ExportTargets(ctx, AllTargets)
		if err != nil {
			t.Fatal(err)
		}
		for _, row := range want.LibraryElement.Rows {
			row[columnIndex(want.LibraryElement.Columns, "folder_id")] = str("0")
			row[columnIndex(want.LibraryElement.Columns, "folder_uid")] = str("general")
		}
		if _, err := db.DB.ExecContext(ctx, "UPDATE library_element SET folder_id = id + 1000, folder_uid = NULL"); err != nil {
			t.Fatal(err)
		}
		state := func() string {
			return dashboardDatabaseState(t, ctx, db) + rbacState(t, ctx, db) + rbacSequenceState(t, ctx, db)
		}
		before := state()
		if _, err := db.ExportTargets(ctx, AllTargets); err == nil || !strings.Contains(err.Error(), "--rehome-orphan-library-panels") {
			t.Fatal("default export accepted missing library folders", err)
		}
		result, err := db.ExportWithOptions(ctx, AllTargets, ExportOptions{RehomeOrphanLibraryPanels: true})
		if err != nil || result.RehomedLibraryPanels != 8 || !reflect.DeepEqual(result.Snapshot, want) {
			t.Fatal("rehome did not preserve all other snapshot fields, references and RBAC", err)
		}
		if state() != before {
			t.Fatal("export changed source data, permissions, principals or sequences")
		}
		for targets := Targets(1); targets <= AllTargets; targets++ {
			if !targets.Has(TargetDashboard) {
				continue
			}
			want, err := want.Select(targets)
			if err != nil {
				t.Fatal(err)
			}
			result, err := db.ExportWithOptions(ctx, targets, ExportOptions{RehomeOrphanLibraryPanels: true})
			if err != nil || result.RehomedLibraryPanels != 8 || !reflect.DeepEqual(result.Snapshot, want) {
				t.Fatal("rehome did not respect target selection", targets, err)
			}
		}
		for _, tc := range []struct {
			name, id string
			uid      any
		}{{"existing folder", "11", nil}, {"existing dashboard", "12", nil}, {"other organization", "77", nil}, {"conflicting general", "1040", "general"}, {"nonempty missing UID", "1040", "missing"}, {"existing UID", "1040", "child"}} {
			t.Run(engine+"/reject_"+tc.name, func(t *testing.T) {
				if _, err := db.DB.ExecContext(ctx, "UPDATE library_element SET folder_id = "+db.bind(1)+", folder_uid = "+db.bind(2)+" WHERE id = 40", tc.id, tc.uid); err != nil {
					t.Fatal(err)
				}
				before := state()
				if result, err := db.ExportWithOptions(ctx, AllTargets, ExportOptions{RehomeOrphanLibraryPanels: true}); err == nil || result != nil {
					t.Fatal("rehome accepted ambiguous folder metadata")
				}
				if state() != before {
					t.Fatal("failed export modified the source")
				}
				if _, err := db.DB.ExecContext(ctx, "UPDATE library_element SET folder_id = 1040, folder_uid = NULL WHERE id = 40"); err != nil {
					t.Fatal(err)
				}
			})
		}
		if _, err := db.DB.ExecContext(ctx, "UPDATE library_element SET folder_uid = '' WHERE id = 40"); err != nil {
			t.Fatal(err)
		}
		emptyBefore := state()
		if result, err := db.ExportWithOptions(ctx, AllTargets, ExportOptions{RehomeOrphanLibraryPanels: true}); err != nil || result.RehomedLibraryPanels != 8 || !reflect.DeepEqual(result.Snapshot, want) || state() != emptyBefore {
			t.Fatal("empty UID orphan was not safely rehomed", err)
		}
		if _, err := db.DB.ExecContext(ctx, "UPDATE library_element SET folder_uid = NULL WHERE id = 40"); err != nil {
			t.Fatal(err)
		}
		if _, err := db.DB.ExecContext(ctx, "UPDATE library_element SET name = "+db.bind(1)+" WHERE id = 41", "共有パネル"); err != nil {
			t.Fatal(err)
		}
		collisionBefore := state()
		if result, err := db.ExportWithOptions(ctx, AllTargets, ExportOptions{RehomeOrphanLibraryPanels: true}); err == nil || !strings.Contains(err.Error(), "duplicate name") || result != nil || state() != collisionBefore {
			t.Fatal("rehome did not safely reject a General name collision", err)
		}
		if _, err := db.DB.ExecContext(ctx, "UPDATE library_element SET name = "+db.bind(1)+" WHERE id = 41", "共有パネル1"); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(t.TempDir(), "rehome.dump.json")
		runCLI := func(rehome bool) (string, error) {
			args := []string{"export", "-db", engine, "-file", path, "--targets", AllTargets.String()}
			if rehome {
				args = append(args, "--rehome-orphan-library-panels")
			}
			cmd := exec.CommandContext(ctx, binary, args...)
			cmd.Env = append(os.Environ(), "GRAFANA_SYNC_DSN="+os.Getenv(strings.ToUpper(engine)+"_TEST_DSN"))
			out, err := cmd.CombinedOutput()
			return string(out), err
		}
		if out, err := runCLI(false); err == nil || !strings.Contains(out, "--rehome-orphan-library-panels") {
			t.Fatal("CLI did not reject orphan folders by default", out, err)
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatal("failed CLI export created a file", err)
		}
		if out, err := runCLI(true); err != nil || !strings.Contains(out, "rehomed 8 orphan library panels to General") || !strings.Contains(out, "folder-inherited access may change") || strings.Contains(out, "shared") {
			t.Fatal("CLI omitted safe repair count or access warning", out, err)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		got, err := Decode(bytes.NewReader(data))
		if err != nil || !reflect.DeepEqual(got, want) || state() != before {
			t.Fatal("CLI changed source state or lost repaired snapshot data", err)
		}
		if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
			t.Fatal("repaired dump permissions", err)
		}
		if out, err := runCLI(true); err == nil || !strings.Contains(out, "existing files are not overwritten") {
			t.Fatal("repaired export overwrote an existing file", out, err)
		}
		if after, err := os.ReadFile(path); err != nil || !bytes.Equal(data, after) || state() != before {
			t.Fatal("failed repeated export changed the file or source", err)
		}
		databases[engine], snapshots[engine] = db, got
	}
	for source, snapshot := range snapshots {
		for target, db := range databases {
			for _, mode := range []ImportMode{ImportRequireEmpty, ImportInsertOnly, ImportSync} {
				t.Run(source+"_to_"+target+"/"+string(mode), func(t *testing.T) {
					clearRBACFixtures(t, ctx, db)
					base, err := snapshot.Select(DefaultTargets)
					if err != nil {
						t.Fatal(err)
					}
					if err := db.Import(ctx, base); err != nil {
						t.Fatal(err)
					}
					mapping := seedRBACPrincipals(t, ctx, db, false)
					s, err := snapshot.Select(TargetDashboard)
					if err != nil {
						t.Fatal(err)
					}
					before := dashboardDatabaseState(t, ctx, db) + rbacState(t, ctx, db) + rbacSequenceState(t, ctx, db)
					invalid := cloneSnapshot(t, s)
					invalid.LibraryElement.Rows[0][columnIndex(invalid.LibraryElement.Columns, "folder_id")] = str("1040")
					invalid.LibraryElement.Rows[0][columnIndex(invalid.LibraryElement.Columns, "folder_uid")] = nil
					if mode == ImportSync {
						_, err = db.Sync(ctx, invalid, SyncOptions{AllowDelete: true, PrincipalMap: mapping})
					} else {
						_, err = db.ImportWithOptions(ctx, invalid, mode, ImportOptions{PrincipalMap: mapping})
					}
					if err == nil || !strings.Contains(err.Error(), "missing folder") || before != dashboardDatabaseState(t, ctx, db)+rbacState(t, ctx, db)+rbacSequenceState(t, ctx, db) {
						t.Fatal("import/sync silently repaired an orphan or changed the target", err)
					}
					if mode == ImportSync {
						if _, err := db.Sync(ctx, s, SyncOptions{DryRun: true, PrincipalMap: mapping}); err != nil {
							t.Fatal(err)
						}
						if before != dashboardDatabaseState(t, ctx, db)+rbacState(t, ctx, db)+rbacSequenceState(t, ctx, db) {
							t.Fatal("repaired snapshot dry-run changed the target")
						}
						if _, err := db.Sync(ctx, s, SyncOptions{AllowDelete: true, PrincipalMap: mapping}); err != nil {
							t.Fatal(err)
						}
					} else if _, err := db.ImportWithOptions(ctx, s, mode, ImportOptions{PrincipalMap: mapping}); err != nil {
						t.Fatal(err)
					}
					got, err := db.ExportTargets(ctx, AllTargets)
					if err != nil || !reflect.DeepEqual(got, snapshot) {
						t.Fatal("repaired panels, 14 connections or explicit permissions did not round trip", err)
					}
				})
			}
		}
	}
}

func TestLibraryTransfer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	dbs := map[string]*Database{}
	snapshots := map[string]*Snapshot{}
	for _, engine := range []string{"mysql", "postgres"} {
		db, err := Open(ctx, engine, os.Getenv(strings.ToUpper(engine)+"_TEST_DSN"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { db.DB.Close() })
		createDashboardFixtures(t, ctx, db)
		seedFixtures(t, ctx, db)
		s, err := librarySnapshot().Select(TargetDashboard)
		if err != nil {
			t.Fatal(err)
		}
		if err := db.Import(ctx, s); err != nil {
			t.Fatal(err)
		}
		snapshots[engine], err = db.exportLegacyTargets(ctx, AllTargets)
		if err != nil {
			t.Fatal(err)
		}
		dbs[engine] = db
	}
	if !reflect.DeepEqual(snapshots["mysql"], snapshots["postgres"]) {
		t.Fatal("library exports differ between engines")
	}
	for source, snapshot := range snapshots {
		for target, db := range dbs {
			for _, mode := range []ImportMode{ImportRequireEmpty, ImportInsertOnly, ImportSync} {
				t.Run(source+"_to_"+target+"/"+string(mode), func(t *testing.T) {
					clearDashboardFixtures(t, ctx, db)
					base, err := snapshot.Select(DefaultTargets)
					if err != nil {
						t.Fatal(err)
					}
					if err := db.Import(ctx, base); err != nil {
						t.Fatal(err)
					}
					s, err := snapshot.Select(TargetDashboard)
					if err != nil {
						t.Fatal(err)
					}
					before := dashboardDatabaseState(t, ctx, db)
					if mode == ImportSync {
						if _, err := db.Sync(ctx, s, SyncOptions{DryRun: true}); err != nil {
							t.Fatal(err)
						}
						if dashboardDatabaseState(t, ctx, db) != before {
							t.Fatal("dry-run changed data or sequences")
						}
						if _, err := db.Sync(ctx, s, SyncOptions{AllowDelete: true}); err != nil {
							t.Fatal(err)
						}
					} else if _, err := db.ImportWithMode(ctx, s, mode); err != nil {
						t.Fatal(err)
					}
					got, err := db.exportLegacyTargets(ctx, AllTargets)
					if err != nil || !reflect.DeepEqual(got, snapshot) {
						t.Fatal("shared panel and 14 connections did not round trip", err)
					}
					before = dashboardDatabaseState(t, ctx, db)
					if _, err := db.ImportWithMode(ctx, base, ImportInsertOnly); err != nil || dashboardDatabaseState(t, ctx, db) != before {
						t.Fatal("default targets changed library data", err)
					}
					s = cloneSnapshot(t, s)
					s.LibraryElement.Rows[0][columnIndex(s.LibraryElement.Columns, "name")] = str("スキップする名前変更")
					if _, err := db.ImportWithMode(ctx, s, ImportInsertOnly); err != nil || dashboardDatabaseState(t, ctx, db) != before {
						t.Fatal("insert-only changed existing library or children", err)
					}
				})
			}
		}
	}
	for engine, db := range dbs {
		t.Run(engine+"_library_sync", func(t *testing.T) {
			testLibrarySync(t, ctx, db, snapshots[engine])
		})
	}
}

func testLibrarySync(t *testing.T, ctx context.Context, db *Database, source *Snapshot) {
	reset := func() {
		clearDashboardFixtures(t, ctx, db)
		if err := db.Import(ctx, source); err != nil {
			t.Fatal(err)
		}
	}
	assertRejected := func(s *Snapshot) {
		before := dashboardDatabaseState(t, ctx, db)
		if _, err := db.Sync(ctx, s, SyncOptions{AllowDelete: true}); err == nil {
			t.Fatal("unsafe library sync was accepted")
		}
		if dashboardDatabaseState(t, ctx, db) != before {
			t.Fatal("failed library sync changed data or sequences")
		}
	}
	reset()
	s := cloneSnapshot(t, source)
	s.LibraryElement.Rows[0][columnIndex(s.LibraryElement.Columns, "model")] = str(`{"uid":"shared","type":"timeseries","datasource":{"uid":"missing"}}`)
	assertRejected(s)
	s = cloneSnapshot(t, source)
	s.LibraryElement.Rows[0][columnIndex(s.LibraryElement.Columns, "type")] = str(strings.Repeat("x", 41))
	s.LibraryElement.Rows[0][columnIndex(s.LibraryElement.Columns, "model")] = str(`{"uid":"shared","type":"` + strings.Repeat("x", 41) + `"}`)
	s.Dashboard.Rows[2][columnIndex(s.Dashboard.Columns, "data")] = str(`{"description":"must roll back","panels":[{"libraryPanel":{"uid":"shared"}}]}`)
	assertRejected(s)
	if _, err := db.DB.ExecContext(ctx, "UPDATE library_element_connection SET kind = 2 WHERE id = 100"); err != nil {
		t.Fatal(err)
	}
	assertRejected(source)
	reset()
	if _, err := db.DB.ExecContext(ctx, "UPDATE dashboard SET org_id = 77 WHERE id = 13"); err != nil {
		t.Fatal(err)
	}
	s = cloneSnapshot(t, source)
	s.Dashboard.Rows = append(s.Dashboard.Rows[:3], s.Dashboard.Rows[4:]...)
	s.LibraryConnection.Rows = append(s.LibraryConnection.Rows[:1], s.LibraryConnection.Rows[2:]...)
	before := dashboardDatabaseState(t, ctx, db)
	if _, err := db.Sync(ctx, s, SyncOptions{AllowDelete: true}); err != nil || dashboardDatabaseState(t, ctx, db) != before {
		t.Fatal("sync failed to retain an out-of-scope dashboard and connection", err)
	}
	s.LibraryElement.Rows[0][columnIndex(s.LibraryElement.Columns, "name")] = str("外部参照を持つパネル")
	assertRejected(s)
	reset()
	if _, err := db.DB.ExecContext(ctx, "INSERT INTO "+db.quote("role")+" (id,org_id,name,version,uid,created,updated) VALUES (9000,9007199254740993,'custom-library-role',0,'custom-library-role','2026-01-02 03:04:05','2026-01-02 03:04:05')"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.ExecContext(ctx, "INSERT INTO permission (id,role_id,action,scope,kind,attribute,identifier,created,updated) VALUES (9000,9000,'library.panels:read','library.panels:uid:shared','library.panels','uid','shared','2026-01-02 03:04:05','2026-01-02 03:04:05')"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.exportLegacyTargets(ctx, TargetDashboard); err == nil {
		t.Fatal("export ignored library-specific custom-role permission")
	}
	assertRejected(source)
	reset()
	legacy := cloneSnapshot(t, source)
	legacy.Version, legacy.LibraryElement, legacy.LibraryConnection = 3, Table{}, Table{}
	for _, row := range legacy.Dashboard.Rows {
		row[columnIndex(legacy.Dashboard.Columns, "data")] = str(`{}`)
	}
	for _, row := range legacy.DashboardVersion.Rows {
		row[columnIndex(legacy.DashboardVersion.Columns, "data")] = str(`{}`)
	}
	permissions := []DashboardPermission{}
	for _, p := range legacy.DashboardPermissions {
		if !strings.HasPrefix(p.Scope, "library.panels:") {
			permissions = append(permissions, p)
		}
	}
	legacy.DashboardPermissions = permissions
	if _, err := db.Sync(ctx, legacy, SyncOptions{AllowDelete: true}); err != nil {
		t.Fatal("legacy sync failed to preserve libraries", err)
	}
	got, err := db.exportLegacyTargets(ctx, AllTargets)
	if err != nil || !reflect.DeepEqual(got.LibraryElement, source.LibraryElement) || !reflect.DeepEqual(got.LibraryConnection, source.LibraryConnection) {
		t.Fatal("legacy sync changed libraries", err)
	}
	for _, entry := range legacy.entries() {
		if entry.group == TargetDashboard {
			entry.table.Rows = [][]*string{}
		}
	}
	legacy.DashboardPermissions = nil
	assertRejected(legacy)
	reset()
	s = cloneSnapshot(t, source)
	other := append([]*string{}, s.LibraryElement.Rows[0]...)
	other[columnIndex(s.LibraryElement.Columns, "id")] = str("41")
	other[columnIndex(s.LibraryElement.Columns, "uid")] = str("alternate")
	other[columnIndex(s.LibraryElement.Columns, "name")] = str("別パネル")
	other[columnIndex(s.LibraryElement.Columns, "model")] = str(`{"uid":"alternate","type":"timeseries"}`)
	s.LibraryElement.Rows = append(s.LibraryElement.Rows, other)
	if _, err := db.Sync(ctx, s, SyncOptions{AllowDelete: true}); err != nil {
		t.Fatal("failed to add a second library panel", err)
	}
	for i, uid := range []string{"alternate", "shared"} {
		row := s.LibraryElement.Rows[i]
		row[columnIndex(s.LibraryElement.Columns, "uid")] = str(uid)
		row[columnIndex(s.LibraryElement.Columns, "model")] = str(`{"uid":"` + uid + `","type":"timeseries"}`)
	}
	name := columnIndex(s.LibraryElement.Columns, "name")
	s.LibraryElement.Rows[0][name], s.LibraryElement.Rows[1][name] = s.LibraryElement.Rows[1][name], s.LibraryElement.Rows[0][name]
	for _, table := range []*Table{&s.Dashboard, &s.DashboardVersion} {
		data := columnIndex(table.Columns, "data")
		for _, row := range table.Rows {
			row[data] = str(strings.ReplaceAll(*row[data], `"shared"`, `"alternate"`))
		}
	}
	if _, err := db.Sync(ctx, s, SyncOptions{AllowDelete: true}); err != nil {
		t.Fatal("failed to exchange library UIDs and names", err)
	}
	s.LibraryElement.Rows[0][columnIndex(s.LibraryElement.Columns, "folder_id")] = str("0")
	s.LibraryElement.Rows[0][columnIndex(s.LibraryElement.Columns, "folder_uid")] = str("")
	if _, err := db.Sync(ctx, s, SyncOptions{AllowDelete: true}); err != nil {
		t.Fatal("failed to move a library panel to the root folder", err)
	}
	reset()
	s = cloneSnapshot(t, source)
	s.LibraryElement.Rows[0][columnIndex(s.LibraryElement.Columns, "id")] = str("41")
	for _, row := range s.LibraryConnection.Rows {
		row[columnIndex(s.LibraryConnection.Columns, "element_id")] = str("41")
	}
	if _, err := db.Sync(ctx, s, SyncOptions{AllowDelete: true}); err != nil {
		t.Fatal("failed to recreate a shared library element with a new ID", err)
	}
	s.LibraryElement.Rows[0][columnIndex(s.LibraryElement.Columns, "name")] = str("更新後")
	if _, err := db.Sync(ctx, s, SyncOptions{AllowDelete: true}); err != nil {
		t.Fatal(err)
	}
	result, err := db.Sync(ctx, s, SyncOptions{AllowDelete: true})
	if err != nil || result.Inserted+result.Updated+result.Deleted != 0 {
		t.Fatal("library sync is not idempotent", err)
	}
	for _, entry := range s.entries() {
		if entry.group == TargetDashboard {
			entry.table.Rows = [][]*string{}
		}
	}
	s.DashboardPermissions = nil
	if _, err := db.Sync(ctx, s, SyncOptions{AllowDelete: true}); err != nil {
		t.Fatal("failed to delete connections before library and dashboard parents", err)
	}
}
