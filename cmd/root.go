package cmd

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

// RootCmd is the entrypoint of the dexa-fix CLI.
var RootCmd = &cobra.Command{
	Use:           "dexa-fix",
	Short:         "Correct smart-scale results using DEXA scan results",
	SilenceUsage:  true,
	SilenceErrors: true,
}

// Execute runs the root command.
func Execute() error {
	c, err := RootCmd.ExecuteContextC(context.Background())
	if err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		s := err.Error()
		if strings.Contains(s, "arg(s)") || strings.Contains(s, "flag") || strings.Contains(s, "command") {
			fmt.Fprintln(os.Stderr)
			fmt.Fprintln(os.Stderr, c.UsageString())
		}
	}
	return err
}
