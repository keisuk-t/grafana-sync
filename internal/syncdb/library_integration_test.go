//go:build integration

package syncdb

import (
	"context"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestLibraryTransfer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	dbs := map[string]*Database{}
	snapshots := map[string]*Snapshot{}
	for _, engine := range []string{"mysql", "postgres"} {
		db, err := Open(ctx, engine, os.Getenv(strings.ToUpper(engine)+"_TEST_DSN"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { db.DB.Close() })
		createDashboardFixtures(t, ctx, db)
		seedFixtures(t, ctx, db)
		s, err := librarySnapshot().Select(TargetDashboard)
		if err != nil {
			t.Fatal(err)
		}
		if err := db.Import(ctx, s); err != nil {
			t.Fatal(err)
		}
		snapshots[engine], err = db.exportLegacyTargets(ctx, AllTargets)
		if err != nil {
			t.Fatal(err)
		}
		dbs[engine] = db
	}
	if !reflect.DeepEqual(snapshots["mysql"], snapshots["postgres"]) {
		t.Fatal("library exports differ between engines")
	}
	for source, snapshot := range snapshots {
		for target, db := range dbs {
			for _, mode := range []ImportMode{ImportRequireEmpty, ImportInsertOnly, ImportSync} {
				t.Run(source+"_to_"+target+"/"+string(mode), func(t *testing.T) {
					clearDashboardFixtures(t, ctx, db)
					base, err := snapshot.Select(DefaultTargets)
					if err != nil {
						t.Fatal(err)
					}
					if err := db.Import(ctx, base); err != nil {
						t.Fatal(err)
					}
					s, err := snapshot.Select(TargetDashboard)
					if err != nil {
						t.Fatal(err)
					}
					before := dashboardDatabaseState(t, ctx, db)
					if mode == ImportSync {
						if _, err := db.Sync(ctx, s, SyncOptions{DryRun: true}); err != nil {
							t.Fatal(err)
						}
						if dashboardDatabaseState(t, ctx, db) != before {
							t.Fatal("dry-run changed data or sequences")
						}
						if _, err := db.Sync(ctx, s, SyncOptions{AllowDelete: true}); err != nil {
							t.Fatal(err)
						}
					} else if _, err := db.ImportWithMode(ctx, s, mode); err != nil {
						t.Fatal(err)
					}
					got, err := db.exportLegacyTargets(ctx, AllTargets)
					if err != nil || !reflect.DeepEqual(got, snapshot) {
						t.Fatal("shared panel and 14 connections did not round trip", err)
					}
					before = dashboardDatabaseState(t, ctx, db)
					if _, err := db.ImportWithMode(ctx, base, ImportInsertOnly); err != nil || dashboardDatabaseState(t, ctx, db) != before {
						t.Fatal("default targets changed library data", err)
					}
					s = cloneSnapshot(t, s)
					s.LibraryElement.Rows[0][columnIndex(s.LibraryElement.Columns, "name")] = str("スキップする名前変更")
					if _, err := db.ImportWithMode(ctx, s, ImportInsertOnly); err != nil || dashboardDatabaseState(t, ctx, db) != before {
						t.Fatal("insert-only changed existing library or children", err)
					}
				})
			}
		}
	}
	for engine, db := range dbs {
		t.Run(engine+"_library_sync", func(t *testing.T) {
			testLibrarySync(t, ctx, db, snapshots[engine])
		})
	}
}

func testLibrarySync(t *testing.T, ctx context.Context, db *Database, source *Snapshot) {
	reset := func() {
		clearDashboardFixtures(t, ctx, db)
		if err := db.Import(ctx, source); err != nil {
			t.Fatal(err)
		}
	}
	assertRejected := func(s *Snapshot) {
		before := dashboardDatabaseState(t, ctx, db)
		if _, err := db.Sync(ctx, s, SyncOptions{AllowDelete: true}); err == nil {
			t.Fatal("unsafe library sync was accepted")
		}
		if dashboardDatabaseState(t, ctx, db) != before {
			t.Fatal("failed library sync changed data or sequences")
		}
	}
	reset()
	s := cloneSnapshot(t, source)
	s.LibraryElement.Rows[0][columnIndex(s.LibraryElement.Columns, "model")] = str(`{"uid":"shared","type":"timeseries","datasource":{"uid":"missing"}}`)
	assertRejected(s)
	s = cloneSnapshot(t, source)
	s.LibraryElement.Rows[0][columnIndex(s.LibraryElement.Columns, "type")] = str(strings.Repeat("x", 41))
	s.LibraryElement.Rows[0][columnIndex(s.LibraryElement.Columns, "model")] = str(`{"uid":"shared","type":"` + strings.Repeat("x", 41) + `"}`)
	s.Dashboard.Rows[2][columnIndex(s.Dashboard.Columns, "data")] = str(`{"description":"must roll back","panels":[{"libraryPanel":{"uid":"shared"}}]}`)
	assertRejected(s)
	if _, err := db.DB.ExecContext(ctx, "UPDATE library_element_connection SET kind = 2 WHERE id = 100"); err != nil {
		t.Fatal(err)
	}
	assertRejected(source)
	reset()
	if _, err := db.DB.ExecContext(ctx, "UPDATE dashboard SET org_id = 77 WHERE id = 13"); err != nil {
		t.Fatal(err)
	}
	s = cloneSnapshot(t, source)
	s.Dashboard.Rows = append(s.Dashboard.Rows[:3], s.Dashboard.Rows[4:]...)
	s.LibraryConnection.Rows = append(s.LibraryConnection.Rows[:1], s.LibraryConnection.Rows[2:]...)
	before := dashboardDatabaseState(t, ctx, db)
	if _, err := db.Sync(ctx, s, SyncOptions{AllowDelete: true}); err != nil || dashboardDatabaseState(t, ctx, db) != before {
		t.Fatal("sync failed to retain an out-of-scope dashboard and connection", err)
	}
	s.LibraryElement.Rows[0][columnIndex(s.LibraryElement.Columns, "name")] = str("外部参照を持つパネル")
	assertRejected(s)
	reset()
	if _, err := db.DB.ExecContext(ctx, "INSERT INTO "+db.quote("role")+" (id,org_id,name,version,uid,created,updated) VALUES (9000,9007199254740993,'custom-library-role',0,'custom-library-role','2026-01-02 03:04:05','2026-01-02 03:04:05')"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.ExecContext(ctx, "INSERT INTO permission (id,role_id,action,scope,kind,attribute,identifier,created,updated) VALUES (9000,9000,'library.panels:read','library.panels:uid:shared','library.panels','uid','shared','2026-01-02 03:04:05','2026-01-02 03:04:05')"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.exportLegacyTargets(ctx, TargetDashboard); err == nil {
		t.Fatal("export ignored library-specific custom-role permission")
	}
	assertRejected(source)
	reset()
	legacy := cloneSnapshot(t, source)
	legacy.Version, legacy.LibraryElement, legacy.LibraryConnection = 3, Table{}, Table{}
	for _, row := range legacy.Dashboard.Rows {
		row[columnIndex(legacy.Dashboard.Columns, "data")] = str(`{}`)
	}
	for _, row := range legacy.DashboardVersion.Rows {
		row[columnIndex(legacy.DashboardVersion.Columns, "data")] = str(`{}`)
	}
	permissions := []DashboardPermission{}
	for _, p := range legacy.DashboardPermissions {
		if !strings.HasPrefix(p.Scope, "library.panels:") {
			permissions = append(permissions, p)
		}
	}
	legacy.DashboardPermissions = permissions
	if _, err := db.Sync(ctx, legacy, SyncOptions{AllowDelete: true}); err != nil {
		t.Fatal("legacy sync failed to preserve libraries", err)
	}
	got, err := db.exportLegacyTargets(ctx, AllTargets)
	if err != nil || !reflect.DeepEqual(got.LibraryElement, source.LibraryElement) || !reflect.DeepEqual(got.LibraryConnection, source.LibraryConnection) {
		t.Fatal("legacy sync changed libraries", err)
	}
	for _, entry := range legacy.entries() {
		if entry.group == TargetDashboard {
			entry.table.Rows = [][]*string{}
		}
	}
	legacy.DashboardPermissions = nil
	assertRejected(legacy)
	reset()
	s = cloneSnapshot(t, source)
	other := append([]*string{}, s.LibraryElement.Rows[0]...)
	other[columnIndex(s.LibraryElement.Columns, "id")] = str("41")
	other[columnIndex(s.LibraryElement.Columns, "uid")] = str("alternate")
	other[columnIndex(s.LibraryElement.Columns, "name")] = str("別パネル")
	other[columnIndex(s.LibraryElement.Columns, "model")] = str(`{"uid":"alternate","type":"timeseries"}`)
	s.LibraryElement.Rows = append(s.LibraryElement.Rows, other)
	if _, err := db.Sync(ctx, s, SyncOptions{AllowDelete: true}); err != nil {
		t.Fatal("failed to add a second library panel", err)
	}
	for i, uid := range []string{"alternate", "shared"} {
		row := s.LibraryElement.Rows[i]
		row[columnIndex(s.LibraryElement.Columns, "uid")] = str(uid)
		row[columnIndex(s.LibraryElement.Columns, "model")] = str(`{"uid":"` + uid + `","type":"timeseries"}`)
	}
	name := columnIndex(s.LibraryElement.Columns, "name")
	s.LibraryElement.Rows[0][name], s.LibraryElement.Rows[1][name] = s.LibraryElement.Rows[1][name], s.LibraryElement.Rows[0][name]
	for _, table := range []*Table{&s.Dashboard, &s.DashboardVersion} {
		data := columnIndex(table.Columns, "data")
		for _, row := range table.Rows {
			row[data] = str(strings.ReplaceAll(*row[data], `"shared"`, `"alternate"`))
		}
	}
	if _, err := db.Sync(ctx, s, SyncOptions{AllowDelete: true}); err != nil {
		t.Fatal("failed to exchange library UIDs and names", err)
	}
	s.LibraryElement.Rows[0][columnIndex(s.LibraryElement.Columns, "folder_id")] = str("0")
	s.LibraryElement.Rows[0][columnIndex(s.LibraryElement.Columns, "folder_uid")] = str("")
	if _, err := db.Sync(ctx, s, SyncOptions{AllowDelete: true}); err != nil {
		t.Fatal("failed to move a library panel to the root folder", err)
	}
	reset()
	s = cloneSnapshot(t, source)
	s.LibraryElement.Rows[0][columnIndex(s.LibraryElement.Columns, "id")] = str("41")
	for _, row := range s.LibraryConnection.Rows {
		row[columnIndex(s.LibraryConnection.Columns, "element_id")] = str("41")
	}
	if _, err := db.Sync(ctx, s, SyncOptions{AllowDelete: true}); err != nil {
		t.Fatal("failed to recreate a shared library element with a new ID", err)
	}
	s.LibraryElement.Rows[0][columnIndex(s.LibraryElement.Columns, "name")] = str("更新後")
	if _, err := db.Sync(ctx, s, SyncOptions{AllowDelete: true}); err != nil {
		t.Fatal(err)
	}
	result, err := db.Sync(ctx, s, SyncOptions{AllowDelete: true})
	if err != nil || result.Inserted+result.Updated+result.Deleted != 0 {
		t.Fatal("library sync is not idempotent", err)
	}
	for _, entry := range s.entries() {
		if entry.group == TargetDashboard {
			entry.table.Rows = [][]*string{}
		}
	}
	s.DashboardPermissions = nil
	if _, err := db.Sync(ctx, s, SyncOptions{AllowDelete: true}); err != nil {
		t.Fatal("failed to delete connections before library and dashboard parents", err)
	}
}
