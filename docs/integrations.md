# Integrations

KubeAmber works at the Kubernetes API, so it applies to any agent that changes
the cluster with its own identity, without code changes in the agent. The
integrations below make that identity explicit, and let agents preview
changes and explain themselves.

## Agents from any provider

KubeAmber never asks which company or model is behind an agent. It recognises
agents by the Kubernetes identity they use. Every cloud maps its own identities
into Kubernetes, so the same policy covers agents from Azure, AWS, Google,
Anthropic, OpenAI or your own code.

**The universal method:**
1. Run `kubectl auth whoami` with the agent's credentials.
2. Put the username or a group it prints into `spec.agents`.
3. Confirm the match with `kubeamber check --as <username> --group <group>`.

| Where the agent runs | How it reaches the cluster | Match it in the policy with |
|---|---|---|
| **AKS** (Azure SRE Agent, agents on Azure AI Foundry, Copilot agents) | A managed identity or service principal, through Entra ID | `groups:` the object ID of an Entra group that holds your agents' identities (more robust than individual IDs) |
| **EKS** (Amazon Bedrock AgentCore, Amazon Q Developer, agents on Lambda or ECS) | An IAM role, through an EKS access entry | `users: ["arn:aws:sts::<account>:assumed-role/<role>/*"]`, or give the access entry `--kubernetes-groups kubeamber:agents` and match the group |
| **GKE** (Gemini CLI, kubectl-ai, agents on Vertex AI) | A Google service account | `users: ["<sa>@<project>.iam.gserviceaccount.com"]`, or a Google Group with Google Groups for RBAC |
| **A workstation** (Claude Code, Codex, Cursor, Copilot in VS Code) | Its own kubeconfig: a client certificate with `O=kubeamber:agents`, or an OIDC token whose groups include it | `groups: ["kubeamber:agents"]` |
| **Inside the cluster** (kagent, HolmesGPT, k8sgpt, your own operator) | A service account | `serviceAccounts: ["kubeamber-agents/*"]` |

A single group such as `kubeamber:agents`, assigned by each cloud's identity
mapping, keeps the policy the same across providers. Separate agent profiles
let you treat providers differently. For example, coding agents can stay out
of production while SRE agents may change it with approval:

```yaml
rules:
  - name: coding-agents-stay-out-of-prod
    match: {agents: [claude-code, gemini-cli], namespaces: ["prod*"]}
    action: deny
    reason: coding agents work in dev; production changes come from the SRE agents
```

The MCP server (`kubeamber mcp`) works with any MCP client, so agents built on
any provider's agent framework can preview changes the same way.

