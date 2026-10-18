package syncdb

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"
)

type ResourceRBAC struct {
	Roles      []RBACRole            `json:"roles"`
	Principals []RBACPrincipal       `json:"principals"`
	Grants     []DashboardPermission `json:"grants"`
	LegacyACL  []RBACACL             `json:"legacy_acl"`
}

type RBACACL struct {
	OrgID      string `json:"org_id"`
	UID        string `json:"uid"`
	Role       string `json:"role"`
	Permission int64  `json:"permission"`
}

type RBACPermission struct {
	Action     string `json:"action"`
	Scope      string `json:"scope"`
	Kind       string `json:"kind"`
	Attribute  string `json:"attribute"`
	Identifier string `json:"identifier"`
}

type RBACBinding struct {
	OrgID           string `json:"org_id"`
	Kind            string `json:"kind"`
	ID              string `json:"id"`
	GroupMappingUID string `json:"group_mapping_uid,omitempty"`
}

type RBACRole struct {
	Ref         string           `json:"ref"`
	OrgID       string           `json:"org_id"`
	Kind        string           `json:"kind"`
	Name        string           `json:"name"`
	UID         string           `json:"uid"`
	Version     int64            `json:"version"`
	Description string           `json:"description"`
	DisplayName string           `json:"display_name"`
	GroupName   string           `json:"group_name"`
	Hidden      bool             `json:"hidden"`
	Permissions []RBACPermission `json:"permissions"`
	Bindings    []RBACBinding    `json:"bindings"`
}

type RBACMember struct {
	ID         string `json:"id"`
	Permission int64  `json:"permission"`
	External   bool   `json:"external"`
}

type RBACPrincipal struct {
	OrgID          string       `json:"org_id"`
	Kind           string       `json:"kind"`
	ID             string       `json:"id"`
	UID            string       `json:"uid"`
	Name           string       `json:"name"`
	OrgRole        string       `json:"org_role,omitempty"`
	Admin          bool         `json:"admin,omitempty"`
	Disabled       bool         `json:"disabled,omitempty"`
	ServiceAccount bool         `json:"service_account,omitempty"`
	ExternalUID    string       `json:"external_uid,omitempty"`
	Provisioned    bool         `json:"provisioned,omitempty"`
	Members        []RBACMember `json:"members,omitempty"`
}

type PrincipalMapping struct {
	OrgID     string `json:"org_id"`
	Kind      string `json:"kind"`
	SourceID  string `json:"source_id"`
	TargetID  string `json:"target_id"`
	TargetUID string `json:"target_uid"`
}

type PrincipalMap struct {
	Version  int                `json:"version"`
	Mappings []PrincipalMapping `json:"mappings"`
}

func DecodePrincipalMap(r io.Reader) (*PrincipalMap, error) {
	data, err := io.ReadAll(io.LimitReader(r, (16<<20)+1))
	if err != nil {
		return nil, errors.New("cannot read principal map")
	}
	if len(data) > 16<<20 {
		return nil, errors.New("principal map exceeds 16 MiB size limit")
	}
	if err := uniqueJSONFields(json.NewDecoder(bytes.NewReader(data))); err != nil {
		return nil, err
	}
	var m PrincipalMap
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if dec.Decode(&m) != nil || dec.Decode(new(any)) != io.EOF {
		return nil, errors.New("invalid principal map JSON")
	}
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return &m, nil
}

func principalKey(org, kind, id string) string { return org + "/" + kind + "/" + id }

func validID(id string, zero bool) bool {
	n, err := strconv.ParseInt(id, 10, 64)
	return err == nil && (n > 0 || zero && n == 0) && strconv.FormatInt(n, 10) == id
}

func (m *PrincipalMap) Validate() error {
	if m == nil {
		return nil
	}
	if m.Version != 1 || m.Mappings == nil {
		return errors.New("principal map requires version 1 and mappings array")
	}
	seen, targets := map[string]bool{}, map[string]bool{}
	users, teams := map[string]string{}, map[string]string{}
	reverse := map[string]string{}
	uids := map[string]string{}
	identities := map[string]string{}
	for _, p := range m.Mappings {
		key, target := principalKey(p.OrgID, p.Kind, p.SourceID), principalKey(p.OrgID, p.Kind, p.TargetID)
		if !validID(p.OrgID, true) || !validID(p.SourceID, false) || !validID(p.TargetID, false) || (p.Kind != "user" && p.Kind != "team") || p.TargetUID == "" || len(p.TargetUID) > 40 || seen[key] || targets[target] {
			return errors.New("invalid, duplicate or ambiguous principal mapping")
		}
		global := users
		if p.Kind == "team" {
			global = teams
		}
		if prior, ok := global[p.SourceID]; ok && prior != p.TargetID {
			return errors.New("principal mapping differs across organizations")
		}
		targetKey := p.Kind + "/" + p.TargetID
		uidKey := p.Kind + "/" + p.TargetUID
		if prior, ok := reverse[targetKey]; ok && prior != p.SourceID {
			return errors.New("principal mapping merges different identities")
		}
		if prior, ok := uids[targetKey]; ok && prior != p.TargetUID {
			return errors.New("principal UID mapping differs across organizations")
		}
		if prior, ok := identities[uidKey]; ok && prior != p.TargetID {
			return errors.New("principal UID mapping is ambiguous")
		}
		reverse[targetKey] = p.SourceID
		uids[targetKey] = p.TargetUID
		identities[uidKey] = p.TargetID
		global[p.SourceID] = p.TargetID
		seen[key], targets[target] = true, true
	}
	return nil
}

