# Changelog

All notable changes are recorded here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow
[Semantic Versioning](https://semver.org/). Before 1.0, minor versions may
include breaking changes, which are called out.

## [Unreleased]

### Added

- `kubeamber bridge`: connects held changes to ServiceNow Change Management
  ([proposal 0001](docs/proposals/0001-change-management.md)). It opens a
  ticket for each held change, records the ServiceNow approver, honours the
  ticket's planned window, and closes the ticket with an implementation note
  or cancels it. Install with `kubectl apply -k deploy/servicenow-bridge`.
- `approvals.requireExternal` policy option: only the change-management system
  can approve; local rejections still work.
- ChangeRequest API (additive): `spec.factors`, `spec.approval.source`,
  `spec.approval.externalRef` and `status.external`, plus a `Ticket` printer
  column. `kubeamber list` and the MCP `kubeamber_change_request` tool show the
  ticket.
- Webhook rules for the bridge identity (`--bridge-service-account`): one
  immutable ticket link, and decisions only for that ticket.
- `--as` global flag to impersonate a user, like `kubectl --as`.
- `kubeamber agent-kubeconfig`: gives an agent its own service account,
  bound only the access its owner chooses (and never more than the owner
  holds), with a short-lived kubeconfig and a `--revoke` kill switch. The
  agent is reviewed as an agent, its owner is recorded on every held change
  (`spec.owner`), and the owner cannot approve those changes unless
  `approvals.allowOwnerApproval` is set.
- Simulated ServiceNow (`hack/fakesnow`) and ten new end-to-end scenarios.
- `make docker-build-local`: small image from a host-compiled binary, used by
  the kind targets.

### Security

- Agents minting tokens for service accounts (`serviceaccounts/token`) were
  not reviewed, so an agent with RBAC to do so could switch identity, for
  example to the bridge's. The webhook now reviews token requests (+45, held
  by default; denied for KubeAmber's own service accounts).
- Agents cannot create, change or delete service accounts that carry
  `kubeamber.dev/owner`.

### Fixed

- An approved change could be held again when the API server retried the
  write after a conflict, because the retry re-ran admission after the
  approval had been used. The identical change is now allowed for 30 seconds
  after its approval is used.

### v0.1.0: initial release

- Validating admission webhook (`kubeamber webhook`) that reviews changes made by
  agent identities and passes everyone else through.
- Webhook `matchConditions` generated from the policy and kept in sync on
  reload, so only agent requests reach the webhook.
- Deterministic risk scoring (0-100) covering operations, sensitive kinds,
  workload replica and image changes, host access, protected namespaces,
  cluster scope and a `kubeamber.dev/critical` label.
- Policy document `kubeamber.dev/v1alpha1` `Policy`: agents, thresholds, protected
  namespaces, first-match rules, weekly and one-off freeze windows, approval
  TTLs and Prometheus guards; strict parsing and hot reload.
- Hold-and-retry approvals stored as `ChangeRequest` resources, bound to a
  change fingerprint and consumable once.
- Two-person rule enforced by the webhook; built-in self-protection of KubeAmber's
  namespace, webhook, CRD, RBAC and change requests.
- `kubeamber check` for offline and CI evaluation (exit codes 0 / 2 / 3).
- `kubeamber list`, `kubeamber approve` and `kubeamber reject`.
- `kubeamber mcp`: MCP server with `kubeamber_preview`, `kubeamber_policy` and
  `kubeamber_change_request`.
- OpenTelemetry traces (with links to the agent's trace through
  `kubeamber.dev/traceparent`), metrics and Kubernetes audit annotations.
- Self-managed TLS stored in a Secret, or a user-provided certificate.
