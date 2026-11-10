package cmd

import (
	"github.com/spf13/cobra"
	"github.com/trueforge-org/forgetool/v4/pkg/info"
)

var infoCmd = &cobra.Command{
	Use:     "info",
	Short:   "Prints information about the DexaFix binary",
	Example: "dexafix info",
	Run: func(cmd *cobra.Command, args []string) {
		info.NewInfo().Print()
	},
}

func init() {
	RootCmd.AddCommand(infoCmd)
}
