package syncdb

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func rbacSnapshot() *Snapshot {
	s := librarySnapshot()
	s.Version = 5
	r := &ResourceRBAC{Roles: []RBACRole{}, Principals: []RBACPrincipal{}, Grants: []DashboardPermission{}, LegacyACL: []RBACACL{}}
	indices := map[string]int{}
	for _, g := range s.DashboardPermissions {
		ref := g.OrgID + "/" + managedRole(g.Role)
		index, ok := indices[ref]
		if !ok {
			index = len(r.Roles)
			indices[ref] = index
			r.Roles = append(r.Roles, RBACRole{Ref: ref, OrgID: g.OrgID, Kind: "builtin", Name: managedRole(g.Role), UID: g.Role, Permissions: []RBACPermission{}, Bindings: []RBACBinding{{OrgID: g.OrgID, Kind: "builtin", ID: g.Role}}})
		}
		g.Role = ref
		r.Grants = append(r.Grants, g)
		kind, uid, _ := strings.Cut(g.Scope, ":uid:")
		r.Roles[index].Permissions = append(r.Roles[index].Permissions, RBACPermission{Action: g.Action, Scope: g.Scope, Kind: kind, Attribute: "uid", Identifier: uid})
	}
	s.DashboardPermissions = nil
	s.RBAC = r
	org := s.Scope.OrgIDs[0]
	r.Principals = []RBACPrincipal{{OrgID: org, Kind: "user", ID: "9007199254740994", UID: "source-user-uid", Name: "source-user", OrgRole: "Viewer"}, {OrgID: org, Kind: "team", ID: "91", UID: "source-team-uid", Name: "source-team", Members: []RBACMember{{ID: "9007199254740994", Permission: 0}}}}
	for _, kind := range []string{"user", "team", "custom", "builtin"} {
		id, name := "9007199254740994", "managed:users:9007199254740994:permissions"
		if kind == "team" {
			id, name = "91", "managed:teams:91:permissions"
		}
		if kind == "custom" {
			name = "custom:reader"
		}
		if kind == "builtin" {
			id, name = "Admin", managedRole("Admin")
		}
		bkind := kind
		if kind == "custom" {
			bkind = "user"
		}
		role := RBACRole{Ref: kind, OrgID: org, Kind: kind, Name: name, UID: "source-" + kind, Permissions: []RBACPermission{{Action: "dashboards:read", Scope: "dashboards:uid:board", Kind: "dashboards", Attribute: "uid", Identifier: "board"}}, Bindings: []RBACBinding{{OrgID: org, Kind: bkind, ID: id}}}
		r.Roles = append(r.Roles, role)
		r.Grants = append(r.Grants, DashboardPermission{OrgID: org, Role: role.Ref, Action: "dashboards:read", Scope: "dashboards:uid:board"})
	}
	r.LegacyACL = append(r.LegacyACL, RBACACL{OrgID: org, UID: "board", Role: "user", Permission: 1})
	return s
}

