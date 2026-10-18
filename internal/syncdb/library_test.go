package syncdb

import (
	"encoding/json"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

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
		{"nested model", func(s *Snapshot) { s.LibraryElement.Rows[0][8] = str(`{"libraryPanel":{"uid":"shared"}}`) }},
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
