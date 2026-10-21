# CNCF Sandbox application: draft answers

> **DRAFT.** Working answers for the CNCF Sandbox application form
> (https://github.com/cncf/sandbox). Not submitted. Update before submitting,
> and close the gaps listed at the end first.

## Project name

KubeAmber

## Project summary (one line)

A vendor-neutral admission layer that scores every change an AI agent makes
to a Kubernetes cluster, allowing low-risk changes, holding risky ones for
human approval and denying dangerous ones, with reasons the agent can act on.

## Problem statement

AI agents (kagent, HolmesGPT, kubectl-ai, Claude Code and many in-house
agents) are moving from reading clusters to changing them. Today an operator
has two choices: give the agent write RBAC and trust it, or give it none. RBAC
cannot tell a rollout restart from scaling production to zero, and it never
asks a human. Approval gates exist inside individual agent frameworks, but each
works only for that framework, and none works for an agent the platform team
did not build. There is no vendor-neutral, cluster-side control that judges
agent changes by risk and pulls in a human.

## What the project does

- Identifies agent identities from a declarative policy and routes only their
  requests to a validating webhook, using CEL `matchConditions` generated from
  that policy.
- Scores each change with a deterministic, explainable risk model (no LLM in
  the decision path), and applies rules, freeze windows and Prometheus SLO
  guards.
- Holds risky changes as `ChangeRequest` resources. A human approves them with
  `kubeamber approve`; the agent's identical retry is then allowed exactly once.
  The two-person rule is enforced by the webhook itself.
- Offers an MCP server so agents can preview a change before making it.
- Emits OpenTelemetry traces linked to the agent's own trace, metrics, and
  Kubernetes audit annotations.

## Why CNCF

- The problem is cloud-native and cross-vendor. Every distribution, managed
  Kubernetes service and agent framework needs the same control, and a neutral
  home makes it adoptable by competitors.
- It composes with existing CNCF projects: Kubernetes admission and CEL,
  OpenTelemetry, Prometheus, kagent and HolmesGPT (both CNCF Sandbox), and
  Kyverno and OPA Gatekeeper alongside it.
- A shared definition of "risky agent change" and an approval protocol are
  more useful as community standards than as one vendor's feature.

## Cloud-native fit

Kubernetes-native APIs (CRD `kubeamber.dev/v1alpha1` ChangeRequest), declarative
policy, stateless replicas, kubectl-style CLI, OpenTelemetry-native
observability, no external dependencies beyond the API server.

## Similar projects and differentiation

| Project | Relationship |
|---|---|
| Kyverno, OPA Gatekeeper, Kubewarden, ValidatingAdmissionPolicy | General policy engines. Each can express "deny X", but none has the notion of an agent, a risk score, or a built-in human approval and one-use retry protocol. KubeAmber runs alongside them. |
| kagent (CNCF Sandbox) | Agent framework with its own approval gates for its own tool calls. KubeAmber is framework-agnostic and works at the API server, so it also covers agents kagent did not launch. Natural integration target. |
| HolmesGPT (CNCF Sandbox) | Troubleshooting agent. A consumer of KubeAmber rather than a competitor: gating its write actions is a use case. |
| L7 access gateways (e.g. hoop.dev) | Sit in the data path and need traffic routed through them. KubeAmber needs no change to how agents connect. |
| MCP gateways | Gate tool calls at the protocol layer; they cannot see what a tool does to the cluster. KubeAmber judges the resulting API change. |

## Roadmap

See [ROADMAP.md](../ROADMAP.md). Highlights: Helm chart, AgentPolicy CRD,
approval notifications (Slack/Teams), signed releases with SBOMs,
namespace-scoped policies, native kagent and HolmesGPT integration, a
proposal to align agent-change telemetry with the OpenTelemetry GenAI
semantic conventions, and a conformance suite.

## License

Apache License 2.0.

## Code of conduct

CNCF Code of Conduct ([CODE_OF_CONDUCT.md](../CODE_OF_CONDUCT.md)).

## Maintainers

See [MAINTAINERS.md](../MAINTAINERS.md). Currently one maintainer (Sathpal
Singh, independent).

## Adopters

None yet. See [ADOPTERS.md](../ADOPTERS.md).

## Trademark and name

The name "KubeAmber" was chosen after checking for collisions with existing
open-source projects and companies in the cloud-native and AI-agent spaces.
Several earlier candidates were already in use. The `kubeamber.dev` domain
(also the API group) is registered to the maintainer and can be transferred
to the Linux Foundation on acceptance. Before submission: secure the
`kubeamber` GitHub organisation and confirm the name is clear for transfer.

## Additional information

KubeAmber is developed with AI coding assistance, disclosed and governed by
the policy in [CONTRIBUTING.md](../CONTRIBUTING.md#ai-assistance). The
maintainers review and own every change.

## Prerequisites still missing

- [ ] At least three maintainers from two or more organisations (the CNCF
  lifecycle page lists this for Sandbox entry).
- [ ] Production or pilot adopters willing to be listed.
- [ ] OpenSSF Best Practices badge (passing level) and OpenSSF Scorecard results.
- [ ] Security self-assessment (TAG Security template), building on [threat-model.md](threat-model.md).
- [ ] Public community meetings or office hours, with notes.
- [ ] Tagged, signed releases with SBOMs.
- [ ] Contributor ladder and a documented release process.
- [x] Domain `kubeamber.dev` registered.
- [ ] `kubeamber.dev` serving the docs (GitHub Pages, HTTPS enforced).
