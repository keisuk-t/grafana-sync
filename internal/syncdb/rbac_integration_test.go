//go:build integration

package syncdb

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

func createRBACFixtures(t *testing.T, ctx context.Context, db *Database) {
	t.Helper()
	for _, table := range []string{"user_role", "team_role", "dashboard_acl", "team_member", "team", "org_user", "user"} {
		if _, err := db.DB.ExecContext(ctx, "DROP TABLE IF EXISTS "+db.quote(table)); err != nil {
			t.Fatal(err)
		}
	}
	createDashboardFixtures(t, ctx, db)
	id, boolean, stamp := "BIGINT AUTO_INCREMENT PRIMARY KEY", "BOOLEAN", "DATETIME(6)"
	if db.Engine == "postgres" {
		id, stamp = "BIGSERIAL PRIMARY KEY", "TIMESTAMP(6)"
	}
	for table, columns := range map[string]string{
		"user":          "id BIGINT PRIMARY KEY, uid VARCHAR(40) UNIQUE, login VARCHAR(190), is_admin " + boolean + ", is_disabled " + boolean + ", is_service_account " + boolean + ", password VARCHAR(190), salt VARCHAR(190), rands VARCHAR(190)",
		"org_user":      "id " + id + ", org_id BIGINT, user_id BIGINT, role VARCHAR(20), UNIQUE(org_id,user_id)",
		"team":          "id BIGINT PRIMARY KEY, org_id BIGINT, uid VARCHAR(40), name VARCHAR(190), external_uid VARCHAR(40), is_provisioned " + boolean + ", UNIQUE(org_id,uid)",
		"team_member":   "id " + id + ", org_id BIGINT, team_id BIGINT, user_id BIGINT, permission BIGINT, external " + boolean + ", UNIQUE(org_id,team_id,user_id)",
		"user_role":     "id " + id + ", org_id BIGINT, user_id BIGINT, role_id BIGINT, created " + stamp + ", group_mapping_uid VARCHAR(40), UNIQUE(org_id,user_id,role_id)",
		"team_role":     "id " + id + ", org_id BIGINT, team_id BIGINT, role_id BIGINT, created " + stamp + ", UNIQUE(org_id,team_id,role_id)",
		"dashboard_acl": "id " + id + ", org_id BIGINT, dashboard_id BIGINT, user_id BIGINT, team_id BIGINT, " + db.quote("role") + " VARCHAR(20), permission BIGINT, created " + stamp + ", updated " + stamp,
	} {
		if _, err := db.DB.ExecContext(ctx, "CREATE TABLE "+db.quote(table)+" ("+columns+")"); err != nil {
			t.Fatal(err)
		}
	}
}

