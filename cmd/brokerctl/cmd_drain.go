package main

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/antoniospapantoniou/bazel-broker/internal/cli"
)

// newAdmissionCmd builds one admission-control command. drain/pause/resume are
// parallel actions over the parallel /admission/{drain,pause,resume} routes, so
// they are sibling top-level commands (not nested). All degrade to
// ExitNotImplemented (6) on 501 until E5 lands.
func newAdmissionCmd(opt *cli.GlobalOpts, use, short, verb string, do func(context.Context, *cli.Client) error) *cobra.Command {
	return &cobra.Command{
		Use:   use,
		Short: short,
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := cli.NewClient(*opt)
			if err != nil {
				return err
			}
			if err := do(cmd.Context(), c); err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if opt.JSON {
				return cli.RenderJSON(out, map[string]string{"admission": verb})
			}
			fmt.Fprintf(out, "admission %s\n", verb)
			return nil
		},
	}
}

func newDrainCmd(opt *cli.GlobalOpts) *cobra.Command {
	return newAdmissionCmd(opt, "drain", "Stop admitting new builds (E5)", "drained",
		func(ctx context.Context, c *cli.Client) error { return c.Drain(ctx) })
}

func newPauseCmd(opt *cli.GlobalOpts) *cobra.Command {
	return newAdmissionCmd(opt, "pause", "Pause admission (E5)", "paused",
		func(ctx context.Context, c *cli.Client) error { return c.Pause(ctx) })
}

func newResumeCmd(opt *cli.GlobalOpts) *cobra.Command {
	return newAdmissionCmd(opt, "resume", "Resume admitting builds (E5)", "resumed",
		func(ctx context.Context, c *cli.Client) error { return c.Resume(ctx) })
}
