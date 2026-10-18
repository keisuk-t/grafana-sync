package syncdb

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strings"
	"time"
)

type legacyACLRow struct {
	RBACACL
	ID string
}
type legacyACLInsert struct {
	org, dashboard, kind, principal string
	permission                      int64
}

func rowIsFolder(t Table, row []*string) bool { return cell(t, row, "is_folder") == "1" }

func aclKey(a RBACACL) string { return a.OrgID + "/" + a.UID + "/" + a.Role }

func validateLegacyACL(r *ResourceRBAC, dashboards Table) error {
	roles := map[string]RBACRole{}
	for _, role := range r.Roles {
		roles[role.Ref] = role
	}
	grants := map[DashboardPermission]bool{}
	for _, g := range r.Grants {
		grants[g] = true
		family, level := actionSetLevel(g.Action)
		if level != 0 {
			for action := range aclActions(family, level, true) {
				expanded := g
				expanded.Action = action
				grants[expanded] = true
			}
		}
	}
	resources := map[string]string{}
	for _, resource := range permissionResources(dashboards, Table{}) {
		resources[resource.org+"/"+resource.uid] = resource.kind
	}
	seen := map[string]bool{}
	for _, a := range r.LegacyACL {
		role, ok := roles[a.Role]
		kind, found := resources[a.OrgID+"/"+a.UID]
		if !ok || !found || role.OrgID != a.OrgID || (role.Kind != "builtin" && role.Kind != "user" && role.Kind != "team") || seen[aclKey(a)] || (a.Permission != 1 && a.Permission != 2 && a.Permission != 4) {
			return errors.New("invalid, ambiguous or unsupported legacy dashboard ACL")
		}
		seen[aclKey(a)] = true
		annotationActions := []string{"annotations:read", "annotations:create", "annotations:write", "annotations:delete"}
		annotations := false
		for _, action := range annotationActions {
			annotations = annotations || grants[DashboardPermission{OrgID: a.OrgID, Role: a.Role, Action: action, Scope: kind + ":uid:" + a.UID}]
		}
		actions := aclActions(kind, a.Permission, annotations)
		for _, action := range []string{"dashboards:view", "dashboards:edit", "dashboards:admin", "folders:view", "folders:edit", "folders:admin"} {
			if !grants[DashboardPermission{OrgID: a.OrgID, Role: a.Role, Action: action, Scope: kind + ":uid:" + a.UID}] {
				continue
			}
			family, level := actionSetLevel(action)
			for expanded := range aclActions(family, level, true) {
				if !actions[expanded] {
					return errors.New("legacy dashboard ACL disagrees with current RBAC action set")
				}
			}
		}
		allActions := append(annotationActions, "dashboards:read", "dashboards:write", "dashboards:delete", "dashboards:create", "dashboards.permissions:read", "dashboards.permissions:write", "folders:create", "folders:read", "folders:write", "folders:delete", "folders.permissions:read", "folders.permissions:write", "library.panels:read", "library.panels:create", "library.panels:write", "library.panels:delete", "alert.rules:read", "alert.rules:create", "alert.rules:write", "alert.rules:delete", "alert.silences:read", "alert.silences:create", "alert.silences:write")
		for _, action := range allActions {
			g := DashboardPermission{OrgID: a.OrgID, Role: a.Role, Action: action, Scope: kind + ":uid:" + a.UID}
			if grants[g] != actions[action] {
				return errors.New("legacy dashboard ACL disagrees with current RBAC; reconcile permissions before export or import")
			}
		}
	}
	return nil
}

func actionSetLevel(action string) (string, int64) {
	family, name, _ := strings.Cut(action, ":")
	if family != "dashboards" && family != "folders" {
		return "", 0
	}
	switch name {
	case "view":
		return family, 1
	case "edit":
		return family, 2
	case "admin":
		return family, 4
	}
	return "", 0
}

func aclActions(kind string, level int64, annotations bool) map[string]bool {
	actions := map[string]bool{"dashboards:read": true}
	if level >= 2 {
		actions["dashboards:write"] = true
		actions["dashboards:delete"] = true
	}
	if kind == "folders" && level >= 2 {
		actions["dashboards:create"] = true
	}
	if level == 4 {
		actions["dashboards.permissions:read"] = true
		actions["dashboards.permissions:write"] = true
	}
	if kind == "folders" {
		actions["folders:read"] = true
		if level >= 2 {
			actions["folders:write"] = true
			actions["folders:delete"] = true
		}
		if level == 4 {
			actions["folders.permissions:read"] = true
			actions["folders.permissions:write"] = true
		}
		for _, family := range []string{"library.panels", "alert.rules", "alert.silences"} {
			actions[family+":read"] = true
			if level >= 2 {
				actions[family+":create"] = true
				actions[family+":write"] = true
				if family != "alert.silences" {
					actions[family+":delete"] = true
				}
			}
		}
	}
	annotationActions := []string{"annotations:read", "annotations:create", "annotations:write", "annotations:delete"}
	if annotations {
		actions["annotations:read"] = true
		if level >= 2 {
			for _, v := range annotationActions {
				actions[v] = true
			}
		}
	}
	return actions
}

