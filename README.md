# KubeAmber

**Traffic lights for AI agents on Kubernetes.**

AI agents such as kagent, HolmesGPT, kubectl-ai and Claude Code now get write
access to clusters. RBAC can only say yes or no to a verb on a resource: it
cannot tell a routine rollout restart from scaling production to zero, and it
never asks a human. KubeAmber is a vendor-neutral admission webhook that scores
every change an agent makes and gives it a light:

| | Verdict | What happens |
|---|---|---|
| 🟢 green | `allow` | Routine change. Applied at once; the agent sees its risk score. |
| 🟡 amber | `hold` | Risky change. Waits for a human to approve it, then the agent retries once. |
| 🔴 red | `deny` | Dangerous change. Refused, with the reason and a hint to find a safer path. |

Every verdict carries a reason the agent can read and act on.

> Status: **alpha (v0.1.0)**. The `kubeamber.dev/v1alpha1` API may change between
> minor releases.

## How it works

```
 AI agent (any framework)
   │  kubectl / client-go, as its own identity
   ▼
 kube-apiserver ── RBAC: is this verb allowed at all?
   │
   │  matchConditions (CEL, generated from the policy):
   │  only agent identities are sent on; humans and controllers never are
   ▼
 kubeamber webhook ── risk score 0-100 + rules + freeze windows + SLO guards
   │
   ├─ ALLOW  change applied; kubectl prints the score as a warning
   ├─ HOLD   request denied for now, ChangeRequest cr-3fa9c2d1-x7k2q recorded
   │           human:  kubeamber approve cr-3fa9c2d1-x7k2q
   │           agent:  retries the identical change → allowed once
   └─ DENY   request denied with the reasons; agent is told not to retry
```

- **Agents only.** KubeAmber writes the webhook's `matchConditions` from the
  policy, so requests from people and controllers never reach it. This makes
  `failurePolicy: Fail` safe: if KubeAmber is down, only agents are blocked.
- **Explainable scoring.** The score is deterministic and every point has a
  sentence attached. There is no LLM in the decision path. See
  [docs/risk-model.md](docs/risk-model.md).
- **Two-person rule.** Agents cannot create, approve or delete change requests.
  The identity that asked for a change cannot approve it, and an approval is
  good for exactly one retry of exactly that change.
- **Self-protection.** Agents cannot modify KubeAmber's namespace, CRD, RBAC or
  change requests, whatever the policy says. A ValidatingAdmissionPolicy stops
  them changing admission webhooks, which Kubernetes never sends to webhooks.
  See the [threat model](docs/threat-model.md) for the limits.
- **Traceable.** Decisions are OpenTelemetry spans linked to the agent's own
  trace. They also appear in the Kubernetes audit log.

## What an agent sees

Score manifests offline with `kubeamber check`, the same engine the webhook runs.
This is the policy from [examples/policy.yaml](examples/policy.yaml):

```console
$ kubeamber check -f scale-to-zero.yaml --old live-checkout-prod.yaml --policy policy.yaml --agent sre-agent
HOLD   UPDATE Deployment prod/checkout  risk 75/100  (replicas 3 → 0)
          +10  modifies a resource
           +5  changes a workload
          +35  scales checkout from 3 to 0 replicas (takes it offline)
          +25  namespace "prod" is protected
         ->   risk 75/100 is at or above the hold threshold (30)

$ kubeamber check -f escalate-privileges.yaml --policy policy.yaml --agent sre-agent
DENY   CREATE ClusterRoleBinding sre-agent-is-admin  risk 60/100
           +5  creates a resource
          +45  changes cluster-wide permissions (possible privilege escalation)
          +10  cluster-scoped resource (affects every namespace)
         ->   agents may not change RBAC, because that would let them grant themselves more power
```

In the cluster, the agent gets the same reasoning back from the API server,
plus what to do next (one line, wrapped here):

```console
$ kubectl scale deploy/checkout -n prod --replicas=0
Error from server (Forbidden): admission webhook "agents.kubeamber.dev" denied the request:
kubeamber: HOLD (risk 75/100, replicas 3 → 0): risk 75/100 is at or above the hold threshold (30).
Factors: +10 modifies a resource, +5 changes a workload, +35 scales checkout from 3 to 0 replicas
(takes it offline), +25 namespace "prod" is protected. Held for human approval as change request
cr-098251c5-p96t4 (expires in 1h0m0s). Ask an operator to run `kubeamber approve cr-098251c5-p96t4`,
then retry this exact change.

$ kubeamber approve cr-098251c5-p96t4 --reason "ok, investigating the crash loop"
approved cr-098251c5-p96t4: UPDATE Deployment prod/checkout requested by sre-agent (risk 75)
The agent can now retry the identical change once.

$ kubectl scale deploy/checkout -n prod --replicas=0
Warning: kubeamber: ALLOW (approved by kubernetes-admin in change request cr-098251c5-p96t4): ok, investigating the crash loop
deployment.apps/checkout scaled
```

