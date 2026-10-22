package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/events"

	"github.com/kubeamber/kubeamber/internal/agentid"
	"github.com/kubeamber/kubeamber/internal/changerequest"
	"github.com/kubeamber/kubeamber/internal/guards"
	"github.com/kubeamber/kubeamber/internal/kube"
	"github.com/kubeamber/kubeamber/internal/telemetry"
	"github.com/kubeamber/kubeamber/internal/version"
	"github.com/kubeamber/kubeamber/internal/webhook"
	"github.com/kubeamber/kubeamber/pkg/decide"
	"github.com/kubeamber/kubeamber/pkg/policy"
)

func webhookCmd(kf *kube.Flags) *cobra.Command {
	var o struct {
		policy, addr, metricsAddr                 string
		certFile, keyFile                         string
		namespace, serviceAccount, service        string
		bridgeServiceAccount                      string
		webhookConfig, admissionPolicy, tlsSecret string
		events                                    string
		reload, resync                            time.Duration
	}
	cmd := &cobra.Command{
		Use:   "webhook",
		Short: "Run the admission webhook (inside the cluster)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
			slog.SetDefault(log)

			metrics, shutdown, err := telemetry.Setup(ctx, version.Version, o.metricsAddr != "")
			if err != nil {
				return fmt.Errorf("telemetry: %w", err)
			}
			defer func() { _ = shutdown(context.Background()) }()

			p, err := policy.Load(o.policy)
			if err != nil {
				return err
			}
			current := policy.NewCurrent(p)

			cfg, err := kf.Config()
			if err != nil {
				return err
			}
			cs, err := kubernetes.NewForConfig(cfg)
			if err != nil {
				return err
			}
			dyn, err := dynamic.NewForConfig(cfg)
			if err != nil {
				return err
			}
			selfUser := policy.ServiceAccountUsername(o.namespace + "/" + o.serviceAccount)
			if a := p.MatchAgent(selfUser, nil); a != nil {
				return fmt.Errorf("policy agent %q matches KubeAmber's own service account %s", a.Name, selfUser)
			}
			var bridgeUser string
			if o.bridgeServiceAccount != "" {
				bridgeUser = policy.ServiceAccountUsername(o.namespace + "/" + o.bridgeServiceAccount)
				if a := p.MatchAgent(bridgeUser, nil); a != nil {
					return fmt.Errorf("policy agent %q matches the bridge's service account %s", a.Name, bridgeUser)
				}
			}

			var tlsCfg *tls.Config
			var caPEM []byte
			if o.certFile != "" {
				cert, err := tls.LoadX509KeyPair(o.certFile, o.keyFile)
				if err != nil {
					return err
				}
				tlsCfg = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
			} else {
				certs := &webhook.Certs{Client: cs, Namespace: o.namespace, SecretName: o.tlsSecret, ServiceName: o.service}
				cert, ca, err := certs.Ensure(ctx)
				if err != nil {
					return err
				}
				caPEM = ca
				tlsCfg = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
			}

			configurator := &webhook.Configurator{Client: cs, Name: o.webhookConfig, AdmissionPolicy: o.admissionPolicy, SelfUser: selfUser}
			sync := func(p *policy.Policy) {
				changed, err := configurator.Sync(ctx, caPEM, p)
				switch {
				case err != nil:
					log.Error("syncing webhook configuration", "error", err)
				case changed:
					log.Info("webhook configuration updated", "agents", len(p.Spec.Agents))
				}
			}
			sync(p)
			go current.Watch(ctx, o.policy, o.reload, log, sync)
			// Re-apply the configuration periodically too: `helm upgrade` or
			// `kubectl apply -k` resets the CA bundle and the agent match
			// condition to their placeholders, which would leave agents
			// unreviewed until the next restart.
			go func() {
				t := time.NewTicker(o.resync)
				defer t.Stop()
				for {
					select {
					case <-ctx.Done():
						return
					case <-t.C:
						sync(current.Get())
					}
				}
			}()

			eventOutcomes, err := webhook.ParseEventOutcomes(o.events)
			if err != nil {
				return fmt.Errorf("--events: %w", err)
			}
			var recorder events.EventRecorder
			if len(eventOutcomes) > 0 {
				broadcaster := events.NewBroadcaster(&events.EventSinkImpl{Interface: cs.EventsV1()})
				broadcaster.StartRecordingToSink(ctx.Done())
				defer broadcaster.Shutdown()
				recorder = broadcaster.NewRecorder(scheme.Scheme, "kubeamber.dev/webhook")
			}

			engine := &decide.Engine{
				Policy: current.Get,
				Guard:  guards.NewPrometheus(),
				Self:   decide.Self{Namespace: o.namespace, WebhookName: o.webhookConfig},
			}
			h := webhook.NewHandler(webhook.Handler{
				Engine: engine, Store: &changerequest.Store{Client: dyn}, Policy: current.Get,
				SelfUser: selfUser, BridgeUser: bridgeUser, Owners: &agentid.Owners{Client: cs}, Log: log,
				Events: recorder, EventOutcomes: eventOutcomes,
			})

			mux := http.NewServeMux()
			mux.Handle("/validate", h)
			mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
			srv := &http.Server{Addr: o.addr, Handler: mux, TLSConfig: tlsCfg, ReadHeaderTimeout: 10 * time.Second}
			var metricsSrv *http.Server
			if metrics != nil {
				metricsMux := http.NewServeMux()
				metricsMux.Handle("/metrics", metrics)
				metricsSrv = &http.Server{Addr: o.metricsAddr, Handler: metricsMux, ReadHeaderTimeout: 10 * time.Second}
				go func() {
					// Metrics are best effort: a problem serving them must not stop
					// admission reviews.
					if err := metricsSrv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
						log.Error("serving metrics", "addr", o.metricsAddr, "error", err)
					}
				}()
			}
			go func() {
				<-ctx.Done()
				sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				_ = srv.Shutdown(sctx)
				if metricsSrv != nil {
					_ = metricsSrv.Shutdown(sctx)
				}
			}()
			log.Info("kubeamber webhook listening", "addr", o.addr, "metrics", o.metricsAddr, "version", version.Version,
				"policy", p.Metadata.Name, "agents", len(p.Spec.Agents), "telemetry", telemetry.Enabled())
			if err := srv.ListenAndServeTLS("", ""); !errors.Is(err, http.ErrServerClosed) {
				return err
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&o.policy, "policy", "/etc/kubeamber/policy.yaml", "policy file")
	f.StringVar(&o.addr, "addr", ":9443", "HTTPS listen address")
	f.StringVar(&o.metricsAddr, "metrics-addr", ":9090", "address to serve Prometheus metrics on /metrics, over plain HTTP (empty to disable)")
	f.StringVar(&o.certFile, "tls-cert-file", "", "serving certificate (default: self-managed in a Secret)")
	f.StringVar(&o.keyFile, "tls-key-file", "", "serving key")
	f.StringVar(&o.namespace, "namespace", envOr("POD_NAMESPACE", "kubeamber-system"), "namespace KubeAmber runs in")
	f.StringVar(&o.serviceAccount, "service-account", envOr("SERVICE_ACCOUNT", "kubeamber"), "KubeAmber's service account")
	f.StringVar(&o.service, "service", "kubeamber", "webhook Service name")
	f.StringVar(&o.bridgeServiceAccount, "bridge-service-account", "kubeamber-bridge", "service account of `kubeamber bridge`, allowed to record decisions from a change-management system (empty to disable)")
	f.StringVar(&o.webhookConfig, "webhook-config", "kubeamber", "ValidatingWebhookConfiguration name")
	f.StringVar(&o.admissionPolicy, "admission-policy", "kubeamber-protect-admission", "ValidatingAdmissionPolicy that blocks agents from changing admission webhooks (empty to skip)")
	f.StringVar(&o.tlsSecret, "tls-secret", "kubeamber-webhook-tls", "Secret holding the self-managed certificate")
	f.StringVar(&o.events, "events", strings.Join(webhook.EventOutcomes, ","), "decisions to record as Kubernetes Events on the changed object: a comma-separated list of allow, hold, deny, approved, rejected, or all, or none")
	f.DurationVar(&o.reload, "reload-interval", 5*time.Second, "how often to check the policy file for changes")
	f.DurationVar(&o.resync, "resync-interval", 30*time.Second, "how often to re-apply the webhook configuration (CA bundle and agent match condition)")
	return cmd
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
