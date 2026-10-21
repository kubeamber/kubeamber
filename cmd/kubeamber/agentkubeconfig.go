package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/kubeamber/kubeamber/internal/agentid"
	"github.com/kubeamber/kubeamber/internal/kube"
	"github.com/kubeamber/kubeamber/pkg/policy"
)

func agentKubeconfigCmd(kf *kube.Flags) *cobra.Command {
	var o struct {
		namespace, role, output, policyFile, policyConfigMap string
		namespaces                                           []string
		clusterWide, revoke, force, allowUngated             bool
		duration                                             time.Duration
	}
	cmd := &cobra.Command{
		Use:   "agent-kubeconfig AGENT",
		Short: "Give an AI agent its own short-lived identity and kubeconfig",
		Long: `Create an identity for an agent you launch (a service account named after the
agent and you, e.g. claude-code-alice), bind it only the access you choose,
and write a kubeconfig with a short-lived token for it. KubeAmber then
reviews the agent's changes as an agent, records you as its owner, and does
not let you approve its held changes yourself.

You cannot give an agent more than you have: the API server refuses to bind a
role you do not hold. --revoke deletes the identity: its access ends at once
and every token issued for it stops authenticating within seconds.`,
		Example: `  kubeamber agent-kubeconfig claude-code --role edit --namespace dev --namespace staging
  KUBECONFIG=./claude-code.kubeconfig claude
  kubeamber agent-kubeconfig claude-code --revoke`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			out := cmd.OutOrStdout()
			cfg, err := kf.Config()
			if err != nil {
				return err
			}
			cs, err := kubernetes.NewForConfig(cfg)
			if err != nil {
				return err
			}
			me, err := whoami(ctx, cs)
			if err != nil {
				return fmt.Errorf("finding your identity: %w", err)
			}
			id := agentid.Identity{Namespace: o.namespace, Agent: args[0], Owner: me, Role: o.role,
				Namespaces: o.namespaces, ClusterWide: o.clusterWide}
			name, err := id.ServiceAccountName()
			if err != nil {
				return err
			}
			if o.revoke {
				if err := agentid.Revoke(ctx, cs, o.namespace, name, me, o.force); err != nil {
					return err
				}
				fmt.Fprintf(out, "revoked %s/%s: its access is removed now, and every token issued for it stops authenticating within seconds\n", o.namespace, name)
				return nil
			}
			if o.role != "" && len(o.namespaces) == 0 && !o.clusterWide {
				return fmt.Errorf("--role needs --namespace (repeatable) or --cluster-wide")
			}
			if o.duration < 10*time.Minute || o.duration > 24*time.Hour {
				return fmt.Errorf("--duration must be between 10m and 24h; mint a new kubeconfig when it expires")
			}
			user, _ := id.Username()

			profile, perr := gatedAs(ctx, cs, o.policyFile, o.policyConfigMap, user, o.namespace)
			switch {
			case perr != nil:
				fmt.Fprintf(cmd.ErrOrStderr(), "warning: could not read KubeAmber's policy to confirm %s is gated (%v)\n", user, perr)
			case profile == "" && !o.allowUngated:
				return fmt.Errorf("KubeAmber's policy does not treat %s as an agent, so its changes would not be reviewed. "+
					"Add serviceAccounts: [%q] to an agent profile, or pass --allow-ungated", user, o.namespace+"/*")
			}

			if _, err := agentid.Ensure(ctx, cs, id); err != nil {
				return err
			}
			token, expires, err := agentid.Token(ctx, cs, o.namespace, name, o.duration)
			if err != nil {
				return err
			}
			defaultNS := "default"
			if len(o.namespaces) > 0 {
				defaultNS = o.namespaces[0]
			}
			data, err := agentid.Kubeconfig(cfg, args[0], defaultNS, token)
			if err != nil {
				return err
			}
			path := o.output
			if path == "" {
				path = args[0] + ".kubeconfig"
			}
			if err := writeKubeconfig(out, path, data); err != nil {
				return err
			}
			if path == "-" {
				return nil
			}

			access := "none bound here (it has what administrators granted service accounts in " + o.namespace + ")"
			switch {
			case o.role != "" && o.clusterWide:
				access = o.role + " across the cluster"
			case o.role != "":
				access = o.role + " in " + strings.Join(o.namespaces, ", ")
			}
			gate := fmt.Sprintf("reviewed as agent profile %q", profile)
			if profile == "" {
				gate = "NOT confirmed to be reviewed by KubeAmber"
			}
			w := func(k, v string) { fmt.Fprintf(out, "  %-11s %s\n", k, v) }
			fmt.Fprintf(out, "Agent identity ready for %s:\n", args[0])
			w("Identity", user)
			w("Owner", me+" (you cannot approve this agent's held changes)")
			w("Access", access)
			w("KubeAmber", gate)
			w("Expires", fmt.Sprintf("%s (in %s)", expires.UTC().Format(time.RFC3339), o.duration))
			w("Kubeconfig", path)
			w("Use it", "KUBECONFIG="+path+" <your agent>")
			w("Revoke", "kubeamber agent-kubeconfig "+args[0]+" --revoke")
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&o.namespace, "agents-namespace", "kubeamber-agents", "namespace for agent service accounts")
	f.StringVar(&o.role, "role", "", "ClusterRole to bind to the agent, e.g. edit or view (default: bind nothing)")
	f.StringSliceVar(&o.namespaces, "namespace", nil, "namespace to bind --role in (repeatable); the first is the kubeconfig's default")
	f.BoolVar(&o.clusterWide, "cluster-wide", false, "bind --role across the cluster")
	f.DurationVar(&o.duration, "duration", 8*time.Hour, "token lifetime, 10m to 24h")
	f.StringVarP(&o.output, "output", "o", "", "kubeconfig path, or - for stdout (default AGENT.kubeconfig)")
	f.StringVar(&o.policyFile, "policy", "", "policy file to check the identity against (default: the in-cluster policy)")
	f.StringVar(&o.policyConfigMap, "policy-configmap", "kubeamber-system/kubeamber-policy", "in-cluster policy ConfigMap, namespace/name")
	f.BoolVar(&o.allowUngated, "allow-ungated", false, "mint the identity even if KubeAmber's policy would not review it")
	f.BoolVar(&o.revoke, "revoke", false, "delete the agent's identity and invalidate all its tokens")
	f.BoolVar(&o.force, "force", false, "with --revoke, revoke an agent someone else owns")
	return cmd
}

// gatedAs reports the policy agent profile that would review user, a service
// account in agentsNS.
func gatedAs(ctx context.Context, cs kubernetes.Interface, file, configMap, user, agentsNS string) (string, error) {
	var data []byte
	if file != "" {
		var err error
		if data, err = os.ReadFile(file); err != nil {
			return "", err
		}
	} else {
		ns, name, ok := strings.Cut(configMap, "/")
		if !ok {
			return "", fmt.Errorf("--policy-configmap must be namespace/name")
		}
		cm, err := cs.CoreV1().ConfigMaps(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return "", err
		}
		data = []byte(cm.Data["policy.yaml"])
	}
	p, err := policy.Parse(data)
	if err != nil {
		return "", err
	}
	groups := []string{"system:serviceaccounts", "system:serviceaccounts:" + agentsNS, "system:authenticated"}
	if a := p.MatchAgent(user, groups); a != nil {
		return a.Name, nil
	}
	return "", nil
}

// writeKubeconfig writes the file readable only by its owner: it holds a credential.
func writeKubeconfig(out io.Writer, path string, data []byte) error {
	if path == "-" {
		_, err := out.Write(data)
		return err
	}
	return os.WriteFile(path, data, 0o600)
}
