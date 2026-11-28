package syncdb

import (
	"context"
	"encoding/json"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func exclusionSnapshot() *Snapshot {
	s := dashboardSnapshot()
	s.Version = 5
	s.DashboardPermissions = nil
	s.RBAC = &ResourceRBAC{Roles: []RBACRole{}, Principals: []RBACPrincipal{}, Grants: []DashboardPermission{}, LegacyACL: []RBACACL{}}
	libraries := librarySnapshot()
	s.LibraryElement, s.LibraryConnection = libraries.LibraryElement, libraries.LibraryConnection
	s.LibraryConnection.Rows = [][]*string{}
	return s
}

func TestExcludeLibraryPanelDashboards(t *testing.T) {
	for _, tc := range []struct {
		name    string
		edit    func(*Snapshot)
		removed bool
	}{
		{"ordinary", func(s *Snapshot) {}, false},
		{"null", func(s *Snapshot) { s.Dashboard.Rows[2][4] = str(`{"panels":[{"libraryPanel":null}]}`) }, false},
		{"current", func(s *Snapshot) { s.Dashboard.Rows[2][4] = str(`{"panels":[{"libraryPanel":{"uid":"shared"}}]}`) }, true},
		{"row", func(s *Snapshot) {
			s.Dashboard.Rows[2][4] = str(`{"panels":[{"panels":[{"libraryPanel":{"uid":"missing"}}]}]}`)
		}, true},
		{"invalid UID", func(s *Snapshot) { s.Dashboard.Rows[2][4] = str(`{"panels":[{"libraryPanel":{"uid":3}}]}`) }, true},
		{"missing UID", func(s *Snapshot) { s.Dashboard.Rows[2][4] = str(`{"panels":[{"libraryPanel":{}}]}`) }, true},
		{"history only", func(s *Snapshot) {
			s.DashboardVersion.Rows[0][3] = str(`{"panels":[{"libraryPanel":{"uid":"deleted"}}]}`)
		}, true},
		{"connection only", func(s *Snapshot) { s.LibraryConnection.Rows = librarySnapshot().LibraryConnection.Rows[:1] }, true},
		{"folder connection", func(s *Snapshot) {
			s.LibraryConnection.Rows = [][]*string{{str("100"), str("40"), str("1"), str("11")}}
		}, false},
		{"orphan connection", func(s *Snapshot) {
			s.LibraryConnection.Rows = [][]*string{{str("100"), str("40"), str("1"), str("404")}}
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := exclusionSnapshot()
			tc.edit(source)
			before, _ := json.Marshal(source)
			s := *source
			excluded, err := s.excludeLibraryPanelDashboards()
			if err != nil || excluded["12"] != tc.removed || len(excluded) > 1 {
				t.Fatal("incorrect dashboard exclusion", err)
			}
			if len(s.LibraryElement.Rows) != 0 || len(s.LibraryConnection.Rows) != 0 || !s.LibraryPanelsExcluded || !reflect.DeepEqual(s.Folder, source.Folder) {
				t.Fatal("library exclusion did not preserve folders or clear library tables")
			}
			if tc.removed {
				if len(s.Dashboard.Rows) != 2 || len(s.DashboardTag.Rows) != 0 || len(s.DashboardVersion.Rows) != 0 {
					t.Fatal("dependent rows were not excluded")
				}
			} else if !reflect.DeepEqual(s.Dashboard, source.Dashboard) || !reflect.DeepEqual(s.DashboardVersion, source.DashboardVersion) || !reflect.DeepEqual(s.DashboardTag, source.DashboardTag) {
				t.Fatal("ordinary dashboard data changed")
			}
			if err := s.Validate(); err != nil {
				t.Fatal(err)
			}
			after, _ := json.Marshal(source)
			if string(before) != string(after) {
				t.Fatal("source rows were mutated")
			}
		})
	}
	for _, table := range []string{"current", "history"} {
		s := exclusionSnapshot()
		if table == "current" {
			s.Dashboard.Rows[2][4] = str(`{"panels":`)
		} else {
			s.DashboardVersion.Rows[0][3] = str(`{"panels":`)
		}
		before, _ := json.Marshal(s)
		if _, err := s.excludeLibraryPanelDashboards(); err == nil {
			t.Fatal("invalid JSON was silently excluded")
		}
		after, _ := json.Marshal(s)
		if string(before) != string(after) {
			t.Fatal("failed exclusion mutated input")
		}
	}
}

func TestExcludedLibrarySnapshotSafety(t *testing.T) {
	s := exclusionSnapshot()
	if _, err := s.excludeLibraryPanelDashboards(); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Decode(strings.NewReader(string(data)))
	if err != nil || !reflect.DeepEqual(got, s) {
		t.Fatal("exclusion marker did not round trip", err)
	}
	for targets := Targets(1); targets <= AllTargets; targets++ {
		selected, err := s.Select(targets)
		if err != nil || selected.LibraryPanelsExcluded != targets.Has(TargetDashboard) {
			t.Fatal("selection lost or leaked marker", err)
		}
	}
	for _, options := range []SyncOptions{{DryRun: true}, {AllowDelete: true}, {}} {
		if _, err := (&Database{}).Sync(context.Background(), got, options); err == nil || !strings.Contains(err.Error(), "dashboard sync is not allowed") {
			t.Fatal("filtered sync was not rejected before connecting", err)
		}
	}
	for _, value := range []string{"null", `"true"`, "1"} {
		invalid := strings.Replace(string(data), `"library_panels_excluded":true`, `"library_panels_excluded":`+value, 1)
		if _, err := Decode(strings.NewReader(invalid)); err == nil {
			t.Fatal("invalid exclusion marker accepted")
		}
	}
	for _, version := range []int{2, 3, 4} {
		invalid := strings.Replace(string(data), `"version":5`, `"version":`+strconv.Itoa(version), 1)
		if _, err := Decode(strings.NewReader(invalid)); err == nil {
			t.Fatal("legacy exclusion marker accepted")
		}
	}
	bad := *s
	bad.LibraryElement = librarySnapshot().LibraryElement
	if err := bad.Validate(); err == nil {
		t.Fatal("excluded snapshot contains library rows")
	}
	bad = *s
	bad.LibraryConnection = librarySnapshot().LibraryConnection
	if err := bad.Validate(); err == nil {
		t.Fatal("excluded snapshot contains connections")
	}
	bad = *s
	bad.DashboardVersion.Rows = append([][]*string{}, s.DashboardVersion.Rows...)
	bad.DashboardVersion.Rows[0] = append([]*string{}, s.DashboardVersion.Rows[0]...)
	bad.DashboardVersion.Rows[0][3] = str(`{"panels":[{"libraryPanel":{"uid":"missing"}}]}`)
	if err := bad.Validate(); err == nil {
		t.Fatal("excluded marker weakened reference validation")
	}
	plain, _ := json.Marshal(exclusionSnapshot())
	if strings.Contains(string(plain), "library_panels_excluded") {
		t.Fatal("default format changed")
	}
}

func TestExcludeLibraryFlagCombinations(t *testing.T) {
	for _, tc := range []struct {
		targets Targets
		options ExportOptions
	}{
		{DefaultTargets, ExportOptions{ExcludeLibraryPanelDashboards: true}},
		{AllTargets, ExportOptions{ExcludeLibraryPanelDashboards: true, RehomeOrphanLibraryPanels: true}},
		{AllTargets, ExportOptions{ExcludeLibraryPanelDashboards: true, RenameCollidingLibraryPanels: true}},
	} {
		if _, err := (&Database{}).ExportWithOptions(context.Background(), tc.targets, tc.options); err == nil {
			t.Fatal("invalid combination accepted")
		}
	}
}