func seedRBACPrincipals(t *testing.T, ctx context.Context, db *Database, target bool) *PrincipalMap {
	t.Helper()
	org := "9007199254740993"
	offset, prefix := 0, "source"
	if target {
		offset, prefix = 400, "target"
	}
	mapping := &PrincipalMap{Version: 1, Mappings: []PrincipalMapping{}}
	for _, n := range []int{101, 102, 103} {
		id := strconv.Itoa(n + offset)
		uid := prefix + "-user-" + id
		role := "Viewer"
		if n == 102 {
			role = "Editor"
		}
		if _, err := db.DB.ExecContext(ctx, "INSERT INTO "+db.quote("user")+" (id,uid,login,is_admin,is_disabled,is_service_account,password,salt,rands) VALUES ("+db.bindList(9)+")", id, uid, prefix+"-login-"+id, false, false, n == 103, "credential-canary-never-exported", "salt-canary", "rands-canary"); err != nil {
			t.Fatal(err)
		}
		if _, err := db.DB.ExecContext(ctx, "INSERT INTO org_user (org_id,user_id,role) VALUES ("+db.bindList(3)+")", org, id, role); err != nil {
			t.Fatal(err)
		}
		mapping.Mappings = append(mapping.Mappings, PrincipalMapping{OrgID: org, Kind: "user", SourceID: strconv.Itoa(n), TargetID: id, TargetUID: uid})
	}
	team := strconv.Itoa(201 + offset)
	uid := prefix + "-team-" + team
	if _, err := db.DB.ExecContext(ctx, "INSERT INTO team (id,org_id,uid,name,external_uid,is_provisioned) VALUES ("+db.bindList(6)+")", team, org, uid, prefix+"-team", "", false); err != nil {
		t.Fatal(err)
	}
	for _, n := range []int{101, 102} {
		if _, err := db.DB.ExecContext(ctx, "INSERT INTO team_member (org_id,team_id,user_id,permission,external) VALUES ("+db.bindList(5)+")", org, team, strconv.Itoa(n+offset), 0, false); err != nil {
			t.Fatal(err)
		}
	}
	mapping.Mappings = append(mapping.Mappings, PrincipalMapping{OrgID: org, Kind: "team", SourceID: "201", TargetID: team, TargetUID: uid})
	if target {
		now := time.Now().UTC()
		for _, entry := range []struct {
			name, columns string
			values        []any
		}{
			{"role", "id,org_id,name,uid,version,created,updated", []any{10001, 0, "fixed:test:unselected", "unselected-role", 1, now, now}},
			{"permission", "id,role_id,action,scope,kind,attribute,identifier,created,updated", []any{10001, 10001, "users:read", "global.users:*", "global.users", "", "", now, now}},
			{"builtin_role", "id,role_id,org_id,role,created,updated", []any{10001, 10001, 0, "Admin", now, now}},
			{"user_role", "id,role_id,org_id,user_id,created", []any{10001, 10001, org, 501, now}},
			{"team_role", "id,role_id,org_id,team_id,created", []any{10001, 10001, org, 601, now}},
			{"dashboard_acl", "id,org_id,dashboard_id,user_id,team_id,role,permission,created,updated", []any{10001, 0, -1, 0, 0, "Viewer", 1, now, now}},
		} {
			columns := strings.Split(entry.columns, ",")
			for i, c := range columns {
				columns[i] = db.quote(c)
			}
			if _, err := db.DB.ExecContext(ctx, "INSERT INTO "+db.quote(entry.name)+" ("+strings.Join(columns, ",")+") VALUES ("+db.bindList(len(entry.values))+")", entry.values...); err != nil {
				t.Fatal(err)
			}
		}
	}
	return mapping
}

