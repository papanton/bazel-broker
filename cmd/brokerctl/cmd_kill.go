package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/papanton/bazel-broker/internal/cli"
)

// newKillCmd stops a build: POST /builds/{invocation_id}/kill. Degrades cleanly to
// ExitNotImplemented (6) on 501 until E3 lands; a 404 (unknown id) is ExitBroker.
func newKillCmd(opt *cli.GlobalOpts) *cobra.Command {
	return &cobra.Command{
		Use:   "kill <invocation_id>",
		Short: "Stop a build by invocation id",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := cli.NewClient(*opt)
			if err != nil {
				return err
			}
			res, err := c.Kill(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if opt.JSON {
				return cli.RenderJSON(cmd.OutOrStdout(), res)
			}
			outcome := res.Outcome
			if outcome == "" {
				outcome = "killed"
			}
			fmt.Fprintf(cmd.OutOrStdout(), "kill %s: %s\n", args[0], outcome)
			return nil
		},
	}
}