## Quickstart

You need Docker, [kind](https://kind.sigs.k8s.io/), kubectl and Go 1.26.

```sh
git clone https://github.com/kubeamber/kubeamber && cd kubeamber
make kind-demo
```

This creates a kind cluster, builds and loads the image, deploys KubeAmber, and
plays an agent with `cluster-admin` through 27 scenarios
([hack/e2e.sh](hack/e2e.sh)): allow, hold, approve, retry, reject, deny, and
attempts to switch KubeAmber off. RBAC allows every one of them; KubeAmber
decides.

To install on an existing cluster (Kubernetes 1.30+, for `matchConditions`):

```sh
kubectl apply -k "https://github.com/kubeamber/kubeamber/deploy?ref=main"
```

Then edit the `kubeamber-policy` ConfigMap in `kubeamber-system`; see
[docs/policy.md](docs/policy.md). Policy changes are picked up within seconds,
with no restart.

Approvals happen from your own kubeconfig. RBAC decides who may approve: bind
the `kubeamber-approver` ClusterRole to your on-call group.

```sh
kubeamber list                                  # pending change requests
kubeamber approve cr-3fa9c2d1-x7k2q --reason "incident 4411, approved by on-call"
kubeamber reject  cr-3fa9c2d1-x7k2q --reason "scale to 1 instead"
```

## Using it with your agent

**In-cluster agents** (kagent, HolmesGPT, your own operator): run them under
service accounts in the `kubeamber-agents` namespace, or list their service
accounts in the policy:

```yaml
agents:
  - name: holmesgpt
    serviceAccounts: ["holmesgpt/holmesgpt-holmes-service-account"]
```

**Workstation agents** (Claude Code, kubectl-ai, Cursor): give the agent its
own kubeconfig whose client certificate or OIDC token carries the group
`kubeamber:agents`. KubeAmber cannot tell an agent apart from you if it uses your
credentials.

**Preview before acting (MCP).** `kubeamber mcp` serves three read-only tools over
stdio: `kubeamber_preview`, `kubeamber_policy` and `kubeamber_change_request`. With
them an agent can check a change before it makes it, and follow up on held
changes.

```sh
claude mcp add kubeamber -- kubeamber mcp --policy policy.yaml --agent workstation-agents
```

```json
{
  "mcpServers": {
    "kubeamber": {
      "command": "kubeamber",
      "args": ["mcp", "--policy", "/path/to/policy.yaml", "--agent", "workstation-agents"]
    }
  }
}
```

**Say why, and link the trace.** Agents can annotate the objects they change.
The intent is shown to approvers, and the traceparent becomes an OpenTelemetry
span link from KubeAmber's decision to the agent's reasoning trace.

```yaml
metadata:
  annotations:
    kubeamber.dev/intent: "checkout pods are crash-looping; stop them while I investigate"
    kubeamber.dev/traceparent: "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
```

More in [docs/integrations.md](docs/integrations.md).

## How it compares

KubeAmber complements these tools; it does not replace them. It runs alongside
Kyverno or Gatekeeper on the same cluster.

| | Knows who is an agent | Judges by risk, not just verb | Pulls in a human | Works with any agent framework |
|---|---|---|---|---|
| RBAC | no | no | no | yes |
| Kyverno, OPA Gatekeeper, ValidatingAdmissionPolicy | via rules you write | via rules you write | no (approval happens outside, e.g. GitOps review) | yes |
| Agent-framework approval gates (e.g. kagent) | yes | per tool call | yes | only that framework |
| L7 access gateways | yes | command patterns | yes | if traffic is routed through the gateway |
| **KubeAmber** | yes, from the policy | yes, scored and explained | yes, one-use approvals | yes, at the Kubernetes API |

Use RBAC to set the outer bounds of what an agent may touch. Use Kyverno or
Gatekeeper for organisation-wide rules that apply to everyone. Use KubeAmber to
decide which of an agent's allowed changes need a human.

## Documentation

- [Architecture](docs/architecture.md): request flow, approval protocol, failure modes
- [Policy reference](docs/policy.md)
- [Risk model](docs/risk-model.md): every factor and its points
- [Threat model](docs/threat-model.md): what KubeAmber does and does not protect against
- [Integrations](docs/integrations.md): kagent, HolmesGPT, kubectl-ai, MCP clients, GitOps, OpenTelemetry
- [Roadmap](ROADMAP.md)

## Community

KubeAmber aims to become a CNCF Sandbox project. Issues, design discussion and
pull requests are welcome; see [CONTRIBUTING.md](CONTRIBUTING.md). All commits
need a DCO sign-off. The project follows the
[CNCF Code of Conduct](CODE_OF_CONDUCT.md). Report security issues privately
as described in [SECURITY.md](SECURITY.md).

If you run KubeAmber, please add yourself to [ADOPTERS.md](ADOPTERS.md).

## License

Apache License 2.0. See [LICENSE](LICENSE).
