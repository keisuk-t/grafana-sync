package syncdb

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"unicode/utf8"
)

func (s *Snapshot) rehomeOrphanLibraryPanels() (int, error) {
	count, _, err := s.rehomeOrphanLibraryPanelsWithOptions(false)
	return count, err
}

func (s *Snapshot) rehomeOrphanLibraryPanelsWithOptions(renameCollisions bool) (int, int, error) {
	if err := requireColumns(s.LibraryElement, "library_element", "folder_id", "folder_uid"); err != nil {
		return 0, 0, err
	}
	if renameCollisions {
		if err := requireColumns(s.LibraryElement, "library_element", "id", "org_id", "name"); err != nil {
			return 0, 0, err
		}
	}
	dashboards := syncRowsByID(s.Dashboard, "id")
	count := 0
	moved := make(map[string]bool)
	for _, row := range s.LibraryElement.Rows {
		id, err := strconv.ParseInt(cell(s.LibraryElement, row, "folder_id"), 10, 64)
		if err != nil || id <= 0 || cell(s.LibraryElement, row, "folder_uid") != "" || dashboards[id] != nil {
			continue
		}
		idValue, uidValue := "0", "general"
		row[columnIndex(s.LibraryElement.Columns, "folder_id")] = &idValue
		row[columnIndex(s.LibraryElement.Columns, "folder_uid")] = &uidValue
		moved[cell(s.LibraryElement, row, "id")] = true
		count++
	}
	if !renameCollisions {
		return count, 0, nil
	}
	renamed, err := s.renameRehomedLibraryPanels(moved)
	return count, renamed, err
}

func (s *Snapshot) renameRehomedLibraryPanels(moved map[string]bool) (int, error) {
	type nameKey struct{ org, folder, name string }
	names := make(map[nameKey]bool)
	reserved := make(map[nameKey]bool)
	var candidates [][]*string
	for _, row := range s.LibraryElement.Rows {
		if moved[cell(s.LibraryElement, row, "id")] {
			candidates = append(candidates, row)
			reserved[nameKey{cell(s.LibraryElement, row, "org_id"), "general", cell(s.LibraryElement, row, "name")}] = true
			continue
		}
		folder := cell(s.LibraryElement, row, "folder_uid")
		if folder == "" {
			folder = "general"
		}
		names[nameKey{cell(s.LibraryElement, row, "org_id"), folder, cell(s.LibraryElement, row, "name")}] = true
	}
	slices.SortFunc(candidates, func(a, b []*string) int {
		left, _ := syncID(a, s.LibraryElement.Columns, "id")
		right, _ := syncID(b, s.LibraryElement.Columns, "id")
		return cmp.Compare(left, right)
	})
	renamed := 0
	for _, row := range candidates {
		id := cell(s.LibraryElement, row, "id")
		name := cell(s.LibraryElement, row, "name")
		key := nameKey{cell(s.LibraryElement, row, "org_id"), "general", name}
		if !names[key] {
			names[key] = true
			continue
		}
		if !utf8.ValidString(name) {
			return 0, fmt.Errorf("cannot rename library panel with invalid name encoding (panel ID %s)", id)
		}
		allocated := false
		for attempt := 1; attempt <= len(s.LibraryElement.Rows)+1; attempt++ {
			suffix := " [migrated-" + id
			if attempt > 1 {
				suffix += "-" + strconv.Itoa(attempt)
			}
			suffix += "]"
			limit := 150 - utf8.RuneCountInString(suffix)
			if limit < 1 {
				return 0, fmt.Errorf("cannot rename library panel within the name limit (panel ID %s)", id)
			}
			prefix := []rune(name)
			if len(prefix) > limit {
				prefix = prefix[:limit]
			}
			candidate := string(prefix) + suffix
			key.name = candidate
			if names[key] || reserved[key] {
				continue
			}
			row[columnIndex(s.LibraryElement.Columns, "name")] = &candidate
			names[key] = true
			renamed++
			allocated = true
			break
		}
		if !allocated {
			return 0, fmt.Errorf("cannot generate unique library panel name (panel ID %s)", id)
		}
	}
	return renamed, nil
}

func libraryReferences(data string) ([]string, error) {
	document, err := dashboardDocument(data)
	if err != nil {
		return nil, err
	}
	refs := []string{}
	valid := true
	walkDocument(document, func(key string, value any) {
		if key != "libraryPanel" || value == nil {
			return
		}
		panel, ok := value.(map[string]any)
		uid, _ := panel["uid"].(string)
		if !ok || uid == "" {
			valid = false
		}
		refs = append(refs, uid)
	})
	if !valid {
		return nil, errors.New("invalid library panel reference")
	}
	return refs, nil
}

