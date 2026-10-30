# KubeAmber Helm chart

Installs [KubeAmber](https://github.com/kubeamber/kubeamber): traffic lights
for AI agents on Kubernetes.

```sh
helm install kubeamber oci://ghcr.io/kubeamber/charts/kubeamber --version 0.3.1 \
  --namespace kubeamber-system --create-namespace --wait
```

KubeAmber is **one per cluster**: its webhook configuration, admission policy
and CRD are cluster-wide. Kubernetes 1.30 or later.

After installing, the chart's notes show how to check that KubeAmber is
reviewing agents, who may approve held changes, and how to give an agent its
own identity. The [installation guide](https://github.com/kubeamber/kubeamber/blob/main/docs/install.md)
covers the policy, upgrades, uninstalling, Argo CD and Flux, and
troubleshooting.

## What it installs

The webhook is a Deployment: two replicas by default, spread across nodes,
with a disruption budget. The API server calls it through a Service, so more
replicas add availability, not coverage. It does not need to run on every
node, so the chart does not offer a DaemonSet.

| Resource | Name |
|---|---|
| Webhook Deployment, Service, PodDisruptionBudget | `kubeamber` |
| ServiceMonitor (`metrics.serviceMonitor.enabled`) | `kubeamber` |
| PrometheusRule with alerts (`metrics.prometheusRule.enabled`) | `kubeamber` |
| Grafana dashboard ConfigMap (`metrics.grafanaDashboard.enabled`) | `kubeamber-grafana-dashboard` |
| ValidatingWebhookConfiguration | `kubeamber` |
| ValidatingAdmissionPolicy and binding (`admissionPolicy.enabled`) | `kubeamber-protect-admission` |
| ChangeRequest CRD (`crds.install`), kept on uninstall | `changerequests.kubeamber.dev` |
| Policy ConfigMap | `kubeamber-policy` |
| ClusterRoles for approvers and viewers | `kubeamber-approver`, `kubeamber-viewer` |
| Agents namespace (`agentsNamespace.create`), kept on uninstall | `kubeamber-agents` |
| ServiceNow bridge (`bridge.enabled`) | `kubeamber-bridge` |
| Notifier (`notify.enabled`) | `kubeamber-notify` |

## Values

| Key | Default | Description |
|---|---|---|
| `image.repository` | `ghcr.io/kubeamber/kubeamber` | Image for the webhook and the bridge. |
| `image.tag` | chart `appVersion` | |
| `webhook.replicas` | `2` | |
| `webhook.failurePolicy` | `Fail` | Fail closed: agents wait while KubeAmber is down. Humans and controllers never reach the webhook. |
| `webhook.timeoutSeconds` | `5` | |
| `webhook.env` | `[]` | Extra environment, e.g. `OTEL_EXPORTER_OTLP_ENDPOINT`. |
| `webhook.events` | `hold,deny,approved,rejected` | Decisions recorded as Kubernetes Events on the changed objects; `all` adds allows, empty turns Events off. |
| `webhook.tls.existingSecret` | `""` | Use this `kubernetes.io/tls` Secret instead of KubeAmber's self-managed certificate. |
| `webhook.tls.certManagerCertificate` | `""` | With `existingSecret`: `namespace/certificate` for cert-manager's CA injection. |
| `webhook.tls.caBundle` | `""` | With `existingSecret`: the CA (PEM), if not using cert-manager. |
| `metrics.enabled` / `.port` | `true` / `9090` | Serve Prometheus metrics on `/metrics` over plain HTTP ([metrics](https://github.com/kubeamber/kubeamber/blob/main/docs/integrations.md#prometheus-and-grafana-alloy)). |
| `metrics.scrapeAnnotations` | `true` | Add `prometheus.io/scrape`, `/port` and `/path` pod annotations. |
| `metrics.serviceMonitor.enabled` | `false` | Create a Prometheus Operator ServiceMonitor (needs its CRD). |
| `metrics.serviceMonitor.interval` / `.labels` | `30s` / `{}` | Scrape interval, and labels your Prometheus selects ServiceMonitors by. |
| `metrics.prometheusRule.enabled` | `false` | Create a PrometheusRule with KubeAmber's [alerts](https://github.com/kubeamber/kubeamber/blob/main/docs/integrations.md#alert-rules) (needs its CRD). |
| `metrics.prometheusRule.labels` / `.ruleLabels` / `.disabled` | `{}` / `{}` / `[]` | Labels your Prometheus selects rules by; labels added to every alert (they win over the rules' own, `severity` included); alert names to leave out. |
| `metrics.grafanaDashboard.enabled` | `false` | Create a ConfigMap with the [KubeAmber dashboard](https://github.com/kubeamber/kubeamber/blob/main/docs/integrations.md#grafana-dashboard) for Grafana's dashboard sidecar. |
| `metrics.grafanaDashboard.namespace` / `.labels` / `.annotations` | release namespace / `grafana_dashboard: "1"` / `{}` | Where the sidecar looks, the label it selects by, and e.g. `grafana_folder`. |
| `admissionPolicy.enabled` | `true` | Block agents from changing admission webhooks. |
| `agentsNamespace.create` / `.name` | `true` / `kubeamber-agents` | |
| `crds.install` | `true` | |
| `policy` | see `values.yaml` | The policy document ([reference](https://github.com/kubeamber/kubeamber/blob/main/docs/policy.md)). Reloaded without a restart. |
| `bridge.enabled` | `false` | Run the ServiceNow change-management bridge. |
| `bridge.credentialsSecret` | `kubeamber-servicenow` | Secret with `client-id`/`client-secret` or `username`/`password`. |
| `bridge.config` | see `values.yaml` | The bridge configuration ([guide](https://github.com/kubeamber/kubeamber/blob/main/docs/integrations.md#servicenow-change-management)). |
| `notify.enabled` | `false` | Run `kubeamber notify`: Slack, Teams or webhook messages about held changes and their outcomes. |
| `notify.secretName` | `kubeamber-notify` | Secret with the webhook URLs, mounted at `/etc/kubeamber/notify-secrets`. |
| `notify.config` | see `values.yaml` | Channels and routes ([guide](https://github.com/kubeamber/kubeamber/blob/main/docs/integrations.md#notifications-in-slack-teams-or-any-webhook)). |

Every key is documented in [`values.yaml`](values.yaml).

## Upgrades

Keep your settings in a values file and pass it to every upgrade:

```sh
helm upgrade kubeamber oci://ghcr.io/kubeamber/charts/kubeamber --version <new version> \
  --namespace kubeamber-system -f kubeamber-values.yaml
```

If you set values with `--set`, use `--reset-then-reuse-values` (Helm 3.14 or
later) to keep them. `--reuse-values` also keeps the previous version's
defaults, so settings a new version adds are missing.

KubeAmber writes two things into its webhook configuration at runtime: the
CA bundle it serves with, and the match condition that sends it only agent
traffic. The chart reads both back during `helm upgrade`, so an upgrade never
leaves agents unreviewed. KubeAmber also re-applies them every 30 seconds.
With Argo CD, which renders the chart without access to the cluster, see
[the installation guide](https://github.com/kubeamber/kubeamber/blob/main/docs/install.md#with-argo-cd-or-flux).

## Uninstalling

```sh
helm uninstall kubeamber --namespace kubeamber-system
```

The ChangeRequest CRD, with every change request, and the `kubeamber-agents`
namespace, with agents' service accounts, are kept. Delete them with
`kubectl delete crd changerequests.kubeamber.dev` and
`kubectl delete namespace kubeamber-agents`.

## Examples

Use cert-manager for the serving certificate:

```yaml
webhook:
  tls:
    existingSecret: kubeamber-tls
    certManagerCertificate: kubeamber-system/kubeamber
```

Connect held changes to ServiceNow:

```sh
kubectl -n kubeamber-system create secret generic kubeamber-servicenow \
  --from-literal=client-id=... --from-literal=client-secret=...
helm upgrade kubeamber oci://ghcr.io/kubeamber/charts/kubeamber --version 0.3.1 \
  -n kubeamber-system --reset-then-reuse-values \
  --set bridge.enabled=true \
  --set bridge.config.servicenow.instanceURL=https://example.service-now.com
```
