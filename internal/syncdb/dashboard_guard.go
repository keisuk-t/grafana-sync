package syncdb

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

func (d *Database) tableExists(ctx context.Context, tx *sql.Tx, name string) (bool, error) {
	query := "SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = DATABASE() AND table_name = ?"
	if d.Engine == "postgres" {
		query = "SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = current_schema() AND table_name = $1"
	}
	var count int
	if err := tx.QueryRowContext(ctx, query, name).Scan(&count); err != nil {
		return false, errors.New("cannot inspect database table inventory")
	}
	return count > 0, nil
}

func (d *Database) checkDashboardStorage(ctx context.Context, tx *sql.Tx) error {
	exists, err := d.tableExists(ctx, tx, "kv_store")
	if err != nil || !exists {
		return err
	}
	rows, err := tx.QueryContext(ctx, "SELECT "+d.quote("key")+", "+d.quote("value")+" FROM "+d.quote("kv_store")+" WHERE "+d.quote("namespace")+" IN ('unified.dualwrite', 'storage.dualwriting')")
	if err != nil {
		return errors.New("cannot inspect dashboard storage mode")
	}
	defer rows.Close()
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return errors.New("cannot read dashboard storage mode")
		}
		if !strings.Contains(key, "dashboard") && !strings.Contains(key, "folder") {
			continue
		}
		var status struct {
			WriteLegacy  *bool `json:"write_legacy"`
			WriteUnified bool  `json:"write_unified"`
			ReadUnified  bool  `json:"read_unified"`
			Migrating    int64 `json:"migrating"`
			Migrated     int64 `json:"migrated"`
		}
		if json.Unmarshal([]byte(value), &status) != nil || status.WriteLegacy == nil || !*status.WriteLegacy || status.WriteUnified || status.ReadUnified || status.Migrating != 0 || status.Migrated != 0 {
			return errors.New("dashboard transfer requires legacy SQL storage only; Unified Storage, dual-write and migration states are unsupported")
		}
	}
	if rows.Err() != nil {
		return errors.New("cannot finish checking dashboard storage mode")
	}
	return nil
}

func (d *Database) guardDashboardDependencies(ctx context.Context, tx *sql.Tx, dashboards Table, destructive bool) error {
	if len(dashboards.Rows) == 0 {
		return nil
	}
	for _, spec := range []struct {
		table, id, uid, org, filter string
		destructiveOnly             bool
	}{
		{"dashboard_acl", "dashboard_id", "", "", "", false},
		{"dashboard_provisioning", "dashboard_id", "", "", "", false},
		{"dashboard_public", "", "dashboard_uid", "org_id", "", false},
		{"library_element_connection", "connection_id", "", "", "", false},
		{"library_element", "folder_id", "", "org_id", "", true},
		{"library_element", "", "folder_uid", "org_id", "", true},
		{"star", "dashboard_id", "", "", "", true},
		{"playlist_item", "value", "", "", "type = 'dashboard_by_id'", true},
		{"playlist_item", "", "value", "", "type = 'dashboard_by_uid'", true},
		{"annotation", "dashboard_id", "", "org_id", "", true},
		{"annotation", "", "dashboard_uid", "org_id", "", true},
		{"alert", "dashboard_id", "", "org_id", "", true},
		{"alert_rule", "", "dashboard_uid", "org_id", "", true},
		{"alert_rule", "", "namespace_uid", "org_id", "", true},
	} {
		if spec.destructiveOnly && !destructive {
			continue
		}
		exists, err := d.tableExists(ctx, tx, spec.table)
		if err != nil {
			return err
		}
		if !exists {
			continue
		}
		for _, row := range dashboards.Rows {
			column, value := spec.id, cell(dashboards, row, "id")
			if spec.uid != "" {
				column, value = spec.uid, cell(dashboards, row, "uid")
			}
			query := "SELECT 1 FROM " + d.quote(spec.table) + " WHERE " + d.quote(column) + " = " + d.bind(1)
			args := []any{value}
			if spec.org != "" {
				query += " AND " + d.quote(spec.org) + " = " + d.bind(2)
				args = append(args, rowOrg(dashboards, row))
			}
			if spec.filter != "" {
				query += " AND " + spec.filter
			}
			var one int
			err = tx.QueryRowContext(ctx, query+" LIMIT 1", args...).Scan(&one)
			if err == nil {
				return fmt.Errorf("dashboard has unsupported dependencies in %s; no changes applied", spec.table)
			}
			if err != sql.ErrNoRows {
				return fmt.Errorf("cannot inspect dashboard dependencies in %s", spec.table)
			}
		}
	}
	_, err := d.readDashboardPermissions(ctx, tx, dashboards)
	return err
}
