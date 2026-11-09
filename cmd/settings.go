package cmd

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	mainbackend "github.com/trueforge-org/dexa-fix/pkg/backend/main"
	"github.com/trueforge-org/dexa-fix/pkg/store/sqlstore"
)

var settingsCmd = &cobra.Command{
	Use:   "settings",
	Short: "Recover from server settings that locked you out",
}

var settingsResetNetworkCmd = &cobra.Command{
	Use:   "reset-network",
	Short: "Clear the base URL, the trusted proxies and the PROXY protocol setting",
	Long: `Clear the base URL, the trusted proxies and the PROXY protocol setting, which are normally
changed on the Admin page. Use it when a wrong value made the Admin page unreachable (for example a
base URL that does not match the address you open, so that sign-in cookies are not accepted).

It works on the database directly, so it needs the same database and encryption key settings as the
server. A running server reads the settings at start and when they are saved on the Admin page, so
restart it afterwards.`,
	Example: `dexa-fix settings reset-network`,
	Args:    cobra.NoArgs,
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
		st, err := sqlstore.Open(ctx, driver, dsn, dbOptions(cmd, sqlstore.WithCipher(c), sqlstore.RequireMigrated())...)
		if err != nil {
			return err
		}
		defer st.Close()
		for _, name := range mainbackend.NetworkSettingNames {
			if err := st.DeleteSetting(ctx, name); err != nil {
				return err
			}
		}
		fmt.Fprintln(cmd.OutOrStdout(), "base URL, trusted proxies and PROXY protocol cleared; restart the server")
		return nil
	},
}

func init() {
	addDBFlags(settingsResetNetworkCmd)
	addKeyFlags(settingsResetNetworkCmd)
	settingsCmd.AddCommand(settingsResetNetworkCmd)
	RootCmd.AddCommand(settingsCmd)
}