func seedRBACRole(t *testing.T, ctx context.Context, db *Database, role RBACRole) string {
	t.Helper()
	tx, err := db.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	id, err := db.createRBACRole(ctx, tx, role)
	if err != nil {
		t.Fatal(err)
	}
	if role.Kind == "external" {
		if _, err := tx.ExecContext(ctx, "UPDATE "+db.quote("role")+" SET uid="+db.bind(1)+" WHERE id="+db.bind(2), role.UID, id); err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range role.Permissions {
		if _, err := tx.ExecContext(ctx, "INSERT INTO permission (role_id,action,scope,kind,attribute,identifier,created,updated) VALUES ("+db.bindList(8)+")", id, p.Action, p.Scope, p.Kind, p.Attribute, p.Identifier, time.Now().UTC(), time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return id
}

func seedRBACSource(t *testing.T, ctx context.Context, db *Database) *Snapshot {
	t.Helper()
	seedDashboardFixtures(t, ctx, db)
	seedRBACPrincipals(t, ctx, db, false)
	org := "9007199254740993"
	for _, spec := range []struct{ kind, id string }{{"user", "101"}, {"user", "103"}, {"team", "201"}, {"builtin", "Admin"}, {"custom", "102"}} {
		name := "managed:" + spec.kind + "s:" + spec.id + ":permissions"
		bindingKind := spec.kind
		if spec.kind == "builtin" {
			name = managedRole(spec.id)
		}
		if spec.kind == "custom" {
			name = "custom:dashboard-reader"
			bindingKind = "user"
		}
		role := RBACRole{OrgID: org, Kind: spec.kind, Name: name, UID: "custom-reader", Version: 1, Description: "Scoped dashboard role", Permissions: []RBACPermission{{Action: "dashboards:read", Scope: "dashboards:uid:board", Kind: "dashboards", Attribute: "uid", Identifier: "board"}}, Bindings: []RBACBinding{{OrgID: org, Kind: bindingKind, ID: spec.id}}}
		if spec.kind == "team" {
			role.Permissions = append(role.Permissions, RBACPermission{Action: "folders:read", Scope: "folders:uid:root", Kind: "folders", Attribute: "uid", Identifier: "root"})
		}
		if spec.kind == "builtin" {
			role.Permissions = append(role.Permissions, RBACPermission{Action: "dashboards.permissions:write", Scope: "dashboards:uid:board", Kind: "dashboards", Attribute: "uid", Identifier: "board"})
		}
		seedRBACRole(t, ctx, db, role)
	}
	if _, err := db.DB.ExecContext(ctx, "INSERT INTO dashboard_acl (org_id,dashboard_id,user_id,team_id,permission,created,updated) VALUES ("+db.bindList(7)+")", org, 12, 101, 0, 1, time.Now().UTC(), time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	before := rbacState(t, ctx, db)
	sequences := rbacSequenceState(t, ctx, db)
	s, err := db.ExportTargets(ctx, AllTargets)
	if err != nil {
		t.Fatal(err)
	}
	if before != rbacState(t, ctx, db) || sequences != rbacSequenceState(t, ctx, db) {
		t.Fatal("RBAC export modified source principals, grants or tables")
	}
	data, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"credential-canary-never-exported", "salt-canary", "rands-canary", `"password"`, `"salt"`, `"auth_token"`} {
		if strings.Contains(string(data), secret) {
			t.Fatal("export included credential fields")
		}
	}
	return s
}

func clearRBACFixtures(t *testing.T, ctx context.Context, db *Database) {
	t.Helper()
	for _, name := range []string{"user_role", "team_role", "dashboard_acl", "team_member", "team", "org_user", "user"} {
		if _, err := db.DB.ExecContext(ctx, "DELETE FROM "+db.quote(name)); err != nil {
			t.Fatal(err)
		}
	}
	clearDashboardFixtures(t, ctx, db)
}

func rbacState(t *testing.T, ctx context.Context, db *Database) string {
	t.Helper()
	tx, err := db.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	tables := map[string]Table{}
	for _, name := range []string{"org", "data_source", "dashboard", "folder", "permission", "role", "builtin_role", "user_role", "team_role", "dashboard_acl", "user", "org_user", "team", "team_member"} {
		table, err := db.readTable(ctx, tx, tableEntry{name: name, key: "id"})
		if err != nil {
			t.Fatal(err)
		}
		tables[name] = table
	}
	data, err := json.Marshal(tables)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func rbacSequenceState(t *testing.T, ctx context.Context, db *Database) string {
	t.Helper()
	if db.Engine != "postgres" {
		return ""
	}
	tx, err := db.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	sequences := map[string]Table{}
	for _, name := range []string{"role", "permission", "builtin_role", "user_role", "team_role", "dashboard_acl"} {
		table, err := db.readTable(ctx, tx, tableEntry{name: name + "_id_seq", key: "last_value"})
		if err != nil {
			t.Fatal(err)
		}
		sequences[name] = table
	}
	data, err := json.Marshal(sequences)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func semanticRBAC(r *ResourceRBAC, mapping *PrincipalMap) []string {
	ids := map[string]string{}
	if mapping != nil {
		for _, m := range mapping.Mappings {
			ids[principalKey(m.OrgID, m.Kind, m.SourceID)] = m.TargetID
		}
	}
	roles := map[string]string{}
	for _, role := range r.Roles {
		if mapping != nil {
			role = remapRBACRole(role, ids)
		}
		roles[role.Ref] = role.OrgID + "/" + role.Name
	}
	result := []string{}
	for _, g := range r.Grants {
		result = append(result, g.OrgID+"/"+g.Scope+"/"+g.Action+"/"+roles[g.Role])
	}
	for _, a := range r.LegacyACL {
		result = append(result, "acl/"+a.OrgID+"/"+a.UID+"/"+roles[a.Role]+"/"+strconv.FormatInt(a.Permission, 10))
	}
	slices.Sort(result)
	return result
}

func TestRBACTransfer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	dbs := map[string]*Database{}
	snapshots := map[string]*Snapshot{}
	for _, engine := range []string{"mysql", "postgres"} {
		db, err := Open(ctx, engine, os.Getenv(strings.ToUpper(engine)+"_TEST_DSN"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { db.DB.Close() })
		createRBACFixtures(t, ctx, db)
		dbs[engine] = db
		snapshots[engine] = seedRBACSource(t, ctx, db)
	}
	for _, source := range []string{"mysql", "postgres"} {
		for _, target := range []string{"mysql", "postgres"} {
			for targets := Targets(1); targets <= AllTargets; targets++ {
				for _, mode := range []ImportMode{ImportRequireEmpty, ImportInsertOnly, ImportSync} {
					t.Run(source+"_to_"+target+"/"+targets.String()+"/"+string(mode), func(t *testing.T) {
						db := dbs[target]
						clearRBACFixtures(t, ctx, db)
						mapping := seedRBACPrincipals(t, ctx, db, true)
						all := snapshots[source]
						s, err := all.Select(targets)
						if err != nil {
							t.Fatal(err)
						}
						tx, err := db.DB.BeginTx(ctx, nil)
						if err != nil {
							t.Fatal(err)
						}
						defer tx.Rollback()
						for _, entry := range all.entries() {
							if !targets.Has(entry.group) {
								if _, err := db.insertTable(ctx, tx, entry, ImportRequireEmpty); err != nil {
									t.Fatal(err)
								}
							}
						}
						if !targets.Has(TargetDashboard) {
							dash, err := all.Select(TargetDashboard)
							if err != nil {
								t.Fatal(err)
							}
							dash.principalMap = mapping
							plan, err := db.planDashboardPermissions(ctx, tx, dash, ImportSync)
							if err != nil {
								t.Fatal(err)
							}
							if err := db.applyDashboardPermissions(ctx, tx, plan); err != nil {
								t.Fatal(err)
							}
						}
						if err := tx.Commit(); err != nil {
							t.Fatal(err)
						}
						before := rbacState(t, ctx, db)
						if mode == ImportSync {
							sequences := rbacSequenceState(t, ctx, db)
							result, err := db.Sync(ctx, s, SyncOptions{DryRun: true, PrincipalMap: mapping})
							if err != nil {
								t.Fatal(err)
							}
							if rbacState(t, ctx, db) != before || sequences != rbacSequenceState(t, ctx, db) {
								t.Fatal("dry-run changed principal, role, grant or dashboard data")
							}
							if targets.Has(TargetDashboard) && (result.RBACRoles.Inserted != len(s.RBAC.Roles) || result.RBACUserAssignments.Inserted != 3 || result.RBACTeamAssignments.Inserted != 1 || result.DashboardACL.Inserted != 1) {
								t.Fatal("inaccurate RBAC dry-run counters", result)
							}
							if _, err := db.Sync(ctx, s, SyncOptions{AllowDelete: true, PrincipalMap: mapping}); err != nil {
								t.Fatal(err)
							}
						} else {
							if _, err := db.ImportWithOptions(ctx, s, mode, ImportOptions{PrincipalMap: mapping}); err != nil {
								t.Fatal(err)
							}
						}
						got, err := db.ExportTargets(ctx, AllTargets)
						if err != nil {
							t.Fatal(err)
						}
						if !reflect.DeepEqual(semanticRBAC(all.RBAC, mapping), semanticRBAC(got.RBAC, nil)) {
							t.Fatal("principal-mapped RBAC did not round trip")
						}
						for _, table := range []string{"role", "permission", "user_role", "team_role", "builtin_role", "dashboard_acl"} {
							var id int64
							if err := db.DB.QueryRowContext(ctx, "SELECT MIN(id) FROM "+db.quote(table)+" WHERE id<>10001").Scan(&id); err != nil || id <= 10001 {
								t.Fatal("RBAC IDs were copied or sequence did not advance", table, id, err)
							}
						}
						before = rbacState(t, ctx, db)
						if mode == ImportSync {
							result, err := db.Sync(ctx, s, SyncOptions{AllowDelete: true, PrincipalMap: mapping})
							if err != nil || result.Inserted+result.Updated+result.Deleted != 0 {
								t.Fatal("RBAC sync not idempotent", err, result)
							}
						}
						if mode == ImportInsertOnly {
							if _, err := db.ImportWithOptions(ctx, s, mode, ImportOptions{}); err != nil {
								t.Fatal("skipped parents should not require principal mapping", err)
							}
						}
						if rbacState(t, ctx, db) != before {
							t.Fatal("idempotent import changed roles, principals or grants")
						}
					})
				}
			}
		}
	}
	for engine, db := range dbs {
		t.Run(engine+"_rbac_guards", func(t *testing.T) { testRBACGuards(t, ctx, db, snapshots[engine]) })
	}
}

func testRBACGuards(t *testing.T, ctx context.Context, db *Database, source *Snapshot) {
	reset := func() *PrincipalMap {
		clearRBACFixtures(t, ctx, db)
		seedFixtures(t, ctx, db)
		return seedRBACPrincipals(t, ctx, db, true)
	}
	s, err := source.Select(TargetDashboard)
	if err != nil {
		t.Fatal(err)
	}
	for _, failure := range []string{"missing map", "wrong uid", "missing user", "wrong flags", "wrong org role", "team membership", "duplicate assignment"} {
		t.Run(failure, func(t *testing.T) {
			mapping := reset()
			switch failure {
			case "missing map":
				mapping = nil
			case "wrong uid":
				mapping.Mappings[0].TargetUID = "incorrect"
			case "missing user":
				_, err = db.DB.ExecContext(ctx, "DELETE FROM "+db.quote("user")+" WHERE id=501")
			case "wrong flags":
				_, err = db.DB.ExecContext(ctx, "UPDATE "+db.quote("user")+" SET is_admin=TRUE WHERE id=501")
			case "wrong org role":
				_, err = db.DB.ExecContext(ctx, "UPDATE org_user SET role='Admin' WHERE user_id=501")
			case "team membership":
				_, err = db.DB.ExecContext(ctx, "DELETE FROM team_member WHERE user_id=502")
			case "duplicate assignment":
				seedRBACRole(t, ctx, db, RBACRole{OrgID: s.Scope.OrgIDs[0], Kind: "user", Name: "managed:users:501:permissions", Version: 1, Bindings: []RBACBinding{{OrgID: s.Scope.OrgIDs[0], Kind: "user", ID: "502"}}})
			}
			if err != nil {
				t.Fatal(err)
			}
			before := rbacState(t, ctx, db)
			if _, err := db.ImportWithOptions(ctx, s, ImportRequireEmpty, ImportOptions{PrincipalMap: mapping}); err == nil {
				t.Fatal("accepted invalid principal/assignment")
			}
			if rbacState(t, ctx, db) != before {
				t.Fatal("preflight failure changed target")
			}
		})
	}
	for _, kind := range []string{"global", "mixed", "fixed"} {
		t.Run(kind, func(t *testing.T) {
			mapping := reset()
			snapshot := cloneSnapshot(t, s)
			org, name := "0", "fixed:global-folder-reader"
			if kind == "mixed" {
				org, name = s.Scope.OrgIDs[0], "custom:mixed-reader"
			}
			if kind == "fixed" {
				org, name = s.Scope.OrgIDs[0], "fixed:folder-reader"
			}
			role := RBACRole{Ref: "prerequisite", OrgID: org, Name: name, UID: "prerequisite-uid", Version: 1, Kind: roleKind(name, org), Permissions: []RBACPermission{{Action: "folders:read", Scope: "folders:uid:root", Kind: "folders", Attribute: "uid", Identifier: "root"}}, Bindings: []RBACBinding{{OrgID: s.Scope.OrgIDs[0], Kind: "builtin", ID: "Viewer"}}}
			if kind == "mixed" {
				role.Permissions = append(role.Permissions, RBACPermission{Action: "datasources:read", Scope: "datasources:*", Kind: "datasources", Attribute: "*", Identifier: "*"})
			}
			snapshot.RBAC.Roles = append(snapshot.RBAC.Roles, role)
			snapshot.RBAC.Grants = append(snapshot.RBAC.Grants, DashboardPermission{OrgID: s.Scope.OrgIDs[0], Role: role.Ref, Action: "folders:read", Scope: "folders:uid:root"})
			before := rbacState(t, ctx, db)
			if _, err := db.ImportWithOptions(ctx, snapshot, ImportRequireEmpty, ImportOptions{PrincipalMap: mapping}); err == nil {
				t.Fatal("created missing global/fixed/mixed role")
			}
			if before != rbacState(t, ctx, db) {
				t.Fatal("failed prerequisite changed target")
			}
			id := seedRBACRole(t, ctx, db, role)
			if _, err := db.ImportWithOptions(ctx, snapshot, ImportRequireEmpty, ImportOptions{PrincipalMap: mapping}); err != nil {
				t.Fatal("matching pre-existing role prerequisite rejected", err)
			}
			if _, err := db.DB.ExecContext(ctx, "UPDATE "+db.quote("role")+" SET description='changed' WHERE id="+db.bind(1), id); err != nil {
				t.Fatal(err)
			}
			before = rbacState(t, ctx, db)
			if _, err := db.Sync(ctx, snapshot, SyncOptions{AllowDelete: true, PrincipalMap: mapping}); err == nil {
				t.Fatal("overwrote differing shared role")
			}
			if before != rbacState(t, ctx, db) {
				t.Fatal("differing shared role changed target")
			}
		})
	}
	t.Run("new custom role cannot include target-only resources", func(t *testing.T) {
		mapping := reset()
		wanted := cloneSnapshot(t, s)
		for i := range wanted.RBAC.Roles {
			r := &wanted.RBAC.Roles[i]
			if r.Kind == "custom" {
				r.Permissions = append(r.Permissions, RBACPermission{Action: "dashboards:read", Scope: "dashboards:uid:target-only", Kind: "dashboards", Attribute: "uid", Identifier: "target-only"})
			}
		}
		if _, err := db.DB.ExecContext(ctx, "INSERT INTO dashboard (id,org_id,uid,title,data,version,is_folder,folder_id,folder_uid,has_acl,is_public) VALUES ("+db.bindList(11)+")", 9901, s.Scope.OrgIDs[0], "target-only", "Target-only", `{"title":"Target-only","uid":"target-only"}`, 1, false, 0, "", false, false); err != nil {
			t.Fatal(err)
		}
		before := rbacState(t, ctx, db)
		if _, err := db.Sync(ctx, wanted, SyncOptions{AllowDelete: true, PrincipalMap: mapping}); err == nil || !strings.Contains(err.Error(), "custom RBAC role includes permissions outside selected resources") {
			t.Fatal("accepted partial custom role definition", err)
		}
		if before != rbacState(t, ctx, db) {
			t.Fatal("custom role preflight changed target")
		}
	})
	for _, collision := range []string{"same uid", "same name", "case insensitive"} {
		t.Run(collision, func(t *testing.T) {
			mapping := reset()
			role := RBACRole{}
			for _, r := range s.RBAC.Roles {
				if r.Kind == "custom" {
					role = r
				}
			}
			role = remapRBACRole(role, map[string]string{principalKey(s.Scope.OrgIDs[0], "user", "102"): "502"})
			switch collision {
			case "same uid":
				role.Name = "custom:different"
			case "same name":
				role.UID = "different-uid"
			case "case insensitive":
				if db.Engine != "mysql" {
					t.Skip("MySQL collation-specific collision")
				}
				role.Name = strings.ToUpper(role.Name)
				role.UID = "case-uid"
				role.Kind = "external"
			}
			seedRBACRole(t, ctx, db, role)
			before := rbacState(t, ctx, db)
			if _, err := db.ImportWithOptions(ctx, s, ImportRequireEmpty, ImportOptions{PrincipalMap: mapping}); err == nil {
				t.Fatal("accepted role identity collision")
			}
			if before != rbacState(t, ctx, db) {
				t.Fatal("collision changed data")
			}
		})
	}
	t.Run("sync selected managed grants only", func(t *testing.T) {
		mapping := reset()
		if _, err := db.ImportWithOptions(ctx, s, ImportRequireEmpty, ImportOptions{PrincipalMap: mapping}); err != nil {
			t.Fatal(err)
		}
		var roleID string
		if err := db.DB.QueryRowContext(ctx, "SELECT id FROM "+db.quote("role")+" WHERE name='managed:users:503:permissions'").Scan(&roleID); err != nil {
			t.Fatal(err)
		}
		if _, err := db.DB.ExecContext(ctx, "INSERT INTO permission (role_id,action,scope,kind,attribute,identifier,created,updated) VALUES ("+db.bindList(8)+")", roleID, "folders:read", "folders:uid:unselected", "folders", "uid", "unselected", time.Now().UTC(), time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
		outside := RBACRole{OrgID: "77", Kind: "builtin", Name: managedRole("Viewer"), Version: 1, Bindings: []RBACBinding{{OrgID: "77", Kind: "builtin", ID: "Viewer"}}, Permissions: []RBACPermission{{Action: "dashboards:read", Scope: "dashboards:uid:board", Kind: "dashboards", Attribute: "uid", Identifier: "board"}}}
		outsideID := seedRBACRole(t, ctx, db, outside)
		wanted := cloneSnapshot(t, s)
		ref := ""
		for _, r := range wanted.RBAC.Roles {
			if r.Name == "managed:users:103:permissions" {
				ref = r.Ref
			}
		}
		wanted.RBAC.Grants = slices.DeleteFunc(wanted.RBAC.Grants, func(g DashboardPermission) bool { return g.Role == ref })
		wanted.RBAC.Roles = slices.DeleteFunc(wanted.RBAC.Roles, func(r RBACRole) bool { return r.Ref == ref })
		result, err := db.Sync(ctx, wanted, SyncOptions{AllowDelete: true, PrincipalMap: mapping})
		if err != nil || result.DashboardPermissions.Deleted != 1 {
			t.Fatal("managed grant sync", err, result)
		}
		for _, id := range []string{roleID, outsideID} {
			var count int
			if err := db.DB.QueryRowContext(ctx, "SELECT COUNT(*) FROM permission WHERE role_id="+db.bind(1), id).Scan(&count); err != nil || count != 1 {
				t.Fatal("unselected resource or organization permission changed", err)
			}
		}
		var count int
		if err := db.DB.QueryRowContext(ctx, "SELECT COUNT(*) FROM user_role WHERE role_id="+db.bind(1), roleID).Scan(&count); err != nil || count != 1 {
			t.Fatal("inert assignment was deleted", err)
		}
		before := rbacState(t, ctx, db)
		if _, err := db.Sync(ctx, wanted, SyncOptions{AllowDelete: true, PrincipalMap: mapping}); err != nil || before != rbacState(t, ctx, db) {
			t.Fatal("grant removal not idempotent", err)
		}
		wrong := cloneSnapshot(t, wanted)
		wrong.RBAC.Roles = slices.DeleteFunc(wrong.RBAC.Roles, func(r RBACRole) bool { return r.Kind == "custom" })
		refs := map[string]bool{}
		for _, r := range wrong.RBAC.Roles {
			refs[r.Ref] = true
		}
		wrong.RBAC.Grants = slices.DeleteFunc(wrong.RBAC.Grants, func(g DashboardPermission) bool { return !refs[g.Role] })
		if _, err := db.Sync(ctx, wrong, SyncOptions{AllowDelete: true, PrincipalMap: mapping}); err == nil || before != rbacState(t, ctx, db) {
			t.Fatal("sync removed unrelated custom permission", err)
		}
	})
	// A failed permission insert happens after dashboard/role/assignment writes.
	mapping := reset()
	if _, err := db.DB.ExecContext(ctx, "ALTER TABLE permission ADD CONSTRAINT reject_rbac_fixture CHECK (action <> 'dashboards:read')"); err != nil {
		t.Fatal(err)
	}
	before := rbacState(t, ctx, db)
	if _, err := db.ImportWithOptions(ctx, s, ImportRequireEmpty, ImportOptions{PrincipalMap: mapping}); err == nil {
		t.Fatal("expected late permission constraint failure")
	}
	if before != rbacState(t, ctx, db) {
		t.Fatal("late RBAC failure did not roll back graph and dashboards")
	}
	query := "ALTER TABLE permission DROP CONSTRAINT reject_rbac_fixture"
	if db.Engine == "mysql" {
		query = "ALTER TABLE permission DROP CHECK reject_rbac_fixture"
	}
	if _, err := db.DB.ExecContext(ctx, query); err != nil {
		t.Fatal(err)
	}
	// Source legacy ACL disagreement and orphan/cross-org assignments fail closed.
	mapping = reset()
	if _, err := db.ImportWithOptions(ctx, s, ImportRequireEmpty, ImportOptions{PrincipalMap: mapping}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.ExecContext(ctx, "UPDATE dashboard_acl SET permission=4"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExportTargets(ctx, TargetDashboard); err == nil {
		t.Fatal("export accepted contradictory ACL")
	}
}
