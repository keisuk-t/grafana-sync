package syncdb

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"reflect"
	"slices"
	"strings"
	"time"
)

type rbacPlan struct {
	insert, remove []DashboardPermission
	roles          []RBACRole
	ids            map[string]string
	count          TableResult
	roleCount      TableResult
	userCount      TableResult
	teamCount      TableResult
	builtinCount   TableResult
	aclCount       TableResult
	aclRemove      []legacyACLRow
	aclInsert      []legacyACLInsert
	removed        bool
}

type ImportOptions struct{ PrincipalMap *PrincipalMap }

func addRBACCounts(result *ImportResult, p *rbacPlan) {
	if p == nil {
		return
	}
	result.RBACRoles = p.roleCount
	result.RBACUserAssignments = p.userCount
	result.RBACTeamAssignments = p.teamCount
	result.RBACBuiltinAssignments = p.builtinCount
	result.DashboardACL = p.aclCount
	for _, c := range []TableResult{p.roleCount, p.userCount, p.teamCount, p.builtinCount, p.aclCount} {
		result.Inserted += c.Inserted
		result.Unchanged += c.Unchanged
		result.Deleted += c.Deleted
		result.Updated += c.Updated
		result.Skipped += c.Skipped
	}
}

func (d *Database) affectedPermissionResources(ctx context.Context, tx *sql.Tx, s *Snapshot, mode ImportMode) ([]permissionResource, error) {
	resources := []permissionResource{}
	for _, entry := range []struct {
		name  string
		table Table
	}{{"dashboard", s.Dashboard}, {"library_element", s.LibraryElement}} {
		stored, _, err := d.readSyncTarget(ctx, tx, tableEntry{name: entry.name, key: "id"})
		if err != nil {
			return nil, err
		}
		affected := entry.table
		affected.Rows = append([][]*string{}, entry.table.Rows...)
		if mode == ImportInsertOnly {
			affected.Rows = nil
			ids := syncRowsByID(stored, "id")
			for _, row := range entry.table.Rows {
				if ids[parseID(cell(entry.table, row, "id"))] == nil {
					affected.Rows = append(affected.Rows, row)
				}
			}
		}
		if mode == ImportSync {
			for _, row := range stored.Rows {
				if s.scopeIDs()[rowOrg(stored, row)] {
					affected.Rows = append(affected.Rows, reorderRow(row, stored.Columns, affected.Columns))
				}
			}
		}
		if entry.name == "dashboard" {
			resources = append(resources, permissionResources(affected, Table{})...)
		} else {
			resources = append(resources, permissionResources(Table{}, affected)...)
		}
	}
	return resources, nil
}

func (d *Database) mapRBACPrincipals(ctx context.Context, tx *sql.Tx, source *ResourceRBAC, mapping *PrincipalMap, needed map[string]bool) (map[string]string, error) {
	if err := mapping.Validate(); err != nil {
		return nil, err
	}
	bySource := map[string]PrincipalMapping{}
	if mapping != nil {
		for _, m := range mapping.Mappings {
			bySource[principalKey(m.OrgID, m.Kind, m.SourceID)] = m
		}
	}
	for _, r := range source.Roles {
		if needed[r.Ref] {
			for _, b := range r.Bindings {
				if b.Kind != "builtin" {
					needed[principalKey(b.OrgID, b.Kind, b.ID)] = true
				}
			}
		}
	}
	for _, p := range source.Principals {
		if p.Kind == "team" && needed[principalKey(p.OrgID, p.Kind, p.ID)] {
			for _, m := range p.Members {
				needed[principalKey(p.OrgID, "user", m.ID)] = true
			}
		}
	}
	refs := map[string]RBACBinding{}
	ids := map[string]string{}
	for _, p := range source.Principals {
		key := principalKey(p.OrgID, p.Kind, p.ID)
		if !needed[key] {
			continue
		}
		m, ok := bySource[key]
		if !ok {
			return nil, errors.New("RBAC requires an explicit --principal-map entry for every referenced user, service account and team member")
		}
		ids[key] = m.TargetID
		b := RBACBinding{OrgID: p.OrgID, Kind: p.Kind, ID: m.TargetID}
		refs[principalKey(b.OrgID, b.Kind, b.ID)] = b
	}
	targets, err := d.readRBACPrincipals(ctx, tx, refs)
	if err != nil {
		return nil, err
	}
	byTarget := map[string]RBACPrincipal{}
	for _, p := range targets {
		byTarget[principalKey(p.OrgID, p.Kind, p.ID)] = p
	}
	for _, p := range source.Principals {
		key := principalKey(p.OrgID, p.Kind, p.ID)
		if !needed[key] {
			continue
		}
		m := bySource[key]
		target, ok := byTarget[principalKey(p.OrgID, p.Kind, m.TargetID)]
		if !ok || target.UID != m.TargetUID || p.OrgRole != target.OrgRole || p.Admin != target.Admin || p.Disabled != target.Disabled || p.ServiceAccount != target.ServiceAccount || p.ExternalUID != target.ExternalUID || p.Provisioned != target.Provisioned {
			return nil, errors.New("RBAC principal map identity, type, organization role or external provisioning does not match the target")
		}
		expected := append([]RBACMember{}, p.Members...)
		for i := range expected {
			expected[i].ID = ids[principalKey(p.OrgID, "user", expected[i].ID)]
		}
		slices.SortFunc(expected, func(a, b RBACMember) int { return strings.Compare(a.ID, b.ID) })
		members := append([]RBACMember{}, target.Members...)
		slices.SortFunc(members, func(a, b RBACMember) int { return strings.Compare(a.ID, b.ID) })
		if !reflect.DeepEqual(expected, members) {
			return nil, errors.New("RBAC team membership differs after principal mapping; pre-create matching teams and members")
		}
	}
	return ids, nil
}

