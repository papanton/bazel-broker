package main

import (
	"github.com/spf13/cobra"

	"github.com/antoniospapantoniou/bazel-broker/internal/cli"
)

// newLsCmd lists builds: GET /builds. --json echoes E2's {"builds":[…]} verbatim
// (the /verify contract); the default is a human table.
func newLsCmd(opt *cli.GlobalOpts) *cobra.Command {
	return &cobra.Command{
		Use:   "ls",
		Short: "List active and recent builds",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := cli.NewClient(*opt)
			if err != nil {
				return err
			}
			resp, err := c.ListBuildsResponse(cmd.Context())
			if err != nil {
				return err
			}
			if opt.JSON {
				return cli.RenderJSON(cmd.OutOrStdout(), resp)
			}
			cli.RenderBuildsTable(cmd.OutOrStdout(), resp.Builds)
			return nil
		},
	}
}