func (d *Database) readLegacyACL(ctx context.Context, tx *sql.Tx, dashboards Table, orgs []string, r *ResourceRBAC, all bool) ([]legacyACLRow, error) {
	result := []legacyACLRow{}
	exists, err := d.tableExists(ctx, tx, "dashboard_acl")
	if err != nil || !exists {
		return result, err
	}
	roles := map[string]string{}
	for _, role := range r.Roles {
		roles[role.OrgID+"/"+role.Name] = role.Ref
	}
	byID := map[string][]*string{}
	ids := []string{}
	for _, row := range dashboards.Rows {
		key := cell(dashboards, row, "id")
		if _, ok := byID[key]; !ok {
			ids = append(ids, key)
		}
		byID[key] = row
	}
	keys := ids
	column := "dashboard_id"
	if all {
		keys = orgs
		column = "org_id"
	}
	for batch := range slices.Chunk(keys, dashboardQueryBatchSize) {
		args := make([]any, len(batch))
		for i, key := range batch {
			args[i] = key
		}
		query := "SELECT id, org_id, dashboard_id, COALESCE(user_id,0), COALESCE(team_id,0), COALESCE(" + d.quote("role") + ",''), permission FROM " + d.quote("dashboard_acl") + " WHERE " + column + " IN (" + d.bindList(len(args)) + ")"
		if all {
			query += " AND dashboard_id>0"
		}
		rows, err := tx.QueryContext(ctx, query, args...)
		if err != nil {
			return nil, errors.New("cannot inspect legacy dashboard ACL schema")
		}
		for rows.Next() {
			var a legacyACLRow
			var dashboard, user, team, role string
			if rows.Scan(&a.ID, &a.OrgID, &dashboard, &user, &team, &role, &a.Permission) != nil {
				rows.Close()
				return nil, errors.New("cannot decode legacy dashboard ACL")
			}
			parent := byID[dashboard]
			if parent == nil || a.OrgID != cell(dashboards, parent, "org_id") {
				rows.Close()
				return nil, errors.New("legacy dashboard ACL has an orphaned or cross-organization dashboard reference")
			}
			name := ""
			count := 0
			if user != "0" {
				name = "managed:users:" + user + ":permissions"
				count++
				if !validID(user, false) {
					count++
				}
			}
			if team != "0" {
				name = "managed:teams:" + team + ":permissions"
				count++
				if !validID(team, false) {
					count++
				}
			}
			if role != "" {
				name = managedRole(role)
				count++
				if builtinName(name) == "" {
					count++
				}
			}
			ref, ok := roles[a.OrgID+"/"+name]
			if count != 1 || !ok {
				rows.Close()
				return nil, errors.New("legacy dashboard ACL has no unambiguous matching managed RBAC role")
			}
			a.UID = cell(dashboards, parent, "uid")
			a.Role = ref
			result = append(result, a)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, errors.New("cannot finish reading legacy dashboard ACL")
		}
	}
	check := *r
	check.LegacyACL = []RBACACL{}
	for _, a := range result {
		check.LegacyACL = append(check.LegacyACL, a.RBACACL)
	}
	if err := validateLegacyACL(&check, dashboards); err != nil {
		return nil, err
	}
	return result, nil
}

