//nolint:gochecknoinits,gochecknoglobals
package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

var (
	version = "development"
	commit  = "none"
	date    = "some time ago"
	home    = "https://github.com/lukaspj/kube-tf-reconciler"
)

// versionCmd represents the version command.
//
//nolint:exhaustruct
var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Prints the version of the tool",
	Run: func(_ *cobra.Command, _ []string) {
		fmt.Printf("krec %s (%s) %s [%s]\n", version, commit, date, home) //nolint:forbidigo
	},
}

//nolint:gochecknoinits
func init() {
	rootCmd.AddCommand(versionCmd)
}
