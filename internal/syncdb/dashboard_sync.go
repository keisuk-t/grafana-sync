package syncdb

import (
	"context"
	"database/sql"
	"errors"
)

func (d *Database) prepareDashboardSync(ctx context.Context, tx *sql.Tx, s *Snapshot, plans []*syncTablePlan, targets map[string]Table) error {
	if !s.Selection().Has(TargetDashboard) {
		return nil
	}
	final := make(map[string]Table)
	for _, plan := range plans {
		table := targets[plan.entry.name]
		removed := make(map[int64]bool)
		for _, id := range plan.remove {
			removed[id] = true
		}
		updated := make(map[int64][]*string)
		for _, row := range plan.update {
			id, _ := syncID(row, plan.entry.table.Columns, "id")
			updated[id] = row
		}
		rows := [][]*string{}
		for _, row := range table.Rows {
			id, _ := syncID(row, table.Columns, "id")
			if removed[id] {
				continue
			}
			if replacement := updated[id]; replacement != nil {
				row = reorderRow(replacement, plan.entry.table.Columns, table.Columns)
			}
			rows = append(rows, row)
		}
		for _, row := range plan.insert {
			rows = append(rows, reorderRow(row, plan.entry.table.Columns, table.Columns))
		}
		table.Rows = rows
		final[plan.entry.name] = table
		if plan.entry.name != "dashboard" {
			continue
		}
		identity := table
		identity.Rows = [][]*string{}
		stored := syncRowsByID(targets["dashboard"], "id")
		for _, id := range plan.remove {
			identity.Rows = append(identity.Rows, stored[id])
		}
		for _, row := range plan.update {
			id, _ := syncID(row, plan.entry.table.Columns, "id")
			old := plan.previous[id]
			changed := false
			for _, column := range []string{"org_id", "uid", "is_folder", "has_acl", "is_public"} {
				if cell(*plan.entry.table, row, column) != cell(targets["dashboard"], old, column) {
					changed = true
				}
			}
			if changed {
				identity.Rows = append(identity.Rows, old)
			}
		}
		if err := d.guardDashboardDependencies(ctx, tx, identity, true); err != nil {
			return err
		}
	}
	if s.Version < 4 {
		if err := d.preserveLegacyLibraries(ctx, tx, s, final); err != nil {
			return err
		}
		for _, name := range []string{"library_element", "library_element_connection"} {
			targets[name] = final[name]
		}
	}
	if err := protectLibrarySync(s, plans, targets); err != nil {
		return err
	}
	return d.validateDashboardOutcome(ctx, tx, s, final)
}

func (d *Database) applyDashboardSync(ctx context.Context, tx *sql.Tx, plans map[string]*syncTablePlan, kinds map[string]map[string]string) error {
	if plans["dashboard"] == nil {
		return nil
	}
	for _, name := range []string{"library_element_connection", "dashboard_tag", "dashboard_version"} {
		plan := plans[name]
		if plan == nil {
			continue
		}
		replacement := *plan
		replacement.remove = append([]int64{}, plan.remove...)
		for _, row := range plan.update {
			id, _ := syncID(row, plan.entry.table.Columns, "id")
			replacement.remove = append(replacement.remove, id)
		}
		if err := d.syncDelete(ctx, tx, &replacement); err != nil {
			return err
		}
	}
	for _, name := range []string{"library_element", "dashboard", "folder"} {
		if plans[name] == nil {
			continue
		}
		if err := d.syncDelete(ctx, tx, plans[name]); err != nil {
			return err
		}
	}
	for _, name := range []string{"folder", "dashboard", "library_element"} {
		if plans[name] == nil {
			continue
		}
		if err := d.syncUpdate(ctx, tx, plans[name], kinds[name]); err != nil {
			return err
		}
		if err := d.syncInsert(ctx, tx, plans[name]); err != nil {
			return err
		}
	}
	for _, name := range []string{"dashboard_tag", "dashboard_version", "library_element_connection"} {
		if plans[name] == nil {
			continue
		}
		plan := *plans[name]
		plan.insert = append(append([][]*string{}, plan.insert...), plan.update...)
		if err := d.syncInsert(ctx, tx, &plan); err != nil {
			return err
		}
	}
	return nil
}

func (d *Database) checkDashboardUnique(ctx context.Context, tx *sql.Tx, s *Snapshot) error {
	for _, entry := range []struct {
		name  string
		table Table
	}{{"dashboard", s.Dashboard}, {"folder", s.Folder}, {"library_element", s.LibraryElement}} {
		if len(entry.table.Rows) < 2 {
			continue
		}
		query, err := d.syncUniqueQuery(ctx, tx, entry.name, "uid")
		if err != nil {
			return errors.New("cannot inspect dashboard UID collation")
		}
		values := []struct {
			OrgID int64  `json:"org_id"`
			Value string `json:"value"`
		}{}
		for _, row := range entry.table.Rows {
			values = append(values, struct {
				OrgID int64  `json:"org_id"`
				Value string `json:"value"`
			}{rowOrg(entry.table, row), cell(entry.table, row, "uid")})
		}
		if err := d.checkUniquePayload(ctx, tx, query, values); err != nil {
			return err
		}
	}
	return nil
}
