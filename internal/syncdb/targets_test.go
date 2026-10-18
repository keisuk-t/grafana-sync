package syncdb

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func dashboardSnapshot() *Snapshot {
	s := validSnapshot()
	s.Version, s.Targets = 3, AllTargets.Names()
	s.Scope = Scope{OrgIDs: []string{"9007199254740993"}}
	s.DashboardPermissions = []DashboardPermission{
		{"9007199254740993", "Viewer", "folders:read", "folders:uid:root"},
		{"9007199254740993", "Editor", "dashboards:write", "dashboards:uid:board"},
	}
	s.DataSource.Columns = append(s.DataSource.Columns, "uid")
	s.DataSource.Rows[0] = append(s.DataSource.Rows[0], str("metrics-uid"))
	s.DataKeys.Columns = append(s.DataKeys.Columns, "active")
	s.DataKeys.Rows[0] = append(s.DataKeys.Rows[0], str("1"))
	s.Dashboard = Table{Columns: []string{"id", "org_id", "uid", "title", "data", "version", "is_folder", "folder_id", "folder_uid", "has_acl", "is_public"}, Rows: [][]*string{
		{str("10"), str("9007199254740993"), str("root"), str("ルート"), str(`{"title":"ルート"}`), str("1"), str("1"), str("0"), nil, str("0"), str("0")},
		{str("11"), str("9007199254740993"), str("child"), str("子フォルダ"), str(`{"title":"子フォルダ"}`), str("1"), str("1"), str("10"), str("root"), str("0"), str("0")},
		{str("12"), str("9007199254740993"), str("board"), str("ボード"), str(`{"title":"ボード","panels":[{"datasource":{"type":"prometheus","uid":"metrics-uid"}}]}`), str("2"), str("0"), str("11"), str("child"), str("0"), str("0")},
	}}
	s.Folder = Table{Columns: []string{"id", "org_id", "uid", "title", "parent_uid"}, Rows: [][]*string{
		{str("100"), str("9007199254740993"), str("root"), str("ルート"), nil},
		{str("101"), str("9007199254740993"), str("child"), str("子フォルダ"), str("root")},
	}}
	s.DashboardTag = Table{Columns: []string{"id", "dashboard_id", "term", "org_id", "dashboard_uid"}, Rows: [][]*string{{str("200"), str("12"), str("タグ"), str("9007199254740993"), str("board")}}}
	s.DashboardVersion = Table{Columns: []string{"id", "dashboard_id", "version", "data"}, Rows: [][]*string{
		{str("300"), str("12"), str("1"), str(`{"title":"ボード"}`)},
		{str("301"), str("12"), str("2"), s.Dashboard.Rows[2][4]},
	}}
	return s
}

func TestTargetsAndSnapshots(t *testing.T) {
	for targets := Targets(1); targets <= AllTargets; targets++ {
		parsed, err := ParseTargets(targets.String())
		if err != nil || parsed != targets {
			t.Fatalf("targets %v: %v", targets, err)
		}
		s, err := dashboardSnapshot().Select(targets)
		if err != nil {
			t.Fatal(err)
		}
		if len(s.entries()) != map[Targets]int{TargetOrg: 1, TargetDatasource: 3, DefaultTargets: 4, TargetDashboard: 4, TargetOrg | TargetDashboard: 5, TargetDatasource | TargetDashboard: 7, AllTargets: 8}[targets] {
			t.Fatal("wrong dependency tables")
		}
		data, err := json.Marshal(s)
		if err != nil {
			t.Fatal(err)
		}
		got, err := Decode(strings.NewReader(string(data)))
		if err != nil || !reflect.DeepEqual(s, got) {
			t.Fatalf("version 3 roundtrip %s: %v", targets, err)
		}
		if _, err := s.Select(AllTargets); targets != AllTargets && err == nil {
			t.Fatal("accepted absent target")
		}
	}
	for _, name := range []string{"", "org,", ",org", "org,,dashboard", "org,org", "datasource,datasource", "data_source", "all", "dashboard,unknown", " org", "Org"} {
		if _, err := ParseTargets(name); err == nil {
			t.Fatalf("accepted %q", name)
		}
	}
	if got, err := ParseTargets("dashboard,datasource,org"); err != nil || got != AllTargets {
		t.Fatal("order was significant")
	}
	legacy, err := validSnapshot().Select(DefaultTargets)
	if err != nil || legacy.Version != 3 || len(legacy.ScopeIDs()) != 1 {
		t.Fatal("version 2 compatibility", err)
	}
}

