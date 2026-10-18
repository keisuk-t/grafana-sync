package syncdb

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5/pgconn"
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
	exists, err := d.tableExists(ctx, tx, "resource")
	if err != nil {
		return err
	}
	if exists {
		var found int
		query := "SELECT 1 FROM " + d.quote("resource") + " WHERE (" + d.quote("group") + " = 'dashboard.grafana.app' AND " + d.quote("resource") + " = 'dashboards') OR (" + d.quote("group") + " = 'folder.grafana.app' AND " + d.quote("resource") + " = 'folders') LIMIT 1"
		err := tx.QueryRowContext(ctx, query).Scan(&found)
		if err == nil {
			return errors.New("dashboard transfer requires legacy SQL storage only; Unified Storage contains dashboards or folders")
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return errors.New("cannot inspect Unified Storage resources")
		}
	}
	exists, err = d.tableExists(ctx, tx, "kv_store")
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
	return d.guardDashboardDependenciesWithRBAC(ctx, tx, dashboards, destructive, false)
}

func (d *Database) guardDashboardDependenciesWithRBAC(ctx context.Context, tx *sql.Tx, dashboards Table, destructive, rbac bool) error {
	if len(dashboards.Rows) == 0 {
		return nil
	}
	for _, spec := range []struct {
		table, id, uid, org, filter string
		destructiveOnly             bool
	}{
		{"dashboard_acl", "dashboard_id", "", "", "", false},
		{"dashboard_provisioning", "dashboard_id", "", "", "", false},
		{"dashboard_public", "", "dashboard_uid", "org_id", "", true},
		{"star", "dashboard_id", "", "", "", true},
		{"playlist_item", "value", "", "", "type = 'dashboard_by_id'", true},
		{"playlist_item", "", "value", "", "type = 'dashboard_by_uid'", true},
		{"annotation", "dashboard_id", "", "org_id", "", true},
		{"annotation", "", "dashboard_uid", "org_id", "", true},
		{"alert", "dashboard_id", "", "org_id", "", true},
		{"alert_rule", "", "dashboard_uid", "org_id", "", true},
		{"alert_rule", "", "namespace_uid", "org_id", "", true},
	} {
		if spec.table == "dashboard_acl" && rbac {
			continue
		}
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
		for batch := range slices.Chunk(dashboards.Rows, dashboardQueryBatchSize) {
			column, field := spec.id, "id"
			if spec.uid != "" {
				column, field = spec.uid, "uid"
			}
			key := d.quote(column)
			if spec.org != "" {
				key = "(" + key + ", " + d.quote(spec.org) + ")"
			}
			values := make([]string, 0, len(batch))
			args := make([]any, 0, 2*len(batch))
			for _, row := range batch {
				value := d.bind(len(args) + 1)
				args = append(args, cell(dashboards, row, field))
				if spec.org != "" {
					value = "(" + value + ", " + d.bind(len(args)+1) + ")"
					args = append(args, rowOrg(dashboards, row))
				}
				values = append(values, value)
			}
			query := "SELECT 1 FROM " + d.quote(spec.table) + " WHERE " + key + " IN (" + strings.Join(values, ", ") + ")"
			if spec.filter != "" {
				query += " AND " + spec.filter
			}
			var one int
			err = tx.QueryRowContext(ctx, query+" LIMIT 1", args...).Scan(&one)
			if err == nil {
				return fmt.Errorf("dashboard has unsupported dependencies in %s; no changes applied", spec.table)
			}
			if err != sql.ErrNoRows {
				return dashboardDependencyError(spec.table, err)
			}
		}
	}
	if rbac {
		return nil
	}
	_, err := d.readRBAC(ctx, tx, permissionResources(dashboards, Table{}))
	return err
}

const dashboardQueryBatchSize = 256

var dependencySQLState = regexp.MustCompile(`^[0-9A-Z]{5}$`)

func dashboardDependencyError(table string, err error) error {
	message := "database query failed"
	var my *mysql.MySQLError
	var pg *pgconn.PgError
	switch {
	case errors.Is(err, context.Canceled):
		message = "operation canceled"
	case errors.Is(err, context.DeadlineExceeded):
		message = "query deadline exceeded; increase --timeout for large exports or slow database connections"
	case errors.As(err, &my):
		message = fmt.Sprintf("MySQL error %d", my.Number)
		switch my.Number {
		case 1142, 1143:
			message += "; SELECT permission is required on the dependency table and columns"
		case 1054:
			message += "; expected dependency column is missing; check the Grafana database schema"
		case 1146:
			message += "; dependency table is missing; check the Grafana database schema"
		}
	case errors.As(err, &pg):
		if dependencySQLState.MatchString(pg.Code) {
			message = "PostgreSQL SQLSTATE " + pg.Code
		}
		switch pg.Code {
		case "42501":
			message += "; SELECT permission is required on the dependency table and columns"
		case "42703":
			message += "; expected dependency column is missing; check the Grafana database schema"
		case "42P01":
			message += "; dependency table is missing; check the Grafana database schema"
		}
	}
	return fmt.Errorf("cannot inspect dashboard dependencies in %s: %s", table, message)
}