func TestRBACSnapshot(t *testing.T) {
	for targets := Targets(1); targets <= AllTargets; targets++ {
		s, err := rbacSnapshot().Select(targets)
		if err != nil {
			t.Fatal(err)
		}
		data, err := json.Marshal(s)
		if err != nil {
			t.Fatal(err)
		}
		got, err := Decode(strings.NewReader(string(data)))
		if err != nil || !reflect.DeepEqual(s, got) {
			t.Fatal("v5 round trip", targets, err)
		}
		if !targets.Has(TargetDashboard) && (got.RBAC != nil || strings.Contains(string(data), "source-user-uid")) {
			t.Fatal("unselected RBAC serialized")
		}
	}
	for _, tc := range []struct {
		name   string
		mutate func(*Snapshot)
	}{
		{"missing envelope", func(s *Snapshot) { s.RBAC = nil }},
		{"missing acl array", func(s *Snapshot) { s.RBAC.LegacyACL = nil }},
		{"missing roles array", func(s *Snapshot) { s.RBAC.Roles = nil }},
		{"duplicate principal", func(s *Snapshot) { s.RBAC.Principals = append(s.RBAC.Principals, s.RBAC.Principals[0]) }},
		{"missing team member", func(s *Snapshot) { s.RBAC.Principals[1].Members[0].ID = "99" }},
		{"cross org", func(s *Snapshot) { s.RBAC.Principals[0].OrgID = "17" }},
		{"bad user metadata", func(s *Snapshot) { s.RBAC.Principals[0].Members = []RBACMember{{ID: "1"}} }},
		{"duplicate role", func(s *Snapshot) { s.RBAC.Roles = append(s.RBAC.Roles, s.RBAC.Roles[0]) }},
		{"duplicate role uid", func(s *Snapshot) { s.RBAC.Roles[1].UID = s.RBAC.Roles[0].UID }},
		{"missing binding", func(s *Snapshot) { s.RBAC.Roles[0].Bindings = nil }},
		{"managed binding mismatch", func(s *Snapshot) { s.RBAC.Roles[0].Bindings[0].ID = "Admin" }},
		{"omitted grant", func(s *Snapshot) { s.RBAC.Grants = s.RBAC.Grants[1:] }},
		{"grant role missing", func(s *Snapshot) { s.RBAC.Grants[0].Role = "missing" }},
		{"duplicate grant", func(s *Snapshot) { s.RBAC.Grants = append(s.RBAC.Grants, s.RBAC.Grants[0]) }},
		{"malformed permission", func(s *Snapshot) { s.RBAC.Roles[0].Permissions[0].Identifier = "wrong" }},
		{"acl mismatch", func(s *Snapshot) { s.RBAC.LegacyACL[0].Permission = 4 }},
		{"acl custom role", func(s *Snapshot) { s.RBAC.LegacyACL[0].Role = "custom" }},
		{"acl duplicate", func(s *Snapshot) { s.RBAC.LegacyACL = append(s.RBAC.LegacyACL, s.RBAC.LegacyACL[0]) }},
		{"acl absent resource", func(s *Snapshot) { s.RBAC.LegacyACL[0].UID = "missing" }},
		{"legacy permission field", func(s *Snapshot) { s.DashboardPermissions = []DashboardPermission{{OrgID: s.Scope.OrgIDs[0]}} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := rbacSnapshot()
			tc.mutate(s)
			if err := s.Validate(); err == nil {
				t.Fatal("accepted invalid RBAC")
			}
		})
	}
	data, _ := json.Marshal(rbacSnapshot())
	for _, bad := range []string{strings.Replace(string(data), `"grants":`, `"password":"not-exportable","grants":`, 1), strings.Replace(string(data), `"version":5`, `"version":5,"dashboard_permissions":[]`, 1)} {
		if _, err := Decode(strings.NewReader(bad)); err == nil {
			t.Fatal("accepted ambiguous or credential fields")
		}
	}
}

func TestPrincipalMap(t *testing.T) {
	valid := `{"version":1,"mappings":[{"org_id":"9007199254740993","kind":"user","source_id":"9007199254740994","target_id":"51","target_uid":"target-user-uid"}]}`
	m, err := DecodePrincipalMap(strings.NewReader(valid))
	if err != nil || m.Mappings[0].SourceID != "9007199254740994" {
		t.Fatal(err)
	}
	for _, bad := range []string{
		strings.Replace(valid, `"version":1`, `"version":2`, 1),
		strings.Replace(valid, `"version":1`, `"version":1,"version":1`, 1),
		strings.Replace(valid, `"target_uid":"target-user-uid"`, `"password":"ignored","target_uid":"target-user-uid"`, 1),
		strings.Replace(valid, `"source_id":"9007199254740994"`, `"source_id":9007199254740994`, 1),
		strings.Replace(valid, `"target_uid":"target-user-uid"`, `"target_uid":""`, 1),
		strings.Replace(valid, `"kind":"user"`, `"kind":"builtin"`, 1),
		valid + `{}`,
	} {
		if _, err := DecodePrincipalMap(strings.NewReader(bad)); err == nil {
			t.Fatal("accepted invalid principal mapping")
		}
	}
	for _, kind := range []string{"same source", "same target", "cross organization"} {
		copy := *m
		copy.Mappings = append([]PrincipalMapping{}, m.Mappings...)
		other := copy.Mappings[0]
		if kind == "same target" {
			other.SourceID = "7"
		}
		if kind == "cross organization" {
			other.OrgID = "77"
			other.TargetID = "56"
		}
		copy.Mappings = append(copy.Mappings, other)
		if err := copy.Validate(); err == nil {
			t.Fatal("accepted ambiguous mapping", kind)
		}
	}
}

