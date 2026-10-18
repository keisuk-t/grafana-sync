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
		s.LibraryElement.Rows[0][8] = str(` {"uid":"shared","type":"timeseries","libraryPanel":{"uid":"","name":"共有パネル","version":1},"datasource":{"uid":"metrics-uid"}} `)
		for i := 1; i < 8; i++ {
			row := append([]*string{}, s.LibraryElement.Rows[0]...)
			row[0], row[4], row[5] = str(strconv.Itoa(40+i)), str("shared-"+strconv.Itoa(i)), str("共有パネル"+strconv.Itoa(i))
			row[8] = str(`{"uid":"shared-` + strconv.Itoa(i) + `","type":"timeseries","libraryPanel":{"uid":"shared-` + strconv.Itoa(i) + `"}}`)
			if i == 1 {
				row[8] = str(`{"uid":"shared-1","type":"timeseries","libraryPanel":{}}`)
			} else if i == 2 {
				row[8] = str(`{"uid":"shared-2","type":"timeseries","libraryPanel":{"name":"stale metadata","version":1}}`)
			} else if i <= 6 {
				row[8] = str(`{"uid":"shared-` + strconv.Itoa(i) + `","type":"timeseries","libraryPanel":{"uid":"","name":"stale metadata","version":1}}`)
			}
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
		// Grafana's schema permits dangling connections; relax this fixture's extra FK.
		constraintQuery := "SELECT CONSTRAINT_NAME FROM information_schema.KEY_COLUMN_USAGE WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'library_element_connection' AND COLUMN_NAME = 'connection_id' AND REFERENCED_TABLE_NAME = 'dashboard'"
		drop := "DROP FOREIGN KEY "
		if engine == "postgres" {
			constraintQuery = "SELECT conname FROM pg_constraint WHERE conrelid = 'library_element_connection'::regclass AND confrelid = 'dashboard'::regclass AND contype = 'f'"
			drop = "DROP CONSTRAINT "
		}
		var constraint string
		if err := db.DB.QueryRowContext(ctx, constraintQuery).Scan(&constraint); err != nil {
			t.Fatal(err)
		}
		if _, err := db.DB.ExecContext(ctx, "ALTER TABLE library_element_connection "+drop+db.quote(constraint)); err != nil {
			t.Fatal(err)
		}
		if _, err := db.DB.ExecContext(ctx, "DELETE FROM dashboard WHERE id BETWEEN 20 AND 25"); err != nil {
			t.Fatal(err)
		}
		original := dashboardDatabaseState(t, ctx, db) + rbacState(t, ctx, db) + rbacSequenceState(t, ctx, db)
		baseline, err := db.ExportWithOptions(ctx, AllTargets, ExportOptions{})
		if err != nil {
			t.Fatal(err)
		}
		want := baseline.Snapshot
		if baseline.OmittedOrphanLibraryConnections != 6 || len(want.LibraryConnection.Rows) != 8 || original != dashboardDatabaseState(t, ctx, db)+rbacState(t, ctx, db)+rbacSequenceState(t, ctx, db) {
			t.Fatal("export did not omit exactly six dangling connections without source changes")
		}
		for targets := Targets(1); targets <= AllTargets; targets++ {
			expected, err := want.Select(targets)
			if err != nil {
				t.Fatal(err)
			}
			count := 0
			if targets.Has(TargetDashboard) {
				count = 6
			}
			result, err := db.ExportWithOptions(ctx, targets, ExportOptions{})
			if err != nil || result.OmittedOrphanLibraryConnections != count || !reflect.DeepEqual(result.Snapshot, expected) {
				t.Fatal("connection omission did not respect target selection", targets, err)
			}
		}
		for _, tc := range []struct{ name, change, restore, diagnostic string }{
			{"orphan unsupported kind", "kind = 2 WHERE id = 108", "kind = 1 WHERE id = 108", "unsupported kind"},
			{"invalid dashboard ID", "connection_id = 0 WHERE id = 108", "connection_id = 20 WHERE id = 108", "missing dashboard"},
			{"folder reference", "connection_id = 11 WHERE id = 100", "connection_id = 12 WHERE id = 100", "missing dashboard"},
			{"other organization", "connection_id = 77 WHERE id = 100", "connection_id = 12 WHERE id = 100", "another organization"},
			{"invalid connection ID", "id = -108 WHERE id = 108", "id = 108 WHERE id = -108", "invalid id"},
		} {
			t.Run(engine+"/reject_connection_"+tc.name, func(t *testing.T) {
				if _, err := db.DB.ExecContext(ctx, "UPDATE library_element_connection SET "+tc.change); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if _, err := db.DB.ExecContext(ctx, "UPDATE library_element_connection SET "+tc.restore); err != nil {
						t.Error(err)
					}
				})
				before := dashboardDatabaseState(t, ctx, db) + rbacState(t, ctx, db) + rbacSequenceState(t, ctx, db)
				result, err := db.ExportWithOptions(ctx, AllTargets, ExportOptions{})
				if err == nil || result != nil || !strings.Contains(err.Error(), tc.diagnostic) || strings.Contains(err.Error(), "shared") || strings.Contains(err.Error(), "共有パネル") || before != dashboardDatabaseState(t, ctx, db)+rbacState(t, ctx, db)+rbacSequenceState(t, ctx, db) {
					t.Fatal("export omitted an ambiguous connection, exposed values or changed the source", err)
				}
			})
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
		if err != nil || result.RehomedLibraryPanels != 8 || result.OmittedOrphanLibraryConnections != 6 || !reflect.DeepEqual(result.Snapshot, want) {
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
			if err != nil || result.RehomedLibraryPanels != 8 || result.OmittedOrphanLibraryConnections != 6 || !reflect.DeepEqual(result.Snapshot, want) {
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
		renameOptions := ExportOptions{RehomeOrphanLibraryPanels: true, RenameCollidingLibraryPanels: true}
		renamedWant := cloneSnapshot(t, want)
		renamedWant.LibraryElement.Rows[1][columnIndex(renamedWant.LibraryElement.Columns, "name")] = str("共有パネル [migrated-41]")
		for targets := Targets(1); targets <= AllTargets; targets++ {
			if !targets.Has(TargetDashboard) {
				continue
			}
			expected, err := renamedWant.Select(targets)
			if err != nil {
				t.Fatal(err)
			}
			result, err := db.ExportWithOptions(ctx, targets, renameOptions)
			if err != nil || result.RehomedLibraryPanels != 8 || result.RenamedLibraryPanels != 1 || result.OmittedOrphanLibraryConnections != 6 || !reflect.DeepEqual(result.Snapshot, expected) || state() != collisionBefore {
				t.Fatal("rename changed more than the colliding orphan name and folder columns", targets, err)
			}
		}
		renamePath := filepath.Join(t.TempDir(), "renamed.dump.json")
		cmd := exec.CommandContext(ctx, binary, "export", "-db", engine, "-file", renamePath, "--targets", AllTargets.String(), "--rehome-orphan-library-panels", "--rename-colliding-library-panels")
		cmd.Env = append(os.Environ(), "GRAFANA_SYNC_DSN="+os.Getenv(strings.ToUpper(engine)+"_TEST_DSN"))
		out, err := cmd.CombinedOutput()
		if err != nil || !strings.Contains(string(out), "renamed 1 rehomed library panels") || !strings.Contains(string(out), "omitted 6 orphan library connections") || !strings.Contains(string(out), "panel model unchanged") || strings.Contains(string(out), "共有パネル") || strings.Contains(string(out), "shared") {
			t.Fatal("CLI did not safely report the rename count", err)
		}
		renamedData, err := os.ReadFile(renamePath)
		if err != nil {
			t.Fatal(err)
		}
		renamed, err := Decode(bytes.NewReader(renamedData))
		if err != nil || !reflect.DeepEqual(renamed, renamedWant) || state() != collisionBefore {
			t.Fatal("CLI renamed export lost model, connections or permissions or changed the source", err)
		}
		snapshots[engine+"-renamed"] = renamed
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
		if out, err := runCLI(true); err != nil || !strings.Contains(out, "rehomed 8 orphan library panels to General") || !strings.Contains(out, "omitted 6 orphan library connections") || !strings.Contains(out, "folder-inherited access may change") || strings.Contains(out, "shared") {
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
					invalid = cloneSnapshot(t, s)
					invalid.LibraryConnection.Rows = append(invalid.LibraryConnection.Rows, []*string{str("200"), str("40"), str("1"), str("99999")})
					if mode == ImportSync {
						if _, err := db.Sync(ctx, invalid, SyncOptions{DryRun: true, PrincipalMap: mapping}); err == nil {
							t.Fatal("dry-run accepted an orphan connection in input")
						}
						_, err = db.Sync(ctx, invalid, SyncOptions{AllowDelete: true, PrincipalMap: mapping})
					} else {
						_, err = db.ImportWithOptions(ctx, invalid, mode, ImportOptions{PrincipalMap: mapping})
					}
					if err == nil || !strings.Contains(err.Error(), "missing dashboard") || before != dashboardDatabaseState(t, ctx, db)+rbacState(t, ctx, db)+rbacSequenceState(t, ctx, db) {
						t.Fatal("import/sync omitted an invalid input connection or changed the target", err)
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
					if mode == ImportSync && strings.HasSuffix(source, "-renamed") {
						if _, err := db.DB.ExecContext(ctx, "UPDATE library_element SET name = "+db.bind(1)+" WHERE id = 41", "destination-name"); err != nil {
							t.Fatal(err)
						}
						before := dashboardDatabaseState(t, ctx, db) + rbacState(t, ctx, db) + rbacSequenceState(t, ctx, db)
						result, err := db.Sync(ctx, s, SyncOptions{DryRun: true, PrincipalMap: mapping})
						if err != nil || result.LibraryElement.Updated != 1 || before != dashboardDatabaseState(t, ctx, db)+rbacState(t, ctx, db)+rbacSequenceState(t, ctx, db) {
							t.Fatal("renamed snapshot dry-run did not plan a name update without writing", err)
						}
						if _, err := db.Sync(ctx, s, SyncOptions{AllowDelete: true, PrincipalMap: mapping}); err != nil {
							t.Fatal("could not sync the renamed library panel", err)
						}
					}
					got, err := db.ExportTargets(ctx, AllTargets)
					if err != nil || !reflect.DeepEqual(got, snapshot) {
						t.Fatal("repaired panels, eight live connections or explicit permissions did not round trip", err)
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
		s := librarySnapshot()
		s.LibraryElement.Rows[0][8] = str(` {"uid":"shared","type":"timeseries","libraryPanel":{"uid":"","name":"共有パネル","version":1},"datasource":{"uid":"metrics-uid"}} `)
		s, err = s.Select(TargetDashboard)
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
	for _, tc := range []struct{ model, diagnostic string }{
		{`{"libraryPanel":{"uid":"foreign-uid-canary"}}`, "references another library panel at its root"},
		{`{"libraryPanel":{"uid":null,"name":"private-name-canary"}}`, "invalid top-level libraryPanel metadata"},
		{`{"libraryPanel":{"uid":false,"name":"private-name-canary"}}`, "invalid top-level libraryPanel metadata"},
		{`{"libraryPanel":{"uid":"shared"},"options":{"libraryPanel":{"uid":"shared"}}}`, "below the model root"},
		{`{"libraryPanel":{"name":"private-name-canary"},"options":{"libraryPanel":{"uid":"foreign-uid-canary"}}}`, "below the model root"},
		{`{"libraryPanel":{"uid":"","name":"private-name-canary"},"options":{"libraryPanel":{"uid":"foreign-uid-canary"}}}`, "below the model root"},
	} {
		s := cloneSnapshot(t, source)
		s.LibraryElement.Rows[0][columnIndex(s.LibraryElement.Columns, "model")] = str(tc.model)
		assertRejected(s)
		if _, err := db.DB.ExecContext(ctx, "UPDATE library_element SET model = "+db.bind(1)+" WHERE id = 40", tc.model); err != nil {
			t.Fatal(err)
		}
		before := dashboardDatabaseState(t, ctx, db)
		if _, err := db.ExportTargets(ctx, AllTargets); err == nil || !strings.Contains(err.Error(), tc.diagnostic) || !strings.Contains(err.Error(), "(panel ID 40)") || strings.Contains(err.Error(), "canary") || strings.Contains(err.Error(), "shared") {
			t.Fatal("export accepted unsupported model references or exposed their values", err)
		}
		if dashboardDatabaseState(t, ctx, db) != before {
			t.Fatal("rejected library export modified the source")
		}
		reset()
	}
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