func builtinName(name string) string {
	for _, b := range []string{"Viewer", "Editor", "Admin"} {
		if name == managedRole(b) {
			return b
		}
	}
	return ""
}

func roleKind(name, org string) string {
	if org == "0" {
		return "external"
	}
	if builtinName(name) != "" {
		return "builtin"
	}
	for _, kind := range []string{"user", "team"} {
		prefix := "managed:" + kind + "s:"
		if strings.HasPrefix(name, prefix) && strings.HasSuffix(name, ":permissions") {
			id := strings.TrimSuffix(strings.TrimPrefix(name, prefix), ":permissions")
			if validID(id, false) {
				return kind
			}
		}
	}
	if strings.HasPrefix(name, "custom:") {
		return "custom"
	}
	return "external"
}

func permissionKey(p RBACPermission) string { return p.Action + "\x00" + p.Scope }
func bindingKey(b RBACBinding) string {
	return principalKey(b.OrgID, b.Kind, b.ID) + "/" + b.GroupMappingUID
}

func resourceAction(kind, action string) bool {
	switch action {
	case "dashboards:view", "dashboards:edit", "dashboards:admin", "dashboards:read", "dashboards:write", "dashboards:delete", "dashboards.permissions:read", "dashboards.permissions:write", "annotations:read", "annotations:create", "annotations:write", "annotations:delete":
		return kind == "dashboards" || kind == "folders"
	case "folders:view", "folders:edit", "folders:admin", "folders:create", "folders:read", "folders:write", "folders:delete", "folders.permissions:read", "folders.permissions:write", "dashboards:create", "alert.rules:read", "alert.rules:create", "alert.rules:write", "alert.rules:delete", "alert.silences:read", "alert.silences:create", "alert.silences:write":
		return kind == "folders"
	case "library.panels:create", "library.panels:read", "library.panels:write", "library.panels:delete":
		return kind == "folders" || kind == "library.panels"
	}
	return false
}

