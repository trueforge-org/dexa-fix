package cmd

import (
	"context"
	"fmt"
	"strconv"

	"github.com/spf13/cobra"

	"github.com/trueforge-org/dexa-fix/pkg/store/sqlstore"
)

const defaultSQLiteDSN = "file:dexa-fix.db"

// addDBFlags registers --db-driver, --dsn and --dsn-file (with their DEXAFIX_* environment
// defaults) on c. A connection string with a password is refused on the command line.
func addDBFlags(c *cobra.Command) {
	c.Flags().String("db-driver", envOr("DEXAFIX_DB_DRIVER", sqlstore.DriverSQLite), "database driver: sqlite or postgres")
	c.Flags().String("dsn", envOr("DEXAFIX_DSN", defaultSQLiteDSN), "database connection string (without a password: use DEXAFIX_DSN or --dsn-file for those)")
	c.Flags().String("dsn-file", envOr("DEXAFIX_DSN_FILE", ""), "file holding the database connection string, for strings with a password")
	c.Flags().Bool("without-body", envOr("DEXAFIX_WITHOUT_BODY", "false") == "true", "this database does not hold the scale readings and DEXA scans because a body service of its own does (dexa-fix serve body); keeps the commands from creating their tables here")
}

// dbOptions adds the options the shared database flags ask for to opts.
func dbOptions(c *cobra.Command, opts ...sqlstore.Option) []sqlstore.Option {
	if without, _ := c.Flags().GetBool("without-body"); without {
		opts = append(opts, sqlstore.WithoutBody())
	}
	return opts
}

