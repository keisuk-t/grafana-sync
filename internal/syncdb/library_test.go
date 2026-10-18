package syncdb

import (
	"context"
	"encoding/json"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func TestRehomeOrphanLibraryPanels(t *testing.T) {
	for _, version := range []struct {
		name string
		make func() *Snapshot
	}{{"v4", librarySnapshot}, {"v5", rbacSnapshot}} {
		for _, tc := range []struct {
			name      string
			id, uid   *string
			rehome    bool
			wantValid bool
		}{
			{"NULL UID orphan", str("404"), nil, true, true},
			{"empty UID orphan", str("404"), str(""), true, true},
			{"UID folder", str("0"), str("child"), false, true},
			{"consistent folder", str("11"), str("child"), false, true},
			{"root NULL", str("0"), nil, false, true},
			{"root empty", str("0"), str(""), false, true},
			{"root general", str("0"), str("general"), false, true},
			{"existing folder ID", str("11"), nil, false, false},
			{"existing dashboard ID", str("12"), nil, false, false},
			{"conflicting general", str("404"), str("general"), false, false},
			{"missing UID reference", str("404"), str("missing"), false, false},
			{"existing UID with missing ID", str("404"), str("child"), false, false},
			{"NULL ID", nil, nil, false, false},
			{"negative ID", str("-1"), nil, false, false},
			{"invalid ID", str("not-an-id"), nil, false, false},
		} {
			t.Run(version.name+"/"+tc.name, func(t *testing.T) {
				s := version.make()
				row := s.LibraryElement.Rows[0]
				row[columnIndex(s.LibraryElement.Columns, "folder_id")] = tc.id
				row[columnIndex(s.LibraryElement.Columns, "folder_uid")] = tc.uid
				want := version.make()
				want.LibraryElement.Rows[0][columnIndex(want.LibraryElement.Columns, "folder_id")] = tc.id
				want.LibraryElement.Rows[0][columnIndex(want.LibraryElement.Columns, "folder_uid")] = tc.uid
				wantCount := 0
				if tc.rehome {
					if err := s.Validate(); err == nil || !strings.Contains(err.Error(), "--rehome-orphan-library-panels") || strings.Contains(err.Error(), "shared") {
						t.Fatal("orphan was not safely rejected by default", err)
					}
					wantCount = 1
					want.LibraryElement.Rows[0][columnIndex(want.LibraryElement.Columns, "folder_id")] = str("0")
					want.LibraryElement.Rows[0][columnIndex(want.LibraryElement.Columns, "folder_uid")] = str("general")
				}
				count, err := s.rehomeOrphanLibraryPanels()
				if err != nil || count != wantCount || !reflect.DeepEqual(s, want) {
					t.Fatal("rehome changed more than the orphan's folder fields", count, err)
				}
				if err := s.Validate(); (err == nil) != tc.wantValid {
					t.Fatal("unexpected validation after rehome", err)
				}
				if count, err := s.rehomeOrphanLibraryPanels(); err != nil || count != 0 || !reflect.DeepEqual(s, want) {
					t.Fatal("rehome is not idempotent", count, err)
				}
			})
		}
	}
	db := &Database{}
	if _, err := db.ExportWithOptions(context.Background(), DefaultTargets, ExportOptions{RehomeOrphanLibraryPanels: true}); err == nil || !strings.Contains(err.Error(), "requires --targets including dashboard") {
		t.Fatal("rehome accepted without a dashboard target before connecting", err)
	}
}

func TestRehomeOrphanLibraryPanelsRejectsNameCollision(t *testing.T) {
	s := librarySnapshot()
	row := s.LibraryElement.Rows[0]
	row[columnIndex(s.LibraryElement.Columns, "folder_id")] = str("404")
	row[columnIndex(s.LibraryElement.Columns, "folder_uid")] = nil
	other := append([]*string{}, row...)
	other[columnIndex(s.LibraryElement.Columns, "id")] = str("41")
	other[columnIndex(s.LibraryElement.Columns, "uid")] = str("other")
	other[columnIndex(s.LibraryElement.Columns, "folder_id")] = str("0")
	other[columnIndex(s.LibraryElement.Columns, "folder_uid")] = str("")
	other[columnIndex(s.LibraryElement.Columns, "model")] = str(`{"uid":"other","type":"timeseries"}`)
	s.LibraryElement.Rows = append(s.LibraryElement.Rows, other)
	if count, err := s.rehomeOrphanLibraryPanels(); err != nil || count != 1 {
		t.Fatal(count, err)
	}
	if err := s.Validate(); err == nil || !strings.Contains(err.Error(), "duplicate name") {
		t.Fatal("rehome accepted a name collision in General", err)
	}
}

func TestRenameCollidingRehomedLibraryPanels(t *testing.T) {
	s := librarySnapshot()
	base := s.LibraryElement.Rows[0]
	set := func(row []*string, column, value string) {
		row[columnIndex(s.LibraryElement.Columns, column)] = str(value)
	}
	set(base, "folder_id", "404")
	set(base, "folder_uid", "")
	set(base, "name", "同じ名前")
	add := func(id, name, folderID, folderUID string) {
		row := append([]*string{}, base...)
		set(row, "id", id)
		set(row, "uid", "panel-"+id)
		set(row, "name", name)
		set(row, "folder_id", folderID)
		set(row, "folder_uid", folderUID)
		set(row, "model", `{"uid":"panel-`+id+`","type":"timeseries"}`)
		s.LibraryElement.Rows = append(s.LibraryElement.Rows, row)
	}
	add("41", "同じ名前", "405", "")
	add("42", "既存名", "0", "general")
	add("43", "既存名", "406", "")
	add("44", "同じ名前 [migrated-41]", "407", "")
	add("45", "同じ名前 [migrated-41-2]", "0", "general")
	add("46", "同じ名前", "408", "")
	s.LibraryElement.Rows[0], s.LibraryElement.Rows[1] = s.LibraryElement.Rows[1], s.LibraryElement.Rows[0]
	models := make(map[string]string)
	for _, row := range s.LibraryElement.Rows {
		models[cell(s.LibraryElement, row, "id")] = cell(s.LibraryElement, row, "model")
	}
	if count, err := s.rehomeOrphanLibraryPanels(); err != nil || count != 5 {
		t.Fatal("legacy rehome changed behavior", count, err)
	}
	if err := s.Validate(); err == nil || !strings.Contains(err.Error(), "duplicate name") {
		t.Fatal("unapproved rename accepted collision", err)
	}
	s = librarySnapshot()
	base = s.LibraryElement.Rows[0]
	set(base, "folder_id", "404")
	set(base, "folder_uid", "")
	set(base, "name", "同じ名前")
	add("41", "同じ名前", "405", "")
	add("42", "既存名", "0", "general")
	add("43", "既存名", "406", "")
	add("44", "同じ名前 [migrated-41]", "407", "")
	add("45", "同じ名前 [migrated-41-2]", "0", "general")
	add("46", "同じ名前", "408", "")
	s.LibraryElement.Rows[0], s.LibraryElement.Rows[1] = s.LibraryElement.Rows[1], s.LibraryElement.Rows[0]
	count, renamed, err := s.rehomeOrphanLibraryPanelsWithOptions(true)
	if err != nil || count != 5 || renamed != 3 {
		t.Fatal("wrong relocation or rename count", count, renamed, err)
	}
	want := map[string]string{
		"40": "同じ名前", "41": "同じ名前 [migrated-41-3]", "42": "既存名",
		"43": "既存名 [migrated-43]", "44": "同じ名前 [migrated-41]",
		"45": "同じ名前 [migrated-41-2]", "46": "同じ名前 [migrated-46]",
	}
	for _, row := range s.LibraryElement.Rows {
		id := cell(s.LibraryElement, row, "id")
		if name := cell(s.LibraryElement, row, "name"); name != want[id] || cell(s.LibraryElement, row, "model") != models[id] {
			t.Fatal("changed protected data or chose an unstable name", id)
		}
	}
	if err := s.Validate(); err != nil {
		t.Fatal("renamed snapshot is invalid", err)
	}
	before, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	if count, renamed, err = s.rehomeOrphanLibraryPanelsWithOptions(true); err != nil || count != 0 || renamed != 0 {
		t.Fatal("rename is not idempotent", count, renamed, err)
	}
	after, err := json.Marshal(s)
	if err != nil || string(before) != string(after) {
		t.Fatal("repeat rename modified snapshot", err)
	}
}

func TestRenameRehomedLibraryPanelNameLimit(t *testing.T) {
	s := librarySnapshot()
	row := s.LibraryElement.Rows[0]
	row[columnIndex(s.LibraryElement.Columns, "folder_id")] = str("404")
	row[columnIndex(s.LibraryElement.Columns, "folder_uid")] = nil
	row[columnIndex(s.LibraryElement.Columns, "name")] = str(strings.Repeat("長", 150))
	other := append([]*string{}, row...)
	other[columnIndex(s.LibraryElement.Columns, "id")] = str("41")
	other[columnIndex(s.LibraryElement.Columns, "uid")] = str("other")
	other[columnIndex(s.LibraryElement.Columns, "model")] = str(`{"uid":"other","type":"timeseries"}`)
	s.LibraryElement.Rows = append(s.LibraryElement.Rows, other)
	count, renamed, err := s.rehomeOrphanLibraryPanelsWithOptions(true)
	if err != nil || count != 2 || renamed != 1 {
		t.Fatal(count, renamed, err)
	}
	name := cell(s.LibraryElement, other, "name")
	if len([]rune(name)) != 150 || !strings.HasSuffix(name, " [migrated-41]") || !strings.HasPrefix(name, "長") || cell(s.LibraryElement, row, "name") != strings.Repeat("長", 150) {
		t.Fatal("name was not safely limited")
	}
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestRenameRehomedLibraryPanelsDoesNotRepairOtherCollisions(t *testing.T) {
	s := librarySnapshot()
	other := append([]*string{}, s.LibraryElement.Rows[0]...)
	other[columnIndex(s.LibraryElement.Columns, "id")] = str("41")
	other[columnIndex(s.LibraryElement.Columns, "uid")] = str("other")
	other[columnIndex(s.LibraryElement.Columns, "model")] = str(`{"uid":"other","type":"timeseries"}`)
	s.LibraryElement.Rows = append(s.LibraryElement.Rows, other)
	for _, folder := range []string{"child", "general"} {
		for _, row := range s.LibraryElement.Rows {
			row[columnIndex(s.LibraryElement.Columns, "folder_uid")] = str(folder)
		}
		before, err := json.Marshal(s)
		if err != nil {
			t.Fatal(err)
		}
		if count, renamed, err := s.rehomeOrphanLibraryPanelsWithOptions(true); err != nil || count != 0 || renamed != 0 {
			t.Fatal("renamed a panel that was not rehomed", count, renamed, err)
		}
		after, err := json.Marshal(s)
		if err != nil || string(before) != string(after) {
			t.Fatal("changed non-orphan collision", err)
		}
		if err := s.Validate(); err == nil || !strings.Contains(err.Error(), "duplicate name") {
			t.Fatal("accepted non-orphan collision", err)
		}
	}
	db := &Database{}
	if _, err := db.ExportWithOptions(context.Background(), TargetDashboard, ExportOptions{RenameCollidingLibraryPanels: true}); err == nil || !strings.Contains(err.Error(), "requires --rehome-orphan-library-panels") {
		t.Fatal("accepted rename option without rehome before connecting", err)
	}
}

func librarySnapshot() *Snapshot {
	s := dashboardSnapshot()
	s.Version = 4
	s.LibraryElement = Table{Columns: []string{"id", "org_id", "folder_id", "folder_uid", "uid", "name", "kind", "type", "model", "version"}, Rows: [][]*string{
		{str("40"), str("9007199254740993"), str("0"), str("child"), str("shared"), str("共有パネル"), str("1"), str("timeseries"), str(`{"uid":"shared","type":"timeseries","datasource":{"uid":"metrics-uid"}}`), str("1")},
	}}
	s.LibraryConnection = Table{Columns: []string{"id", "element_id", "kind", "connection_id"}, Rows: [][]*string{}}
	s.Dashboard.Rows[2][4] = str(`{"panels":[{"libraryPanel":{"uid":"shared"}}]}`)
	s.DashboardVersion.Rows[1][3] = s.Dashboard.Rows[2][4]
	for i := range 14 {
		if i > 0 {
			row := append([]*string{}, s.Dashboard.Rows[2]...)
			row[0], row[2], row[3] = str(strconv.Itoa(12+i)), str("board-"+strconv.Itoa(i)), str("ボード"+strconv.Itoa(i))
			s.Dashboard.Rows = append(s.Dashboard.Rows, row)
		}
		s.LibraryConnection.Rows = append(s.LibraryConnection.Rows, []*string{str(strconv.Itoa(100 + i)), str("40"), str("1"), str(strconv.Itoa(12 + i))})
	}
	s.DashboardPermissions = append(s.DashboardPermissions, DashboardPermission{"9007199254740993", "Viewer", "library.panels:read", "library.panels:uid:shared"})
	return s
}

func TestLibrarySnapshots(t *testing.T) {
	for targets := Targets(1); targets <= AllTargets; targets++ {
		s, err := librarySnapshot().Select(targets)
		if err != nil {
			t.Fatal(err)
		}
		data, err := json.Marshal(s)
		if err != nil {
			t.Fatal(err)
		}
		got, err := Decode(strings.NewReader(string(data)))
		if err != nil || !reflect.DeepEqual(s, got) {
			t.Fatal("version 4 did not round trip", targets, err)
		}
		if strings.Contains(string(data), `"library_element"`) != targets.Has(TargetDashboard) {
			t.Fatal("library tables do not follow dashboard selection")
		}
	}
}

func TestLibraryValidation(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*Snapshot)
	}{
		{"missing library", func(s *Snapshot) { s.LibraryElement.Rows = nil }},
		{"missing connection", func(s *Snapshot) { s.LibraryConnection.Rows = s.LibraryConnection.Rows[:13] }},
		{"orphan library", func(s *Snapshot) { s.LibraryConnection.Rows[0][1] = str("999") }},
		{"orphan dashboard", func(s *Snapshot) { s.LibraryConnection.Rows[0][3] = str("999") }},
		{"connection kind", func(s *Snapshot) { s.LibraryConnection.Rows[0][2] = str("2") }},
		{"element kind", func(s *Snapshot) { s.LibraryElement.Rows[0][6] = str("2") }},
		{"empty element UID with empty metadata UID", func(s *Snapshot) {
			s.LibraryElement.Rows[0][4] = str("")
			s.LibraryElement.Rows[0][8] = str(`{"libraryPanel":{"uid":""}}`)
		}},
		{"other organization", func(s *Snapshot) { s.LibraryElement.Rows[0][1] = str("77") }},
		{"folder missing", func(s *Snapshot) { s.LibraryElement.Rows[0][3] = str("missing") }},
		{"folder mismatch", func(s *Snapshot) { s.LibraryElement.Rows[0][2] = str("10") }},
		{"invalid model", func(s *Snapshot) { s.LibraryElement.Rows[0][8] = str(`{`) }},
		{"wrong model uid", func(s *Snapshot) { s.LibraryElement.Rows[0][8] = str(`{"uid":"other"}`) }},
		{"wrong model type", func(s *Snapshot) { s.LibraryElement.Rows[0][8] = str(`{"type":"other"}`) }},
		{"nested model", func(s *Snapshot) { s.LibraryElement.Rows[0][8] = str(`{"options":{"libraryPanel":{"uid":"shared"}}}`) }},
		{"invalid version", func(s *Snapshot) { s.LibraryElement.Rows[0][9] = str("0") }},
		{"bad reference", func(s *Snapshot) { s.Dashboard.Rows[2][4] = str(`{"panels":[{"libraryPanel":{}}]}`) }},
		{"missing history reference", func(s *Snapshot) {
			s.DashboardVersion.Rows[0][3] = str(`{"panels":[{"libraryPanel":{"uid":"absent"}}]}`)
		}},
		{"folder connection", func(s *Snapshot) { s.LibraryConnection.Rows[0][3] = str("10") }},
		{"duplicate connection", func(s *Snapshot) { s.LibraryConnection.Rows[1][3] = str("12") }},
		{"legacy format", func(s *Snapshot) { s.Version = 3 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := librarySnapshot()
			tc.edit(s)
			if err := s.Validate(); err == nil {
				t.Fatal("accepted invalid library state")
			}
		})
	}
}

