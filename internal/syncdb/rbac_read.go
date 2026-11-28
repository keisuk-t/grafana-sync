package syncdb

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

func (d *Database) readRBAC(ctx context.Context, tx *sql.Tx, resources []permissionResource) (*ResourceRBAC, error) {
	r := &ResourceRBAC{Roles: []RBACRole{}, Principals: []RBACPrincipal{}, Grants: []DashboardPermission{}, LegacyACL: []RBACACL{}}
	selected := map[string][]string{}
	for _, v := range resources {
		key := v.kind + ":uid:" + v.uid
		if !slices.Contains(selected[key], v.org) {
			selected[key] = append(selected[key], v.org)
		}
	}
	scopes := make([]string, 0, len(selected))
	candidates := map[string][]string{}
	for scope, orgs := range selected {
		candidates[scope] = append(candidates[scope], orgs...)
		kind, uid, _ := strings.Cut(scope, ":uid:")
		other := ""
		if kind == "folders" {
			other = "dashboards"
		}
		if kind == "dashboards" {
			other = "folders"
		}
		if other != "" {
			key := other + ":uid:" + uid
			candidates[key] = append(candidates[key], orgs...)
		}
	}
	for scope := range candidates {
		scopes = append(scopes, scope)
	}
	slices.Sort(scopes)
	ids := map[string]bool{}
	grants := map[DashboardPermission]bool{}
	for batch := range slices.Chunk(scopes, dashboardQueryBatchSize) {
		args := make([]any, len(batch))
		for i, s := range batch {
			args[i] = s
		}
		query := "SELECT p.role_id, r.org_id, p.scope, p.action, p.kind, p.attribute, p.identifier FROM " + d.quote("permission") + " p LEFT JOIN " + d.quote("role") + " r ON r.id=p.role_id WHERE p.scope IN (" + d.bindList(len(args)) + ")"
		rows, err := tx.QueryContext(ctx, query, args...)
		if err != nil {
			return nil, dashboardDependencyError("permission", err)
		}
		for rows.Next() {
			var id, scope, action, kind, attr, uid string
			var org sql.NullString
			if err := rows.Scan(&id, &org, &scope, &action, &kind, &attr, &uid); err != nil {
				rows.Close()
				return nil, errors.New("cannot read resource RBAC permissions")
			}
			if !org.Valid {
				rows.Close()
				return nil, errors.New("resource RBAC permission references a missing role")
			}
			for _, resourceOrg := range candidates[scope] {
				if org.String != "0" && org.String != resourceOrg {
					continue
				}
				if !slices.Contains(selected[scope], resourceOrg) {
					rows.Close()
					return nil, errors.New("RBAC permission scope has the wrong resource kind")
				}
				if scope != kind+":uid:"+uid || attr != "uid" || !resourceAction(kind, action) {
					rows.Close()
					return nil, errors.New("malformed or unsupported resource RBAC permission")
				}
				p := DashboardPermission{OrgID: resourceOrg, Role: id, Scope: scope, Action: action}
				if !grants[p] {
					r.Grants = append(r.Grants, p)
					grants[p] = true
				}
				ids[id] = true
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, dashboardDependencyError("permission", err)
		}
	}
	roles, err := d.readRBACRoles(ctx, tx, ids)
	if err != nil {
		return nil, err
	}
	r.Roles = roles
	refs := map[string]RBACBinding{}
	for _, role := range roles {
		for _, b := range role.Bindings {
			if b.Kind != "builtin" {
				refs[principalKey(b.OrgID, b.Kind, b.ID)] = b
			}
		}
	}
	r.Principals, err = d.readRBACPrincipals(ctx, tx, refs)
	if err != nil {
		return nil, err
	}
	slices.SortFunc(r.Grants, func(a, b DashboardPermission) int {
		return strings.Compare(a.OrgID+"/"+a.Scope+"/"+a.Role+"/"+a.Action, b.OrgID+"/"+b.Scope+"/"+b.Role+"/"+b.Action)
	})
	return r, nil
}

func (d *Database) readRBACRoles(ctx context.Context, tx *sql.Tx, ids map[string]bool) ([]RBACRole, error) {
	roles := []RBACRole{}
	keys := make([]string, 0, len(ids))
	for id := range ids {
		keys = append(keys, id)
	}
	slices.Sort(keys)
	byID := map[string]*RBACRole{}
	for batch := range slices.Chunk(keys, dashboardQueryBatchSize) {
		args := make([]any, len(batch))
		for i, id := range batch {
			args[i] = id
		}
		rows, err := tx.QueryContext(ctx, "SELECT id, org_id, name, uid, version, COALESCE(description,''), COALESCE(display_name,''), COALESCE(group_name,''), COALESCE(hidden,FALSE) FROM "+d.quote("role")+" WHERE id IN ("+d.bindList(len(args))+") ORDER BY id", args...)
		if err != nil {
			return nil, errors.New("cannot read RBAC role metadata; check supported Grafana schema")
		}
		for rows.Next() {
			var role RBACRole
			if rows.Scan(&role.Ref, &role.OrgID, &role.Name, &role.UID, &role.Version, &role.Description, &role.DisplayName, &role.GroupName, &role.Hidden) != nil {
				rows.Close()
				return nil, errors.New("cannot decode RBAC role metadata")
			}
			role.Kind = roleKind(role.Name, role.OrgID)
			role.Permissions = []RBACPermission{}
			role.Bindings = []RBACBinding{}
			roles = append(roles, role)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, errors.New("cannot finish reading RBAC roles")
		}
	}
	if len(roles) != len(keys) {
		return nil, errors.New("missing RBAC role")
	}
	for i := range roles {
		byID[roles[i].Ref] = &roles[i]
	}
	for batch := range slices.Chunk(keys, dashboardQueryBatchSize) {
		args := make([]any, len(batch))
		for i, id := range batch {
			args[i] = id
		}
		rows, err := tx.QueryContext(ctx, "SELECT role_id, action, scope, COALESCE(kind,''), COALESCE(attribute,''), COALESCE(identifier,'') FROM "+d.quote("permission")+" WHERE role_id IN ("+d.bindList(len(args))+") ORDER BY role_id, action, scope", args...)
		if err != nil {
			return nil, errors.New("cannot inspect complete RBAC role permissions")
		}
		for rows.Next() {
			var id string
			var p RBACPermission
			if rows.Scan(&id, &p.Action, &p.Scope, &p.Kind, &p.Attribute, &p.Identifier) != nil {
				rows.Close()
				return nil, errors.New("cannot decode RBAC role permission")
			}
			byID[id].Permissions = append(byID[id].Permissions, p)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, errors.New("cannot finish reading RBAC role permissions")
		}
		for _, spec := range []struct{ table, kind, column string }{{"builtin_role", "builtin", "role"}, {"user_role", "user", "user_id"}, {"team_role", "team", "team_id"}} {
			exists, err := d.tableExists(ctx, tx, spec.table)
			if err != nil {
				return nil, err
			}
			if !exists {
				continue
			}
			group := "''"
			if spec.kind == "user" {
				columns, err := d.rbacColumns(ctx, tx, spec.table)
				if err != nil {
					return nil, err
				}
				if columns["group_mapping_uid"] {
					group = "COALESCE(group_mapping_uid,'')"
				}
			}
			rows, err := tx.QueryContext(ctx, "SELECT role_id, org_id, "+d.quote(spec.column)+", "+group+" FROM "+d.quote(spec.table)+" WHERE role_id IN ("+d.bindList(len(args))+") ORDER BY role_id, org_id, "+d.quote(spec.column), args...)
			if err != nil {
				return nil, errors.New("cannot inspect RBAC assignments")
			}
			for rows.Next() {
				var id string
				var b RBACBinding
				b.Kind = spec.kind
				if rows.Scan(&id, &b.OrgID, &b.ID, &b.GroupMappingUID) != nil {
					rows.Close()
					return nil, errors.New("cannot decode RBAC assignment")
				}
				byID[id].Bindings = append(byID[id].Bindings, b)
			}
			err = rows.Err()
			rows.Close()
			if err != nil {
				return nil, errors.New("cannot finish reading RBAC assignments")
			}
		}
	}
	for _, role := range roles {
		if !validID(role.OrgID, true) {
			return nil, errors.New("malformed RBAC role organization")
		}
		seen := map[string]bool{}
		for _, b := range role.Bindings {
			if !validID(b.OrgID, true) || role.OrgID != "0" && role.OrgID != b.OrgID || seen[bindingKey(b)] {
				return nil, errors.New("malformed, duplicate or cross-organization RBAC assignment")
			}
			seen[bindingKey(b)] = true
		}
		if role.Kind == "builtin" || role.Kind == "user" || role.Kind == "team" {
			if len(role.Bindings) != 1 || role.Bindings[0].Kind != role.Kind || role.Bindings[0].GroupMappingUID != "" {
				return nil, errors.New("managed RBAC role has incompatible assignments")
			}
			b := role.Bindings[0]
			if role.Kind == "builtin" && b.ID != builtinName(role.Name) || role.Kind != "builtin" && role.Name != "managed:"+role.Kind+"s:"+b.ID+":permissions" {
				return nil, errors.New("managed RBAC role name disagrees with assignment")
			}
		}
	}
	return roles, nil
}

func (d *Database) rbacColumns(ctx context.Context, tx *sql.Tx, table string) (map[string]bool, error) {
	rows, err := tx.QueryContext(ctx, "SELECT * FROM "+d.quote(table)+" WHERE 1=0")
	if err != nil {
		return nil, errors.New("cannot inspect RBAC dependency schema")
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		return nil, errors.New("cannot inspect RBAC columns")
	}
	result := map[string]bool{}
	for _, c := range columns {
		result[c] = true
	}
	return result, nil
}

func (d *Database) readRBACPrincipals(ctx context.Context, tx *sql.Tx, refs map[string]RBACBinding) ([]RBACPrincipal, error) {
	result := []RBACPrincipal{}
	for _, kind := range []string{"team", "user"} {
		groups := map[string][]string{}
		for _, b := range refs {
			if b.Kind == kind {
				groups[b.OrgID] = append(groups[b.OrgID], b.ID)
			}
		}
		orgs := make([]string, 0, len(groups))
		for org := range groups {
			orgs = append(orgs, org)
		}
		slices.Sort(orgs)
		for _, org := range orgs {
			slices.Sort(groups[org])
			for batch := range slices.Chunk(groups[org], dashboardQueryBatchSize) {
				args := []any{org}
				for _, id := range batch {
					args = append(args, id)
				}
				binds := make([]string, len(batch))
				for i := range batch {
					binds[i] = d.bind(i + 2)
				}
				query := "SELECT t.id, t.uid, t.name, COALESCE(t.external_uid,''), COALESCE(t.is_provisioned,FALSE) FROM " + d.quote("team") + " t WHERE t.org_id=" + d.bind(1) + " AND t.id IN (" + strings.Join(binds, ",") + ") ORDER BY t.id"
				if kind == "user" {
					join := " JOIN " + d.quote("org_user") + " ou ON ou.user_id=u.id AND ou.org_id=" + d.bind(1)
					role := "ou.role"
					if org == "0" {
						join = ""
						role = "''"
						args = args[1:]
						binds = make([]string, len(batch))
						for i := range batch {
							binds[i] = d.bind(i + 1)
						}
					}
					query = "SELECT u.id, u.uid, u.login, COALESCE(u.is_admin,FALSE), COALESCE(u.is_disabled,FALSE), COALESCE(u.is_service_account,FALSE), " + role + " FROM " + d.quote("user") + " u" + join + " WHERE u.id IN (" + strings.Join(binds, ",") + ") ORDER BY u.id"
				}
				rows, err := tx.QueryContext(ctx, query, args...)
				if err != nil {
					return nil, errors.New("cannot inspect RBAC principal identity or organization membership")
				}
				principals := []RBACPrincipal{}
				for rows.Next() {
					p := RBACPrincipal{OrgID: org, Kind: kind}
					var err error
					if kind == "team" {
						err = rows.Scan(&p.ID, &p.UID, &p.Name, &p.ExternalUID, &p.Provisioned)
					} else {
						err = rows.Scan(&p.ID, &p.UID, &p.Name, &p.Admin, &p.Disabled, &p.ServiceAccount, &p.OrgRole)
					}
					if err != nil {
						rows.Close()
						return nil, errors.New("cannot decode RBAC principal identity")
					}
					principals = append(principals, p)
				}
				err = rows.Err()
				rows.Close()
				if err != nil || len(principals) != len(batch) {
					return nil, errors.New("missing or ambiguous RBAC principal or organization membership")
				}
				if kind == "team" {
					members := map[string][]RBACMember{}
					rows, err := tx.QueryContext(ctx, "SELECT team_id, user_id, COALESCE(permission,0), COALESCE(external,FALSE) FROM "+d.quote("team_member")+" WHERE org_id="+d.bind(1)+" AND team_id IN ("+strings.Join(binds, ",")+") ORDER BY team_id,user_id", args...)
					if err != nil {
						return nil, errors.New("cannot inspect RBAC team membership")
					}
					for rows.Next() {
						var id string
						var m RBACMember
						if rows.Scan(&id, &m.ID, &m.Permission, &m.External) != nil {
							rows.Close()
							return nil, errors.New("cannot decode RBAC team membership")
						}
						members[id] = append(members[id], m)
						b := RBACBinding{OrgID: org, Kind: "user", ID: m.ID}
						refs[principalKey(org, "user", m.ID)] = b
					}
					err = rows.Err()
					rows.Close()
					if err != nil {
						return nil, errors.New("cannot finish reading RBAC team membership")
					}
					for i := range principals {
						principals[i].Members = members[principals[i].ID]
					}
				}
				result = append(result, principals...)
			}
		}
	}
	slices.SortFunc(result, func(a, b RBACPrincipal) int {
		return strings.Compare(principalKey(a.OrgID, a.Kind, a.ID), principalKey(b.OrgID, b.Kind, b.ID))
	})
	return result, nil
}

func roleIDString(id int64) string { return strconv.FormatInt(id, 10) }

func canonicalizeManagedRBAC(r *ResourceRBAC) {
	refs := map[string]string{}
	for i := range r.Roles {
		role := &r.Roles[i]
		original := role.Ref
		ref := "role:" + role.UID
		if role.Kind == "builtin" || role.Kind == "user" || role.Kind == "team" {
			ref = role.OrgID + "/" + role.Name
			sum := sha256.Sum256([]byte(ref))
			role.UID = fmt.Sprintf("%x", sum[:20])
		}
		refs[original] = ref
		role.Ref = ref
	}
	for i := range r.Grants {
		if ref, ok := refs[r.Grants[i].Role]; ok {
			r.Grants[i].Role = ref
		}
	}
	for i := range r.LegacyACL {
		r.LegacyACL[i].Role = refs[r.LegacyACL[i].Role]
	}
	slices.SortFunc(r.LegacyACL, func(a, b RBACACL) int { return strings.Compare(aclKey(a), aclKey(b)) })
	slices.SortFunc(r.Roles, func(a, b RBACRole) int { return strings.Compare(a.Ref, b.Ref) })
	slices.SortFunc(r.Grants, func(a, b DashboardPermission) int {
		return strings.Compare(a.OrgID+"/"+a.Scope+"/"+a.Role+"/"+a.Action, b.OrgID+"/"+b.Scope+"/"+b.Role+"/"+b.Action)
	})
}
