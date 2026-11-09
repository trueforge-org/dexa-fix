package cmd

import (
	"context"
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/trueforge-org/dexa-fix/pkg/models"
	"github.com/trueforge-org/dexa-fix/pkg/store"
	"github.com/trueforge-org/dexa-fix/pkg/store/sqlstore"
)

var userCmd = &cobra.Command{
	Use:   "user",
	Short: "Account recovery that does not need to sign in",
}

var userReset2FACmd = &cobra.Command{
	Use:   "reset-2fa <e-mail>",
	Short: "Remove two-step verification from an account",
	Long: `Remove two-step verification (the one-time password and the recovery codes) from the account
with this e-mail address. Use it when nobody who is signed in can do it from the Admin page, for
example when the only administrator lost their authenticator app and their recovery codes.

It works on the database directly, so it needs the same database and encryption key settings as the
server. If an administrator requires two-step verification, the user sets it up again at the next
sign-in. The change is not written to the audit log of a running server.`,
	Example: `dexa-fix user reset-2fa admin@example.com`,
	Args:    cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		driver, dsn, err := dbFlags(cmd)
		if err != nil {
			return err
		}
		ctx := cmd.Context()
		if ctx == nil {
			ctx = context.Background()
		}
		c, err := loadCipher(ctx, cmd, driver, dsn)
		if err != nil {
			return err
		}
		st, err := sqlstore.Open(ctx, driver, dsn, sqlstore.WithCipher(c), sqlstore.RequireMigrated())
		if err != nil {
			return err
		}
		defer st.Close()

		u, err := st.GetUserByEmail(ctx, models.NormalizeEmail(args[0]))
		if errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("no account has the e-mail address %q", args[0])
		}
		if err != nil {
			return err
		}
		if err := st.DeleteTOTP(ctx, u.ID); err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "two-step verification removed for %s\n", u.Name)
		return nil
	},
}

func init() {
	addDBFlags(userReset2FACmd)
	addKeyFlags(userReset2FACmd)
	userCmd.AddCommand(userReset2FACmd)
	RootCmd.AddCommand(userCmd)
}
