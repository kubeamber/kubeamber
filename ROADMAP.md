# Roadmap

Priorities change with user feedback; open an issue to argue for or against
any item. Larger items get a design issue before implementation.

## v0.1: foundations (current)

- [x] Validating admission webhook for agent identities, with `matchConditions` generated from the policy
- [x] Deterministic, explainable risk scoring (0-100) with documented factors
- [x] Policy file: agents, thresholds, protected namespaces, rules, freeze windows, approvals, hot reload
- [x] Hold-and-retry approval protocol with `ChangeRequest` (`kubeamber.dev/v1alpha1`), one-use fingerprinted approvals
- [x] Two-person rule and self-protection enforced in the webhook
- [x] Prometheus guards (for example, SLO burn rate)
- [x] `kubeamber check` for offline and CI evaluation, with exit codes
- [x] `kubeamber list` / `approve` / `reject`
- [x] MCP server for agent-side previews
- [x] OpenTelemetry traces with links to agent traces, metrics, audit annotations
- [x] Self-managed webhook TLS, or bring your own certificate
- [x] Kustomize install and a kind end-to-end demo

## v0.2: production readiness

- Helm chart
- `AgentPolicy` CRD as an alternative to the ConfigMap file, with status conditions
- Prometheus `/metrics` endpoint alongside OTLP
- [x] ServiceNow change management through `kubeamber bridge` ([proposal 0001](docs/proposals/0001-change-management.md), phase 1)
- Approval notifications and one-click decisions in Slack and Microsoft Teams
- Change management phase 2: agents working under human-raised tickets, standard changes for allowed changes, emergency changes during incidents
- More change-management providers: Jira Service Management, BMC Helix, Freshservice
- Certificate rotation without restart
- Tamper detection: watch KubeAmber's webhook configuration and admission policy, restore them if an agent removes them, and emit a `kubeamber.tamper` event (admission policies are exempt from admission, so this is the remaining layer beyond RBAC)
- Signed releases, SBOMs and provenance (goreleaser, cosign, SLSA)
- OpenSSF Best Practices badge and Scorecard

## v0.3: depth

- Namespace-scoped policies owned by application teams, layered under the cluster policy
- Native integrations with kagent and HolmesGPT (agent identity, intent and trace context passed automatically)
- Change preview for GitOps: score the diff between a pull request and the live cluster
- Per-agent budgets and rate limits (for example, at most N held or allowed destructive changes per hour)
- Richer blast-radius signals (PodDisruptionBudgets, HPA bounds, service dependencies)

## Later

- A proposal to the OpenTelemetry GenAI semantic conventions for agent-change and approval telemetry
- A conformance suite, so agent frameworks can show they behave correctly under hold and deny
- Policy authoring and simulation against recorded audit logs
- Support for clusters older than Kubernetes 1.30 (no `matchConditions`)