func TestLibraryModelSelfMetadata(t *testing.T) {
	for _, version := range []struct {
		name string
		make func() *Snapshot
	}{{"v4", librarySnapshot}, {"v5", rbacSnapshot}} {
		for _, tc := range []struct {
			name, model, wantError string
		}{
			{"absent", `{}`, ""},
			{"null", `{"libraryPanel":null}`, ""},
			{"self", `{"libraryPanel":{"uid":"shared"}}`, ""},
			{"self metadata", ` {"uid":"shared","type":"timeseries","libraryPanel":{"uid":"shared","name":"stale-name-canary","version":1},"options":{"text":"content-canary"}} `, ""},
			{"empty metadata object", `{"libraryPanel":{}}`, ""},
			{"missing UID", `{"libraryPanel":{"name":"private-name-canary"}}`, ""},
			{"metadata without UID", ` {"uid":"shared","type":"timeseries","libraryPanel":{"name":"stale-name-canary","version":1},"options":{"text":"content-canary"}} `, ""},
			{"empty UID", `{"libraryPanel":{"uid":""}}`, ""},
			{"metadata with empty UID", ` {"uid":"shared","type":"timeseries","libraryPanel":{"uid":"","name":"stale-name-canary","version":1},"options":{"text":"content-canary"}} `, ""},
			{"nested null", `{"libraryPanel":{"uid":"shared"},"options":{"libraryPanel":null}}`, ""},
			{"foreign", `{"libraryPanel":{"uid":"foreign-uid-canary","name":"private-name-canary"}}`, "references another library panel at its root"},
			{"whitespace UID", `{"libraryPanel":{"uid":" "}}`, "references another library panel at its root"},
			{"null UID", `{"libraryPanel":{"uid":null}}`, "invalid top-level libraryPanel metadata"},
			{"boolean UID", `{"libraryPanel":{"uid":false}}`, "invalid top-level libraryPanel metadata"},
			{"numeric UID", `{"libraryPanel":{"uid":40}}`, "invalid top-level libraryPanel metadata"},
			{"object UID", `{"libraryPanel":{"uid":{"uid":"shared"}}}`, "invalid top-level libraryPanel metadata"},
			{"nonobject", `{"libraryPanel":"private-value-canary"}`, "invalid top-level libraryPanel metadata"},
			{"array", `{"libraryPanel":[{"uid":"shared"}]}`, "invalid top-level libraryPanel metadata"},
			{"nested self", `{"options":{"libraryPanel":{"uid":"shared"}}}`, "below the model root"},
			{"nested foreign", `{"panels":[{"libraryPanel":{"uid":"foreign-uid-canary"}}]}`, "below the model root"},
			{"self and nested", `{"libraryPanel":{"uid":"shared"},"options":{"libraryPanel":{"uid":"shared"}}}`, "below the model root"},
			{"nested within metadata", `{"libraryPanel":{"uid":"shared","extra":{"libraryPanel":{"uid":"shared"}}}}`, "below the model root"},
			{"nested malformed", `{"libraryPanel":{"uid":"shared"},"options":{"libraryPanel":{}}}`, "below the model root"},
			{"missing UID and nested self", `{"libraryPanel":{"name":"private-name-canary"},"options":{"libraryPanel":{"uid":"shared"}}}`, "below the model root"},
			{"nested within UIDless metadata", `{"libraryPanel":{"extra":{"libraryPanel":{"uid":"foreign-uid-canary"}}}}`, "below the model root"},
			{"malformed within UIDless metadata", `{"libraryPanel":{"extra":{"libraryPanel":{}}}}`, "below the model root"},
			{"empty UID and nested reference", `{"libraryPanel":{"uid":""},"options":{"libraryPanel":{"uid":"foreign-uid-canary"}}}`, "below the model root"},
			{"nested within empty UID metadata", `{"libraryPanel":{"uid":"","extra":{"libraryPanel":{"uid":"shared"}}}}`, "below the model root"},
			{"nested empty UID", `{"libraryPanel":{"uid":""},"options":{"libraryPanel":{"uid":""}}}`, "below the model root"},
		} {
			t.Run(version.name+"/"+tc.name, func(t *testing.T) {
				s := version.make()
				s.LibraryElement.Rows[0][columnIndex(s.LibraryElement.Columns, "model")] = str(tc.model)
				before, err := json.Marshal(s)
				if err != nil {
					t.Fatal(err)
				}
				err = s.Validate()
				if tc.wantError != "" {
					if err == nil || !strings.Contains(err.Error(), tc.wantError) || !strings.Contains(err.Error(), "(panel ID 40)") || strings.Contains(err.Error(), "canary") || strings.Contains(err.Error(), "shared") {
						t.Fatal("incorrect or unsafe model diagnostic", err)
					}
				} else {
					if err != nil {
						t.Fatal("rejected valid self metadata", err)
					}
					got, err := Decode(strings.NewReader(string(before)))
					if err != nil || !reflect.DeepEqual(got, s) {
						t.Fatal("self metadata did not round trip unchanged", err)
					}
				}
				after, err := json.Marshal(s)
				if err != nil || string(before) != string(after) {
					t.Fatal("model validation modified the snapshot", err)
				}
			})
		}
	}
}

func TestLibraryMetadataDoesNotRelaxDashboardReferences(t *testing.T) {
	for _, version := range []struct {
		name string
		make func() *Snapshot
	}{{"v4", librarySnapshot}, {"v5", rbacSnapshot}} {
		for _, table := range []string{"dashboard", "dashboard_version"} {
			for _, metadata := range []string{`{}`, `{"name":"private-name-canary"}`, `{"uid":""}`} {
				t.Run(version.name+"/"+table+"/"+metadata, func(t *testing.T) {
					s := version.make()
					s.LibraryElement.Rows[0][columnIndex(s.LibraryElement.Columns, "model")] = str(`{"libraryPanel":{"uid":"","name":"private-name-canary"}}`)
					target, row := &s.Dashboard, 2
					if table == "dashboard_version" {
						target, row = &s.DashboardVersion, 0
					}
					target.Rows[row][columnIndex(target.Columns, "data")] = str(`{"panels":[{"libraryPanel":` + metadata + `}]}`)
					if err := s.Validate(); err == nil || !strings.Contains(err.Error(), "invalid library panel reference") || strings.Contains(err.Error(), "canary") {
						t.Fatal("model metadata relaxed dashboard/history references", err)
					}
				})
			}
		}
	}
}
