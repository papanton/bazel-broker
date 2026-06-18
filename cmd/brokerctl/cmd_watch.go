package main

import (
	"github.com/mattn/go-isatty"
	"github.com/spf13/cobra"

	"github.com/papanton/bazel-broker/internal/cli"
)

// newWatchCmd streams live build upserts over WS /events. Human mode redraws a
// live table; --json emits NDJSON (one event per line). Reconnects with backoff
// unless --once; Ctrl-C exits 0.
func newWatchCmd(opt *cli.GlobalOpts) *cobra.Command {
	var once bool
	cmd := &cobra.Command{
		Use:   "watch",
		Short: "Stream live build updates over WebSocket",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := cli.NewClient(*opt)
			if err != nil {
				return err
			}
			tty := isTerminal(cmd)
			return cli.RunWatch(cmd.Context(), c, cli.WatchOpts{
				JSON: opt.JSON,
				Once: once,
				TTY:  tty && !opt.JSON,
			}, cmd.OutOrStdout())
		},
	}
	cmd.Flags().BoolVar(&once, "once", false, "exit on first disconnect instead of reconnecting (for scripts/verify)")
	return cmd
}

// isTerminal reports whether the command's stdout is an interactive TTY.
func isTerminal(cmd *cobra.Command) bool {
	if f, ok := cmd.OutOrStdout().(interface{ Fd() uintptr }); ok {
		return isatty.IsTerminal(f.Fd())
	}
	return false
}
