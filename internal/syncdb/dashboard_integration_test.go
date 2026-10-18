//go:build integration

package syncdb

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

func createDashboardFixtures(t *testing.T, ctx context.Context, db *Database) {
	t.Helper()
	createFixtures(t, ctx, db)
	if _, err := db.DB.ExecContext(ctx, "DROP TABLE "+db.quote("dashboard")); err != nil {
		t.Fatal(err)
	}
	id, stamp, boolean, suffix := "BIGSERIAL PRIMARY KEY", "TIMESTAMP(6)", "BOOLEAN", ""
	if db.Engine == "mysql" {
		id, stamp, boolean, suffix = "BIGINT PRIMARY KEY AUTO_INCREMENT", "DATETIME(6)", "TINYINT(1)", " ENGINE=InnoDB DEFAULT CHARSET=utf8mb4"
	}
	definitions := map[string]string{
		"dashboard":                  "id " + id + ", org_id BIGINT NOT NULL, uid VARCHAR(40) NOT NULL, title VARCHAR(255) NOT NULL, data TEXT NOT NULL, version BIGINT NOT NULL, is_folder " + boolean + " NOT NULL, folder_id BIGINT NOT NULL, folder_uid VARCHAR(40), has_acl " + boolean + " NOT NULL, is_public " + boolean + " NOT NULL, UNIQUE(org_id,uid), UNIQUE(org_id,folder_uid,title)",
		"folder":                     "id " + id + ", org_id BIGINT NOT NULL, uid VARCHAR(40) NOT NULL, title VARCHAR(255) NOT NULL, parent_uid VARCHAR(40), UNIQUE(org_id,uid), UNIQUE(org_id,parent_uid,title)",
		"dashboard_tag":              "id " + id + ", dashboard_id BIGINT NOT NULL, term VARCHAR(50) NOT NULL, org_id BIGINT, dashboard_uid VARCHAR(40), UNIQUE(dashboard_id,term), FOREIGN KEY(dashboard_id) REFERENCES dashboard(id)",
		"dashboard_version":          "id " + id + ", dashboard_id BIGINT NOT NULL, version BIGINT NOT NULL, data TEXT NOT NULL, UNIQUE(dashboard_id,version), FOREIGN KEY(dashboard_id) REFERENCES dashboard(id)",
		"library_element":            "id " + id + ", org_id BIGINT NOT NULL, folder_id BIGINT NOT NULL, folder_uid VARCHAR(40), uid VARCHAR(40) NOT NULL, name VARCHAR(150) NOT NULL, kind BIGINT NOT NULL, type VARCHAR(40) NOT NULL, model TEXT NOT NULL, version BIGINT NOT NULL, UNIQUE(org_id,uid), UNIQUE(org_id,folder_uid,name,kind)",
		"library_element_connection": "id " + id + ", element_id BIGINT NOT NULL, kind BIGINT NOT NULL, connection_id BIGINT NOT NULL, UNIQUE(element_id,kind,connection_id), FOREIGN KEY(element_id) REFERENCES library_element(id), FOREIGN KEY(connection_id) REFERENCES dashboard(id)",
		"role":                       "id " + id + ", name VARCHAR(190) NOT NULL, version BIGINT NOT NULL, org_id BIGINT NOT NULL, uid VARCHAR(40) NOT NULL UNIQUE, description VARCHAR(255) NOT NULL DEFAULT '', display_name VARCHAR(190) NOT NULL DEFAULT '', group_name VARCHAR(190) NOT NULL DEFAULT '', hidden " + boolean + " NOT NULL DEFAULT FALSE, created " + stamp + " NOT NULL, updated " + stamp + " NOT NULL, UNIQUE(org_id,name)",
		"builtin_role":               "id " + id + ", role_id BIGINT NOT NULL, " + db.quote("role") + " VARCHAR(190) NOT NULL, org_id BIGINT NOT NULL, created " + stamp + " NOT NULL, updated " + stamp + " NOT NULL, FOREIGN KEY(role_id) REFERENCES " + db.quote("role") + "(id)",
		"permission":                 "id " + id + ", role_id BIGINT NOT NULL, action VARCHAR(190) NOT NULL, scope VARCHAR(190) NOT NULL, kind VARCHAR(40) NOT NULL, attribute VARCHAR(40) NOT NULL, identifier VARCHAR(40) NOT NULL, created " + stamp + " NOT NULL, updated " + stamp + " NOT NULL, UNIQUE(role_id,action,scope), FOREIGN KEY(role_id) REFERENCES " + db.quote("role") + "(id)",
		"kv_store":                   "id " + id + ", namespace VARCHAR(190), " + db.quote("key") + " VARCHAR(190), " + db.quote("value") + " TEXT",
	}
	for _, name := range []string{"dashboard", "folder", "library_element", "library_element_connection", "dashboard_tag", "dashboard_version", "role", "builtin_role", "permission", "kv_store"} {
		if _, err := db.DB.ExecContext(ctx, "CREATE TABLE "+db.quote(name)+" ("+definitions[name]+")"+suffix); err != nil {
			t.Fatal(name, err)
		}
		if name != "dashboard" {
			t.Cleanup(func() {
				if _, err := db.DB.Exec("DROP TABLE " + db.quote(name)); err != nil {
					t.Error(err)
				}
			})
		}
	}
}

