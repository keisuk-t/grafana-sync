package syncdb

import "errors"

func hasLibraryPanelReference(data string) (bool, error) {
	document, err := dashboardDocument(data)
	if err != nil {
		return false, err
	}
	found := false
	walkDocument(document, func(key string, value any) {
		if key == "libraryPanel" && value != nil {
			found = true
		}
	})
	return found, nil
}

func (s *Snapshot) excludeLibraryPanelDashboards() (map[string]bool, error) {
	for _, entry := range []struct {
		table   Table
		name    string
		columns []string
	}{
		{s.Dashboard, "dashboard", []string{"id", "is_folder", "data"}},
		{s.DashboardVersion, "dashboard_version", []string{"dashboard_id", "data"}},
		{s.DashboardTag, "dashboard_tag", []string{"dashboard_id"}},
		{s.LibraryConnection, "library_element_connection", []string{"kind", "connection_id"}},
	} {
		if err := requireColumns(entry.table, entry.name, entry.columns...); err != nil {
			return nil, err
		}
	}
	parents := map[string][]*string{}
	excluded := map[string]bool{}
	for _, row := range s.Dashboard.Rows {
		id := cell(s.Dashboard, row, "id")
		if !validID(id, false) || parents[id] != nil {
			return nil, errors.New("dashboard has invalid or duplicate id")
		}
		parents[id] = row
		found, err := hasLibraryPanelReference(cell(s.Dashboard, row, "data"))
		if err != nil {
			return nil, err
		}
		if found && cell(s.Dashboard, row, "is_folder") == "0" {
			excluded[id] = true
		}
	}
	for _, row := range s.DashboardVersion.Rows {
		found, err := hasLibraryPanelReference(cell(s.DashboardVersion, row, "data"))
		if err != nil {
			return nil, err
		}
		id := cell(s.DashboardVersion, row, "dashboard_id")
		if found && parents[id] != nil && cell(s.Dashboard, parents[id], "is_folder") == "0" {
			excluded[id] = true
		}
	}
	for _, row := range s.LibraryConnection.Rows {
		id := cell(s.LibraryConnection, row, "connection_id")
		if cell(s.LibraryConnection, row, "kind") == "1" && parents[id] != nil && cell(s.Dashboard, parents[id], "is_folder") == "0" {
			excluded[id] = true
		}
	}
	for _, entry := range []struct {
		table *Table
		key   string
	}{{&s.Dashboard, "id"}, {&s.DashboardVersion, "dashboard_id"}, {&s.DashboardTag, "dashboard_id"}} {
		rows := make([][]*string, 0, len(entry.table.Rows))
		for _, row := range entry.table.Rows {
			if !excluded[cell(*entry.table, row, entry.key)] {
				rows = append(rows, row)
			}
		}
		entry.table.Rows = rows
	}
	s.LibraryElement.Rows = [][]*string{}
	s.LibraryConnection.Rows = [][]*string{}
	s.LibraryPanelsExcluded = true
	return excluded, nil
}
