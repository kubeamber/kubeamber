# Changelog

All notable changes are recorded here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow
[Semantic Versioning](https://semver.org/). Before 1.0, minor versions may
include breaking changes, which are called out.

## [Unreleased]

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