func (d *Database) planLegacyACL(ctx context.Context, tx *sql.Tx, s *Snapshot, mode ImportMode, allowed map[string]bool, current *ResourceRBAC, identities map[string]string, p *rbacPlan) error {
	stored, _, err := d.readSyncTarget(ctx, tx, tableEntry{name: "dashboard", key: "id"})
	if err != nil {
		return err
	}
	candidates := stored
	candidates.Rows = nil
	ids := map[string]bool{}
	for _, row := range stored.Rows {
		kind := "dashboards"
		if rowIsFolder(stored, row) {
			kind = "folders"
		}
		if allowed[cell(stored, row, "org_id")+"/"+kind+":uid:"+cell(stored, row, "uid")] {
			candidates.Rows = append(candidates.Rows, row)
			ids[cell(stored, row, "id")] = true
		}
	}
	for _, row := range s.Dashboard.Rows {
		kind := "dashboards"
		if rowIsFolder(s.Dashboard, row) {
			kind = "folders"
		}
		if !ids[cell(s.Dashboard, row, "id")] && allowed[cell(s.Dashboard, row, "org_id")+"/"+kind+":uid:"+cell(s.Dashboard, row, "uid")] {
			candidates.Rows = append(candidates.Rows, reorderRow(row, s.Dashboard.Columns, candidates.Columns))
		}
	}
	rows, err := d.readLegacyACL(ctx, tx, candidates, nil, current, false)
	if err != nil {
		return err
	}
	if mode != ImportSync && len(rows) > 0 {
		return errors.New("new resource identities already have legacy dashboard ACLs")
	}
	present := map[string]legacyACLRow{}
	for _, a := range rows {
		present[aclKey(a.RBACACL)] = a
	}
	wanted := map[string]bool{}
	roles := map[string]RBACRole{}
	for _, role := range s.RBAC.Roles {
		roles[role.Ref] = remapRBACRole(role, identities)
	}
	parents := map[string]string{}
	kinds := map[string]string{}
	for _, row := range s.Dashboard.Rows {
		key := cell(s.Dashboard, row, "org_id") + "/" + cell(s.Dashboard, row, "uid")
		parents[key] = cell(s.Dashboard, row, "id")
		kind := "dashboards"
		if rowIsFolder(s.Dashboard, row) {
			kind = "folders"
		}
		kinds[key] = kind
	}
	for _, a := range s.RBAC.LegacyACL {
		key := a.OrgID + "/" + a.UID
		role := roles[a.Role]
		if !allowed[a.OrgID+"/"+kinds[key]+":uid:"+a.UID] {
			p.aclCount.Skipped++
			continue
		}
		mapped := a
		mapped.Role = p.ids[a.Role]
		logical := aclKey(mapped)
		wanted[logical] = true
		if old, ok := present[logical]; ok && old.Permission == a.Permission {
			p.aclCount.Unchanged++
			continue
		}
		b := role.Bindings[0]
		p.aclInsert = append(p.aclInsert, legacyACLInsert{org: a.OrgID, dashboard: parents[key], kind: b.Kind, principal: b.ID, permission: a.Permission})
		p.aclCount.Inserted++
		if old, ok := present[logical]; ok {
			p.aclRemove = append(p.aclRemove, old)
			p.aclCount.Deleted++
		}
	}
	for key, a := range present {
		if !wanted[key] {
			p.aclRemove = append(p.aclRemove, a)
			p.aclCount.Deleted++
		}
	}
	if len(p.aclInsert) > 0 {
		exists, err := d.tableExists(ctx, tx, "dashboard_acl")
		if err != nil {
			return err
		}
		if !exists {
			return errors.New("snapshot contains legacy dashboard ACLs but the target table is missing")
		}
	}
	return nil
}

func (d *Database) applyRBACDeletes(ctx context.Context, tx *sql.Tx, p *rbacPlan) error {
	if p == nil || p.removed {
		return nil
	}
	for _, a := range p.aclRemove {
		if _, err := tx.ExecContext(ctx, "DELETE FROM "+d.quote("dashboard_acl")+" WHERE id="+d.bind(1), a.ID); err != nil {
			return errors.New("cannot delete selected legacy dashboard ACL")
		}
	}
	for _, g := range p.remove {
		if _, err := tx.ExecContext(ctx, "DELETE FROM "+d.quote("permission")+" WHERE role_id="+d.bind(1)+" AND action="+d.bind(2)+" AND scope="+d.bind(3), g.Role, g.Action, g.Scope); err != nil {
			return errors.New("cannot delete scoped RBAC permission")
		}
	}
	p.removed = true
	return nil
}

func (d *Database) applyLegacyACL(ctx context.Context, tx *sql.Tx, p *rbacPlan) error {
	for _, a := range p.aclInsert {
		user, team, role := "0", "0", any(nil)
		if a.kind == "user" {
			user = a.principal
		}
		if a.kind == "team" {
			team = a.principal
		}
		if a.kind == "builtin" {
			role = a.principal
		}
		now := time.Now().UTC()
		query := "INSERT INTO " + d.quote("dashboard_acl") + " (org_id,dashboard_id,user_id,team_id," + d.quote("role") + ",permission,created,updated) VALUES (" + d.bindList(8) + ")"
		if _, err := tx.ExecContext(ctx, query, a.org, a.dashboard, user, team, role, a.permission, now, now); err != nil {
			return errors.New("cannot insert mapped legacy dashboard ACL")
		}
	}
	return nil
}
