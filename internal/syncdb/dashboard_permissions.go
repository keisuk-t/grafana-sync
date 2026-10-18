package syncdb

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Managed role and permission IDs are local to each Grafana installation.
type DashboardPermission struct {
	OrgID  string `json:"org_id"`
	Role   string `json:"role"`
	Action string `json:"action"`
	Scope  string `json:"scope"`
}

func (s *Snapshot) permissionEnvelope() *[]DashboardPermission {
	if !s.Selection().Has(TargetDashboard) {
		return nil
	}
	rows := append([]DashboardPermission{}, s.DashboardPermissions...)
	return &rows
}

func managedRole(role string) string {
	return "managed:builtins:" + strings.ToLower(role) + ":permissions"
}

func permissionScope(t Table, row []*string) string {
	kind := "dashboards"
	if cell(t, row, "is_folder") == "1" {
		kind = "folders"
	}
	return kind + ":uid:" + cell(t, row, "uid")
}

var permissionAction = regexp.MustCompile(`^(dashboards(\.permissions)?|folders(\.permissions)?|library\.panels|alert\.rules|alert\.silences|annotations):[a-z]+$`)

func (s *Snapshot) validateDashboardPermissions() error {
	resources := make(map[string]bool)
	for _, row := range s.Dashboard.Rows {
		resources[cell(s.Dashboard, row, "org_id")+"/"+permissionScope(s.Dashboard, row)] = true
	}
	seen := make(map[DashboardPermission]bool)
	for _, p := range s.DashboardPermissions {
		if (p.Role != "Viewer" && p.Role != "Editor") || !permissionAction.MatchString(p.Action) || len(p.Action) > 190 || !resources[p.OrgID+"/"+p.Scope] || seen[p] {
			return errors.New("invalid, duplicate or unsupported dashboard permission")
		}
		seen[p] = true
	}
	return nil
}

func (d *Database) checkBuiltinBinding(ctx context.Context, tx *sql.Tx, id int64, org, role string) error {
	var count, matching int
	query := "SELECT COUNT(*), COALESCE(SUM(CASE WHEN org_id = " + d.bind(1) + " AND " + d.quote("role") + " = " + d.bind(2) + " THEN 1 ELSE 0 END), 0) FROM " + d.quote("builtin_role") + " WHERE role_id = " + d.bind(3)
	if err := tx.QueryRowContext(ctx, query, org, role, id).Scan(&count, &matching); err != nil || count != 1 || matching != 1 {
		return errors.New("managed dashboard role has a missing or incompatible builtin-role binding")
	}
	for _, table := range []string{"user_role", "team_role"} {
		exists, err := d.tableExists(ctx, tx, table)
		if err != nil {
			return err
		}
		if !exists {
			continue
		}
		if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+d.quote(table)+" WHERE role_id = "+d.bind(1), id).Scan(&count); err != nil || count != 0 {
			return errors.New("managed dashboard role has unsupported individual assignments")
		}
	}
	return nil
}