func dbFlags(c *cobra.Command) (driver, dsn string, err error) {
	driver, _ = c.Flags().GetString("db-driver")
	dsn, err = dsnFlag(c, "dsn")
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
"version" prints the current schema version.

Rolling back ("down", or "to" a lower version) drops the tables and columns those migrations created,
and the data in them. It only runs with --yes, after showing what it would roll back.`,
	Example: `dexa-fix db migrate status
DEXAFIX_DSN=postgres://user:pass@localhost/dexafix dexa-fix db migrate up --db-driver postgres
dexa-fix db migrate up --db-driver postgres --dsn-file /run/secrets/dexafix_dsn`,
	Args: cobra.RangeArgs(0, 2),
	RunE: func(cmd *cobra.Command, args []string) error {
		driver, dsn, err := dbFlags(cmd)
		if err != nil {
			return err
		}
		ctx := cmd.Context()
		if ctx == nil {
			ctx = context.Background()
		}
		st, err := sqlstore.Open(ctx, driver, dsn, dbOptions(cmd, sqlstore.WithoutMigrate())...)
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
			cur, err := st.SchemaVersion(ctx)
			if err != nil {
				return err
			}
			if err := confirmRollback(ctx, cmd, st, max(cur-1, 0)); err != nil {
				return err
			}
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
			if err := confirmRollback(ctx, cmd, st, v); err != nil {
				return err
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

// confirmRollback refuses to roll the schema back to target unless --yes was given, and says what
// would be dropped. Moving up, or staying put, needs no confirmation.
func confirmRollback(ctx context.Context, cmd *cobra.Command, st *sqlstore.Store, target int64) error {
	cur, err := st.SchemaVersion(ctx)
	if err != nil {
		return err
	}
	if target >= cur {
		return nil
	}
	if yes, _ := cmd.Flags().GetBool("yes"); yes {
		return nil
	}
	sts, err := st.MigrationStatus(ctx)
	if err != nil {
		return err
	}
	out := cmd.ErrOrStderr()
	fmt.Fprintf(out, "This would roll the schema back from version %d to %d, undoing:\n", cur, target)
	for _, s := range sts {
		if s.State == "applied" && s.Source.Version > target {
			fmt.Fprintf(out, "  %s\n", s.Source.Path)
		}
	}
	fmt.Fprintln(out, "That drops the tables and columns those migrations created, together with their data.")
	return fmt.Errorf("refusing to roll back without --yes (make a backup first)")
}

var dbCopyCmd = &cobra.Command{
	Use:   "copy",
	Short: "Copy all data from one database to another (e.g. SQLite to Postgres)",
	Long: `Copy every row from the source database to the target database, keeping ids. The target is migrated to
the latest schema first and must be empty. The copy is a single transaction on the target.`,
	Example: `DEXAFIX_TO_DSN=postgres://user:pass@localhost/dexafix \
  dexa-fix db copy --from-driver sqlite --from-dsn file:dexa-fix.db --to-driver postgres
dexa-fix db copy --from-dsn file:dexa-fix.db --to-driver postgres --to-dsn-file /run/secrets/target_dsn`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		fromDriver, _ := cmd.Flags().GetString("from-driver")
		fromDSN, err := dsnFlag(cmd, "from-dsn")
		if err != nil {
			return err
		}
		toDriver, _ := cmd.Flags().GetString("to-driver")
		toDSN, err := dsnFlag(cmd, "to-dsn")
		if err != nil {
			return err
		}
		if fromDSN == "" || toDSN == "" || toDriver == "" {
			return fmt.Errorf("a source and target connection string and --to-driver are required (--from-dsn / DEXAFIX_FROM_DSN, --to-dsn / DEXAFIX_TO_DSN)")
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
		fmt.Fprintf(out, "audit events:    %d\n", stats.AuditEvents)
		return nil
	},
}

// rekeyBody re-encrypts the body data of a database that holds only that (the body service's own).
// --dsn and the key flags are the body service's.
func rekeyBody(ctx context.Context, cmd *cobra.Command, driver, dsn string, dryRun bool) error {
	c, err := loadCipherFor(ctx, cmd, driver, dsn, func() (bool, error) { return sqlstore.HasEncryptedBodyValues(ctx, driver, dsn) })
	if err != nil {
		return err
	}
	st, err := sqlstore.OpenBody(ctx, driver, dsn, sqlstore.WithCipher(c), sqlstore.RequireMigrated())
	if err != nil {
		return err
	}
	defer st.Close()
	stats, err := st.Rekey(ctx, dryRun)
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	verb := "re-encrypted"
	if dryRun {
		verb = "to re-encrypt"
		fmt.Fprintln(out, "dry run: nothing was changed")
	}
	fmt.Fprintf(out, "scale readings and DEXA scans %s: %d\n", verb, stats.Changed)
	fmt.Fprintf(out, "already under the current key: %d\n", stats.Current)
	return nil
}

var dbRekeyCmd = &cobra.Command{
	Use:   "rekey",
	Short: "Re-encrypt every encrypted value with the current encryption key",
	Long: `Re-encrypt every encrypted value with the current encryption key and recompute the e-mail
lookup hashes, in a single transaction: it completes or changes nothing. It can be repeated; values
already under the current key are skipped.

The old keys must be given as previous keys (DEXAFIX_ENCRYPTION_KEY_PREVIOUS or
--encryption-key-previous-file). Stop the server first and back up the database and the key.
--dry-run only reports how many values would change, and fails if any value cannot be opened.

With a body service of its own, the scale readings and DEXA scans are in another database with a key
of its own. Rotate that one with --body, pointing --dsn and the key flags at the body service's
database and key; give the main database --without-body.`,
	Example: `DEXAFIX_ENCRYPTION_KEY=<new> DEXAFIX_ENCRYPTION_KEY_PREVIOUS=<old> dexa-fix db rekey --dry-run
DEXAFIX_ENCRYPTION_KEY=<new> DEXAFIX_ENCRYPTION_KEY_PREVIOUS=<old> dexa-fix db rekey`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		driver, dsn, err := dbFlags(cmd)
		if err != nil {
			return err
		}
		dryRun, _ := cmd.Flags().GetBool("dry-run")
		ctx := cmd.Context()
		if ctx == nil {
			ctx = context.Background()
		}
		if body, _ := cmd.Flags().GetBool("body"); body {
			return rekeyBody(ctx, cmd, driver, dsn, dryRun)
		}
		c, err := loadCipher(ctx, cmd, driver, dsn)
		if err != nil {
			return err
		}
		st, err := sqlstore.Open(ctx, driver, dsn, dbOptions(cmd, sqlstore.WithCipher(c), sqlstore.RequireMigrated())...)
		if err != nil {
			return err
		}
		defer st.Close()

		stats, err := st.Rekey(ctx, dryRun)
		if err != nil {
			return err
		}
		out := cmd.OutOrStdout()
		verb := "re-encrypted"
		if dryRun {
			verb = "to re-encrypt"
			fmt.Fprintln(out, "dry run: nothing was changed")
		}
		fmt.Fprintf(out, "e-mail addresses %s: %d\n", verb, stats.Emails)
		fmt.Fprintf(out, "integrations %s:     %d\n", verb, stats.Integrations)
		fmt.Fprintf(out, "settings %s:         %d\n", verb, stats.Settings)
		fmt.Fprintf(out, "auth providers %s:   %d\n", verb, stats.Providers)
		fmt.Fprintf(out, "two-step secrets %s: %d\n", verb, stats.TOTP)
		fmt.Fprintf(out, "scale readings and DEXA scans %s: %d\n", verb, stats.Body)
		fmt.Fprintf(out, "e-mail lookups recomputed: %d\n", stats.Lookups)
		fmt.Fprintf(out, "already under the current key: %d\n", stats.Current)
		return nil
	},
}

func init() {
	addDBFlags(dbMigrateCmd)
	addDBFlags(dbRekeyCmd)
	addKeyFlags(dbRekeyCmd)
	dbRekeyCmd.Flags().Bool("dry-run", false, "only report what would change")
	dbRekeyCmd.Flags().Bool("body", false, "rotate the key of the body service's own database (scale readings and DEXA scans); --dsn and the key flags are then that service's")
	dbCopyCmd.Flags().String("from-driver", sqlstore.DriverSQLite, "source database driver: sqlite or postgres")
	dbMigrateCmd.Flags().Bool("yes", false, "confirm rolling the schema back, which drops tables and their data")
	dbCopyCmd.Flags().String("from-dsn", envOr("DEXAFIX_FROM_DSN", envOr("DEXAFIX_DSN", "")), "source connection string (without a password: use DEXAFIX_FROM_DSN or --from-dsn-file for those)")
	dbCopyCmd.Flags().String("from-dsn-file", envOr("DEXAFIX_FROM_DSN_FILE", ""), "file holding the source connection string")
	dbCopyCmd.Flags().String("to-driver", envOr("DEXAFIX_TO_DRIVER", ""), "target database driver: sqlite or postgres")
	dbCopyCmd.Flags().String("to-dsn", envOr("DEXAFIX_TO_DSN", ""), "target connection string (without a password: use DEXAFIX_TO_DSN or --to-dsn-file for those)")
	dbCopyCmd.Flags().String("to-dsn-file", envOr("DEXAFIX_TO_DSN_FILE", ""), "file holding the target connection string")
	dbCmd.AddCommand(dbMigrateCmd, dbCopyCmd, dbRekeyCmd)
	RootCmd.AddCommand(dbCmd)
}
