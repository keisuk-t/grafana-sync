package syncdb

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

func cell(table Table, row []*string, name string) string {
	i := columnIndex(table.Columns, name)
	if i < 0 || row[i] == nil {
		return ""
	}
	return *row[i]
}

func rowOrg(table Table, row []*string) int64 {
	id, _ := strconv.ParseInt(cell(table, row, "org_id"), 10, 64)
	return id
}

func resourceKey(org int64, uid string) string { return strconv.FormatInt(org, 10) + ":" + uid }

func requireColumns(table Table, name string, columns ...string) error {
	for _, column := range columns {
		if columnIndex(table.Columns, column) < 0 {
			return fmt.Errorf("%s requires %s", name, column)
		}
	}
	return nil
}

func (s *Snapshot) validateDashboards(scope map[int64]bool) error {
	if err := s.validateDashboardState(scope, true); err != nil {
		return err
	}
	return s.validateDashboardPermissions()
}

func (s *Snapshot) validateDashboardState(scope map[int64]bool, payload bool) error {
	for _, spec := range []struct {
		name    string
		table   Table
		columns []string
	}{
		{"dashboard", s.Dashboard, []string{"org_id", "uid", "title", "data", "version", "is_folder", "folder_id", "folder_uid", "has_acl", "is_public"}},
		{"folder", s.Folder, []string{"org_id", "uid", "title", "parent_uid"}},
		{"dashboard_tag", s.DashboardTag, []string{"dashboard_id", "term", "org_id", "dashboard_uid"}},
		{"dashboard_version", s.DashboardVersion, []string{"dashboard_id", "version", "data"}},
	} {
		if err := requireColumns(spec.table, spec.name, spec.columns...); err != nil {
			return err
		}
	}
	byID := syncRowsByID(s.Dashboard, "id")
	byUID := make(map[string][]*string)
	folders := make(map[string][]*string)
	for _, row := range s.Dashboard.Rows {
		org, uid := rowOrg(s.Dashboard, row), cell(s.Dashboard, row, "uid")
		key := resourceKey(org, uid)
		if !scope[org] || uid == "" || cell(s.Dashboard, row, "title") == "" || byUID[key] != nil {
			return errors.New("dashboard has invalid organization, UID or title")
		}
		byUID[key] = row
		for _, column := range []string{"is_folder", "has_acl", "is_public"} {
			value := cell(s.Dashboard, row, column)
			if value != "0" && value != "1" {
				return errors.New("dashboard has invalid boolean metadata")
			}
		}
		if payload && (cell(s.Dashboard, row, "has_acl") == "1" || cell(s.Dashboard, row, "is_public") == "1") {
			return errors.New("dashboard custom ACLs and public sharing are not supported")
		}
		if _, err := positiveID(row[columnIndex(s.Dashboard.Columns, "version")]); err != nil {
			return errors.New("dashboard has invalid version")
		}
		if payload {
			if _, err := dashboardDocument(cell(s.Dashboard, row, "data")); err != nil {
				return err
			}
		}
	}
	for _, row := range s.Folder.Rows {
		org, uid := rowOrg(s.Folder, row), cell(s.Folder, row, "uid")
		key := resourceKey(org, uid)
		dash := byUID[key]
		if !scope[org] || uid == "" || folders[key] != nil || dash == nil || cell(s.Dashboard, dash, "is_folder") != "1" {
			return errors.New("folder does not match a dashboard folder in scope")
		}
		if cell(s.Folder, row, "title") != cell(s.Dashboard, dash, "title") || cell(s.Folder, row, "parent_uid") != cell(s.Dashboard, dash, "folder_uid") {
			return errors.New("folder metadata differs from dashboard folder")
		}
		folders[key] = row
	}
	for _, row := range s.Dashboard.Rows {
		org, uid := rowOrg(s.Dashboard, row), cell(s.Dashboard, row, "uid")
		if cell(s.Dashboard, row, "is_folder") == "1" && folders[resourceKey(org, uid)] == nil {
			return errors.New("dashboard folder has no folder metadata")
		}
		parent := cell(s.Dashboard, row, "folder_uid")
		parentRow := byUID[resourceKey(org, parent)]
		folderID, err := strconv.ParseInt(cell(s.Dashboard, row, "folder_id"), 10, 64)
		if err != nil || folderID < 0 {
			return errors.New("invalid dashboard folder_id")
		}
		if parent == "" {
			if folderID != 0 {
				return errors.New("root dashboard has a folder_id")
			}
		} else {
			if parentRow == nil || cell(s.Dashboard, parentRow, "is_folder") != "1" {
				return errors.New("dashboard references a missing folder")
			}
			if folderID != 0 && cell(s.Dashboard, parentRow, "id") != strconv.FormatInt(folderID, 10) {
				return errors.New("dashboard folder ID and UID disagree")
			}
			if cell(s.Dashboard, row, "is_folder") != "1" && folderID == 0 {
				return errors.New("dashboard folder ID is missing")
			}
		}
	}
	for key, row := range folders {
		seen := map[string]bool{key: true}
		for parent := cell(s.Folder, row, "parent_uid"); parent != ""; {
			key := resourceKey(rowOrg(s.Folder, row), parent)
			if seen[key] || folders[key] == nil {
				return errors.New("folder hierarchy is cyclic or incomplete")
			}
			seen[key] = true
			parent = cell(s.Folder, folders[key], "parent_uid")
		}
	}
	for _, spec := range []struct {
		name  string
		table Table
		key   string
	}{
		{"dashboard_tag", s.DashboardTag, "term"}, {"dashboard_version", s.DashboardVersion, "version"},
	} {
		keys := make(map[string]bool)
		for _, row := range spec.table.Rows {
			id, err := positiveID(row[columnIndex(spec.table.Columns, "dashboard_id")])
			parent := byID[id]
			if err != nil || parent == nil {
				return fmt.Errorf("%s references a missing dashboard", spec.name)
			}
			key := strconv.FormatInt(id, 10) + ":" + cell(spec.table, row, spec.key)
			if keys[key] {
				return fmt.Errorf("%s has a duplicate logical key", spec.name)
			}
			keys[key] = true
			if spec.name == "dashboard_tag" {
				if cell(spec.table, row, "term") == "" || rowOrg(spec.table, row) != rowOrg(s.Dashboard, parent) || cell(spec.table, row, "dashboard_uid") != cell(s.Dashboard, parent, "uid") {
					return errors.New("dashboard tag metadata differs from its parent")
				}
			} else {
				if _, err := positiveID(row[columnIndex(spec.table.Columns, "version")]); err != nil {
					return errors.New("invalid dashboard history version")
				}
				if payload {
					if _, err := dashboardDocument(cell(spec.table, row, "data")); err != nil {
						return err
					}
				}
			}
		}
	}
	return nil
}

