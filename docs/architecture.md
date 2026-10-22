# Architecture

KubeAmber is a single Go binary, `kubeamber`, with these subcommands:

| Command | Runs | Purpose |
|---|---|---|
| `kubeamber webhook` | in the cluster (Deployment in `kubeamber-system`) | Validating admission webhook. Reviews agent changes and human decisions on change requests. |
| `kubeamber check` | CI or a laptop, no cluster needed | Scores manifests offline with the same engine. Exit code 0 / 2 / 3 for allow / hold / deny. |
| `kubeamber list`, `approve`, `reject` | an operator's workstation | Lists and decides held changes, using the operator's own kubeconfig identity. |
| `kubeamber mcp` | next to an AI agent (stdio) | MCP server so the agent can preview a change and follow up on held ones. |

The decision logic lives in importable packages:

- `pkg/policy`: the Policy document, agent matching, globs, freeze windows, hot reload.
- `pkg/risk`: deterministic 0-100 scoring with an explanation for every point.
- `pkg/decide`: combines self-protection, rules, score thresholds, freeze windows and guards into a verdict.
- `pkg/fingerprint`: identifies a change so an approval covers exactly that change.

Cluster-facing code is in `internal/`: `webhook` (admission handler, TLS,
webhook configuration sync), `changerequest` (ChangeRequest store),
`guards` (Prometheus), `mcpserver`, `manifest` and `telemetry`.

## What runs in the cluster

