package main

import (
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/spf13/cobra"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/restmapper"

	"github.com/kubeamber/kubeamber/internal/changerequest"
	"github.com/kubeamber/kubeamber/internal/kube"
	"github.com/kubeamber/kubeamber/internal/mcpserver"
	"github.com/kubeamber/kubeamber/pkg/decide"
	"github.com/kubeamber/kubeamber/pkg/policy"
)

func mcpCmd(kf *kube.Flags) *cobra.Command {
	var policyPath, agent string
	var offline bool
	cmd := &cobra.Command{
		Use:   "mcp --policy POLICY --agent NAME",
		Short: "Serve KubeAmber's preview tools to an AI agent over MCP (stdio)",
		Long: `Run an MCP server on stdin/stdout so an agent can preview changes before
making them. With cluster access (the default), previews compare against the
live object and the agent can check on held changes.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// stdout carries the MCP protocol; logs go to stderr.
			log := slog.New(slog.NewTextHandler(os.Stderr, nil))
			p, err := policy.Load(policyPath)
			if err != nil {
				return err
			}
			if p.Agent(agent) == nil {
				return fmt.Errorf("policy has no agent named %q", agent)
			}
			current := policy.NewCurrent(p)
			go current.Watch(cmd.Context(), policyPath, 5*time.Second, log, nil)

			s := &mcpserver.Server{
				Engine: &decide.Engine{Policy: current.Get},
				Policy: current.Get,
				Agent:  agent,
			}
			if !offline {
				cfg, err := kf.Config()
				if err != nil {
					return fmt.Errorf("cluster access (use --offline to run without): %w", err)
				}
				dyn, err := dynamic.NewForConfig(cfg)
				if err != nil {
					return err
				}
				dc, err := discovery.NewDiscoveryClientForConfig(cfg)
				if err != nil {
					return err
				}
				s.Dynamic = dyn
				s.Mapper = restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(dc))
				s.Store = &changerequest.Store{Client: dyn}
			}
			return s.Run(cmd.Context())
		},
	}
	cmd.Flags().StringVar(&policyPath, "policy", "", "policy file (required)")
	cmd.Flags().StringVar(&agent, "agent", "", "agent profile the connected agent runs as (required)")
	cmd.Flags().BoolVar(&offline, "offline", false, "do not contact the cluster")
	_ = cmd.MarkFlagRequired("policy")
	_ = cmd.MarkFlagRequired("agent")
	return cmd
}