func TestDashboardValidation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*Snapshot)
	}{
		{"empty targets", func(s *Snapshot) { s.Targets = nil }},
		{"duplicate targets", func(s *Snapshot) { s.Targets = []string{"org", "org"} }},
		{"noncanonical target names", func(s *Snapshot) { s.Targets = []string{"org,datasource", "dashboard"} }},
		{"missing scope", func(s *Snapshot) { s.Scope.OrgIDs = nil }},
		{"scope duplicate", func(s *Snapshot) { s.Scope.OrgIDs = append(s.Scope.OrgIDs, s.Scope.OrgIDs[0]) }},
		{"scope zero", func(s *Snapshot) { s.Scope.OrgIDs = []string{"0"} }},
		{"scope mismatch", func(s *Snapshot) { s.Scope.OrgIDs = []string{"22"} }},
		{"missing table", func(s *Snapshot) { s.DashboardVersion = Table{} }},
		{"missing parent", func(s *Snapshot) { s.Dashboard.Rows[2][8] = str("absent") }},
		{"folder id mismatch", func(s *Snapshot) { s.Dashboard.Rows[2][7] = str("10") }},
		{"folder UID mismatch", func(s *Snapshot) { s.Folder.Rows[1][2] = str("absent") }},
		{"folder title mismatch", func(s *Snapshot) { s.Folder.Rows[1][3] = str("different") }},
		{"duplicate UID", func(s *Snapshot) { s.Dashboard.Rows[2][2] = str("root") }},
		{"folder cycle", func(s *Snapshot) {
			s.Dashboard.Rows[0][7] = str("11")
			s.Dashboard.Rows[0][8] = str("child")
			s.Folder.Rows[0][4] = str("child")
		}},
		{"orphan tag", func(s *Snapshot) { s.DashboardTag.Rows[0][1] = str("99") }},
		{"tag UID mismatch", func(s *Snapshot) { s.DashboardTag.Rows[0][4] = str("absent") }},
		{"orphan history", func(s *Snapshot) { s.DashboardVersion.Rows[0][1] = str("99") }},
		{"duplicate history version", func(s *Snapshot) { s.DashboardVersion.Rows[0][2] = str("2") }},
		{"custom ACL", func(s *Snapshot) { s.Dashboard.Rows[2][9] = str("1") }},
		{"public", func(s *Snapshot) { s.Dashboard.Rows[2][10] = str("1") }},
		{"library panel", func(s *Snapshot) { s.Dashboard.Rows[2][4] = str(`{"panels":[{"libraryPanel":{"uid":"x"}}]}`) }},
		{"invalid document", func(s *Snapshot) { s.Dashboard.Rows[2][4] = str(`[]`) }},
		{"unknown role", func(s *Snapshot) { s.DashboardPermissions[0].Role = "Admin" }},
		{"wrong permission scope", func(s *Snapshot) { s.DashboardPermissions[0].Scope = "folders:*" }},
		{"wrong permission kind", func(s *Snapshot) { s.DashboardPermissions[0].Scope = "dashboards:uid:root" }},
		{"wrong permission organization", func(s *Snapshot) { s.DashboardPermissions[0].OrgID = "22" }},
		{"wrong permission action", func(s *Snapshot) { s.DashboardPermissions[0].Action = "users:delete" }},
		{"duplicate permission", func(s *Snapshot) { s.DashboardPermissions = append(s.DashboardPermissions, s.DashboardPermissions[0]) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := dashboardSnapshot()
			tc.mutate(s)
			if s.Validate() == nil {
				t.Fatal("accepted invalid snapshot")
			}
		})
	}
	data, err := json.Marshal(dashboardSnapshot())
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{
		strings.Replace(string(data), `"version":3`, `"version":3,"version":3`, 1),
		strings.Replace(string(data), `"tables":{`, `"tables":{"other":{"columns":["id"],"rows":[]},`, 1),
		strings.Replace(string(data), `"scope":{`, `"scope":{"extra":true,`, 1),
		strings.Replace(string(data), `"tables":{`, `"org":{"columns":[],"rows":[]},"tables":{`, 1),
	} {
		if _, err := Decode(strings.NewReader(bad)); err == nil {
			t.Fatal("accepted invalid JSON envelope")
		}
	}
}

func TestSnapshotEnvelopeFields(t *testing.T) {
	legacy, err := json.Marshal(validSnapshot())
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"targets", "scope", "tables", "dashboard_permissions"} {
		input := strings.Replace(string(legacy), `"version":2`, `"version":2,"`+name+`":null`, 1)
		if _, err := Decode(strings.NewReader(input)); err == nil {
			t.Fatalf("version 2 accepted %s", name)
		}
	}
	selected, err := dashboardSnapshot().Select(DefaultTargets)
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(selected)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"org", "data_source", "data_keys", "secrets", "dashboard_permissions"} {
		input := strings.Replace(string(data), `"version":3`, `"version":3,"`+name+`":null`, 1)
		if _, err := Decode(strings.NewReader(input)); err == nil {
			t.Fatalf("version 3 accepted misplaced %s", name)
		}
	}
}

func TestDatasourceReferences(t *testing.T) {
	refs, dynamic, err := dashboardReferences(`{"panels":[{"datasource":{"uid":"metrics-uid"},"targets":[{"datasource":"logs"}]},{"datasource":"${DS}"},{"datasource":{"uid":"-- Mixed --"}},{"datasource":null}]}`)
	if err != nil || !dynamic || !reflect.DeepEqual(refs, []datasourceReference{{"metrics-uid", true}, {"logs", false}}) {
		// Object iteration order is intentionally unspecified.
		if err != nil || !dynamic || len(refs) != 2 {
			t.Fatalf("reference extraction: %v %v %v", refs, dynamic, err)
		}
	}
}
