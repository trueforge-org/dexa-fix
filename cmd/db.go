package cmd

import (
	"context"
	"fmt"
	"strconv"

	"github.com/spf13/cobra"

	"github.com/trueforge-org/dexa-fix/pkg/store/sqlstore"
)

const defaultSQLiteDSN = "file:dexa-fix.db"

// addDBFlags registers --db-driver and --dsn (with their DEXAFIX_* environment defaults) on c.
func addDBFlags(c *cobra.Command) {
	c.Flags().String("db-driver", envOr("DEXAFIX_DB_DRIVER", sqlstore.DriverSQLite), "database driver: sqlite or postgres")
	c.Flags().String("dsn", envOr("DEXAFIX_DSN", defaultSQLiteDSN), "database connection string")
}

func dbFlags(c *cobra.Command) (driver, dsn string) {
	driver, _ = c.Flags().GetString("db-driver")
	dsn, _ = c.Flags().GetString("dsn")
	return
}

var dbCmd = &cobra.Command{
	Use:   "db",
	Short: "Manage the database schema and data",
}

var dbMigrateCmd = &cobra.Command{
	Use:   "migrate [up|down|status|version|to N]",
	Short: "Apply, roll back or inspect schema migrations",
	Long: `Manage schema migrations. "up" applies all pending migrations (the default), "down" rolls back the
latest one, "to N" migrates up or down to version N (0 = empty), "status" lists all migrations and
"version" prints the current schema version.`,
	Example: `dexa-fix db migrate status
dexa-fix db migrate up --db-driver postgres --dsn postgres://user:pass@localhost/dexafix`,
	Args: cobra.RangeArgs(0, 2),
	RunE: func(cmd *cobra.Command, args []string) error {
		driver, dsn := dbFlags(cmd)
		ctx := cmd.Context()
		if ctx == nil {
			ctx = context.Background()
		}
		st, err := sqlstore.Open(ctx, driver, dsn, sqlstore.WithoutMigrate())
		if err != nil {
			return err
		}
		defer st.Close()

		action := "up"
		if len(args) > 0 {
			action = args[0]
		}
		out := cmd.OutOrStdout()
		switch action {
		case "up":
			res, err := st.Migrate(ctx)
			if err != nil {
				return err
			}
			if len(res) == 0 {
				fmt.Fprintln(out, "already up to date")
			}
			for _, r := range res {
				fmt.Fprintf(out, "applied %s\n", r.Source.Path)
			}
		case "down":
			r, err := st.MigrateDown(ctx)
			if err != nil {
				return err
			}
			fmt.Fprintf(out, "rolled back %s\n", r.Source.Path)
		case "to":
			if len(args) != 2 {
				return fmt.Errorf("usage: db migrate to <version>")
			}
			v, err := strconv.ParseInt(args[1], 10, 64)
			if err != nil || v < 0 {
				return fmt.Errorf("invalid version %q", args[1])
			}
			if _, err := st.MigrateTo(ctx, v); err != nil {
				return err
			}
			fmt.Fprintf(out, "schema at version %d\n", v)
		case "status":
			sts, err := st.MigrationStatus(ctx)
			if err != nil {
				return err
			}
			for _, s := range sts {
				state := "pending"
				if s.State == "applied" {
					state = "applied " + s.AppliedAt.Format("2006-01-02 15:04:05")
				}
				fmt.Fprintf(out, "%-24s %s\n", s.Source.Path, state)
			}
		case "version":
			v, err := st.SchemaVersion(ctx)
			if err != nil {
				return err
			}
			fmt.Fprintln(out, v)
		default:
			return fmt.Errorf("unknown action %q (use up, down, to, status or version)", action)
		}
		return nil
	},
}

var dbCopyCmd = &cobra.Command{
	Use:   "copy",
	Short: "Copy all data from one database to another (e.g. SQLite to Postgres)",
	Long: `Copy every row from the source database to the target database, keeping ids. The target is migrated to
the latest schema first and must be empty. The copy is a single transaction on the target.`,
	Example: `dexa-fix db copy --from-driver sqlite --from-dsn file:dexa-fix.db \
  --to-driver postgres --to-dsn postgres://user:pass@localhost/dexafix`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		fromDriver, _ := cmd.Flags().GetString("from-driver")
		fromDSN, _ := cmd.Flags().GetString("from-dsn")
		toDriver, _ := cmd.Flags().GetString("to-driver")
		toDSN, _ := cmd.Flags().GetString("to-dsn")
		if fromDSN == "" || toDSN == "" || toDriver == "" {
			return fmt.Errorf("--from-dsn, --to-driver and --to-dsn are required")
		}
		ctx := cmd.Context()
		if ctx == nil {
			ctx = context.Background()
		}
		// The source is never modified: it must already be migrated by the same binary version.
		src, err := sqlstore.Open(ctx, fromDriver, fromDSN, sqlstore.RequireMigrated())
		if err != nil {
			return fmt.Errorf("source: %w", err)
		}
		defer src.Close()
		dst, err := sqlstore.Open(ctx, toDriver, toDSN)
		if err != nil {
			return fmt.Errorf("target: %w", err)
		}
		defer dst.Close()

		stats, err := sqlstore.Copy(ctx, src, dst)
		if err != nil {
			return err
		}
		out := cmd.OutOrStdout()
		fmt.Fprintf(out, "users:           %d\n", stats.Users)
		fmt.Fprintf(out, "dexa scans:      %d\n", stats.DexaScans)
		fmt.Fprintf(out, "scale readings:  %d\n", stats.ScaleReads)
		fmt.Fprintf(out, "integrations:    %d\n", stats.Integrations)
		fmt.Fprintf(out, "identities:      %d\n", stats.Identities)
		fmt.Fprintf(out, "access tokens:   %d\n", stats.AccessTokens)
		fmt.Fprintf(out, "settings:        %d\n", stats.Settings)
		fmt.Fprintf(out, "auth providers:  %d\n", stats.Providers)
		return nil
	},
}

func init() {
	addDBFlags(dbMigrateCmd)
	dbCopyCmd.Flags().String("from-driver", sqlstore.DriverSQLite, "source database driver: sqlite or postgres")
	dbCopyCmd.Flags().String("from-dsn", envOr("DEXAFIX_DSN", ""), "source connection string")
	dbCopyCmd.Flags().String("to-driver", "", "target database driver: sqlite or postgres")
	dbCopyCmd.Flags().String("to-dsn", "", "target connection string")
	dbCmd.AddCommand(dbMigrateCmd, dbCopyCmd)
	RootCmd.AddCommand(dbCmd)
}