func TestResourceAction(t *testing.T) {
	for _, kind := range []string{"dashboards", "folders", "library.panels"} {
		for _, action := range []string{"dashboards:unknown", "dashboards.public:write", "users:read", "alert.silences:delete", "folders:bypass"} {
			if resourceAction(kind, action) {
				t.Fatal("accepted unknown/outside-target action", kind, action)
			}
		}
	}
	if resourceAction("dashboards", "folders:read") || resourceAction("library.panels", "dashboards:read") || !resourceAction("folders", "alert.rules:read") || !resourceAction("dashboards", "annotations:write") {
		t.Fatal("resource action scope validation")
	}
}

func TestRBACACLLevels(t *testing.T) {
	for _, kind := range []string{"dashboards", "folders"} {
		for _, level := range []int64{1, 2, 4} {
			for _, annotations := range []bool{false, true} {
				s := rbacSnapshot()
				uid := "board"
				if kind == "folders" {
					uid = "root"
				}
				scope := kind + ":uid:" + uid
				actions := []string{"dashboards:read"}
				if level >= 2 {
					actions = append(actions, "dashboards:write", "dashboards:delete")
				}
				if level == 4 {
					actions = append(actions, "dashboards.permissions:read", "dashboards.permissions:write")
				}
				if annotations {
					actions = append(actions, "annotations:read")
					if level >= 2 {
						actions = append(actions, "annotations:create", "annotations:write", "annotations:delete")
					}
				}
				if kind == "folders" {
					actions = append(actions, "folders:read", "library.panels:read", "alert.rules:read", "alert.silences:read")
					if level >= 2 {
						actions = append(actions, "folders:write", "folders:delete", "dashboards:create", "library.panels:create", "library.panels:write", "library.panels:delete", "alert.rules:create", "alert.rules:write", "alert.rules:delete", "alert.silences:create", "alert.silences:write")
					}
					if level == 4 {
						actions = append(actions, "folders.permissions:read", "folders.permissions:write")
					}
				}
				s.RBAC.Grants = slices.DeleteFunc(s.RBAC.Grants, func(g DashboardPermission) bool { return g.Role == "user" })
				for i := range s.RBAC.Roles {
					if s.RBAC.Roles[i].Ref != "user" {
						continue
					}
					s.RBAC.Roles[i].Permissions = []RBACPermission{}
					for _, action := range actions {
						s.RBAC.Roles[i].Permissions = append(s.RBAC.Roles[i].Permissions, RBACPermission{Action: action, Scope: scope, Kind: kind, Attribute: "uid", Identifier: uid})
						s.RBAC.Grants = append(s.RBAC.Grants, DashboardPermission{OrgID: s.Scope.OrgIDs[0], Role: "user", Action: action, Scope: scope})
					}
				}
				s.RBAC.LegacyACL = []RBACACL{{OrgID: s.Scope.OrgIDs[0], Role: "user", UID: uid, Permission: level}}
				if err := s.Validate(); err != nil {
					t.Fatal(kind, level, annotations, err)
				}
				if annotations {
					for _, coarseOnly := range []bool{false, true} {
						encoded, err := json.Marshal(s)
						if err != nil {
							t.Fatal(err)
						}
						mixed, err := Decode(strings.NewReader(string(encoded)))
						if err != nil {
							t.Fatal(err)
						}
						set := kind + ":" + map[int64]string{1: "view", 2: "edit", 4: "admin"}[level]
						if coarseOnly {
							mixed.RBAC.Grants = slices.DeleteFunc(mixed.RBAC.Grants, func(g DashboardPermission) bool { return g.Role == "user" })
						}
						mixed.RBAC.Grants = append(mixed.RBAC.Grants, DashboardPermission{OrgID: s.Scope.OrgIDs[0], Role: "user", Action: set, Scope: scope})
						for i := range mixed.RBAC.Roles {
							r := &mixed.RBAC.Roles[i]
							if r.Ref != "user" {
								continue
							}
							if coarseOnly {
								r.Permissions = []RBACPermission{}
							}
							r.Permissions = append(r.Permissions, RBACPermission{Action: set, Scope: scope, Kind: kind, Attribute: "uid", Identifier: uid})
						}
						if err := mixed.Validate(); err != nil {
							t.Fatal("action set ACL", kind, level, coarseOnly, err)
						}
						if level > 1 {
							mixed.RBAC.LegacyACL[0].Permission = 1
							if err := mixed.Validate(); err == nil {
								t.Fatal("action set exceeds ACL")
							}
						}
					}
				}
				s.RBAC.LegacyACL[0].Permission = 3
				if err := s.Validate(); err == nil {
					t.Fatal("unsupported ACL level accepted")
				}
			}
		}
	}
}
