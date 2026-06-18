// Command brokerctl is the control+observe CLI over the local broker daemon.
// It is a thin, stateless HTTP/WS client: the logic lives in internal/cli, and
// cobra here is just the subcommand dispatcher.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/antoniospapantoniou/bazel-broker/internal/cli"
	"github.com/antoniospapantoniou/bazel-broker/internal/version"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	root := newRootCmd()
	if err := root.ExecuteContext(ctx); err != nil {
		os.Exit(cli.ExitCodeFor(os.Stderr, err))
	}
}

func newRootCmd() *cobra.Command {
	var opt cli.GlobalOpts
	root := &cobra.Command{
		Use:           "brokerctl",
		Short:         "Control and observe Bazel builds via the local broker",
		Version:       version.String(),
		SilenceUsage:  true, // don't dump usage on runtime errors
		SilenceErrors: true, // we print errors ourselves with exit-code mapping
	}
	pf := root.PersistentFlags()
	pf.BoolVar(&opt.JSON, "json", false, "machine-readable JSON output")
	pf.StringVar(&opt.ConfigPath, "config", "", "path to config.json (default: $BAZEL_BROKER_CONFIG / XDG / ~/.config)")
	pf.IntVar(&opt.Port, "port", 0, "broker loopback port (overrides config; host stays loopback)")
	pf.StringVar(&opt.Token, "token", "", "bearer token (overrides config)")
	pf.DurationVar(&opt.Timeout, "timeout", 5*time.Second, "per-request timeout (not applied to watch)")

	root.AddCommand(
		newVersionCmd(),
		newLsCmd(&opt),
		newKillCmd(&opt),
		newDrainCmd(&opt),
		newPauseCmd(&opt),
		newResumeCmd(&opt),
		newWatchCmd(&opt),
		newProfileCmd(&opt),
	)
	return root
}

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print version",
		Args:  cobra.NoArgs,
		Run: func(c *cobra.Command, _ []string) {
			c.Println(version.String())
		},
	}
}