func remapRBACRole(role RBACRole, ids map[string]string) RBACRole {
	role.Bindings = append([]RBACBinding{}, role.Bindings...)
	for i := range role.Bindings {
		b := &role.Bindings[i]
		if b.Kind != "builtin" {
			b.ID = ids[principalKey(b.OrgID, b.Kind, b.ID)]
		}
	}
	if role.Kind == "user" || role.Kind == "team" {
		role.Name = "managed:" + role.Kind + "s:" + role.Bindings[0].ID + ":permissions"
	}
	return role
}

func equalRBACBindings(a, b []RBACBinding) bool {
	a = append([]RBACBinding{}, a...)
	b = append([]RBACBinding{}, b...)
	slices.SortFunc(a, func(a, b RBACBinding) int { return strings.Compare(bindingKey(a), bindingKey(b)) })
	slices.SortFunc(b, func(a, b RBACBinding) int { return strings.Compare(bindingKey(a), bindingKey(b)) })
	return reflect.DeepEqual(a, b)
}

func equalRBACDefinition(a, b RBACRole) bool {
	if a.OrgID != b.OrgID || a.Name != b.Name || a.UID != b.UID || a.Version != b.Version || a.Description != b.Description || a.DisplayName != b.DisplayName || a.GroupName != b.GroupName || a.Hidden != b.Hidden {
		return false
	}
	pa, pb := append([]RBACPermission{}, a.Permissions...), append([]RBACPermission{}, b.Permissions...)
	slices.SortFunc(pa, func(a, b RBACPermission) int { return strings.Compare(permissionKey(a), permissionKey(b)) })
	slices.SortFunc(pb, func(a, b RBACPermission) int { return strings.Compare(permissionKey(a), permissionKey(b)) })
	return reflect.DeepEqual(pa, pb)
}

func (d *Database) resolveRBACRoles(ctx context.Context, tx *sql.Tx, source []RBACRole) (map[string]*RBACRole, error) {
	ids := map[string]bool{}
	matches := map[string]string{}
	for batch := range slices.Chunk(source, dashboardQueryBatchSize) {
		args := []any{}
		conditions := []string{}
		for _, role := range batch {
			ref := d.bind(len(args) + 1)
			args = append(args, role.Ref)
			condition := "(org_id=" + d.bind(len(args)+1) + " AND name=" + d.bind(len(args)+2) + ")"
			args = append(args, role.OrgID, role.Name)
			if role.Kind == "custom" || role.Kind == "external" {
				condition = "(" + condition + " OR uid=" + d.bind(len(args)+1) + ")"
				args = append(args, role.UID)
			}
			conditions = append(conditions, "SELECT "+ref+" AS source_ref, id FROM "+d.quote("role")+" WHERE "+condition)
		}
		rows, err := tx.QueryContext(ctx, strings.Join(conditions, " UNION ALL "), args...)
		if err != nil {
			return nil, errors.New("cannot resolve target RBAC roles")
		}
		for rows.Next() {
			var ref, id string
			if rows.Scan(&ref, &id) != nil {
				rows.Close()
				return nil, errors.New("cannot decode target RBAC role")
			}
			if previous, ok := matches[ref]; ok && previous != id {
				rows.Close()
				return nil, errors.New("target RBAC role UID or organization-name collision")
			}
			matches[ref] = id
			ids[id] = true
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, errors.New("cannot finish resolving RBAC roles")
		}
	}
	roles, err := d.readRBACRoles(ctx, tx, ids)
	if err != nil {
		return nil, err
	}
	byID := map[string]*RBACRole{}
	for i := range roles {
		role := &roles[i]
		byID[role.Ref] = role
	}
	result := map[string]*RBACRole{}
	for _, role := range source {
		target := byID[matches[role.Ref]]
		if target != nil && (target.Name != role.Name || target.OrgID != role.OrgID) {
			return nil, errors.New("target RBAC role UID or organization-name collision")
		}
		result[role.Ref] = target
	}
	return result, nil
}

