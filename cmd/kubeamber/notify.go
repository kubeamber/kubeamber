package main

import (
	"fmt"
	"log/slog"
	"os"
	"slices"
	"time"

	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/yaml"

	"github.com/kubeamber/kubeamber/internal/changerequest"
	"github.com/kubeamber/kubeamber/internal/kube"
	"github.com/kubeamber/kubeamber/internal/notify"
	"github.com/kubeamber/kubeamber/internal/version"
	"github.com/kubeamber/kubeamber/pkg/policy"
)

// notifyConfig is the file `kubeamber notify --config` reads.
type notifyConfig struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Metadata   struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Spec struct {
		// Cluster names this cluster in every message, for shared channels.
		Cluster string `json:"cluster,omitempty"`
		// Interval between passes. Default 15s.
		Interval metav1.Duration `json:"interval,omitempty"`
		// RemindAfter sends one reminder for a request still pending this
		// long. Default 15m; "0s" turns reminders off.
		RemindAfter *metav1.Duration `json:"remindAfter,omitempty"`
		Channels    []channelConfig  `json:"channels"`
		// Routes choose channels per message. Without routes, every channel
		// gets every message.
		Routes []notify.Route `json:"routes,omitempty"`
	} `json:"spec"`
}

type channelConfig struct {
	Name    string           `json:"name"`
	Slack   *urlConfig       `json:"slack,omitempty"`
	Teams   *urlConfig       `json:"teams,omitempty"`
	Webhook *webhookChConfig `json:"webhook,omitempty"`
}

type urlConfig struct {
	// URLFile holds the webhook URL, which is a secret: mount it from a Secret.
	URLFile string `json:"urlFile"`
}

type webhookChConfig struct {
	URLFile string `json:"urlFile"`
	// SecretFile, if set, holds a key used to sign each body (HMAC-SHA256,
	// in the X-KubeAmber-Signature header).
	SecretFile string `json:"secretFile,omitempty"`
}

func loadNotifyConfig(path string) (*notifyConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c notifyConfig
	if err := yaml.UnmarshalStrict(data, &c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if c.APIVersion != "kubeamber.dev/v1alpha1" || c.Kind != "NotifyConfig" {
		return nil, fmt.Errorf("%s: expected apiVersion kubeamber.dev/v1alpha1 and kind NotifyConfig", path)
	}
	if len(c.Spec.Channels) == 0 {
		return nil, fmt.Errorf("%s: spec.channels needs at least one channel", path)
	}
	var names []string
	for i, ch := range c.Spec.Channels {
		if ch.Name == "" || slices.Contains(names, ch.Name) {
			return nil, fmt.Errorf("%s: channels[%d]: name must be set and unique", path, i)
		}
		names = append(names, ch.Name)
		kinds := 0
		for _, set := range []bool{ch.Slack != nil, ch.Teams != nil, ch.Webhook != nil} {
			if set {
				kinds++
			}
		}
		if kinds != 1 {
			return nil, fmt.Errorf("%s: channels[%d] (%s): set exactly one of slack, teams or webhook", path, i, ch.Name)
		}
		if (ch.Slack != nil && ch.Slack.URLFile == "") || (ch.Teams != nil && ch.Teams.URLFile == "") || (ch.Webhook != nil && ch.Webhook.URLFile == "") {
			return nil, fmt.Errorf("%s: channels[%d] (%s): urlFile is required", path, i, ch.Name)
		}
	}
	if err := notify.ValidateRoutes(c.Spec.Routes, names); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if c.Spec.Interval.Duration == 0 {
		c.Spec.Interval.Duration = 15 * time.Second
	}
	if c.Spec.RemindAfter == nil {
		c.Spec.RemindAfter = &metav1.Duration{Duration: 15 * time.Minute}
	}
	return &c, nil
}

func (c *notifyConfig) channels() []notify.Channel {
	ua := "kubeamber/" + version.Version
	var out []notify.Channel
	for _, ch := range c.Spec.Channels {
		switch {
		case ch.Slack != nil:
			out = append(out, notify.NewSlack(ch.Name, ch.Slack.URLFile, nil, ua))
		case ch.Teams != nil:
			out = append(out, notify.NewTeams(ch.Name, ch.Teams.URLFile, nil, ua))
		case ch.Webhook != nil:
			out = append(out, notify.NewWebhook(ch.Name, ch.Webhook.URLFile, ch.Webhook.SecretFile, nil, ua))
		}
	}
	return out
}

func notifyCmd(kf *kube.Flags) *cobra.Command {
	var configPath, policyPath, namespace, stateName string
	cmd := &cobra.Command{
		Use:   "notify --config FILE",
		Short: "Tell people in Slack, Microsoft Teams or a webhook about held changes and what became of them",
		Long: `Send a message when an agent's change is held, again as a reminder if it
waits too long, and when it is approved, rejected, applied or expires. The
messages give the risk, the factors and the commands to approve or reject.
Run it as one replica under its own service account; it only reads
ChangeRequests, and keeps what it has sent in a ConfigMap.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
			cfg, err := loadNotifyConfig(configPath)
			if err != nil {
				return err
			}
			p, err := policy.Load(policyPath)
			if err != nil {
				return fmt.Errorf("reading the policy for its approval TTL: %w", err)
			}
			current := policy.NewCurrent(p)
			go current.Watch(ctx, policyPath, 5*time.Second, log, nil)

			rc, err := kf.Config()
			if err != nil {
				return err
			}
			dyn, err := dynamic.NewForConfig(rc)
			if err != nil {
				return err
			}
			cs, err := kubernetes.NewForConfig(rc)
			if err != nil {
				return err
			}
			n := &notify.Notifier{
				Store:       &changerequest.Store{Client: dyn},
				State:       &notify.ConfigMapState{Client: cs, Namespace: namespace, Name: stateName},
				Channels:    cfg.channels(),
				Routes:      cfg.Spec.Routes,
				Cluster:     cfg.Spec.Cluster,
				RemindAfter: cfg.Spec.RemindAfter.Duration,
				ApprovalTTL: func() time.Duration { return current.Get().Spec.Approvals.TTL.Duration },
				Log:         log,
			}
			log.Info("kubeamber notify started", "version", version.Version, "channels", len(n.Channels),
				"routes", len(n.Routes), "interval", cfg.Spec.Interval.Duration.String(), "state", namespace+"/"+stateName)
			n.Run(ctx, cfg.Spec.Interval.Duration)
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&configPath, "config", "/etc/kubeamber/notify/notify.yaml", "notifier configuration file")
	f.StringVar(&policyPath, "policy", "/etc/kubeamber/policy.yaml", "KubeAmber policy file, read for approvals.ttl")
	f.StringVar(&namespace, "namespace", envOr("POD_NAMESPACE", "kubeamber-system"), "namespace of the state ConfigMap")
	f.StringVar(&stateName, "state-configmap", "kubeamber-notify-state", "ConfigMap that records what has been sent")
	return cmd
}