**Two limits apply whatever the provider.**
- **An agent that acts with a human's credentials is judged as that human.**
  This covers portal assistants that run commands as the signed-in user, and
  a coding agent using your personal kubeconfig. Give every agent its own
  identity; for agents you launch yourself, `kubeamber agent-kubeconfig` does
  it in one command (see [below](#give-each-agent-its-own-identity)).
- **Cloud control-plane calls never reach the Kubernetes API.** Examples are
  deleting a cluster, resizing a node pool, or changing IAM. Govern those with
  the cloud's own controls: Azure Policy, AWS service control policies,
  Google Organization Policy.

## kagent

kagent agents change the cluster through kagent's tool server, which runs
`kubectl` as the service account `kagent/kagent-tools`. Match that account:

```yaml
agents:
  - name: kagent
    serviceAccounts: ["kagent/kagent-tools"]
```

Don't match `kagent/*`: the kagent controller runs in the same namespace and
would then be reviewed as an agent when it manages its own resources.

kagent-tools v0.3.0 returns only `exit status 1` to the agent when a change is
refused, so the agent doesn't see KubeAmber's explanation. Until that changes,
tell the agent in its instructions how to find its ChangeRequest. The
[kagent example](../examples/kagent/README.md) has an Agent that does this, and
a full walkthrough with screenshots.

Keep kagent's own tool approval gates for tool calls; KubeAmber then judges the
resulting cluster changes by risk. The two layers complement each other.

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

## Give each agent its own identity

An agent that uses your kubeconfig is judged as you. `kubeamber
agent-kubeconfig` gives it an identity of its own in one step:

```sh
kubeamber agent-kubeconfig claude-code --role edit --namespace dev --namespace staging --duration 8h
```

This does five things:
1. Creates the service account `kubeamber-agents/claude-code-<you>`,
   annotated with you as its owner (`kubeamber.dev/owner`).
2. Binds the ClusterRole you name in the namespaces you name, or
   cluster-wide with `--cluster-wide`. The API server refuses to bind a role
   you do not hold, so an agent can never get more access than you have.
   Without `--role`, nothing is bound and the identity has whatever
   administrators granted service accounts in `kubeamber-agents`.
3. Checks the identity against KubeAmber's policy, and refuses (unless
   `--allow-ungated`) if KubeAmber would not review it as an agent.
4. Mints a bound token (10 minutes to 24 hours) and writes a kubeconfig,
   readable only by you.
5. From then on, KubeAmber records you as the owner of every change it holds
   for that agent, and refuses your own approval
   (`approvals.allowOwnerApproval` turns that off for one-person teams).

`kubeamber agent-kubeconfig claude-code --revoke` deletes the bindings and
the service account. The agent's access ends at once, and its tokens stop
authenticating within seconds.

Agents cannot work around this:
- Changing or forging the `kubeamber.dev/owner` annotation on a service
  account is denied.
- Minting a token for another service account is held, and denied for
  KubeAmber's own service accounts.

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

## Container image supply chain (Chainguard, Sigstore)

Agents that change workloads also choose images. The policy's
[`spec.images`](policy.md#specimages) holds an agent's change when it adds an
image from outside your trusted repositories, or one not pinned by digest.
Sigstore's policy-controller then checks, for everyone, that an image is
genuinely signed. [examples/chainguard](../examples/chainguard/README.md) puts
the two together with Chainguard's free images, step by step, with no
Chainguard account.

## Notifications in Slack, Teams or any webhook

`kubeamber notify` tells people about held changes where they already work.
Each message gives the change, the risk score and its factors, the agent's
identity and stated intent, and the commands to approve or reject it.

| Event | Sent when |
|---|---|
| `held` | An agent's change is waiting for a person. |
| `reminder` | It is still waiting after `remindAfter` (default 15 minutes; sent once). |
| `approved` / `rejected` | Someone decided, with who and why. |
| `applied` | The agent retried and the approved change was made. |
| `expired` | Nobody decided in time, or the approval was never used. Nothing was changed. |

### Set it up

1. Create the incoming webhooks: in Slack, an
   [incoming webhook](https://api.slack.com/messaging/webhooks); in Microsoft
   Teams, a Workflows flow that posts to a channel when a webhook request is
   received (the older Office 365 connectors are retired).
2. Keep the URLs in a Secret. They are credentials:

   ```sh
   kubectl -n kubeamber-system create secret generic kubeamber-notify \
     --from-literal=slack-url='https://hooks.slack.com/services/...' \
     --from-literal=teams-url='https://...'
   ```

3. Turn it on in the chart:

   ```yaml
   notify:
     enabled: true
     config:
       cluster: prod-eu-1        # shown in every message
       channels:
         - name: platform
           slack: {urlFile: /etc/kubeamber/notify-secrets/slack-url}
         - name: oncall
           teams: {urlFile: /etc/kubeamber/notify-secrets/teams-url}
       routes:
         - channels: [platform]  # everything
         - channels: [oncall]    # only risky holds in production
           events: [held, reminder]
           minRisk: 60
           namespaces: [prod, "prod-*"]
   ```

   With kustomize, edit [deploy/notify/notify.yaml](../deploy/notify/notify.yaml)
   and run `kubectl apply -k deploy/notify`.

**Routes** pick channels per message. A route matches when every field it sets
matches: `events`, `minRisk`, `agents` and `namespaces` (globs). A message goes
to the channels of every matching route, once each. Without routes, every
channel gets every message.

### Generic webhooks

A `webhook` channel posts the message as JSON (`event`, `title`, `cluster`,
`changeRequest`, `agent`, `user`, `owner`, `operation`, `kind`, `namespace`,
`name`, `risk`, `summary`, `factors`, `intent`, `expiresAt`, `decidedBy`,
`reason`, `source`, `ticket`, `ticketURL`), with an `X-KubeAmber-Event` header.
Use it for PagerDuty, Opsgenie, email gateways or your own automation. Give it
a `secretFile` and each body is signed: `X-KubeAmber-Signature: sha256=<hex>`
is the HMAC-SHA256 of the body with that key, so the receiver can check the
message really came from KubeAmber.

### How delivery works

- A message counts as sent only once the channel answers with a 2xx. A channel
  that fails is retried on the next pass (every `interval`, default 15s)
  without repeating the channels that succeeded.
- What has been sent is kept in the `kubeamber-notify-state` ConfigMap, so a
  restart repeats nothing. On its very first start the notifier does not replay
  history: decided requests are marked as told, and pending ones are announced.
- The approval TTL comes from the same policy file the webhook uses, so
  "expired" means the same thing in both.
- Run one replica (the chart and manifests use the `Recreate` strategy), or
  everything would be sent twice.
- Messages tell people how to decide; they don't decide. One-click approval
  from chat needs a safe way to map a chat user to a Kubernetes identity, and
  is planned.

### Security

- Webhook URLs are read from files on every send, so rotating the Secret needs
  no restart. They never appear in logs or errors.
- The agent writes its own intent, so that text is stripped of control
  characters and cut to 300 characters, and Slack markup is escaped: an agent
  cannot `@channel` people or disguise a link.
- Messages carry change details (object names, risk factors, intent). Send them
  to channels whose members may see that.
- The notifier's service account can only read ChangeRequests, and write its
  own state ConfigMap.

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

## Kubernetes Events

KubeAmber records its decisions as Kubernetes Events on the object the agent
tried to change. The team that owns an object sees what happened to it without
reading KubeAmber's logs:

```sh
kubectl -n prod get events --field-selector reason=AgentChangeHeld
kubectl -n prod describe deployment checkout
```

| Reason | Type | Recorded when |
|---|---|---|
| `AgentChangeHeld` | Warning | A change is held. The message names the ChangeRequest and gives the `kubeamber approve` and `kubeamber reject` commands. |
| `AgentChangeDenied` | Warning | A change is denied, with the reasons and risk factors. |
| `AgentChangeApproved` | Normal | An agent's retry is let through because its ChangeRequest was approved. |
| `AgentChangeRejected` | Warning | An agent's retry is refused because a person rejected its ChangeRequest. |
| `AgentChangeAllowed` | Normal | A change is allowed. Off by default, because routine changes would flood the event stream. |

- Events about objects that already exist also appear in `kubectl describe`.
  For a new object, look in `kubectl get events` in its namespace. Events about
  cluster-scoped objects go to the `default` namespace.
- The ChangeRequest is the Event's related object, and the reporting controller
  is `kubeamber.dev/webhook`.
- Dry runs never produce Events.
- Choose which decisions to record with `webhook.events` in the chart, or
  `--events` on the command line (for example `hold,deny`, `all`, or `none`).

Anything that already forwards Kubernetes Events can now carry KubeAmber's
decisions: Grafana Alloy's `loki.source.kubernetes_events`, the OpenTelemetry
Collector's `k8s_events` receiver, or an event exporter that posts to chat.

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
- **Metrics**: `kubeamber.decisions` by outcome and agent, the
  `kubeamber.risk.score` histogram, review latency, the wait for a person's
  decision, and change requests by phase. A rising hold rate means a noisy
  policy, an agent going off the rails, or both.
- **Audit**: `agents.kubeamber.dev/verdict`, `score`, `agent` and
  `change-request` audit annotations join KubeAmber decisions with Kubernetes
  audit events in Loki or any log store.

## Prometheus and Grafana Alloy

Each webhook replica serves its metrics in the Prometheus format on port
`9090`, path `/metrics`, over plain HTTP. This is on by default, with or without
OTLP. Turn it off with `metrics.enabled=false` in the chart, or
`--metrics-addr=` on the command line.

| Metric | Type | Labels | What it counts |
|---|---|---|---|
| `kubeamber_decisions_total` | counter | `gen_ai_agent_name`, `kubeamber_outcome` | Decisions on agent changes. Outcomes: `allow`, `hold`, `deny`, `approved` (an approved retry let through) and `rejected` (refused after a person rejected it). Every agent in the policy starts at 0 for each outcome. |
| `kubeamber_risk_score` | histogram, buckets every 10 points | `gen_ai_agent_name` | The risk score of each agent change. |
| `kubeamber_review_duration_seconds` | histogram, 5 ms to 5 s | `kubeamber_outcome` | How long KubeAmber took to review an agent change. The API server waits `webhook.timeoutSeconds` (5 by default) and then, with the default `failurePolicy: Fail`, refuses the change. |
| `kubeamber_approval_wait_seconds` | histogram, 1 minute to 1 week | `kubeamber_decision` (`approved`, `rejected`), `kubeamber_decision_source` (`cluster`, or the change-management system, such as `servicenow`) | Time from a change being held to a person's decision on it, counted when KubeAmber admits the decision. Server-side dry runs are not counted. |
| `kubeamber_change_requests` | gauge | `kubeamber_phase` (`Pending`, `Approved`, `Rejected`, `Consumed`, `Expired`) | Change requests in the cluster by effective phase: a pending request past its deadline, or an approval past `approvals.ttl`, counts as `Expired`. Listed from the API server on each scrape. |
| `kubeamber_build_info` | gauge, always 1 | `version`, `goversion` | One series per running replica, whether metrics are scraped or pushed. Dashboards and alerts use it to find KubeAmber and to notice when no replica is running. |
| `target_info` | gauge | `service_name`, `service_version`, `host_name` | One series per replica. |

Go runtime and process metrics (`go_*`, `process_*`) are included. Each replica
counts only the requests it reviewed, so add replicas up in queries:

```promql
sum by (gen_ai_agent_name, kubeamber_outcome) (rate(kubeamber_decisions_total[5m]))

# 99th percentile review latency
histogram_quantile(0.99, sum by (le) (rate(kubeamber_review_duration_seconds_bucket[5m])))

# Median time for a person to decide on a held change, over the last day
histogram_quantile(0.5, sum by (le) (increase(kubeamber_approval_wait_seconds_bucket[1d])))
```

`kubeamber_change_requests` is the exception: every replica reports the same
cluster-wide count, so take the largest instead of adding them up:

```promql
max by (kubeamber_phase) (kubeamber_change_requests)
```

`increase()` and `rate()` can't see the first value of a series, only changes
after it. Decision counts start at zero for every agent in the policy, so
they're exact. The histograms appear with their first observation, so after a
replica restarts, its first review per outcome, first decision per source and
first risk score per agent aren't counted in `increase()`. Percentiles barely
move; low counts, like decisions on held changes in a quiet hour, can be one
short per replica.

Pick whichever collection method your cluster already uses:

**Scrape annotations.** The pods carry `prometheus.io/scrape: "true"`,
`prometheus.io/port: "9090"` and `prometheus.io/path: /metrics`, which the usual
`kubernetes-pods` scrape job picks up. In Grafana Alloy, find the pods by label
and keep the port named `metrics`:

```alloy
discovery.kubernetes "kubeamber" {
	role = "pod"

	namespaces {
		names = ["kubeamber-system"]
	}

	selectors {
		role  = "pod"
		label = "app.kubernetes.io/name=kubeamber"
	}
}

discovery.relabel "kubeamber" {
	targets = discovery.kubernetes.kubeamber.targets

	rule {
		source_labels = ["__meta_kubernetes_pod_container_port_name"]
		regex         = "metrics"
		action        = "keep"
	}
}

prometheus.scrape "kubeamber" {
	targets    = discovery.relabel.kubeamber.output
	forward_to = [prometheus.remote_write.default.receiver]
}
```

**ServiceMonitor.** With the Prometheus Operator, or Alloy's
`prometheus.operator.servicemonitors`, let the chart create one:

```sh
helm upgrade kubeamber oci://ghcr.io/kubeamber/charts/kubeamber -n kubeamber-system --reuse-values \
  --set metrics.serviceMonitor.enabled=true --set metrics.serviceMonitor.labels.release=prometheus
```

Set the labels to whatever your Prometheus selects ServiceMonitors by.

**Push instead of scrape.** Prometheus 3 can receive OTLP directly when started
with `--web.enable-otlp-receiver`. Point only the metrics exporter at it, and
KubeAmber pushes the same metrics without a scrape job:

```yaml
webhook:
  env:
    - name: OTEL_EXPORTER_OTLP_METRICS_ENDPOINT
      value: http://prometheus.monitoring:9090/api/v1/otlp/v1/metrics
```

The same works for any OTLP receiver, such as Alloy's `otelcol.receiver.otlp`
or an OpenTelemetry Collector.
