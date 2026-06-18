package main

import (
	"github.com/spf13/cobra"

	"github.com/papanton/bazel-broker/internal/cli"
)

// newProfileCmd resolves a build's profile target and opens it in Perfetto via
// macOS `open`. --print emits the resolved target without opening (headless
// /verify); --json prints {"invocation_id","profile"}. Needs E4 — degrades to
// ExitNotImplemented (6) on 501 until then.
func newProfileCmd(opt *cli.GlobalOpts) *cobra.Command {
	var print bool
	cmd := &cobra.Command{
		Use:   "profile <invocation_id>",
		Short: "Open a build's Bazel profile in Perfetto",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := cli.NewClient(*opt)
			if err != nil {
				return err
			}
			return cli.RunProfile(cmd.Context(), c, args[0], cli.ProfileOpts{
				Print: print,
				JSON:  opt.JSON,
			}, cmd.OutOrStdout())
		},
	}
	cmd.Flags().BoolVar(&print, "print", false, "print the resolved profile target instead of opening it")
	return cmd
}