func (d *Database) planRBAC(ctx context.Context, tx *sql.Tx, s *Snapshot, mode ImportMode) (*dashboardPermissionPlan, error) {
	p := &rbacPlan{ids: map[string]string{}}
	plan := &dashboardPermissionPlan{rbac: p}
	resources, err := d.affectedPermissionResources(ctx, tx, s, mode)
	if err != nil {
		return nil, err
	}
	allowed := map[string]bool{}
	for _, r := range resources {
		allowed[r.org+"/"+r.kind+":uid:"+r.uid] = true
	}
	sourceScopes := map[string]bool{}
	for _, r := range permissionResources(s.Dashboard, s.LibraryElement) {
		sourceScopes[r.org+"/"+r.kind+":uid:"+r.uid] = true
	}
	needed := map[string]bool{}
	incoming := []DashboardPermission{}
	for _, g := range s.RBAC.Grants {
		if !allowed[g.OrgID+"/"+g.Scope] {
			p.count.Skipped++
			continue
		}
		incoming = append(incoming, g)
		needed[g.Role] = true
	}
	identities, err := d.mapRBACPrincipals(ctx, tx, s.RBAC, s.principalMap, needed)
	if err != nil {
		return nil, err
	}
	current, err := d.readRBAC(ctx, tx, resources)
	if err != nil {
		return nil, err
	}
	remapped := []RBACRole{}
	for _, role := range s.RBAC.Roles {
		if needed[role.Ref] {
			remapped = append(remapped, remapRBACRole(role, identities))
		} else {
			p.roleCount.Skipped++
		}
	}
	targetRoles, err := d.resolveRBACRoles(ctx, tx, remapped)
	if err != nil {
		return nil, err
	}
	for _, source := range s.RBAC.Roles {
		if !needed[source.Ref] {
			continue
		}
		role := remapRBACRole(source, identities)
		target := targetRoles[source.Ref]
		if target != nil {
			if target.Kind != role.Kind || !equalRBACBindings(role.Bindings, target.Bindings) {
				return nil, errors.New("target RBAC role has incompatible assignments; no role assignments will be overwritten")
			}
			if role.Kind == "custom" || role.Kind == "external" {
				if !equalRBACDefinition(role, *target) {
					return nil, errors.New("global, fixed, custom or shared RBAC role differs; instance-wide role changes are outside the selected resource scope")
				}
			}
			p.ids[source.Ref] = target.Ref
			p.roleCount.Unchanged++
			for _, b := range role.Bindings {
				switch b.Kind {
				case "user":
					p.userCount.Unchanged++
				case "team":
					p.teamCount.Unchanged++
				case "builtin":
					p.builtinCount.Unchanged++
				}
			}
		} else {
			if role.Kind == "external" {
				return nil, errors.New("global or fixed RBAC role is missing; instance-wide role creation is not supported")
			}
			if role.Kind == "custom" {
				for _, perm := range role.Permissions {
					if !allowed[role.OrgID+"/"+perm.Scope] || !sourceScopes[role.OrgID+"/"+perm.Scope] {
						return nil, errors.New("custom RBAC role includes permissions outside selected resources; pre-create an identical role and assignments")
					}
				}
			}
			for _, b := range role.Bindings {
				if b.GroupMappingUID != "" {
					return nil, errors.New("externally mapped RBAC assignments must already exist unchanged in target")
				}
				switch b.Kind {
				case "user":
					p.userCount.Inserted++
				case "team":
					p.teamCount.Inserted++
				case "builtin":
					p.builtinCount.Inserted++
				}
			}
			p.roles = append(p.roles, role)
			p.ids[source.Ref] = "new:" + source.Ref
			p.roleCount.Inserted++
		}
	}
	wanted, present := map[DashboardPermission]bool{}, map[DashboardPermission]bool{}
	for _, g := range incoming {
		g.Role = p.ids[g.Role]
		wanted[g] = true
	}
	currentRoles := map[string]RBACRole{}
	for _, role := range current.Roles {
		currentRoles[role.Ref] = role
	}
	for _, g := range current.Grants {
		present[g] = true
		if !wanted[g] {
			if mode != ImportSync {
				return nil, errors.New("new resource identities have RBAC permissions not present in snapshot")
			}
			role := currentRoles[g.Role]
			if role.Kind == "external" || role.Kind == "custom" {
				return nil, errors.New("sync would modify a global, fixed or custom role absent or different in snapshot; no changes applied")
			}
			p.remove = append(p.remove, g)
		}
	}
	for g := range wanted {
		if present[g] {
			p.count.Unchanged++
		} else {
			p.insert = append(p.insert, g)
		}
	}
	p.count.Inserted = len(p.insert)
	p.count.Deleted = len(p.remove)
	plan.count = p.count
	if err := d.planLegacyACL(ctx, tx, s, mode, allowed, current, identities, p); err != nil {
		return nil, err
	}
	for _, entry := range []struct {
		name  string
		count int
	}{{"user_role", p.userCount.Inserted}, {"team_role", p.teamCount.Inserted}, {"builtin_role", p.builtinCount.Inserted}} {
		if entry.count > 0 {
			exists, err := d.tableExists(ctx, tx, entry.name)
			if err != nil {
				return nil, err
			}
			if !exists {
				return nil, errors.New("target RBAC assignment table is missing")
			}
		}
	}
	return plan, nil
}