func clearDashboardFixtures(t *testing.T, ctx context.Context, db *Database) {
	t.Helper()
	for _, name := range []string{"permission", "builtin_role", "role", "library_element_connection", "library_element", "dashboard_tag", "dashboard_version", "dashboard", "folder", "secrets", "data_source", "data_keys", "org", "kv_store"} {
		if _, err := db.DB.ExecContext(ctx, "DELETE FROM "+db.quote(name)); err != nil {
			t.Fatal(err)
		}
	}
}

func (db *Database) exportLegacyTargets(ctx context.Context, targets Targets) (*Snapshot, error) {
	s, err := db.ExportTargets(ctx, targets)
	if err != nil {
		return nil, err
	}
	s.Version = 4
	if s.RBAC != nil {
		roles := map[string]RBACRole{}
		for _, r := range s.RBAC.Roles {
			roles[r.Ref] = r
		}
		s.DashboardPermissions = []DashboardPermission{}
		for _, g := range s.RBAC.Grants {
			if roles[g.Role].Kind != "builtin" {
				return nil, errors.New("unsupported individual, team, custom-role")
			}
			g.Role = builtinName(roles[g.Role].Name)
			s.DashboardPermissions = append(s.DashboardPermissions, g)
		}
		slices.SortFunc(s.DashboardPermissions, func(a, b DashboardPermission) int {
			return strings.Compare(a.OrgID+"/"+a.Scope+"/"+a.Role+"/"+a.Action, b.OrgID+"/"+b.Scope+"/"+b.Role+"/"+b.Action)
		})
		s.RBAC = nil
	}
	return s, nil
}

func seedDashboardFixtures(t *testing.T, ctx context.Context, db *Database) *Snapshot {
	t.Helper()
	seedFixtures(t, ctx, db)
	s, err := dashboardSnapshot().Select(TargetDashboard)
	if err != nil {
		t.Fatal(err)
	}
	s.Dashboard.Rows[1][7] = str("0") // Grafana uses only parent_uid for nested folders.
	s.Dashboard.Rows[2][4] = str(`{"title":"ボード","description":"` + strings.Repeat("大", 20000) + `","panels":[{"datasource":{"uid":"metrics-uid"}}]}`)
	s.DashboardVersion.Rows[1][3] = s.Dashboard.Rows[2][4]
	if err := db.Import(ctx, s); err != nil {
		t.Fatal(err)
	}
	all, err := db.exportLegacyTargets(ctx, AllTargets)
	if err != nil {
		t.Fatal(err)
	}
	return all
}