func (d *Database) readDashboardPermissions(ctx context.Context, tx *sql.Tx, dashboards Table) ([]DashboardPermission, error) {
	permissions := []DashboardPermission{}
	checked := make(map[int64]bool)
	type resource struct{ org, scope string }
	resources := []resource{}
	seen := make(map[string]bool)
	expected := make(map[string]bool)
	for _, row := range dashboards.Rows {
		org, uid := strconv.FormatInt(rowOrg(dashboards, row), 10), cell(dashboards, row, "uid")
		expected[org+"/"+permissionScope(dashboards, row)] = true
		for _, kind := range []string{"dashboards", "folders"} {
			scope := kind + ":uid:" + uid
			if !seen[org+"/"+scope] {
				resources = append(resources, resource{org, scope})
				seen[org+"/"+scope] = true
			}
		}
	}
	for batch := range slices.Chunk(resources, dashboardQueryBatchSize) {
		args := make([]any, 0, 2*len(batch))
		conditions := make([]string, 0, len(batch))
		for _, r := range batch {
			conditions = append(conditions, "(p.scope = "+d.bind(len(args)+1)+" AND (r.org_id = "+d.bind(len(args)+2)+" OR r.org_id = 0 OR r.id IS NULL))")
			args = append(args, r.scope, r.org)
		}
		query := "SELECT p.role_id, COALESCE(r.org_id, 0), COALESCE(r.name, ''), p.scope, p.action, p.kind, p.attribute, p.identifier FROM " + d.quote("permission") + " p LEFT JOIN " + d.quote("role") + " r ON r.id = p.role_id WHERE " + strings.Join(conditions, " OR ")
		rows, err := tx.QueryContext(ctx, query, args...)
		if err != nil {
			return nil, dashboardDependencyError("permission", err)
		}
		bindings := make(map[int64]struct{ org, role string })
		for rows.Next() {
			var id int64
			var org, name, scope, action, storedKind, attribute, identifier string
			if err := rows.Scan(&id, &org, &name, &scope, &action, &storedKind, &attribute, &identifier); err != nil {
				rows.Close()
				return nil, errors.New("cannot read dashboard resource permissions")
			}
			role := "Viewer"
			if name == managedRole("Editor") {
				role = "Editor"
			}
			if name != managedRole(role) || !permissionAction.MatchString(action) || !expected[org+"/"+scope] || (storedKind != "dashboards" && storedKind != "folders") || scope != storedKind+":uid:"+identifier || attribute != "uid" {
				rows.Close()
				return nil, errors.New("dashboard has unsupported individual, team, custom-role or malformed RBAC permissions; no changes applied")
			}
			permissions = append(permissions, DashboardPermission{org, role, action, scope})
			bindings[id] = struct{ org, role string }{org, role}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, dashboardDependencyError("permission", err)
		}
		for id, binding := range bindings {
			if !checked[id] {
				if err := d.checkBuiltinBinding(ctx, tx, id, binding.org, binding.role); err != nil {
					return nil, err
				}
				checked[id] = true
			}
		}
	}
	slices.SortFunc(permissions, func(a, b DashboardPermission) int {
		return strings.Compare(a.OrgID+"/"+a.Scope+"/"+a.Role+"/"+a.Action, b.OrgID+"/"+b.Scope+"/"+b.Role+"/"+b.Action)
	})
	return permissions, nil
}

type dashboardPermissionPlan struct {
	insert, remove []DashboardPermission
	count          TableResult
}

