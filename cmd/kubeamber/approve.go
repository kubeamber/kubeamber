package main

import (
	"context"
	"fmt"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	authnv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/duration"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	"github.com/kubeamber/kubeamber/internal/changerequest"
	"github.com/kubeamber/kubeamber/internal/kube"
)

func store(kf *kube.Flags) (*changerequest.Store, *kubernetes.Clientset, error) {
	cfg, err := kf.Config()
	if err != nil {
		return nil, nil, err
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, nil, err
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, nil, err
	}
	return &changerequest.Store{Client: dyn}, cs, nil
}

func listCmd(kf *kube.Flags) *cobra.Command {
	var all bool
	var approvalTTL time.Duration
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List change requests (pending ones by default)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, _, err := store(kf)
			if err != nil {
				return err
			}
			crs, err := s.List(cmd.Context())
			if err != nil {
				return err
			}
			now := time.Now()
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "NAME\tPHASE\tAGENT\tCHANGE\tRISK\tAGE\tSUMMARY")
			shown := 0
			for _, cr := range crs {
				phase := cr.Effective(now, approvalTTL)
				if !all && phase != changerequest.Pending {
					continue
				}
				shown++
				t := cr.Spec.Target
				target := t.Kind + " " + t.Name
				if t.Namespace != "" {
					target = t.Kind + " " + t.Namespace + "/" + t.Name
				}
				summary := cr.Spec.Summary
				if cr.Spec.Intent != "" {
					summary = strings.TrimSpace(summary + ` intent: "` + cr.Spec.Intent + `"`)
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s %s\t%d\t%s\t%s\n", cr.Name, phase, cr.Spec.Agent, cr.Spec.Operation, target,
					cr.Spec.Score, duration.HumanDuration(now.Sub(cr.CreationTimestamp.Time)), summary)
			}
			if err := tw.Flush(); err != nil {
				return err
			}
			if shown == 0 && !all {
				fmt.Fprintln(cmd.OutOrStdout(), "No pending change requests.")
			}
			return nil
		},
	}
	cmd.Flags().BoolVarP(&all, "all", "A", false, "include decided, consumed and expired requests")
	cmd.Flags().DurationVar(&approvalTTL, "approval-ttl", 15*time.Minute, "approval TTL from the policy, used to show expired approvals")
	return cmd
}

func decideCmd(kf *kube.Flags, verb string) *cobra.Command {
	var reason string
	decision := changerequest.DecisionApproved
	if verb == "reject" {
		decision = changerequest.DecisionRejected
	}
	cmd := &cobra.Command{
		Use:   verb + " NAME",
		Short: strings.ToUpper(verb[:1]) + verb[1:] + " a held change",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, cs, err := store(kf)
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			cr, err := s.Get(ctx, args[0])
			if err != nil {
				return err
			}
			me, err := whoami(ctx, cs)
			if err != nil {
				return fmt.Errorf("finding your identity: %w", err)
			}
			if reason == "" {
				reason = verb + "d with kubeamber " + verb
			}
			if err := s.Decide(ctx, cr.Name, decision, me, reason, time.Now()); err != nil {
				return err
			}
			t := cr.Spec.Target
			fmt.Fprintf(cmd.OutOrStdout(), "%s %s: %s %s %s/%s requested by %s (risk %d)\n",
				strings.ToLower(string(decision)), cr.Name, cr.Spec.Operation, t.Kind, t.Namespace, t.Name, cr.Spec.Agent, cr.Spec.Score)
			if decision == changerequest.DecisionApproved {
				fmt.Fprintln(cmd.OutOrStdout(), "The agent can now retry the identical change once.")
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&reason, "reason", "", "why you made this decision (shown to the agent)")
	return cmd
}

func whoami(ctx context.Context, cs kubernetes.Interface) (string, error) {
	r, err := cs.AuthenticationV1().SelfSubjectReviews().Create(ctx, &authnv1.SelfSubjectReview{}, metav1.CreateOptions{})
	if err != nil {
		return "", err
	}
	return r.Status.UserInfo.Username, nil
}