func dashboardDatabaseState(t *testing.T, ctx context.Context, db *Database) string {
	t.Helper()
	tx, err := db.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	state := map[string]Table{}
	for _, entry := range (&Snapshot{Version: 4, Targets: AllTargets.Names()}).entries() {
		table, _, err := db.readSyncTarget(ctx, tx, entry)
		if err != nil {
			t.Fatal(err)
		}
		state[entry.name] = table
	}
	for _, name := range []string{"role", "builtin_role", "permission"} {
		table, _, err := db.readSyncTarget(ctx, tx, tableEntry{name: name, key: "id"})
		if err != nil {
			t.Fatal(err)
		}
		state[name] = table
	}
	if db.Engine == "postgres" {
		for _, name := range []string{"org", "data_source", "secrets", "dashboard", "folder", "dashboard_tag", "dashboard_version", "library_element", "library_element_connection", "role", "builtin_role", "permission"} {
			table, err := db.readTable(ctx, tx, tableEntry{name: name + "_id_seq", key: "last_value"})
			if err != nil {
				t.Fatal(err)
			}
			state[name+"_sequence"] = table
		}
	}
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestDashboardTransfer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	databases := map[string]*Database{}
	snapshots := map[string]*Snapshot{}
	for _, engine := range []string{"mysql", "postgres"} {
		db, err := Open(ctx, engine, os.Getenv(strings.ToUpper(engine)+"_TEST_DSN"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { db.DB.Close() })
		createDashboardFixtures(t, ctx, db)
		databases[engine] = db
		snapshots[engine] = seedDashboardFixtures(t, ctx, db)
	}
	if !reflect.DeepEqual(snapshots["mysql"], snapshots["postgres"]) {
		t.Fatal("dashboard exports differ between database engines")
	}
	for _, source := range []string{"mysql", "postgres"} {
		for _, target := range []string{"mysql", "postgres"} {
			for targets := Targets(1); targets <= AllTargets; targets++ {
				for _, mode := range []ImportMode{ImportRequireEmpty, ImportInsertOnly, ImportSync} {
					t.Run(source+"_to_"+target+"/"+targets.String()+"/"+string(mode), func(t *testing.T) {
						db := databases[target]
						clearDashboardFixtures(t, ctx, db)
						s, err := snapshots[source].Select(targets)
						if err != nil {
							t.Fatal(err)
						}
						// Prepopulate only the unselected groups, without touching selected tables.
						tx, err := db.DB.BeginTx(ctx, nil)
						if err != nil {
							t.Fatal(err)
						}
						defer tx.Rollback()
						if targets.Has(TargetOrg) {
							entry := snapshots[source].entries()[0]
							if _, err := db.insertTable(ctx, tx, entry, ImportRequireEmpty); err != nil {
								t.Fatal(err)
							}
						}
						for _, entry := range snapshots[source].entries() {
							if !targets.Has(entry.group) {
								if _, err := db.insertTable(ctx, tx, entry, ImportRequireEmpty); err != nil {
									t.Fatal(err)
								}
							}
						}
						if !targets.Has(TargetDashboard) {
							if err := db.applyDashboardPermissions(ctx, tx, &dashboardPermissionPlan{insert: snapshots[source].DashboardPermissions}); err != nil {
								t.Fatal(err)
							}
						}
						if targets.Has(TargetOrg) {
							if _, err := tx.ExecContext(ctx, "DELETE FROM "+db.quote("org")); err != nil {
								t.Fatal(err)
							}
						}
						if err := tx.Commit(); err != nil {
							t.Fatal(err)
						}
						if mode == ImportSync {
							before := dashboardDatabaseState(t, ctx, db)
							if _, err := db.Sync(ctx, s, SyncOptions{DryRun: true}); err != nil {
								t.Fatal(err)
							}
							if dashboardDatabaseState(t, ctx, db) != before {
								t.Fatal("dry-run modified data or managed roles")
							}
							if _, err := db.Sync(ctx, s, SyncOptions{AllowDelete: true}); err != nil {
								t.Fatal(err)
							}
						} else if _, err := db.ImportWithMode(ctx, s, mode); err != nil {
							t.Fatal(err)
						}
						got, err := db.exportLegacyTargets(ctx, AllTargets)
						if err != nil {
							t.Fatal(err)
						}
						want := cloneSnapshot(t, snapshots[source])
						if mode == ImportSync && targets.Has(TargetDatasource) {
							for _, row := range want.DataKeys.Rows {
								row[columnIndex(want.DataKeys.Columns, "active")] = str("0")
							}
						}
						if !reflect.DeepEqual(got, want) {
							t.Fatal("selected groups did not round trip or unselected groups changed")
						}
						if mode == ImportSync {
							result, err := db.Sync(ctx, s, SyncOptions{AllowDelete: true})
							if err != nil || result.Inserted+result.Deleted+result.Updated != 0 {
								t.Fatal("sync not idempotent", result, err)
							}
						}
						if mode == ImportInsertOnly {
							before := dashboardDatabaseState(t, ctx, db)
							result, err := db.ImportWithMode(ctx, s, mode)
							if err != nil || result.Inserted != 0 || dashboardDatabaseState(t, ctx, db) != before {
								t.Fatal("insert-only not idempotent", err)
							}
						}
					})
				}
			}
		}
	}
	for engine, db := range databases {
		t.Run(engine+"_dashboard_sync", func(t *testing.T) { testDashboardSync(t, ctx, db, snapshots[engine]) })
		t.Run(engine+"_dashboard_guards", func(t *testing.T) { testDashboardGuards(t, ctx, db, snapshots[engine]) })
		t.Run(engine+"_dashboard_batches", func(t *testing.T) { testDashboardBatches(t, ctx, db, snapshots[engine]) })
	}
}

func testDashboardBatches(t *testing.T, ctx context.Context, db *Database, source *Snapshot) {
	large := cloneSnapshot(t, source)
	org := cell(large.Dashboard, large.Dashboard.Rows[2], "org_id")
	for i := range 3000 {
		row := append([]*string{}, large.Dashboard.Rows[2]...)
		for name, value := range map[string]string{
			"id": strconv.Itoa(1000 + i), "uid": "batch-" + strconv.Itoa(i),
			"title": "Batch " + strconv.Itoa(i), "data": `{"panels":[]}`,
		} {
			row[columnIndex(large.Dashboard.Columns, name)] = str(value)
		}
		large.Dashboard.Rows = append(large.Dashboard.Rows, row)
		large.DashboardPermissions = append(large.DashboardPermissions,
			DashboardPermission{org, "Viewer", "dashboards:read", "dashboards:uid:batch-" + strconv.Itoa(i)},
			DashboardPermission{org, "Editor", "dashboards:write", "dashboards:uid:batch-" + strconv.Itoa(i)},
		)
	}
	clearDashboardFixtures(t, ctx, db)
	if err := db.Import(ctx, large); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	got, err := db.exportLegacyTargets(ctx, AllTargets)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("exported %d dashboards and %d permissions in %s", len(got.Dashboard.Rows), len(got.DashboardPermissions), time.Since(started))
	if !reflect.DeepEqual(got.Dashboard, large.Dashboard) || len(got.DashboardPermissions) != len(large.DashboardPermissions) {
		t.Fatal("batched export changed dashboards or permission counts")
	}
	wanted := make(map[DashboardPermission]bool)
	for _, p := range large.DashboardPermissions {
		wanted[p] = true
	}
	for _, p := range got.DashboardPermissions {
		if !wanted[p] {
			t.Fatal("batched export changed permissions")
		}
		delete(wanted, p)
	}
	if len(wanted) != 0 {
		t.Fatal("batched export lost permissions")
	}
	// The dependency is in the last batch, not the first one.
	if _, err := db.DB.ExecContext(ctx, "CREATE TABLE dashboard_provisioning (dashboard_id BIGINT)"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.ExecContext(ctx, "INSERT INTO dashboard_provisioning VALUES (3999)"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.exportLegacyTargets(ctx, TargetDashboard); err == nil || !strings.Contains(err.Error(), "unsupported dependencies in dashboard_provisioning") {
		t.Fatal("last dependency batch was not checked", err)
	}
	if _, err := db.DB.ExecContext(ctx, "DROP TABLE dashboard_provisioning"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.ExecContext(ctx, "CREATE TABLE dashboard_provisioning (unexpected BIGINT)"); err != nil {
		t.Fatal(err)
	}
	_, err = db.exportLegacyTargets(ctx, TargetDashboard)
	if err == nil || !strings.Contains(err.Error(), "expected dependency column is missing") || !strings.Contains(err.Error(), "dashboard_provisioning") {
		t.Fatal("missing column diagnostic is not actionable", err)
	}
	if _, err := db.DB.ExecContext(ctx, "DROP TABLE dashboard_provisioning"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.ExecContext(ctx, "CREATE TABLE dashboard_public (dashboard_uid VARCHAR(40), org_id BIGINT)"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.ExecContext(ctx, "INSERT INTO dashboard_public VALUES ('batch-2999', 77)"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.exportLegacyTargets(ctx, TargetDashboard); err != nil {
		t.Fatal("batched query matched another organization's UID", err)
	}
	checkDependencies := func() error {
		tx, err := db.DB.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		return db.guardDashboardDependencies(ctx, tx, large.Dashboard, true)
	}
	if err := checkDependencies(); err != nil {
		t.Fatal("batched destructive query matched another organization's UID", err)
	}
	if _, err := db.DB.ExecContext(ctx, "INSERT INTO dashboard_public VALUES ('batch-2999', "+db.bind(1)+")", org); err != nil {
		t.Fatal(err)
	}
	if _, err := db.exportLegacyTargets(ctx, TargetDashboard); err != nil {
		t.Fatal("export rejected excluded public sharing settings", err)
	}
	err = checkDependencies()
	if err == nil || !strings.Contains(err.Error(), "unsupported dependencies in dashboard_public") {
		t.Fatal("last organization/UID batch was not checked", err)
	}
	if _, err := db.DB.ExecContext(ctx, "DROP TABLE dashboard_public"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.ExecContext(ctx, "INSERT INTO "+db.quote("role")+" (name, version, org_id, uid, created, updated) VALUES ('batch-custom-role', 0, 77, 'batch-custom-role', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.ExecContext(ctx, "INSERT INTO permission (role_id, action, scope, kind, attribute, identifier, created, updated) SELECT id, 'dashboards:read', 'dashboards:uid:batch-2999', 'dashboards', 'uid', 'batch-2999', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP FROM "+db.quote("role")+" WHERE name = 'batch-custom-role'"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.exportLegacyTargets(ctx, TargetDashboard); err != nil {
		t.Fatal("batched permissions matched another organization's UID", err)
	}
	if _, err := db.DB.ExecContext(ctx, "UPDATE "+db.quote("role")+" SET org_id = "+db.bind(1)+" WHERE name = 'batch-custom-role'", org); err != nil {
		t.Fatal(err)
	}
	if _, err := db.exportLegacyTargets(ctx, TargetDashboard); err == nil || !strings.Contains(err.Error(), "unsupported individual, team, custom-role") {
		t.Fatal("last permission batch was not checked", err)
	}
	if _, err := db.DB.ExecContext(ctx, "DELETE FROM permission WHERE role_id IN (SELECT id FROM "+db.quote("role")+" WHERE name = 'batch-custom-role')"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.ExecContext(ctx, "DELETE FROM "+db.quote("role")+" WHERE name = 'batch-custom-role'"); err != nil {
		t.Fatal(err)
	}
}

func testDashboardSync(t *testing.T, ctx context.Context, db *Database, source *Snapshot) {
	clearDashboardFixtures(t, ctx, db)
	if err := db.Import(ctx, source); err != nil {
		t.Fatal(err)
	}
	protected := cloneSnapshot(t, source)
	protected.Scope.OrgIDs = []string{"77"}
	for _, entry := range protected.entries() {
		for _, row := range entry.table.Rows {
			for i, column := range entry.table.Columns {
				if column == "org_id" || (entry.name == "org" && column == "id") {
					row[i] = str("77")
					continue
				}
				if column == "id" || column == "dashboard_id" || column == "folder_id" {
					id, err := strconv.ParseInt(cell(*entry.table, row, column), 10, 64)
					if err != nil {
						t.Fatal(err)
					}
					if id != 0 {
						row[i] = str(strconv.FormatInt(id+1000, 10))
					}
				}
			}
		}
	}
	protected.Org.Rows[0][columnIndex(protected.Org.Columns, "name")] = str("protected-org")
	for i := range protected.DashboardPermissions {
		protected.DashboardPermissions[i].OrgID = "77"
	}
	if _, err := db.ImportWithMode(ctx, protected, ImportInsertOnly); err != nil {
		t.Fatal(err)
	}
	allBefore, err := db.exportLegacyTargets(ctx, AllTargets)
	if err != nil {
		t.Fatal(err)
	}
	protectedBefore := dashboardOrganization(allBefore, "77")
	s, err := cloneSnapshot(t, source).Select(TargetDashboard)
	if err != nil {
		t.Fatal(err)
	}
	// Updating a skipped parent must not append versions, tags or permissions.
	swapped := cloneSnapshot(t, s)
	swapped.Dashboard.Rows[0][2], swapped.Dashboard.Rows[2][2] = str("board"), str("root")
	swapped.Dashboard.Rows[1][8] = str("board")
	swapped.Folder.Rows[0][2], swapped.Folder.Rows[1][4] = str("board"), str("board")
	swapped.DashboardTag.Rows[0][columnIndex(swapped.DashboardTag.Columns, "dashboard_uid")] = str("root")
	for i, p := range swapped.DashboardPermissions {
		if p.Scope == "folders:uid:root" {
			swapped.DashboardPermissions[i].Scope = "folders:uid:board"
		}
		if p.Scope == "dashboards:uid:board" {
			swapped.DashboardPermissions[i].Scope = "dashboards:uid:root"
		}
	}
	if _, err := db.Sync(ctx, swapped, SyncOptions{AllowDelete: true}); err != nil {
		t.Fatal("folder/dashboard UID exchange", err)
	}
	if _, err := db.Sync(ctx, s, SyncOptions{AllowDelete: true}); err != nil {
		t.Fatal("restoring folder/dashboard UID exchange", err)
	}
	changed := cloneSnapshot(t, s)
	changed.Dashboard.Rows[2][3] = str("incoming title")
	changed.DashboardTag.Rows[0][0] = str("201")
	changed.DashboardVersion.Rows[0][0] = str("302")
	changed.DashboardPermissions = append(changed.DashboardPermissions, DashboardPermission{"9007199254740993", "Viewer", "dashboards:read", "dashboards:uid:board"})
	before := dashboardDatabaseState(t, ctx, db)
	if result, err := db.ImportWithMode(ctx, changed, ImportInsertOnly); err != nil || result.Inserted != 0 {
		t.Fatal("existing-parent skip", err)
	}
	if before != dashboardDatabaseState(t, ctx, db) {
		t.Fatal("insert-only changed existing dashboard children or permissions")
	}
	// Recreate a dashboard under the same UID and exchange root/child folder UIDs.
	s.Dashboard.Rows[2][0] = str("13")
	s.Dashboard.Rows[2][3] = str("再作成")
	s.Dashboard.Rows[0][2], s.Dashboard.Rows[1][2] = str("child"), str("root")
	s.Dashboard.Rows[1][8], s.Dashboard.Rows[2][8] = str("child"), str("root")
	s.Folder.Rows[0][2], s.Folder.Rows[1][2], s.Folder.Rows[1][4] = str("child"), str("root"), str("child")
	s.DashboardTag.Rows[0][1] = str("13")
	for _, row := range s.DashboardVersion.Rows {
		row[1] = str("13")
	}
	s.DashboardPermissions[0].Scope = "folders:uid:child"
	s.DashboardPermissions[1].Action = "dashboards:read"
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
	planned, err := db.Sync(ctx, s, SyncOptions{DryRun: true})
	if err != nil || planned.Dashboard.Inserted != 1 || planned.Dashboard.Deleted != 1 {
		t.Fatal("invalid recreation plan", err)
	}
	if before != dashboardDatabaseState(t, ctx, db) {
		t.Fatal("dry-run changed dashboards")
	}
	if _, err := db.Sync(ctx, s, SyncOptions{AllowDelete: true}); err != nil {
		t.Fatal(err)
	}
	got, err := db.exportLegacyTargets(ctx, TargetDashboard)
	if err != nil || !reflect.DeepEqual(dashboardOrganization(got, "9007199254740993"), s) {
		t.Fatal("dashboard recreation or UID swap failed", err)
	}
	// Delete all resources only inside the selected organization.
	for _, entry := range s.entries() {
		entry.table.Rows = [][]*string{}
	}
	s.DashboardPermissions = []DashboardPermission{}
	if _, err := db.Sync(ctx, s, SyncOptions{AllowDelete: true}); err != nil {
		t.Fatal(err)
	}
	got, err = db.exportLegacyTargets(ctx, TargetDashboard)
	if err != nil || !reflect.DeepEqual(dashboardOrganization(got, "9007199254740993"), s) {
		t.Fatal("scoped dashboard deletion failed", err)
	}
	allAfter, err := db.exportLegacyTargets(ctx, AllTargets)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(protectedBefore, dashboardOrganization(allAfter, "77")) {
		t.Fatal("sync changed a protected organization's data or permissions")
	}
}

func dashboardOrganization(s *Snapshot, org string) *Snapshot {
	filtered := *s
	filtered.Scope.OrgIDs = []string{org}
	ids := make(map[int64]bool)
	for _, row := range s.Dashboard.Rows {
		if cell(s.Dashboard, row, "org_id") == org {
			id, _ := strconv.ParseInt(cell(s.Dashboard, row, "id"), 10, 64)
			ids[id] = true
		}
	}
	for _, entry := range filtered.entries() {
		if entry.name == "data_keys" {
			continue
		}
		rows := make([][]*string, 0)
		for _, row := range entry.table.Rows {
			keep := cell(*entry.table, row, "org_id") == org
			if entry.name == "org" {
				keep = cell(*entry.table, row, "id") == org
			}
			if entry.name == "dashboard_tag" || entry.name == "dashboard_version" {
				id, _ := strconv.ParseInt(cell(*entry.table, row, "dashboard_id"), 10, 64)
				keep = ids[id]
			}
			if keep {
				rows = append(rows, row)
			}
		}
		entry.table.Rows = rows
	}
	if filtered.Selection().Has(TargetDashboard) {
		filtered.DashboardPermissions = []DashboardPermission{}
	}
	for _, p := range s.DashboardPermissions {
		if p.OrgID == org {
			filtered.DashboardPermissions = append(filtered.DashboardPermissions, p)
		}
	}
	return &filtered
}

func testDashboardGuards(t *testing.T, ctx context.Context, db *Database, source *Snapshot) {
	clearDashboardFixtures(t, ctx, db)
	if err := db.Import(ctx, source); err != nil {
		t.Fatal(err)
	}
	s, err := source.Select(TargetDashboard)
	if err != nil {
		t.Fatal(err)
	}
	before := dashboardDatabaseState(t, ctx, db)
	// A late constraint failure rolls back updates to all selected resources.
	missingHistory := cloneSnapshot(t, s)
	missingHistory.DashboardVersion.Rows[0][columnIndex(missingHistory.DashboardVersion.Columns, "data")] = str(`{"panels":[{"datasource":{"uid":"missing-source"}}]}`)
	if _, err := db.Sync(ctx, missingHistory, SyncOptions{DryRun: true}); err == nil {
		t.Fatal("accepted unresolved datasource in dashboard history")
	}
	bad := cloneSnapshot(t, source)
	bad.DataSource.Rows[0][columnIndex(bad.DataSource.Columns, "user")] = str("changed")
	bad.Dashboard.Rows[2][3] = str(strings.Repeat("x", 300))
	if _, err := db.Sync(ctx, bad, SyncOptions{AllowDelete: true}); err == nil {
		t.Fatal("accepted oversized dashboard title")
	}
	if before != dashboardDatabaseState(t, ctx, db) {
		t.Fatal("failed sync did not roll back data and permissions")
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := db.Sync(cancelled, s, SyncOptions{AllowDelete: true}); err == nil {
		t.Fatal("accepted cancelled context")
	}
	if before != dashboardDatabaseState(t, ctx, db) {
		t.Fatal("cancelled sync changed the database")
	}
	for _, spec := range []struct {
		name, columns, values string
		exportReject          bool
	}{
		{"dashboard_acl", "dashboard_id BIGINT", "12", true},
		{"dashboard_provisioning", "dashboard_id BIGINT", "12", true},
		{"dashboard_public", "dashboard_uid VARCHAR(40), org_id BIGINT, access_token VARCHAR(40), is_enabled BIGINT", "'board',9007199254740993,'test-only-public-token',1", false},
		{"star", "dashboard_id BIGINT", "12", false},
		{"playlist_item", "value VARCHAR(40), type VARCHAR(40)", "'board','dashboard_by_uid'", false},
		{"annotation", "dashboard_id BIGINT,dashboard_uid VARCHAR(40),org_id BIGINT", "12,'board',9007199254740993", false},
		{"alert", "dashboard_id BIGINT,org_id BIGINT", "12,9007199254740993", false},
		{"alert_rule", "dashboard_uid VARCHAR(40),namespace_uid VARCHAR(40),org_id BIGINT", "NULL,'root',9007199254740993", false},
	} {
		if _, err := db.DB.ExecContext(ctx, "CREATE TABLE "+db.quote(spec.name)+" ("+spec.columns+")"); err != nil {
			t.Fatal(err)
		}
		if _, err := db.DB.ExecContext(ctx, "INSERT INTO "+db.quote(spec.name)+" VALUES ("+spec.values+")"); err != nil {
			t.Fatal(err)
		}
		if spec.exportReject {
			if _, err := db.exportLegacyTargets(ctx, TargetDashboard); err == nil {
				t.Errorf("export ignored %s dependency", spec.name)
			}
		} else {
			if _, err := db.exportLegacyTargets(ctx, TargetDashboard); err != nil {
				t.Errorf("export rejected excluded %s settings: %v", spec.name, err)
			}
		}
		if spec.name == "dashboard_public" {
			if _, err := db.DB.ExecContext(ctx, "UPDATE dashboard SET is_public = TRUE WHERE id = 12"); err != nil {
				t.Fatal(err)
			}
			exported, err := db.exportLegacyTargets(ctx, TargetDashboard)
			if err != nil {
				t.Fatal("export rejected the public metadata flag", err)
			}
			for _, row := range exported.Dashboard.Rows {
				if cell(exported.Dashboard, row, "is_public") != "0" {
					t.Fatal("export preserved public sharing metadata")
				}
			}
			data, err := json.Marshal(exported)
			if err != nil || strings.Contains(string(data), "test-only-public-token") || strings.Contains(string(data), "dashboard_public") {
				t.Fatal("export included public sharing settings")
			}
			var public bool
			if err := db.DB.QueryRowContext(ctx, "SELECT is_public FROM dashboard WHERE id = 12").Scan(&public); err != nil || !public {
				t.Fatal("export changed source public metadata", err)
			}
			if _, err := db.DB.ExecContext(ctx, "UPDATE dashboard SET is_public = FALSE WHERE id = 12"); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Sync(ctx, s, SyncOptions{AllowDelete: true}); err != nil {
				t.Fatal("identity-preserving sync rejected target public sharing", err)
			}
			var count int
			if err := db.DB.QueryRowContext(ctx, "SELECT COUNT(*) FROM dashboard_public WHERE access_token = 'test-only-public-token' AND is_enabled = 1").Scan(&count); err != nil || count != 1 {
				t.Fatal("sync changed existing target public sharing", err)
			}
		}
		empty := cloneSnapshot(t, s)
		for _, entry := range empty.entries() {
			entry.table.Rows = [][]*string{}
		}
		empty.DashboardPermissions = []DashboardPermission{}
		if _, err := db.Sync(ctx, empty, SyncOptions{AllowDelete: true}); err == nil {
			t.Errorf("sync ignored %s dependency", spec.name)
		}
		if before != dashboardDatabaseState(t, ctx, db) {
			t.Errorf("dependency error in %s changed data", spec.name)
		}
		if _, err := db.DB.ExecContext(ctx, "DROP TABLE "+db.quote(spec.name)); err != nil {
			t.Fatal(err)
		}
	}
	for _, status := range []string{`{"write_legacy":true,"write_unified":true}`, `{"write_legacy":true,"read_unified":true}`, `{"write_legacy":true,"migrating":1}`, `{"write_legacy":true,"migrated":1}`, `{}`, `not-json`} {
		if _, err := db.DB.ExecContext(ctx, "INSERT INTO kv_store (namespace, "+db.quote("key")+", "+db.quote("value")+") VALUES ('unified.dualwrite','dashboards',"+db.bind(1)+")", status); err != nil {
			t.Fatal(err)
		}
		if _, err := db.exportLegacyTargets(ctx, TargetDashboard); err == nil {
			t.Fatal("export accepted Unified Storage or unknown mode")
		}
		if _, err := db.Sync(ctx, s, SyncOptions{DryRun: true}); err == nil {
			t.Fatal("sync accepted Unified Storage or unknown mode")
		}
		if _, err := db.Export(ctx); err != nil {
			t.Fatal("default selection inspected dashboard storage", err)
		}
		if _, err := db.DB.ExecContext(ctx, "DELETE FROM kv_store"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.DB.ExecContext(ctx, "CREATE TABLE "+db.quote("resource")+" ("+db.quote("group")+" VARCHAR(190), "+db.quote("resource")+" VARCHAR(190))"); err != nil {
		t.Fatal(err)
	}
	for _, row := range [][2]string{{"other.grafana.app", "dashboards"}, {"dashboard.grafana.app", "dashboards"}, {"folder.grafana.app", "folders"}} {
		if _, err := db.DB.ExecContext(ctx, "DELETE FROM "+db.quote("resource")); err != nil {
			t.Fatal(err)
		}
		if _, err := db.DB.ExecContext(ctx, "INSERT INTO "+db.quote("resource")+" ("+db.quote("group")+", "+db.quote("resource")+") VALUES ("+db.bind(1)+", "+db.bind(2)+")", row[0], row[1]); err != nil {
			t.Fatal(err)
		}
		_, err := db.exportLegacyTargets(ctx, TargetDashboard)
		if row[0] == "other.grafana.app" && err != nil || row[0] != "other.grafana.app" && err == nil {
			t.Fatal("incorrect handling of Unified Storage resources", row, err)
		}
		if row[0] != "other.grafana.app" {
			if _, err := db.Sync(ctx, s, SyncOptions{DryRun: true}); err == nil {
				t.Fatal("sync ignored Unified Storage resources")
			}
			if _, err := db.Export(ctx); err != nil {
				t.Fatal("default selection inspected dashboard storage", err)
			}
		}
	}
	if _, err := db.DB.ExecContext(ctx, "DROP TABLE "+db.quote("resource")); err != nil {
		t.Fatal(err)
	}
	// Builtin IDs are never copied; custom/user/team roles are never accepted.
	for _, name := range []string{"managed:users:1:permissions", "managed:teams:1:permissions", "custom-role", "global-custom-role"} {
		org := "9007199254740993"
		if name == "global-custom-role" {
			org = "0"
		}
		if _, err := db.DB.ExecContext(ctx, "UPDATE "+db.quote("role")+" SET name = "+db.bind(1)+", org_id = "+db.bind(2)+" WHERE name = "+db.bind(3), name, org, managedRole("Editor")); err != nil {
			t.Fatal(err)
		}
		if _, err := db.exportLegacyTargets(ctx, TargetDashboard); err == nil {
			t.Fatal("export silently discarded individual RBAC")
		}
		if _, err := db.Sync(ctx, s, SyncOptions{AllowDelete: true}); err == nil {
			t.Fatal("sync silently discarded individual RBAC")
		}
		if _, err := db.DB.ExecContext(ctx, "UPDATE "+db.quote("role")+" SET name = "+db.bind(1)+", org_id = "+db.bind(2)+" WHERE name = "+db.bind(3), managedRole("Editor"), "9007199254740993", name); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.DB.ExecContext(ctx, "DELETE FROM builtin_role"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.exportLegacyTargets(ctx, TargetDashboard); err == nil {
		t.Fatal("accepted missing builtin-role binding")
	}
}