func (d *Database) planDashboardPermissions(ctx context.Context, tx *sql.Tx, s *Snapshot, mode ImportMode) (*dashboardPermissionPlan, error) {
	plan := &dashboardPermissionPlan{}
	if !s.Selection().Has(TargetDashboard) {
		return plan, nil
	}
	stored, _, err := d.readSyncTarget(ctx, tx, tableEntry{name: "dashboard", key: "id"})
	if err != nil {
		return nil, err
	}
	affected := s.Dashboard
	affected.Rows = append([][]*string{}, s.Dashboard.Rows...)
	existingIDs := syncRowsByID(stored, "id")
	if mode == ImportInsertOnly {
		affected.Rows = nil
		for _, row := range s.Dashboard.Rows {
			id, _ := syncID(row, s.Dashboard.Columns, "id")
			if existingIDs[id] == nil {
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
	current, err := d.readDashboardPermissions(ctx, tx, affected)
	if err != nil {
		return nil, err
	}
	if mode != ImportSync && len(current) > 0 {
		return nil, errors.New("new dashboard identities already have resource permissions in the target")
	}
	wanted := make(map[DashboardPermission]bool)
	allowed := make(map[string]bool)
	for _, row := range affected.Rows {
		allowed[cell(affected, row, "org_id")+"/"+permissionScope(affected, row)] = true
	}
	for _, p := range s.DashboardPermissions {
		if mode == ImportInsertOnly && !allowed[p.OrgID+"/"+p.Scope] {
			plan.count.Skipped++
			continue
		}
		wanted[p] = true
	}
	present := make(map[DashboardPermission]bool)
	for _, p := range current {
		present[p] = true
		if !wanted[p] {
			plan.remove = append(plan.remove, p)
		}
	}
	checkedRoles := make(map[string]bool)
	for _, p := range s.DashboardPermissions {
		if !wanted[p] {
			continue
		}
		if present[p] {
			plan.count.Unchanged++
			continue
		}
		if !checkedRoles[p.OrgID+"/"+p.Role] {
			if _, err := d.dashboardRole(ctx, tx, p.OrgID, p.Role, false); err != nil {
				return nil, err
			}
			checkedRoles[p.OrgID+"/"+p.Role] = true
		}
		plan.insert = append(plan.insert, p)
	}
	plan.count.Inserted, plan.count.Deleted = len(plan.insert), len(plan.remove)
	return plan, nil
}

func (d *Database) dashboardRole(ctx context.Context, tx *sql.Tx, org, role string, create bool) (int64, error) {
	var id int64
	err := tx.QueryRowContext(ctx, "SELECT id FROM "+d.quote("role")+" WHERE org_id = "+d.bind(1)+" AND name = "+d.bind(2), org, managedRole(role)).Scan(&id)
	if err == nil {
		return id, d.checkBuiltinBinding(ctx, tx, id, org, role)
	}
	if err != sql.ErrNoRows {
		return 0, errors.New("cannot resolve target dashboard role")
	}
	if !create {
		return 0, nil
	}
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return 0, errors.New("cannot generate managed role UID")
	}
	now := time.Now().UTC()
	args := []any{managedRole(role), int64(0), org, hex.EncodeToString(random[:]), now, now}
	query := "INSERT INTO " + d.quote("role") + " (name, version, org_id, uid, created, updated) VALUES (" + d.bindList(6) + ")"
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
		return 0, errors.New("cannot create target managed dashboard role")
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO "+d.quote("builtin_role")+" ("+d.quote("role")+", role_id, org_id, created, updated) VALUES ("+d.bindList(5)+")", role, id, org, now, now)
	if err != nil {
		return 0, errors.New("cannot bind target managed dashboard role")
	}
	return id, nil
}

func (d *Database) bindList(n int) string {
	values := make([]string, n)
	for i := range values {
		values[i] = d.bind(i + 1)
	}
	return strings.Join(values, ", ")
}

func (d *Database) applyDashboardPermissions(ctx context.Context, tx *sql.Tx, plan *dashboardPermissionPlan) error {
	for _, p := range plan.remove {
		id, err := d.dashboardRole(ctx, tx, p.OrgID, p.Role, false)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM "+d.quote("permission")+" WHERE role_id = "+d.bind(1)+" AND action = "+d.bind(2)+" AND scope = "+d.bind(3), id, p.Action, p.Scope); err != nil {
			return errors.New("cannot delete dashboard permission")
		}
	}
	for _, p := range plan.insert {
		id, err := d.dashboardRole(ctx, tx, p.OrgID, p.Role, true)
		if err != nil {
			return err
		}
		kind, uid, _ := strings.Cut(p.Scope, ":uid:")
		now := time.Now().UTC()
		if _, err := tx.ExecContext(ctx, "INSERT INTO "+d.quote("permission")+" (role_id, action, scope, kind, attribute, identifier, created, updated) VALUES ("+d.bindList(8)+")", id, p.Action, p.Scope, kind, "uid", uid, now, now); err != nil {
			return errors.New("cannot insert dashboard permission")
		}
	}
	return nil
}

func addPermissionCounts(result *ImportResult, count TableResult) {
	result.DashboardPermissions = count
	result.Inserted += count.Inserted
	result.Deleted += count.Deleted
	result.Unchanged += count.Unchanged
	result.Skipped += count.Skipped
}
