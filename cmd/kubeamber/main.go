// Command kubeamber is a safety layer between AI agents and the Kubernetes API.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/kubeamber/kubeamber/internal/kube"
	"github.com/kubeamber/kubeamber/internal/version"
)

// exitError carries a process exit code without printing anything extra.
type exitError struct{ code int }

func (e exitError) Error() string { return fmt.Sprintf("exit %d", e.code) }

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var kf kube.Flags
	root := &cobra.Command{
		Use:   "kubeamber",
		Short: "Traffic lights for AI agents on Kubernetes",
		Long: `KubeAmber reviews every change an AI agent makes to a Kubernetes cluster.
Low-risk changes go straight through. Risky ones are held until a human
approves them, and dangerous ones are denied, each with a reason the agent
can read.`,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().StringVar(&kf.Kubeconfig, "kubeconfig", "", "path to the kubeconfig file")
	root.PersistentFlags().StringVar(&kf.Context, "context", "", "kubeconfig context to use")
	root.PersistentFlags().StringVar(&kf.As, "as", "", "impersonate this user, like kubectl --as")

	root.AddCommand(
		webhookCmd(&kf),
		checkCmd(),
		listCmd(&kf),
		decideCmd(&kf, "approve"),
		decideCmd(&kf, "reject"),
		mcpCmd(&kf),
		bridgeCmd(&kf),
		notifyCmd(&kf),
		agentKubeconfigCmd(&kf),
		&cobra.Command{
			Use:   "version",
			Short: "Print the version",
			Run:   func(cmd *cobra.Command, _ []string) { fmt.Fprintln(cmd.OutOrStdout(), version.Version) },
		},
	)

	err := root.ExecuteContext(ctx)
	var ee exitError
	switch {
	case errors.As(err, &ee):
		os.Exit(ee.code)
	case err != nil:
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
