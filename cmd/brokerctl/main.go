// Command brokerctl is the control+observe CLI. E0 ships the cobra root plus
// `version` and a stub `ls`; E6 adds ls --json, kill, drain, watch, profile.
package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/antoniospapantoniou/bazel-broker/internal/version"
)

func main() {
	root := &cobra.Command{
		Use:   "brokerctl",
		Short: "Control & observe Bazel builds via the broker",
	}
	root.AddCommand(&cobra.Command{
		Use:   "version",
		Short: "Print version",
		Run: func(c *cobra.Command, _ []string) {
			fmt.Fprintln(c.OutOrStdout(), version.String())
		},
	})
	// E6 adds: ls [--json], kill <id>, drain, watch, profile <id>.
	root.AddCommand(&cobra.Command{
		Use:   "ls",
		Short: "List builds (stub — E6)",
		Run: func(c *cobra.Command, _ []string) {
			fmt.Fprintln(c.OutOrStdout(), "[]")
		},
	})
	if err := root.Execute(); err != nil {
		os.Exit(1)
	}
}
