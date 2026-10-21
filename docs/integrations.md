# Integrations

KubeAmber works at the Kubernetes API, so it applies to any agent that changes
the cluster with its own identity, without code changes in the agent. The
integrations below make that identity explicit, and let agents preview
changes and explain themselves.

## kagent

kagent runs agents as Kubernetes resources, each with a service account.

1. Run the agents you want gated in the `kubeamber-agents` namespace, or add their service accounts to the policy:

   ```yaml
   agents:
     - name: kagent
       serviceAccounts: ["kagent/*"]
   ```

2. Keep kagent's own tool approval gates for tool calls; KubeAmber then judges
   the resulting cluster changes by risk. The two layers complement each other.

## HolmesGPT

HolmesGPT mostly reads, but write-capable toolsets and runbooks can change the
cluster. Match the service account its Helm chart creates (check with
`kubectl get sa -n <namespace>`):

```yaml
agents:
  - name: holmesgpt
    serviceAccounts: ["holmesgpt/holmesgpt-holmes-service-account"]
```

## kubectl-ai and other CLI agents

CLI agents use whatever kubeconfig they are given. Give them their own.

- **Client certificate**: issue a certificate with `O=kubeamber:agents`, which
  becomes the group, and a CN such as `agent:kubectl-ai`, which becomes the
  username.
- **OIDC**: have the identity provider add `kubeamber:agents` to the agent's
  groups claim.
- **Service account token**: `kubectl create token <sa> -n kubeamber-agents` and
  put the token in a dedicated kubeconfig.

Check what the API server sees:

```sh
kubectl --kubeconfig agent.kubeconfig auth whoami
```

## Claude Code and other MCP clients

`kubeamber mcp` serves three read-only tools over stdio:

| Tool | Purpose |
|---|---|
| `kubeamber_preview` | Score a manifest (CREATE, UPDATE, DELETE or CONNECT) and return the verdict, factors and advice. With cluster access it compares against the live object. |
| `kubeamber_policy` | Thresholds, protected namespaces, rules and freeze windows that apply to this agent. |
| `kubeamber_change_request` | Phase of a held change and what to do next. |

The server sends instructions telling the agent to preview first, set
`kubeamber.dev/intent` on held changes, and retry only after approval.

Claude Code:

```sh
claude mcp add kubeamber -- kubeamber mcp --policy /path/to/policy.yaml --agent workstation-agents
```

Any client using the common `mcpServers` format:

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

Use `--offline` to run without cluster access. `--kubeconfig` and `--context`
select the cluster. The MCP server only previews; enforcement still happens
in the webhook, so an agent that skips the preview is gated all the same.

## Intent and trace annotations

Agents can set two annotations on the objects they create or update:

| Annotation | Effect |
|---|---|
| `kubeamber.dev/intent` | Free text. Stored on the ChangeRequest, shown by `kubeamber list`, logged with the decision. It never changes the score. |
| `kubeamber.dev/traceparent` | W3C trace context of the agent's span. KubeAmber's `kubeamber.review` span links to it. |

Deletes carry no new object, so for a delete KubeAmber reads the annotations
from the existing object. An agent can annotate first, then delete.

## GitOps

When an agent opens a pull request instead of changing the cluster directly,
the change is applied later by the GitOps controller's identity, which
KubeAmber does not treat as an agent. Gate the pull request instead:

```yaml
# .github/workflows/kubeamber.yaml (sketch)
- name: KubeAmber check
  run: |
    kubectl kustomize overlays/prod | kubeamber check -f - --policy policy.yaml --agent sre-agent
```

`kubeamber check` exits with 0 if everything is allowed, 2 if anything would be
held, and 3 if anything would be denied. Treat 2 as "needs a human review" and
3 as "block". Pass `--old` with the live object to score updates accurately,
and `-o json` for machine-readable output.

## ServiceNow change management

`kubeamber bridge` connects held changes to ServiceNow Change Management, so
agent changes go through the same approval process (CAB, approval levels,
change windows) as everyone else's, and every ticket ends with evidence of what
was applied. The design is in
[proposal 0001](proposals/0001-change-management.md).

1. The agent's change is held. The bridge opens a normal (or emergency) change
   with the agent, its intent, the risk score and every factor, the CMDB item
   for the namespace, and a planned window. `correlation_id` is the
   ChangeRequest name, so a restarted bridge never opens a duplicate.
2. Approvers decide in ServiceNow. The agent's retries are told the ticket
   number and link.
3. When the ticket is approved and Scheduled, the bridge records the approver
   (`servicenow:<user_name>`). The agent's identical retry is then applied
   once, within the window.
4. The bridge writes the implementation note and moves the ticket to Review
   (`close_code=successful`), or cancels it if the change never happened.

Install it after KubeAmber, with credentials in a Secret:

```sh
kubectl -n kubeamber-system create secret generic kubeamber-servicenow \
  --from-literal=client-id=... --from-literal=client-secret=...
# edit deploy/servicenow-bridge/bridge.yaml: instanceURL, fields, configurationItems
kubectl apply -k deploy/servicenow-bridge
```

Then, if ServiceNow must be the only place approvals happen, set
`approvals.requireExternal: true` in the policy.

**ServiceNow side.** Use an OAuth client (client-credentials grant) or a
dedicated integration user. Give it the least privilege that can create and
update change requests and read approvals (for example `itil` plus
`sn_change_read`), not `admin`. The bridge calls
`/api/sn_chg_rest/change/{normal,emergency}`, `/api/now/table/change_request`,
`/api/now/table/sysapproval_approver` and `/oauth_token.do`, outbound from the
cluster; ServiceNow never needs to reach the cluster.

**What leaves the cluster.** The ticket gets the agent, the operation, the
target, the summary, the score and factors, the intent, the fingerprint and
the trace link. Object contents are never sent, so Secret values and
environment variables stay in the cluster.

**Who can approve.** Only the bridge's service account (`kubeamber-bridge`)
can record a decision from ServiceNow, and only for the ticket it linked. The
link is immutable. The webhook enforces this, so a compromised bridge
configuration cannot approve changes without a matching ticket.

**Try it without ServiceNow.** `make kind-demo` runs a simulated instance
(`go run ./hack/fakesnow`) and walks a change through ticket, CAB approval,
apply and close. To approve by hand:

```sh
curl -X POST 'http://127.0.0.1:18080/fake/changes/CHG0030001/approve?by=jane.cab'
```

For a real instance, a free Personal Developer Instance from
developer.servicenow.com works.

## OpenTelemetry and Grafana

Set the standard variables on the webhook Deployment:

```yaml
env:
  - name: OTEL_EXPORTER_OTLP_ENDPOINT
    value: http://otel-collector.observability:4318
  - name: OTEL_SERVICE_NAME
    value: kubeamber
```

Then:

- **Traces**: every agent request produces a `kubeamber.review` span. When the
  agent sets `kubeamber.dev/traceparent`, the span links to the agent's trace,
  so in Tempo or Jaeger you can go from the agent's reasoning to the cluster
  decision and back. Filter by `gen_ai.agent.name` and `kubeamber.outcome`.
- **Metrics**: `kubeamber.decisions` by outcome and agent, and the
  `kubeamber.risk.score` histogram. A rising hold rate means a noisy policy, an
  agent going off the rails, or both.
- **Audit**: `agents.kubeamber.dev/verdict`, `score`, `agent` and
  `change-request` audit annotations join KubeAmber decisions with Kubernetes
  audit events in Loki or any log store.
