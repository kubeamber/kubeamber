package main

import (
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	"sigs.k8s.io/yaml"

	"github.com/kubeamber/kubeamber/internal/changerequest"
	"github.com/kubeamber/kubeamber/internal/changes"
	"github.com/kubeamber/kubeamber/internal/changes/servicenow"
	"github.com/kubeamber/kubeamber/internal/kube"
	"github.com/kubeamber/kubeamber/internal/version"
)

// bridgeConfig is the file `kubeamber bridge --config` reads.
type bridgeConfig struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Metadata   struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Spec struct {
		// Interval between reconciliations. Default 15s.
		Interval metav1.Duration `json:"interval,omitempty"`
		// ApprovalTTL should match the policy's approvals.ttl. Default 15m.
		ApprovalTTL metav1.Duration    `json:"approvalTTL,omitempty"`
		ServiceNow  *servicenow.Config `json:"servicenow,omitempty"`
	} `json:"spec"`
}

func loadBridgeConfig(path string) (*bridgeConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c bridgeConfig
	if err := yaml.UnmarshalStrict(data, &c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if c.APIVersion != "kubeamber.dev/v1alpha1" || c.Kind != "BridgeConfig" {
		return nil, fmt.Errorf("%s: expected apiVersion kubeamber.dev/v1alpha1 and kind BridgeConfig", path)
	}
	if c.Spec.ServiceNow == nil {
		return nil, fmt.Errorf("%s: spec.servicenow is required (the only provider so far)", path)
	}
	if c.Spec.Interval.Duration == 0 {
		c.Spec.Interval.Duration = 15 * time.Second
	}
	if c.Spec.ApprovalTTL.Duration == 0 {
		c.Spec.ApprovalTTL.Duration = 15 * time.Minute
	}
	return &c, nil
}

func bridgeCmd(kf *kube.Flags) *cobra.Command {
	var configPath string
	cmd := &cobra.Command{
		Use:   "bridge --config FILE",
		Short: "Sync held changes with a change-management system (ServiceNow)",
		Long: `Open a change ticket for every held agent change, record the decision made
on it, and close the ticket once the change is applied or abandoned.
Run it as its own Deployment under the kubeamber-bridge service account; the
webhook only accepts external decisions from that identity.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
			cfg, err := loadBridgeConfig(configPath)
			if err != nil {
				return err
			}
			provider, err := servicenow.New(*cfg.Spec.ServiceNow)
			if err != nil {
				return err
			}
			rc, err := kf.Config()
			if err != nil {
				return err
			}
			dyn, err := dynamic.NewForConfig(rc)
			if err != nil {
				return err
			}
			b := &changes.Bridge{
				Provider:    provider,
				Store:       &changerequest.Store{Client: dyn},
				ApprovalTTL: cfg.Spec.ApprovalTTL.Duration,
				Log:         log,
			}
			log.Info("kubeamber bridge started", "version", version.Version, "provider", provider.Name(),
				"instance", cfg.Spec.ServiceNow.InstanceURL, "interval", cfg.Spec.Interval.Duration.String())
			b.Run(cmd.Context(), cfg.Spec.Interval.Duration)
			return nil
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "/etc/kubeamber/bridge.yaml", "bridge configuration file")
	return cmd
}
