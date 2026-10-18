package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/keisuk-t/grafana-sync/internal/syncdb"
)

const usage = `Usage: grafana-sync <export|import> -db <mysql|postgres> -file <snapshot.json> [options]

Transfers --targets org,datasource,dashboard (default: org,datasource).
Datasource includes data_keys and matching datasource secrets. Dashboard includes
legacy SQL folders, tags, versions and Viewer/Editor resource permissions.
Individual/team permissions and Unified Storage are not supported.
Import defaults to --mode require-empty. Use --mode insert-only to add missing
primary keys without changing existing rows. Credentials are read from
GRAFANA_SYNC_DSN (or -dsn-env). Original secret_key or KMS keys remain required.
Use --mode sync --dry-run to inspect changes in the JSON's organizations, then
--mode sync --allow-delete to apply. Selected resources missing from the JSON
are deleted only in scope.org_ids. Unselected groups and shared keys remain.
Stop Grafana and back up the target before importing. Dumps contain secrets.
`

func Run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		_, err := io.WriteString(stdout, usage)
		return err
	}
	command := args[0]
	if command != "export" && command != "import" {
		return errors.New("expected export or import subcommand")
	}
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	flags.SetOutput(stderr)
	engine := flags.String("db", "", "database engine: mysql or postgres")
	file := flags.String("file", "", "snapshot path (export refuses to overwrite)")
	dsnEnv := flags.String("dsn-env", "GRAFANA_SYNC_DSN", "environment variable containing the database DSN")
	timeout := flags.Duration("timeout", 2*time.Minute, "database operation timeout")
	targetNames := flags.String("targets", "org,datasource", "comma-separated resource targets: org,datasource,dashboard")
	modeName := string(syncdb.ImportRequireEmpty)
	var dryRun, allowDelete bool
	if command == "import" {
		flags.StringVar(&modeName, "mode", modeName, "import mode: require-empty, insert-only or sync")
		flags.BoolVar(&dryRun, "dry-run", false, "show sync changes without writing")
		flags.BoolVar(&allowDelete, "allow-delete", false, "approve sync changes, including deletion of missing rows")
	}
	if err := flags.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 || *file == "" || (*engine != "mysql" && *engine != "postgres") || *timeout <= 0 {
		return errors.New("require -db mysql|postgres, -file and a positive -timeout; positional arguments are not supported")
	}
	mode := syncdb.ImportMode(modeName)
	targets, err := syncdb.ParseTargets(*targetNames)
	if err != nil {
		return err
	}
	if err := mode.Validate(); err != nil {
		return err
	}
	if command == "import" {
		if mode != syncdb.ImportSync && (dryRun || allowDelete) {
			return errors.New("--dry-run and --allow-delete require --mode sync")
		}
		if mode == syncdb.ImportSync && !dryRun && !allowDelete {
			return errors.New("sync requires --dry-run or --allow-delete")
		}
	}
	dsn := os.Getenv(*dsnEnv)
	if dsn == "" {
		return errors.New("DSN environment variable is unset or empty")
	}
	var snapshot *syncdb.Snapshot
	if command == "import" {
		input, err := os.Open(*file)
		if err != nil {
			return fmt.Errorf("open snapshot: %w", err)
		}
		snapshot, err = syncdb.Decode(input)
		input.Close()
		if err != nil {
			return err
		}
		snapshot, err = snapshot.Select(targets)
		if err != nil {
			return err
		}
	}
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	db, err := syncdb.Open(ctx, *engine, dsn)
	if err != nil {
		return err
	}
	defer db.DB.Close()
	if command == "import" {
		var result syncdb.ImportResult
		if mode == syncdb.ImportSync {
			result, err = db.Sync(ctx, snapshot, syncdb.SyncOptions{DryRun: dryRun, AllowDelete: allowDelete})
		} else {
			result, err = db.ImportWithMode(ctx, snapshot, mode)
		}
		if err != nil {
			return err
		}
		if err := printSelection(stdout, snapshot); err != nil {
			return err
		}
		if mode == syncdb.ImportSync {
			state := "completed"
			if dryRun {
				state = "planned (no changes applied)"
			}
			if _, err := fmt.Fprintf(stdout, "sync %s: inserted=%d updated=%d deleted=%d unchanged=%d skipped=%d\n", state, result.Inserted, result.Updated, result.Deleted, result.Unchanged, result.Skipped); err != nil {
				return err
			}
			for _, entry := range []struct {
				name  string
				count syncdb.TableResult
			}{{"org", result.Org}, {"data_keys", result.DataKeys}, {"data_source", result.DataSource}, {"secrets", result.Secrets}, {"folder", result.Folder}, {"dashboard", result.Dashboard}, {"dashboard_tag", result.DashboardTag}, {"dashboard_version", result.DashboardVersion}, {"dashboard_permissions", result.DashboardPermissions}} {
				if !tableSelected(entry.name, targets) {
					continue
				}
				if _, err := fmt.Fprintf(stdout, "%s: inserted=%d updated=%d deleted=%d unchanged=%d skipped=%d\n", entry.name, entry.count.Inserted, entry.count.Updated, entry.count.Deleted, entry.count.Unchanged, entry.count.Skipped); err != nil {
					return err
				}
			}
			return nil
		}
		_, err = fmt.Fprintf(stdout, "import completed: mode=%s inserted=%d skipped=%d\n", mode, result.Inserted, result.Skipped)
		return err
	} else {
		snapshot, err = db.ExportTargets(ctx, targets)
		if err != nil {
			return err
		}
		if err := writeSnapshot(*file, snapshot); err != nil {
			return err
		}
		if err := printSelection(stdout, snapshot); err != nil {
			return err
		}
	}
	_, err = fmt.Fprintf(stdout, "%s completed: org=%d data_source=%d data_keys=%d secrets=%d dashboard=%d folder=%d dashboard_tag=%d dashboard_version=%d\n", command, len(snapshot.Org.Rows), len(snapshot.DataSource.Rows), len(snapshot.DataKeys.Rows), len(snapshot.Secrets.Rows), len(snapshot.Dashboard.Rows), len(snapshot.Folder.Rows), len(snapshot.DashboardTag.Rows), len(snapshot.DashboardVersion.Rows))
	return err
}

func tableSelected(name string, targets syncdb.Targets) bool {
	switch name {
	case "org":
		return targets.Has(syncdb.TargetOrg)
	case "data_source", "data_keys", "secrets":
		return targets.Has(syncdb.TargetDatasource)
	default:
		return targets.Has(syncdb.TargetDashboard)
	}
}

func printSelection(out io.Writer, s *syncdb.Snapshot) error {
	_, err := fmt.Fprintf(out, "targets=%s scope.org_ids=%s\n", s.Selection(), strings.Join(s.ScopeIDs(), ","))
	if err == nil && s.Selection().Has(syncdb.TargetDashboard) {
		_, err = fmt.Fprintln(out, "dashboard tables: dashboard,folder,dashboard_tag,dashboard_version; includes Viewer/Editor permissions; warning: dynamic and plugin-specific external references cannot be fully checked")
	}
	return err
}

func writeSnapshot(path string, s *syncdb.Snapshot) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create snapshot (existing files are not overwritten): %w", err)
	}
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	err = enc.Encode(s)
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		os.Remove(path)
		return fmt.Errorf("write snapshot: %w", err)
	}
	return nil
}
