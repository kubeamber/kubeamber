# Changelog

All notable changes are recorded here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow
[Semantic Versioning](https://semver.org/). Before 1.0, minor versions may
include breaking changes, which are called out.

## [Unreleased]

### Added

- A kagent walkthrough ([examples/kagent](examples/kagent/README.md)) with
  screenshots of a kagent agent's changes being allowed, held, approved and
  denied, plus a KubeAmber-aware kagent Agent.
- Prometheus metrics on `/metrics` (port 9090, plain HTTP), on by default:
  `kubeamber_decisions_total` by agent and outcome, the `kubeamber_risk_score`
  histogram, and Go and process metrics. The chart adds scrape annotations and
  can create a ServiceMonitor (`metrics.serviceMonitor.enabled`).
  `--metrics-addr` changes the address, and an empty value turns it off.
- Decisions are recorded as Kubernetes Events on the object the agent tried to
  change, so they show up in `kubectl get events` and `kubectl describe`:
  `AgentChangeHeld` (with the approve and reject commands),
  `AgentChangeDenied`, `AgentChangeApproved` and `AgentChangeRejected`, and
  optionally `AgentChangeAllowed`. Choose them with `--events` or the chart's
  `webhook.events`. The webhook's ClusterRole can now create Events.

### Fixed

- The kagent integration guide matched `kagent/*`, which also reviewed the
  kagent controller. It now matches the tool server's service account,
  `kagent/kagent-tools`.
- Setting only `OTEL_EXPORTER_OTLP_METRICS_ENDPOINT` also started a trace
  exporter that sent to `localhost:4318` and failed. Traces and metrics now
  start only when their own endpoint, or the shared one, is set, so metrics can
  be pushed straight to Prometheus's OTLP receiver.

## [0.2.0] - 2026-10-21

The first published release: signed container images, CLI binaries and the
Helm chart. v0.1.0 was never tagged, so this release covers everything since
the project started.

### Added

- Validating admission webhook (`kubeamber webhook`) that reviews changes made
  by agent identities and passes everyone else through.
- Webhook `matchConditions` generated from the policy and kept in sync, so only
  agent requests reach the webhook.
- Deterministic risk scoring (0-100) covering operations, sensitive kinds,
  workload replica and image changes, host access, token requests, protected
  namespaces, cluster scope and a `kubeamber.dev/critical` label.
- Policy document `kubeamber.dev/v1alpha1` `Policy`: agents, thresholds,
  protected namespaces, first-match rules, weekly and one-off freeze windows,
  approvals and Prometheus guards; strict parsing and hot reload.
- Hold-and-retry approvals stored as `ChangeRequest` resources, bound to a
  change fingerprint and consumable once.
- Two-person rule enforced by the webhook; built-in self-protection of
  KubeAmber's namespace, admission configuration, CRD, RBAC and change
  requests, plus a ValidatingAdmissionPolicy that stops agents changing
  admission webhooks.
- `kubeamber agent-kubeconfig`: gives an agent its own service account, bound
  only the access its owner chooses (and never more than the owner holds),
  with a short-lived kubeconfig and a `--revoke` kill switch. The owner is
  recorded on every held change (`spec.owner`) and cannot approve those
  changes unless `approvals.allowOwnerApproval` is set.
- `kubeamber bridge`: connects held changes to ServiceNow Change Management
  ([proposal 0001](docs/proposals/0001-change-management.md)). It opens a
  ticket for each held change, records the ServiceNow approver, honours the
  ticket's planned window, and closes the ticket with an implementation note
  or cancels it. `approvals.requireExternal` makes the ticket the only place to
  approve. Webhook rules for the bridge identity (`--bridge-service-account`)
  allow one immutable ticket link, and decisions only for that ticket.
- `kubeamber check` for offline and CI evaluation (exit codes 0 / 2 / 3), with
  `--as` and a repeatable `--group`.
- `kubeamber list`, `kubeamber approve` and `kubeamber reject`, and a global
  `--as` flag to impersonate a user, like `kubectl --as`.
- `kubeamber mcp`: MCP server with `kubeamber_preview`, `kubeamber_policy` and
  `kubeamber_change_request`.
- OpenTelemetry traces (with links to the agent's trace through
  `kubeamber.dev/traceparent`), metrics and Kubernetes audit annotations.
- Self-managed TLS stored in a Secret, or a user-provided certificate.
- Helm chart (`charts/kubeamber`), published to
  `oci://ghcr.io/kubeamber/charts`, alongside the kustomize manifests in
  `deploy/`.
- Release workflow: multi-arch images on `ghcr.io/kubeamber/kubeamber` with
  SBOM and provenance, CLI binaries for Linux, macOS and Windows, all signed
  with Sigstore keyless signing ([docs/releasing.md](docs/releasing.md)).
- Project logo and artwork (`artwork/`).
- Integration guides for kagent, HolmesGPT, kubectl-ai, MCP clients, GitOps,
  OpenTelemetry, ServiceNow, and agents from any cloud provider.
- A kind end-to-end suite of 52 scenarios, run in CI against both the
  kustomize and the Helm install, and a simulated ServiceNow
  (`hack/fakesnow`).

### Security

- The webhook reviews `serviceaccounts/token` requests by agents (+45, held by
  default; denied for KubeAmber's own service accounts), so an agent cannot
  switch identity unseen, for example to the bridge's.
- Agents cannot create, change or delete service accounts that carry
  `kubeamber.dev/owner`.

### Fixed

- An approved change could be held again when the API server retried the
  write after a conflict, because the retry re-ran admission after the
  approval had been used. The identical change is now allowed for 30 seconds
  after its approval is used.
- `helm upgrade` or `kubectl apply -k` reset the webhook's agent match
  condition to its placeholder, leaving agents unreviewed until a restart.
  KubeAmber now re-applies it every 30 seconds (`--resync-interval`), and the
  chart keeps the live value during upgrades.

[Unreleased]: https://github.com/kubeamber/kubeamber/compare/v0.2.0...HEAD
[0.2.0]: https://github.com/kubeamber/kubeamber/releases/tag/v0.2.0