func dashboardDocument(data string) (map[string]any, error) {
	var document map[string]any
	decoder := json.NewDecoder(strings.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&document); err != nil || document == nil || !json.Valid([]byte(data)) {
		return nil, errors.New("invalid dashboard JSON document")
	}
	var library bool
	walkDocument(document, func(key string, value any) {
		if key == "libraryPanel" && value != nil {
			library = true
		}
	})
	if library {
		return nil, errors.New("library panels are not supported; migrate their dependencies separately")
	}
	return document, nil
}

func walkDocument(value any, visit func(string, any)) {
	switch v := value.(type) {
	case map[string]any:
		for key, child := range v {
			visit(key, child)
			walkDocument(child, visit)
		}
	case []any:
		for _, child := range v {
			walkDocument(child, visit)
		}
	}
}

type datasourceReference struct {
	value string
	uid   bool
}

func dashboardReferences(data string) ([]datasourceReference, bool, error) {
	document, err := dashboardDocument(data)
	if err != nil {
		return nil, false, err
	}
	refs := []datasourceReference{}
	unresolved := false
	walkDocument(document, func(key string, value any) {
		if key != "datasource" {
			return
		}
		ref := datasourceReference{}
		switch v := value.(type) {
		case string:
			ref.value = v
		case map[string]any:
			ref.value, _ = v["uid"].(string)
			ref.uid = true
			if ref.value == "" {
				unresolved = true
			}
		case nil:
			unresolved = true
		default:
			unresolved = true
		}
		if strings.Contains(ref.value, "$") {
			unresolved = true
			return
		}
		switch ref.value {
		case "", "grafana", "-- Grafana --", "-- Mixed --", "-- Dashboard --", "__expr__", "-100", "-1":
			return
		}
		refs = append(refs, ref)
	})
	return refs, unresolved, nil
}
