package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/keisuk-t/grafana-sync/internal/syncdb"
)

const usage = `Usage: grafana-sync <export|import> -db <mysql|postgres> -file <snapshot.json> [options]

Transfers org, data_source, all data_keys and matching datasource secrets.
Import requires all four target tables to be empty. Credentials are read from
GRAFANA_SYNC_DSN (or -dsn-env). Original secret_key or KMS keys remain required.
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
	if err := flags.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 || *file == "" || (*engine != "mysql" && *engine != "postgres") || *timeout <= 0 {
		return errors.New("require -db mysql|postgres, -file and a positive -timeout; positional arguments are not supported")
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
	}
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	db, err := syncdb.Open(ctx, *engine, dsn)
	if err != nil {
		return err
	}
	defer db.DB.Close()
	if command == "import" {
		if err := db.Import(ctx, snapshot); err != nil {
			return err
		}
	} else {
		snapshot, err = db.Export(ctx)
		if err != nil {
			return err
		}
		if err := writeSnapshot(*file, snapshot); err != nil {
			return err
		}
	}
	_, err = fmt.Fprintf(stdout, "%s completed: org=%d data_source=%d data_keys=%d secrets=%d\n", command, len(snapshot.Org.Rows), len(snapshot.DataSource.Rows), len(snapshot.DataKeys.Rows), len(snapshot.Secrets.Rows))
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