func (s *Snapshot) validateRBAC() error {
	if s.Version < 5 || !s.Selection().Has(TargetDashboard) {
		if s.RBAC != nil {
			return errors.New("RBAC envelope requires a version 5 dashboard target")
		}
		return nil
	}
	r := s.RBAC
	if r == nil || r.Roles == nil || r.Principals == nil || r.Grants == nil || r.LegacyACL == nil || len(s.DashboardPermissions) > 0 {
		return errors.New("version 5 requires explicit RBAC roles, principals and grants arrays")
	}
	principals := map[string]RBACPrincipal{}
	orgScope := s.scopeIDs()
	for _, p := range r.Principals {
		key := principalKey(p.OrgID, p.Kind, p.ID)
		if !validID(p.OrgID, true) || !validID(p.ID, false) || p.UID == "" || len(p.UID) > 40 || (p.Kind != "user" && p.Kind != "team") {
			return errors.New("invalid RBAC principal identity")
		}
		if _, exists := principals[key]; exists {
			return errors.New("duplicate RBAC principal")
		}
		if p.OrgID != "0" && !orgScope[parseID(p.OrgID)] {
			return errors.New("RBAC principal outside organization scope")
		}
		if p.Kind == "user" && (len(p.Members) > 0 || p.ExternalUID != "" || p.Provisioned) || p.Kind == "team" && (p.OrgID == "0" || p.Admin || p.Disabled || p.ServiceAccount || p.OrgRole != "") {
			return errors.New("incompatible RBAC principal metadata")
		}
		if p.Kind == "user" && p.OrgID != "0" && p.OrgRole != "Admin" && p.OrgRole != "Editor" && p.OrgRole != "Viewer" && p.OrgRole != "None" {
			return errors.New("invalid RBAC principal organization role")
		}
		principals[key] = p
	}
	for _, p := range r.Principals {
		seen := map[string]bool{}
		for _, member := range p.Members {
			if _, ok := principals[principalKey(p.OrgID, "user", member.ID)]; !ok || seen[member.ID] || member.Permission < 0 {
				return errors.New("invalid or missing RBAC team member")
			}
			seen[member.ID] = true
		}
	}
	roles := map[string]RBACRole{}
	permissions := map[string]map[string]RBACPermission{}
	uids, names := map[string]bool{}, map[string]bool{}
	for _, role := range r.Roles {
		if role.Ref == "" || !validID(role.OrgID, true) || role.Kind != roleKind(role.Name, role.OrgID) || len(role.Name) > 190 || role.Name == "" || role.UID == "" || len(role.UID) > 40 || role.Version < 0 || role.Permissions == nil || role.Bindings == nil {
			return errors.New("invalid RBAC role definition")
		}
		if _, ok := roles[role.Ref]; ok || uids[role.UID] || names[role.OrgID+"/"+role.Name] {
			return errors.New("duplicate RBAC role")
		}
		if role.OrgID != "0" && !orgScope[parseID(role.OrgID)] {
			return errors.New("RBAC role outside organization scope")
		}
		seen := map[string]bool{}
		permissions[role.Ref] = map[string]RBACPermission{}
		for _, p := range role.Permissions {
			if p.Action == "" || len(p.Action) > 190 || len(p.Scope) > 190 || seen[permissionKey(p)] {
				return errors.New("invalid or duplicate RBAC role permission")
			}
			seen[permissionKey(p)] = true
			permissions[role.Ref][p.Action+"/"+p.Scope] = p
		}
		seen = map[string]bool{}
		for _, b := range role.Bindings {
			if !validID(b.OrgID, true) || b.OrgID != "0" && !orgScope[parseID(b.OrgID)] || role.OrgID != "0" && b.OrgID != role.OrgID || seen[bindingKey(b)] {
				return errors.New("invalid or duplicate RBAC assignment")
			}
			if b.Kind == "builtin" {
				if b.ID != "Viewer" && b.ID != "Editor" && b.ID != "Admin" && b.ID != "None" && b.ID != "Grafana Admin" {
					return errors.New("unknown RBAC builtin assignment")
				}
			} else if _, ok := principals[principalKey(b.OrgID, b.Kind, b.ID)]; !ok || (b.Kind != "user" && b.Kind != "team") {
				return errors.New("missing RBAC assignment principal")
			}
			seen[bindingKey(b)] = true
		}
		if role.Kind == "builtin" || role.Kind == "user" || role.Kind == "team" {
			if len(role.Bindings) != 1 || role.Bindings[0].Kind != role.Kind || role.Bindings[0].GroupMappingUID != "" {
				return errors.New("managed RBAC role has incompatible assignments")
			}
			b := role.Bindings[0]
			if role.Kind == "builtin" && b.ID != builtinName(role.Name) || role.Kind != "builtin" && role.Name != "managed:"+role.Kind+"s:"+b.ID+":permissions" {
				return errors.New("managed RBAC role name disagrees with assignment")
			}
		}
		roles[role.Ref] = role
		uids[role.UID] = true
		names[role.OrgID+"/"+role.Name] = true
	}
	resources := map[string]bool{}
	resourcesByScope := map[string][]string{}
	for _, p := range permissionResources(s.Dashboard, s.LibraryElement) {
		resources[p.org+"/"+p.kind+":uid:"+p.uid] = true
		key := p.kind + ":uid:" + p.uid
		resourcesByScope[key] = append(resourcesByScope[key], p.org)
	}
	seen := map[DashboardPermission]bool{}
	for _, grant := range r.Grants {
		role, ok := roles[grant.Role]
		kind, uid, _ := strings.Cut(grant.Scope, ":uid:")
		if !ok || (role.OrgID != "0" && role.OrgID != grant.OrgID) || !resources[grant.OrgID+"/"+grant.Scope] || !resourceAction(kind, grant.Action) || seen[grant] {
			return errors.New("invalid, duplicate or unsupported RBAC resource grant")
		}
		p, found := permissions[grant.Role][grant.Action+"/"+grant.Scope]
		if !found || p.Kind != kind || p.Attribute != "uid" || p.Identifier != uid {
			return errors.New("RBAC grant disagrees with role definition")
		}
		seen[grant] = true
	}
	for _, role := range r.Roles {
		for _, p := range role.Permissions {
			for _, org := range resourcesByScope[p.Scope] {
				if (role.OrgID == "0" || role.OrgID == org) && !seen[DashboardPermission{OrgID: org, Role: role.Ref, Action: p.Action, Scope: p.Scope}] {
					return errors.New("RBAC envelope omits a resource grant")
				}
			}
		}
	}
	return validateLegacyACL(r, s.Dashboard)
}

func parseID(id string) int64 { n, _ := strconv.ParseInt(id, 10, 64); return n }