func validateLibraryModelReferences(model map[string]any, uid string) error {
	// Grafana can persist top-level metadata with a missing, empty or self UID.
	if value := model["libraryPanel"]; value != nil {
		panel, ok := value.(map[string]any)
		if !ok {
			return errors.New("invalid top-level libraryPanel metadata")
		}
		if value, exists := panel["uid"]; exists {
			stored, ok := value.(string)
			if !ok {
				return errors.New("invalid top-level libraryPanel metadata")
			}
			if stored != "" && stored != uid {
				return errors.New("library panel model references another library panel at its root; nested library panel references are unsupported")
			}
		}
	}
	nested := false
	for _, value := range model {
		walkDocument(value, func(key string, value any) {
			if key == "libraryPanel" && value != nil {
				nested = true
			}
		})
	}
	if nested {
		return errors.New("nested library panel references are unsupported below the model root")
	}
	return nil
}

func (s *Snapshot) validateLibraryState(scope map[int64]bool) error {
	if s.Version < 4 && (len(s.LibraryElement.Columns) != 0 || len(s.LibraryConnection.Columns) != 0) {
		return errors.New("library tables require snapshot version 4")
	}
	if s.Version >= 4 {
		if err := requireColumns(s.LibraryElement, "library_element", "id", "org_id", "folder_id", "folder_uid", "uid", "name", "kind", "type", "model", "version"); err != nil {
			return err
		}
		if err := requireColumns(s.LibraryConnection, "library_element_connection", "id", "element_id", "kind", "connection_id"); err != nil {
			return err
		}
	}
	dashboards := syncRowsByID(s.Dashboard, "id")
	libraries := syncRowsByID(s.LibraryElement, "id")
	byUID := make(map[string][]*string)
	folders := make(map[string][]*string)
	for _, row := range s.Dashboard.Rows {
		if cell(s.Dashboard, row, "is_folder") == "1" {
			folders[resourceKey(rowOrg(s.Dashboard, row), cell(s.Dashboard, row, "uid"))] = row
		}
	}
	names := make(map[string]bool)
	for _, row := range s.LibraryElement.Rows {
		org, uid := rowOrg(s.LibraryElement, row), cell(s.LibraryElement, row, "uid")
		key := resourceKey(org, uid)
		if !scope[org] || uid == "" || cell(s.LibraryElement, row, "name") == "" || byUID[key] != nil || cell(s.LibraryElement, row, "kind") != "1" {
			return errors.New("library_element has invalid organization, UID, name or unsupported kind")
		}
		byUID[key] = row
		if _, err := positiveID(row[columnIndex(s.LibraryElement.Columns, "version")]); err != nil {
			return errors.New("library_element has invalid version")
		}
		folder := cell(s.LibraryElement, row, "folder_uid")
		id, err := strconv.ParseInt(cell(s.LibraryElement, row, "folder_id"), 10, 64)
		if err != nil || id < 0 {
			return errors.New("library_element has invalid folder_id")
		}
		if folder == "" || folder == "general" {
			if id != 0 && folder == "" && dashboards[id] == nil {
				return fmt.Errorf("library panel references a missing folder (panel ID %s, folder ID %d); export with --rehome-orphan-library-panels to explicitly move eligible orphans to General", cell(s.LibraryElement, row, "id"), id)
			}
			folder = "general"
			if id != 0 {
				return fmt.Errorf("root library panel has a folder_id (panel ID %s, folder ID %d)", cell(s.LibraryElement, row, "id"), id)
			}
		} else {
			parent := folders[resourceKey(org, folder)]
			if parent == nil || (id != 0 && cell(s.Dashboard, parent, "id") != strconv.FormatInt(id, 10)) {
				return errors.New("library_element references a missing or incompatible folder")
			}
		}
		name := resourceKey(org, folder) + ":" + cell(s.LibraryElement, row, "name")
		if names[name] {
			return errors.New("library_element has a duplicate name in its folder")
		}
		names[name] = true
		model, err := dashboardDocument(cell(s.LibraryElement, row, "model"))
		if err != nil {
			return errors.New("invalid library panel model JSON")
		}
		if stored, exists := model["uid"]; exists && stored != uid {
			return errors.New("library panel model UID differs from its metadata")
		}
		if stored, exists := model["type"]; exists && stored != cell(s.LibraryElement, row, "type") {
			return errors.New("library panel model type differs from its metadata")
		}
		if err := validateLibraryModelReferences(model, uid); err != nil {
			return fmt.Errorf("%w (panel ID %s)", err, cell(s.LibraryElement, row, "id"))
		}
	}
	connections := make(map[string]bool)
	for _, row := range s.LibraryConnection.Rows {
		element, err := positiveID(row[columnIndex(s.LibraryConnection.Columns, "element_id")])
		if err != nil || libraries[element] == nil || cell(s.LibraryConnection, row, "kind") != "1" {
			return errors.New("library connection has an unsupported kind or missing element")
		}
		id, err := positiveID(row[columnIndex(s.LibraryConnection.Columns, "connection_id")])
		parent := dashboards[id]
		if err != nil || parent == nil || cell(s.Dashboard, parent, "is_folder") != "0" || rowOrg(s.LibraryElement, libraries[element]) != rowOrg(s.Dashboard, parent) {
			return errors.New("library connection references a missing dashboard or another organization")
		}
		key := strconv.FormatInt(id, 10) + "/" + strconv.FormatInt(element, 10)
		if connections[key] {
			return errors.New("duplicate library panel connection")
		}
		connections[key] = true
	}
	check := func(data string, org, id int64, current bool) error {
		refs, err := libraryReferences(data)
		if err != nil {
			return err
		}
		for _, uid := range refs {
			panel := byUID[resourceKey(org, uid)]
			if panel == nil {
				return errors.New("dashboard or history references a missing library panel; re-export with version 4 to include library tables")
			}
			key := strconv.FormatInt(id, 10) + "/" + cell(s.LibraryElement, panel, "id")
			if current && !connections[key] {
				return errors.New("dashboard library panel reference has no matching connection")
			}
		}
		return nil
	}
	for _, row := range s.Dashboard.Rows {
		id, _ := syncID(row, s.Dashboard.Columns, "id")
		if err := check(cell(s.Dashboard, row, "data"), rowOrg(s.Dashboard, row), id, true); err != nil {
			return err
		}
	}
	for _, row := range s.DashboardVersion.Rows {
		id, _ := syncID(row, s.DashboardVersion.Columns, "dashboard_id")
		if err := check(cell(s.DashboardVersion, row, "data"), rowOrg(s.Dashboard, dashboards[id]), id, false); err != nil {
			return err
		}
	}
	return nil
}

