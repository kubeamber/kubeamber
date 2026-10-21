package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/kubeamber/kubeamber/internal/manifest"
	"github.com/kubeamber/kubeamber/pkg/decide"
	"github.com/kubeamber/kubeamber/pkg/policy"
)

// Exit codes for `kubeamber check`, so CI can gate on the strictest verdict.
const (
	exitHold = 2
	exitDeny = 3
)

func checkCmd() *cobra.Command {
	var o struct {
		policy, file, old, as, agent, operation, namespace, subresource, output, at string
		groups                                                                      []string
	}
	cmd := &cobra.Command{
		Use:   "check -f MANIFEST --policy POLICY (--agent NAME | --as USER)",
		Short: "Score manifests offline, as an agent would be judged",
		Long: `Evaluate one or more manifests against a policy without a cluster.
Exit status is 0 if every change is allowed, 2 if any is held and 3 if any
is denied. Prometheus guards are not evaluated offline.`,
		Example: `  kubeamber check -f deploy.yaml --policy policy.yaml --agent sre-agent
  kubeamber check -f deploy.yaml --old live.yaml --policy policy.yaml --as system:serviceaccount:agents:holmes
  kubectl get deploy checkout -o yaml | kubeamber check -f - --operation DELETE --policy policy.yaml --agent sre-agent`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			p, err := policy.Load(o.policy)
			if err != nil {
				return err
			}
			if o.agent == "" && o.as == "" {
				return fmt.Errorf("set --agent or --as")
			}
			if o.agent != "" && p.Agent(o.agent) == nil {
				return fmt.Errorf("policy has no agent named %q", o.agent)
			}
			now := time.Now()
			if o.at != "" {
				if now, err = time.Parse(time.RFC3339, o.at); err != nil {
					return fmt.Errorf("--at: %w", err)
				}
			}
			objs, err := readManifests(cmd.InOrStdin(), o.file)
			if err != nil {
				return err
			}
			var old map[string]any
			if o.old != "" {
				olds, err := readManifests(cmd.InOrStdin(), o.old)
				if err != nil {
					return err
				}
				if len(olds) != 1 || len(objs) != 1 {
					return fmt.Errorf("--old needs exactly one object in each file")
				}
				old = olds[0]
			}

			engine := &decide.Engine{Policy: func() *policy.Policy { return p }}
			type result struct {
				Target    string          `json:"target"`
				Operation string          `json:"operation"`
				Decision  decide.Decision `json:"decision"`
			}
			var results []result
			worst := policy.Allow
			for _, obj := range objs {
				c, err := manifest.ToChange(obj, manifest.Options{
					Operation: o.operation, Namespace: o.namespace, SubResource: o.subresource, Old: old,
				})
				if err != nil {
					return err
				}
				d := engine.Evaluate(cmd.Context(), decide.Request{User: o.as, Groups: o.groups, Agent: o.agent, Change: c, Now: now})
				if !d.IsAgent() {
					return fmt.Errorf("%q is not an agent in policy %s", o.as, o.policy)
				}
				worst = policy.Stricter(worst, d.Verdict)
				results = append(results, result{c.Target(), c.Operation, d})
			}

			out := cmd.OutOrStdout()
			if o.output == "json" {
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				if err := enc.Encode(results); err != nil {
					return err
				}
			} else {
				for _, r := range results {
					printDecision(out, r.Operation, r.Target, r.Decision)
				}
			}
			switch worst {
			case policy.Deny:
				return exitError{exitDeny}
			case policy.Hold:
				return exitError{exitHold}
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&o.policy, "policy", "", "policy file (required)")
	f.StringVarP(&o.file, "filename", "f", "", "manifest file, or - for stdin (required)")
	f.StringVar(&o.old, "old", "", "current version of the object, to score an UPDATE")
	f.StringVar(&o.agent, "agent", "", "evaluate as this agent profile from the policy")
	f.StringVar(&o.as, "as", "", "evaluate as this Kubernetes username")
	f.StringSliceVar(&o.groups, "group", nil, "with --as, a group the user belongs to (repeatable), e.g. an Entra ID group object ID")
	f.StringVar(&o.operation, "operation", "", "CREATE, UPDATE, DELETE or CONNECT (default CREATE, or UPDATE with --old)")
	f.StringVarP(&o.namespace, "namespace", "n", "", "namespace for objects that do not set one")
	f.StringVar(&o.subresource, "subresource", "", "subresource, e.g. exec or scale")
	f.StringVarP(&o.output, "output", "o", "text", "text or json")
	f.StringVar(&o.at, "at", "", "evaluate freeze windows at this RFC3339 time instead of now")
	_ = cmd.MarkFlagRequired("policy")
	_ = cmd.MarkFlagRequired("filename")
	return cmd
}

func readManifests(stdin io.Reader, path string) ([]map[string]any, error) {
	var data []byte
	var err error
	if path == "-" {
		data, err = io.ReadAll(stdin)
	} else {
		data, err = os.ReadFile(path)
	}
	if err != nil {
		return nil, err
	}
	objs, err := manifest.Parse(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if len(objs) == 0 {
		return nil, fmt.Errorf("%s: no objects found", path)
	}
	return objs, nil
}

func printDecision(w io.Writer, op, target string, d decide.Decision) {
	fmt.Fprintf(w, "%-5s  %s %s  risk %d/100", strings.ToUpper(string(d.Verdict)), op, target, d.Score)
	if d.Summary != "" {
		fmt.Fprintf(w, "  (%s)", d.Summary)
	}
	fmt.Fprintln(w)
	for _, f := range d.Factors {
		fmt.Fprintf(w, "         %+4d  %s\n", f.Points, f.Reason)
	}
	for _, r := range d.Reasons {
		fmt.Fprintf(w, "         ->   %s\n", r)
	}
}
