//go:build integration

package syncdb

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestLibraryExclusionTransfer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	binary := filepath.Join(t.TempDir(), "grafana-sync")
	if out, err := exec.CommandContext(ctx, "go", "build", "-o", binary, "../../cmd/grafana-sync").CombinedOutput(); err != nil {
		t.Fatalf("build CLI: %v %s", err, out)
	}
	databases, snapshots := map[string]*Database{}, map[string]*Snapshot{}
	state := func(db *Database) string {
		return dashboardDatabaseState(t, ctx, db) + rbacState(t, ctx, db) + rbacSequenceState(t, ctx, db)
	}
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
		s, err := librarySnapshot().Select(TargetDashboard)
		if err != nil {
			t.Fatal(err)
		}
		if err := db.Import(ctx, s); err != nil {
			t.Fatal(err)
		}
		seedRBACPrincipals(t, ctx, db, false)
		org := s.Scope.OrgIDs[0]
		seedRBACRole(t, ctx, db, RBACRole{OrgID: org, Kind: "user", Name: "managed:users:101:permissions", UID: "excluded-only-user", Permissions: []RBACPermission{{Action: "library.panels:read", Scope: "library.panels:uid:shared", Kind: "library.panels", Attribute: "uid", Identifier: "shared"}}, Bindings: []RBACBinding{{OrgID: org, Kind: "user", ID: "101"}}})
		execSQL := func(query string, args ...any) {
			t.Helper()
			if _, err := db.DB.ExecContext(ctx, query, args...); err != nil {
				t.Fatal("cannot seed exclusion fixture")
			}
		}
		keptData := ` {"title":"kept","panels":[{"libraryPanel":null,"datasource":{"uid":"metrics-uid"}}]} `
		execSQL("INSERT INTO dashboard (id,org_id,uid,title,data,version,is_folder,folder_id,folder_uid,has_acl,is_public) VALUES (30,"+db.bindList(4)+",2,FALSE,11,'child',FALSE,FALSE)", org, "kept", "kept", keptData)
		execSQL("INSERT INTO dashboard_version (id,dashboard_id,version,data) VALUES (330,30,1,'{}'),(331,30,2,"+db.bind(1)+")", keptData)
		execSQL("INSERT INTO dashboard_tag (id,dashboard_id,term,org_id,dashboard_uid) VALUES (230,30,'kept',"+db.bind(1)+",'kept')", org)
		var viewer string
		if err := db.DB.QueryRowContext(ctx, "SELECT id FROM "+db.quote("role")+" WHERE name="+db.bind(1), managedRole("Viewer")).Scan(&viewer); err != nil {
			t.Fatal(err)
		}
		for _, uid := range []string{"board", "kept"} {
			execSQL("INSERT INTO permission (role_id,action,scope,kind,attribute,identifier,created,updated) VALUES ("+db.bindList(3)+",'dashboards','uid',"+db.bind(4)+",CURRENT_TIMESTAMP,CURRENT_TIMESTAMP)", viewer, "dashboards:read", "dashboards:uid:"+uid, uid)
		}
		for _, id := range []int{12, 30} {
			execSQL("INSERT INTO dashboard_acl (org_id,dashboard_id,user_id,team_id,"+db.quote("role")+",permission,created,updated) VALUES ("+db.bindList(2)+",0,0,'Viewer',1,CURRENT_TIMESTAMP,CURRENT_TIMESTAMP)", org, id)
		}
		execSQL("UPDATE dashboard SET data='{}' WHERE id IN (13,14)")
		execSQL("DELETE FROM library_element_connection WHERE connection_id=13")
		execSQL("INSERT INTO dashboard_version (id,dashboard_id,version,data) VALUES (313,13,1,'{\"panels\":[{\"libraryPanel\":{\"uid\":\"deleted-history\"}}]}')")
		execSQL("UPDATE dashboard SET data='{\"panels\":[{\"libraryPanel\":{\"uid\":\"other-org-or-missing\"}}]}' WHERE id=12")
		before := state(db)
		if _, err := db.ExportTargets(ctx, AllTargets); err == nil || !strings.Contains(err.Error(), "missing library panel") || state(db) != before {
			t.Fatal("default export no longer rejects broken references safely", err)
		}
		execSQL("UPDATE library_element SET folder_id=404,folder_uid=NULL,model='invalid library JSON' WHERE id=40")
		execSQL("INSERT INTO library_element (id,org_id,folder_id,folder_uid,uid,name,kind,type,model,version) VALUES (41,"+db.bind(1)+",404,NULL,'unused','unused',1,'timeseries','{}',1)", org)
		before = state(db)
		options := ExportOptions{ExcludeLibraryPanelDashboards: true}
		result, err := db.ExportWithOptions(ctx, AllTargets, options)
		if err != nil {
			t.Fatal(err)
		}
		got := result.Snapshot
		if result.ExcludedLibraryDashboards != 14 || result.ExcludedLibraryHistory != 3 || result.ExcludedLibraryTags != 1 || result.ExcludedLibraryPanels != 2 || result.ExcludedLibraryConnections != 13 {
			t.Fatal("incorrect exclusion counts")
		}
		if len(got.Dashboard.Rows) != 3 || len(got.Folder.Rows) != 2 || len(got.DashboardTag.Rows) != 1 || len(got.DashboardVersion.Rows) != 2 || len(got.LibraryElement.Rows) != 0 || len(got.LibraryConnection.Rows) != 0 || !got.LibraryPanelsExcluded || cell(got.Dashboard, got.Dashboard.Rows[2], "data") != keptData {
			t.Fatal("exclusion changed retained dashboard content or folders")
		}
		if len(got.RBAC.Principals) != 0 || len(got.RBAC.Roles) != 1 || len(got.RBAC.Grants) != 2 || len(got.RBAC.LegacyACL) != 1 || len(got.RBAC.Roles[0].Permissions) != 4 {
			t.Fatal("exclusion did not omit exclusive grants/principals or preserve shared role definition")
		}
		for targets := Targets(1); targets <= AllTargets; targets++ {
			if !targets.Has(TargetDashboard) {
				continue
			}
			want, err := got.Select(targets)
			if err != nil {
				t.Fatal(err)
			}
			exported, err := db.ExportWithOptions(ctx, targets, options)
			if err != nil || !reflect.DeepEqual(exported.Snapshot, want) {
				t.Fatal("exclusion does not respect targets", err)
			}
		}
		path := filepath.Join(t.TempDir(), "filtered.dump.json")
		cmd := exec.CommandContext(ctx, binary, "export", "-db", engine, "-file", path, "--targets", AllTargets.String(), "--exclude-library-panel-dashboards")
		cmd.Env = append(os.Environ(), "GRAFANA_SYNC_DSN="+os.Getenv(strings.ToUpper(engine)+"_TEST_DSN"))
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatal("CLI exclusion failed", err)
		}
		for _, message := range []string{"dashboards=14", "history=3", "tags=1", "library_panels=2", "connections=13", "dashboard sync disabled"} {
			if !strings.Contains(string(out), message) {
				t.Fatal("CLI omitted safe exclusion warning")
			}
		}
		if strings.Contains(string(out), "other-org-or-missing") || strings.Contains(string(out), "deleted-history") || strings.Contains(string(out), "共有パネル") || strings.Contains(string(out), "source-user") {
			t.Fatal("CLI leaked resource values")
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		decoded, err := Decode(bytes.NewReader(data))
		if err != nil || !reflect.DeepEqual(decoded, got) || state(db) != before {
			t.Fatal("CLI changed snapshot or source", err)
		}
		if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
			t.Fatal("unsafe output permissions")
		}
		for _, approval := range []string{"--dry-run", "--allow-delete"} {
			cmd := exec.CommandContext(ctx, binary, "import", "-db", engine, "-file", path, "--targets", "dashboard", "--mode", "sync", approval)
			cmd.Env = append(os.Environ(), "GRAFANA_SYNC_DSN=invalid-dsn")
			if out, err := cmd.CombinedOutput(); err == nil || !strings.Contains(string(out), "dashboard sync is not allowed") {
				t.Fatal("CLI did not reject filtered sync before connecting")
			}
			if _, err := db.Sync(ctx, decoded, SyncOptions{DryRun: approval == "--dry-run", AllowDelete: approval == "--allow-delete"}); err == nil || state(db) != before {
				t.Fatal("API allowed filtered sync or changed target")
			}
		}
		execSQL("INSERT INTO dashboard_acl (org_id,dashboard_id,user_id,team_id,"+db.quote("role")+",permission,created,updated) VALUES ("+db.bind(1)+",404,0,0,'Viewer',1,CURRENT_TIMESTAMP,CURRENT_TIMESTAMP)", org)
		orphaned := state(db)
		if _, err := db.ExportWithOptions(ctx, AllTargets, options); err == nil || !strings.Contains(err.Error(), "orphaned") || state(db) != orphaned {
			t.Fatal("exclusion hid unrelated orphan ACLs", err)
		}
		execSQL("DELETE FROM dashboard_acl WHERE dashboard_id=404")
		databases[engine], snapshots[engine] = db, got
	}
	for source, snapshot := range snapshots {
		for target, db := range databases {
			for _, mode := range []ImportMode{ImportRequireEmpty, ImportInsertOnly} {
				t.Run(source+"_to_"+target+"/"+string(mode), func(t *testing.T) {
					clearRBACFixtures(t, ctx, db)
					base, err := snapshot.Select(DefaultTargets)
					if err != nil {
						t.Fatal(err)
					}
					if err := db.Import(ctx, base); err != nil {
						t.Fatal(err)
					}
					selected, err := snapshot.Select(TargetDashboard)
					if err != nil {
						t.Fatal(err)
					}
					if mode == ImportInsertOnly {
						original, err := librarySnapshot().Select(TargetDashboard)
						if err != nil {
							t.Fatal(err)
						}
						if err := db.Import(ctx, original); err != nil {
							t.Fatal(err)
						}
					}
					if _, err := db.ImportWithMode(ctx, selected, mode); err != nil {
						t.Fatal(err)
					}
					actual, err := db.ExportTargets(ctx, TargetDashboard)
					if err != nil {
						t.Fatal(err)
					}
					if mode == ImportInsertOnly {
						if len(actual.Dashboard.Rows) != 17 || len(actual.LibraryElement.Rows) != 1 || len(actual.LibraryConnection.Rows) != 14 || len(actual.DashboardVersion.Rows) != 4 {
							t.Fatal("insert-only changed excluded target resources")
						}
						before := state(db)
						if _, err := db.ImportWithMode(ctx, selected, mode); err != nil || state(db) != before {
							t.Fatal("repeated insert-only changed target", err)
						}
					} else {
						if !reflect.DeepEqual(actual.Dashboard, selected.Dashboard) || !reflect.DeepEqual(actual.Folder, selected.Folder) || !reflect.DeepEqual(actual.DashboardVersion, selected.DashboardVersion) || !reflect.DeepEqual(actual.DashboardTag, selected.DashboardTag) || !reflect.DeepEqual(semanticRBAC(actual.RBAC, nil), semanticRBAC(selected.RBAC, nil)) || len(actual.LibraryElement.Rows) != 0 || len(actual.LibraryConnection.Rows) != 0 {
							t.Fatal("require-empty lost retained rows or explicit permissions")
						}
					}
					orgData, err := snapshot.Select(DefaultTargets)
					if err != nil || orgData.LibraryPanelsExcluded {
						t.Fatal("non-dashboard selection retained sync restriction", err)
					}
					if _, err := db.Sync(ctx, orgData, SyncOptions{DryRun: true}); err != nil {
						t.Fatal("non-dashboard sync was blocked", err)
					}
					encoded, err := json.Marshal(selected)
					if err != nil || !bytes.Contains(encoded, []byte(`"library_panels_excluded":true`)) {
						t.Fatal("selection dropped exclusion marker", err)
					}
				})
			}
		}
	}
}