func (d *Database) preserveLegacyLibraries(ctx context.Context, tx *sql.Tx, s *Snapshot, final map[string]Table) error {
	if s.Version >= 4 {
		return nil
	}
	for _, name := range []string{"library_element", "library_element_connection"} {
		if _, ok := final[name]; ok {
			continue
		}
		table, _, err := d.readSyncTarget(ctx, tx, tableEntry{name: name, key: "id"})
		if err != nil {
			return err
		}
		final[name] = table
	}
	return nil
}

func protectLibrarySync(s *Snapshot, plans []*syncTablePlan, targets map[string]Table) error {
	scope := s.scopeIDs()
	changed := make(map[int64]bool)
	changedDashboards := make(map[int64]bool)
	for _, plan := range plans {
		if s.Version < 4 && plan.entry.name == "dashboard" {
			for _, id := range plan.remove {
				changedDashboards[id] = true
			}
			for _, row := range plan.update {
				id, _ := syncID(row, plan.entry.table.Columns, "id")
				for _, column := range []string{"org_id", "uid", "is_folder"} {
					if cell(*plan.entry.table, row, column) != cell(Table{Columns: plan.targetColumns}, plan.previous[id], column) {
						changedDashboards[id] = true
					}
				}
			}
		}
		if plan.entry.name == "library_element" {
			for _, id := range plan.remove {
				changed[id] = true
			}
			for _, row := range plan.update {
				id, _ := syncID(row, plan.entry.table.Columns, "id")
				changed[id] = true
			}
		}
	}
	parents := syncRowsByID(targets["dashboard"], "id")
	connections := targets["library_element_connection"]
	for _, row := range connections.Rows {
		element, _ := syncID(row, connections.Columns, "element_id")
		id, _ := syncID(row, connections.Columns, "connection_id")
		parent := parents[id]
		if changedDashboards[id] {
			return errors.New("legacy snapshot cannot change dashboards with library connections; re-export with version 4")
		}
		inScope := parent != nil && scope[rowOrg(targets["dashboard"], parent)]
		if (changed[element] && (!inScope || cell(connections, row, "kind") != "1")) || (inScope && cell(connections, row, "kind") != "1") {
			return errors.New("library panel has protected or unsupported target connections; no changes applied")
		}
	}
	return nil
}
