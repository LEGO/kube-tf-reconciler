package cmd

import (
	"os"

	"github.com/spf13/cobra"
)

//nolint:gochecknoglobals
var cfgFile string

// rootCmd represents the base command when called without any subcommands.
//
//nolint:exhaustruct,gochecknoglobals
var rootCmd = &cobra.Command{
	Use:   "krec",
	Short: "Terraform operator",
	Long:  `Operator for managing terraform resources in Kubernetes.`,
	// Uncomment the following line if your bare application
	// has an action associated with it:
	// Run: func(cmd *cobra.Command, args []string) { },
}

// Execute adds all child commands to the root command and sets flags appropriately.
// This is called by main.main(). It only needs to happen once to the rootCmd.
func Execute() {
	err := rootCmd.Execute()
	if err != nil {
		os.Exit(1)
	}
}