func (d *Database) createRBACRole(ctx context.Context, tx *sql.Tx, role RBACRole) (string, error) {
	uid := role.UID
	if role.Kind != "custom" {
		var data [16]byte
		if _, err := rand.Read(data[:]); err != nil {
			return "", errors.New("cannot generate RBAC role UID")
		}
		uid = hex.EncodeToString(data[:])
	}
	now := time.Now().UTC()
	args := []any{role.Name, role.Version, role.OrgID, uid, now, now, role.Description, role.DisplayName, role.GroupName, role.Hidden}
	query := "INSERT INTO " + d.quote("role") + " (name,version,org_id,uid,created,updated,description,display_name,group_name,hidden) VALUES (" + d.bindList(len(args)) + ")"
	var id int64
	var err error
	if d.Engine == "postgres" {
		err = tx.QueryRowContext(ctx, query+" RETURNING id", args...).Scan(&id)
	} else {
		var result sql.Result
		result, err = tx.ExecContext(ctx, query, args...)
		if err == nil {
			id, err = result.LastInsertId()
		}
	}
	if err != nil {
		return "", errors.New("cannot create scoped RBAC role")
	}
	for _, b := range role.Bindings {
		table, column := "user_role", "user_id"
		args := []any{roleIDString(id), b.OrgID, b.ID, now}
		if b.Kind == "team" {
			table, column = "team_role", "team_id"
		}
		if b.Kind == "builtin" {
			table, column = "builtin_role", "role"
		}
		columns := "role_id, org_id, " + d.quote(column) + ", created"
		if b.Kind == "builtin" {
			columns += ", updated"
			args = append(args, now)
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO "+d.quote(table)+" ("+columns+") VALUES ("+d.bindList(len(args))+")", args...); err != nil {
			return "", errors.New("cannot create scoped RBAC assignment")
		}
	}
	return roleIDString(id), nil
}

func (d *Database) applyRBAC(ctx context.Context, tx *sql.Tx, p *rbacPlan) error {
	if err := d.applyRBACDeletes(ctx, tx, p); err != nil {
		return err
	}
	if d.Engine == "postgres" {
		for _, entry := range []struct {
			name  string
			count int
		}{{"role", len(p.roles)}, {"permission", len(p.insert)}, {"user_role", p.userCount.Inserted}, {"team_role", p.teamCount.Inserted}, {"builtin_role", p.builtinCount.Inserted}, {"dashboard_acl", len(p.aclInsert)}} {
			if entry.count > 0 {
				if err := d.restartSequence(ctx, tx, entry.name, true); err != nil {
					return err
				}
			}
		}
	}
	ids := map[string]string{}
	for _, role := range p.roles {
		id, err := d.createRBACRole(ctx, tx, role)
		if err != nil {
			return err
		}
		ids["new:"+role.Ref] = id
	}
	for _, g := range p.insert {
		id := g.Role
		if resolved, ok := ids[id]; ok {
			id = resolved
		}
		kind, uid, _ := strings.Cut(g.Scope, ":uid:")
		now := time.Now().UTC()
		if _, err := tx.ExecContext(ctx, "INSERT INTO "+d.quote("permission")+" (role_id,action,scope,kind,attribute,identifier,created,updated) VALUES ("+d.bindList(8)+")", id, g.Action, g.Scope, kind, "uid", uid, now, now); err != nil {
			return errors.New("cannot insert scoped RBAC permission")
		}
	}
	return d.applyLegacyACL(ctx, tx, p)
}
