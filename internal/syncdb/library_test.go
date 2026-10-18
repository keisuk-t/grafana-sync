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
			{"nested null", `{"libraryPanel":{"uid":"shared"},"options":{"libraryPanel":null}}`, ""},
			{"foreign", `{"libraryPanel":{"uid":"foreign-uid-canary","name":"private-name-canary"}}`, "references another library panel at its root"},
			{"empty UID", `{"libraryPanel":{"uid":""}}`, "invalid top-level libraryPanel metadata"},
			{"missing UID", `{"libraryPanel":{"name":"private-name-canary"}}`, "invalid top-level libraryPanel metadata"},
			{"null UID", `{"libraryPanel":{"uid":null}}`, "invalid top-level libraryPanel metadata"},
			{"numeric UID", `{"libraryPanel":{"uid":40}}`, "invalid top-level libraryPanel metadata"},
			{"object UID", `{"libraryPanel":{"uid":{"uid":"shared"}}}`, "invalid top-level libraryPanel metadata"},
			{"nonobject", `{"libraryPanel":"private-value-canary"}`, "invalid top-level libraryPanel metadata"},
			{"array", `{"libraryPanel":[{"uid":"shared"}]}`, "invalid top-level libraryPanel metadata"},
			{"nested self", `{"options":{"libraryPanel":{"uid":"shared"}}}`, "below the model root"},
			{"nested foreign", `{"panels":[{"libraryPanel":{"uid":"foreign-uid-canary"}}]}`, "below the model root"},
			{"self and nested", `{"libraryPanel":{"uid":"shared"},"options":{"libraryPanel":{"uid":"shared"}}}`, "below the model root"},
			{"nested within metadata", `{"libraryPanel":{"uid":"shared","extra":{"libraryPanel":{"uid":"shared"}}}}`, "below the model root"},
			{"nested malformed", `{"libraryPanel":{"uid":"shared"},"options":{"libraryPanel":{}}}`, "below the model root"},
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