![What runs in the cluster: kube-apiserver sends agents' changes over HTTPS to the kubeamber Service in kubeamber-system, which spreads them over two webhook pods. The webhook reads the kubeamber-policy ConfigMap and its TLS Secret, keeps the webhook configuration in sync, and creates ChangeRequests that approvers decide. The optional bridge mirrors held changes to ServiceNow. Decisions go to an OpenTelemetry Collector, SLO guards query Prometheus, and each agent signs in as its own ServiceAccount in kubeamber-agents.](images/architecture-in-cluster.png)

## Request flow

```
agent ──► kube-apiserver
            │ authn, RBAC
            │ ValidatingWebhookConfiguration "kubeamber"
            │   webhook agents.kubeamber.dev
            │     matchConditions: request.userInfo is an agent (CEL from policy)
            ▼
          kubeamber webhook  POST /validate
            1. identity → agent profile (policy.spec.agents); not an agent → allow
            2. built-in self-protection → deny
            3. risk.Assess → score + factors
            4. first matching rule → allow (overrides score) | hold | deny
            5. score vs thresholds (unless an allow rule matched)
            6. active freeze windows → hold | deny
            7. Prometheus guards (unless already deny) → hold | deny
            8. strictest verdict wins
            ▼
          allow │ hold │ deny
```

Every verdict that reaches an agent carries `decide.Message`, one line of the
form `kubeamber: HOLD (risk 75/100, replicas 3 → 0): <reasons>. Factors: <factors>`,
followed by what to do next. Agents are language models, so the message is
written for them to read.

Allowed agent changes still return the message as an admission **warning**.
`kubectl` prints it, so the agent and its user see the score even when nothing
was blocked.

## Why deny-and-retry

The API server waits at most 30 seconds for a webhook (KubeAmber's
configuration uses 5), so a held request cannot block until a human answers.
Instead:

1. KubeAmber denies the request and records a `ChangeRequest` (`kubeamber.dev/v1alpha1`,
   cluster-scoped, named like `cr-3fa9c2d1-x7k2q`).
2. A human runs `kubeamber approve cr-3fa9c2d1-x7k2q` (or `reject`).
3. The agent retries the **identical** change. KubeAmber finds the approved
   ChangeRequest with the same fingerprint, marks it `Consumed`, and allows the
   request.

Agents already retry when they are told to, and the denial message says when
and how. The protocol needs no agent-side SDK.

![The amber path: the agent scales a Deployment to zero; KubeAmber creates ChangeRequest cr-1a2b with risk 75 and answers HOLD. An approver approves it with the CLI or ServiceNow, the agent retries the identical change, KubeAmber matches the fingerprint, uses the approval once and answers ALLOW.](images/architecture-amber-path.png)

### Fingerprints

A fingerprint is the SHA-256 of: requesting username, operation, group, kind,
resource, subresource, namespace, name, and (except for deletes) the
normalized object. Normalization:

- drops `status`;
- drops `metadata.resourceVersion`, `uid`, `generation`, `creationTimestamp`,
  `managedFields`, `selfLink`, `deletionTimestamp`, `deletionGracePeriodSeconds`;
- drops the annotations `kubectl.kubernetes.io/last-applied-configuration` and
  `deployment.kubernetes.io/revision` wherever they appear;
- keeps `kubectl.kubernetes.io/restartedAt` but wildcards its value, so a
  retried `kubectl rollout restart` matches the approved one.

JSON encoding sorts map keys, so the fingerprint is canonical. A
different identity, operation, target or content yields a different
fingerprint, and an approval never covers it. The first 40 hex characters are
stored as the label `kubeamber.dev/fingerprint` so lookups are a label-selector
list; the full value in `spec.fingerprint` is compared exactly.

### ChangeRequest lifecycle

```
            human approves             agent retries identical change
 Pending ───────────────────► Approved ───────────────────────────► Consumed
    │                            │
    │ human rejects              │ approvals.ttl passes (default 15m)
    ▼                            ▼
 Rejected                     Expired
    │
 approvals.pendingTTL passes (default 1h) with no decision → Expired
```

- **Pending:** a retry of the same change is denied with "still waiting". No
  duplicate request is raised.
- **Rejected:** retries are denied with the human's reason until
  `pendingTTL` after the decision; after that a retry raises a new request.
- **Expired:** ignored; a retry raises a new request.
- **Consumed:** for 30 seconds the identical change is still allowed, because
  the API server re-runs admission when it retries a write after a conflict
  (for example while a controller updates the object's status). One kubectl
  command can reach the webhook twice, and re-applying an identical change has
  no further effect. After 30 seconds a retry raises a new request.
- **One approval, one use.** Consumption is an update at the resource version
  KubeAmber read. If two retries race, one update succeeds. The other gets a
  conflict, moves on and is held again.

`Expired` is computed from timestamps (`spec.expiresAt`, `spec.approval.at` +
`approvals.ttl`) rather than written by a controller; `kubeamber list` shows the
effective phase.

### Change-management tickets

With `kubeamber bridge` installed, a Pending ChangeRequest is linked to a
ticket in a change-management system (`status.external`), and the decision can
come from there. The webhook still decides everything locally; the bridge
syncs in the background, so ServiceNow latency never reaches the admission
path. A linked request waits until the ticket's planned window closes, and an
approval from the ticket is usable only inside that window. See
[proposal 0001](proposals/0001-change-management.md) for the full protocol and
the rules the webhook enforces on the bridge.

### Notifications

`kubeamber notify` watches ChangeRequests and posts to Slack, Microsoft Teams
or a generic webhook as each one is held, still waiting, approved, rejected,
applied or expired. It only reads ChangeRequests, so it needs no trust from
the webhook, and it is never in the admission path. It reads the policy file
for `approvals.ttl`, so "expired" means the same thing as in the webhook, and
it records what it has sent in the `kubeamber-notify-state` ConfigMap, so a
restart repeats nothing. Delivery is at least once per channel. Run one
replica: two would send everything twice.

### Two-person rule

The same webhook has a second entry, `changerequests.kubeamber.dev`, which
reviews every write to a ChangeRequest except KubeAmber's own:

| Who / what | Result |
|---|---|
| An agent (any operation) | denied |
| A human creating a ChangeRequest | denied; only KubeAmber creates them |
| A human deleting one | allowed (cleanup) |
| An update that changes anything in `spec` other than `spec.approval` | denied |
| `spec.approval.by` not equal to the caller's username | denied |
| `spec.approval.by` equal to `spec.user` (the requester) | denied |
| `spec.approval.at` more than 2 minutes from the webhook's clock | denied (no post-dating to stretch the TTL) |
| Deciding a request that already has a decision, or is not Pending | denied |
| Approving after `spec.expiresAt` | denied |
| `status.phase` not matching the decision (`Approved` / `Rejected`) | denied |

Who may approve at all is ordinary RBAC: `update`/`patch` on
`changerequests.kubeamber.dev`. Bind it to the people who should hold that
power, for example an on-call group.

## Match conditions: agent traffic only

The agents webhook matches CREATE, UPDATE and DELETE on every resource (`*`,
`*/scale`, `pods/eviction`) and CONNECT on `pods/exec`, `pods/attach` and
`pods/portforward`. It would be too
expensive, and too risky, to put every request in the cluster through it.
At startup and on every policy reload, KubeAmber regenerates a CEL match
condition named `kubeamber-agents` from `spec.agents`:

```cel
// generated as one line; wrapped here for reading
request.userInfo.username != 'system:serviceaccount:kubeamber-system:kubeamber' && (
  request.userInfo.username.matches('^system:serviceaccount:kubeamber-agents:.*$') ||
  request.userInfo.groups.exists(g, g == 'kubeamber:agents'))
```

Exact names compile to `==`, and patterns with `*` compile to anchored RE2
`matches()`, with every other character matched literally.

The API server evaluates this in-process. Requests from humans, controllers
and KubeAmber itself are admitted without a network call. This is what
makes `failurePolicy: Fail` safe: if KubeAmber is unavailable, only agents
are blocked (fail-closed), and the rest of the cluster carries on. Other
match conditions you add to the webhook are preserved.

KubeAmber also re-applies the condition and its CA bundle every 30 seconds
(`--resync-interval`), because `helm upgrade` or `kubectl apply -k` resets
them to the manifest's placeholders. The Helm chart also reads the live values
back during an upgrade, so there is no gap at all.

The shipped manifest's condition is `"false"`, so until KubeAmber's first
successful sync the webhook receives nothing and **agents are not gated**.
Check that the sync happened after installing:

```sh
kubectl get validatingwebhookconfiguration kubeamber \
  -o jsonpath='{.webhooks[0].matchConditions[0].expression}'
```

## Self-protection

Checked before the policy, and not configurable. Agents are denied when the
change:

- targets any `kubeamber.dev` resource (ChangeRequests);
- lands in KubeAmber's own namespace (default `kubeamber-system`), or targets that
  Namespace object, including the policy ConfigMap and the TLS Secret;
- targets admission configuration named `kubeamber` or `kubeamber-*`;
- targets a CRD whose name ends in `.kubeamber.dev`;
- targets RBAC objects named `kubeamber` or `kubeamber-*`;
- creates, changes or deletes a service account whose `kubeamber.dev/owner`
  annotation would change, which would hijack or forge a developer's agent
  identity.

The webhook also refuses to start if a policy agent matches its own service
account.

### Admission configuration is a special case

The API server never sends changes to ValidatingWebhookConfiguration or
MutatingWebhookConfiguration objects to webhooks, so that a broken webhook
cannot lock administrators out. KubeAmber's webhook therefore cannot review an
agent deleting it, or any other webhook.

To close that gap KubeAmber ships a ValidatingAdmissionPolicy,
`kubeamber-protect-admission`, which the API server evaluates in-process. It
denies agents any change to webhook configurations. KubeAmber keeps its
`kubeamber-agents` match condition in step with the policy, the same way it does
for the webhook (`--admission-policy`, empty to skip).

Admission policies cannot protect themselves either: the API server exempts
ValidatingAdmissionPolicy and its bindings from admission. An agent whose RBAC
lets it delete that binding could remove the protection and then the webhook.
**Keep `admissionregistration.k8s.io` out of every agent's RBAC.**

## TLS

By default the webhook manages its own certificates. On startup it reads the
Secret `kubeamber-webhook-tls` in its namespace. If the Secret is missing,
expires within 30 days or was issued for another Service name, KubeAmber
generates an ECDSA P-256 CA (10 years) and serving certificate (1 year) and
writes them. It then publishes the CA in the `caBundle` of every webhook in the
configuration. Replicas share the Secret; if two race, the loser re-reads the
winner's certificate.

To use cert-manager or another issuer, pass `--tls-cert-file` and
`--tls-key-file` and inject the CA bundle yourself. Rotation currently happens
on restart; in-process rotation is on the roadmap.

## Policy hot reload

The policy is a file, normally the mounted ConfigMap
`/etc/kubeamber/policy.yaml`. KubeAmber re-reads it every 5 seconds
(`--reload-interval`) and swaps it in atomically when its checksum changes.
Polling follows the kubelet's symlink swap when a mounted ConfigMap is
updated. A file that fails to parse or validate is logged and ignored: a
bad edit never replaces a working policy. Each reload also resyncs the match
condition.

## Telemetry

Telemetry uses OpenTelemetry and the standard `OTEL_*` environment variables.
Each replica serves its metrics for Prometheus on `:9090/metrics`, over plain
HTTP (`--metrics-addr`; empty turns it off). Traces and metrics are pushed over
OTLP/HTTP only when `OTEL_EXPORTER_OTLP_ENDPOINT`, or the traces or metrics
variant, is set; each signal starts only when its own endpoint or the shared one
is configured.

- **Span** `kubeamber.review` per agent request, with attributes
  `gen_ai.agent.name`, `kubeamber.outcome`, `kubeamber.verdict`,
  `kubeamber.risk.score`, `kubeamber.rule`, `kubeamber.change_request`,
  `kubeamber.user`, `kubeamber.operation`, `kubeamber.target.kind`,
  `kubeamber.target.name`, `k8s.namespace.name`. If the object carries
  `kubeamber.dev/traceparent`, the span links to that span context.
- **Metrics**: `kubeamber.decisions` (counter, by `gen_ai.agent.name` and
  `kubeamber.outcome` = allow / hold / deny / approved / rejected) and
  `kubeamber.risk.score` (histogram, buckets every 10 points). Prometheus sees
  them as `kubeamber_decisions_total` and `kubeamber_risk_score`
  ([integrations](integrations.md#prometheus-and-grafana-alloy)).
- **Audit log**: admission responses carry audit annotations, which the API
  server records as `agents.kubeamber.dev/verdict`, `agents.kubeamber.dev/score`,
  `agents.kubeamber.dev/agent` and `agents.kubeamber.dev/change-request`.
- **Logs**: one structured JSON line per decision on stdout, including the
  agent's stated intent.
- **Kubernetes Events**: holds, denials, approved retries and rejections are
  recorded as Events (events.k8s.io) on the changed object, with the
  ChangeRequest as the related object (`--events`;
  [integrations](integrations.md#kubernetes-events)). Recording is
  asynchronous and never delays or changes a decision; dry runs record nothing.

## Failure modes

| Failure | Effect | Why |
|---|---|---|
| KubeAmber pods down or unreachable | Agent requests fail; everyone else unaffected | `failurePolicy: Fail` plus agent-only match conditions |
| Policy file invalid at startup | Webhook does not start | Fail loudly rather than run without a policy |
| Policy file invalid on reload | Previous policy stays in force; error logged | A bad edit must not weaken or break enforcement |
| ChangeRequest lookup or create fails | Change held, agent told to retry shortly | Fail closed |
| Approval consumed concurrently | Losing retry is held again | Optimistic concurrency on the ChangeRequest |
| Prometheus unreachable or query error | `onError` verdict (default hold) | A guard that cannot see is treated as firing |
| Request body cannot be decoded | Denied | Fail closed |
| Dry-run request that would be held | Denied with the verdict; no ChangeRequest recorded | `sideEffects: NoneOnDryRun` |
| Clock skew between approver and webhook > 2m | Approval rejected | Prevents post-dated approvals |
