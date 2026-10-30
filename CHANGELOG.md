# Changelog

All notable changes are recorded here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow
[Semantic Versioning](https://semver.org/). Before 1.0, minor versions may
include breaking changes, which are called out.

## [Unreleased]

### Changed

- [examples/chainguard](examples/chainguard/README.md) compares KubeAmber's
  image across v0.2.0, v0.3.0 and v0.3.1 (OS packages, layers, size, known
  vulnerabilities) and shows how to reproduce it with syft, grype and
  govulncheck.

## [0.3.1] - 2026-10-30

### Changed

- [examples/chainguard](examples/chainguard/README.md) also requires
  KubeAmber's own image to be signed by its release workflow, which works
  with Sigstore policy-controller from v0.3.0 on.
- CI runs `govulncheck` (`make vulncheck`) on every pull request and push, and
  weekly, and fails when a known Go vulnerability is reachable from
  KubeAmber's code.

### Security

- `golang.org/x/net` is updated from v0.59.0 to v0.62.0. v0.3.0 and earlier
  reach five vulnerabilities in its HTTP/2 code, fixed in v0.60.0:
  GO-2026-6612 (critical), GO-2026-6603, GO-2026-6610, GO-2026-6611 and
  GO-2026-6617 (high). Two more in its HTML parser (GO-2026-6860,
  GO-2026-6866) were present but not reachable.
- The CLI binaries of v0.3.0 and earlier were built with Go 1.26.0, whose
  standard library has vulnerabilities KubeAmber reaches (for example
  GO-2026-6617 in `net/http`, GO-2026-6809 in `crypto/x509`). `go.mod` now
  sets `toolchain go1.26.10`, which CI and the release workflow use. The
  container images were already built with Go 1.26.10.

## [0.3.0] - 2026-10-30

### Added

- Image provenance in the risk score, opt-in with the policy's `spec.images`:
  an image a change adds from a repository outside `trusted` scores +30, and
  with `requireDigest` one not pinned by digest scores +10, so an agent that
  swaps in an unknown or movable image needs a human. Scores don't change
  unless `spec.images` is set. `kubeamber check` and MCP previews apply it too,
  and the MCP `kubeamber_policy` tool lists the trusted repositories. See
  [docs/policy.md](docs/policy.md#specimages).
- A Chainguard example ([examples/chainguard](examples/chainguard/README.md)):
  agents may deploy only Chainguard images pinned by digest (`spec.images`),
  Sigstore policy-controller verifies Chainguard's signatures for everyone, how
  to verify KubeAmber's own base and release images, and how to build KubeAmber
  from source into a minimal Wolfi image with melange and apko. No Chainguard
  account needed.
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
- `kubeamber notify`: messages in Slack, Microsoft Teams or any webhook when
  an agent's change is held, a reminder if nobody decides, and again when it
  is approved, rejected, applied or expires. Messages give the risk, the
  factors and the commands to approve or reject. Routes send messages by
  event, risk, agent and namespace. Generic webhooks can be signed with
  HMAC-SHA256. The notifier only reads ChangeRequests, keeps what it has sent
  in a ConfigMap, and is enabled with the chart's `notify.enabled` or
  `deploy/notify`.
- Architecture diagrams in the README and `docs/architecture.md`.
- A website at [kubeamber.github.io/kubeamber](https://kubeamber.github.io/kubeamber/),
  built from `docs/` and the README with MkDocs Material and published by the
  `website` workflow.

### Changed

- The container image is built on Chainguard's `cgr.dev/chainguard/static`
  (Wolfi-based, signed by Chainguard) instead of `gcr.io/distroless/static`.
  It still runs as the non-root user 65532. The image for local kind runs uses
  the same base and digest; it had fallen behind the release image.
  [docs/releasing.md](docs/releasing.md) shows how to verify the base image.
- The logo artwork kit and its generator are no longer kept in the repository;
  the README logo and chart icon are in `docs/images/`.

### Fixed

- Admission verifiers couldn't verify KubeAmber's image or chart: cosign 3
  signs in its new bundle format, stored as an OCI 1.1 referrer, and Sigstore
  policy-controller (up to v0.15.1) reports `no signatures found` for it.
  Releases are now also signed in the legacy `.sig` format, which
  policy-controller reads; `cosign verify` accepts either.
- The kagent integration guide matched `kagent/*`, which also reviewed the
  kagent controller. It now matches the tool server's service account,
  `kagent/kagent-tools`.
- Setting only `OTEL_EXPORTER_OTLP_METRICS_ENDPOINT` also started a trace
  exporter that sent to `localhost:4318` and failed. Traces and metrics now
  start only when their own endpoint, or the shared one, is set, so metrics can
  be pushed straight to Prometheus's OTLP receiver.
- After a certificate renewal, webhook replicas wrote different CA bundles
  into the webhook configuration, so about half of agents' requests and
  approvals failed until every pod restarted
  ([#16](https://github.com/kubeamber/kubeamber/issues/16)). A renewal now
  keeps the CA, the CA key is stored in the Secret, and each replica reloads
  the certificate and publishes the Secret's CA every resync interval. When the
  CA must change, the old one stays in the bundle until the next renewal.
  Certificates from `--tls-cert-file` are reloaded too. Replicas updating the
  webhook configuration at the same time no longer log an error.

### Security

- Agents' subresource requests now reach KubeAmber. Before, an agent could add
  a debug container to any pod (`kubectl debug`), approve a certificate
  signing request for another identity, or proxy into pods, services and node
  kubelets without review, because the webhook rule `"*"` does not match
  subresources. The rules now match `*/*`, and these requests are scored:
  ephemeral containers +35, CSR creation +25 and approval +45, `nodes/proxy`
  +40 on top of the session, namespace finalizers +40
  ([GHSA-9fh4-4cw7-36cw](https://github.com/kubeamber/kubeamber/security/advisories/GHSA-9fh4-4cw7-36cw)).
- Agents could repoint the `kubeamber.dev` API group at a server of their
  own by changing its APIService (`v1alpha1.kubeamber.dev`, which the API
  server creates for the CRD). That server would then answer KubeAmber's reads
  of ChangeRequests, approvals included. Self-protection now denies agents any
  change to an APIService for the group, and other APIService changes score
  +50, because they can feed the cluster false data
  ([GHSA-6jmw-7398-fmpp](https://github.com/kubeamber/kubeamber/security/advisories/GHSA-6jmw-7398-fmpp)).

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

[Unreleased]: https://github.com/kubeamber/kubeamber/compare/v0.3.1...HEAD
[0.3.1]: https://github.com/kubeamber/kubeamber/compare/v0.3.0...v0.3.1
[0.3.0]: https://github.com/kubeamber/kubeamber/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/kubeamber/kubeamber/releases/tag/v0.2.0
