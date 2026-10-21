# Policy reference

A KubeAmber policy is one YAML document, normally mounted from the ConfigMap in
`kubeamber-system` at `/etc/kubeamber/policy.yaml`. The same file drives
`kubeamber check` and `kubeamber mcp`. Unknown fields are rejected, so a typo fails
loudly instead of silently weakening the policy. A complete example is in
[examples/policy.yaml](../examples/policy.yaml).

```yaml
apiVersion: kubeamber.dev/v1alpha1
kind: Policy
metadata:
  name: default
spec:
  agents: [...]               # required
  thresholds: {hold: 30, deny: 80}
  protectedNamespaces: [...]
  rules: [...]
  freezeWindows: [...]
  approvals: {ttl: 15m, pendingTTL: 1h}
  guards: {prometheus: {...}}
```

## Evaluation order

For a request from an identity that matches an agent:

1. **Self-protection** (built in, cannot be configured): changes to KubeAmber
   itself are denied. See [architecture.md](architecture.md#self-protection).
2. **Risk score**: computed for every change. See [risk-model.md](risk-model.md).
3. **Rules**: the first matching rule applies. A `hold` or `deny` rule raises
   the verdict. An `allow` rule means the score thresholds are skipped.
4. **Thresholds**: unless an `allow` rule matched, a score at or above `hold` holds the change and a score at or above `deny` denies it.
5. **Freeze windows**: every active window raises the verdict to its action.
6. **Guards**: unless the verdict is already `deny`, every firing check raises
   the verdict to its action.

The strictest verdict wins (`deny` > `hold` > `allow`). All reasons are
reported, not only the deciding one.

Requests from identities that match no agent are always allowed. The webhook
normally never sees them; see "Match conditions" in
[architecture.md](architecture.md).

## Globs

Wherever a field takes patterns, `*` matches any run of characters,
including none, and every other character matches itself. Unlike shell or
`path.Match` globs, `*` also matches `/` and `:`, which appear in Kubernetes
usernames. `prod-*` matches `prod-eu` but not `prod`; list both if you mean
both.

## `spec.agents` (required)

The identities KubeAmber treats as AI agents. An identity belongs to the first
agent profile it matches.

| Field | Type | Description |
|---|---|---|
| `name` | string, required, unique | Profile name. Used in rules, metrics (`gen_ai.agent.name`), ChangeRequests and `--agent` flags. |
| `users` | []glob | Kubernetes usernames, e.g. `agent:claude-code`. |
| `groups` | []glob | Group names, e.g. `kubeamber:agents`. Matches if any of the identity's groups matches. |
| `serviceAccounts` | []string | `namespace/name`; `name` may be `*` (or any glob). Converted to `system:serviceaccount:<namespace>:<name>`. |

Each profile needs at least one of `users`, `groups` or `serviceAccounts`.
The webhook refuses to start if a profile matches KubeAmber's own service
account.

```yaml
agents:
  - name: sre-agent
    serviceAccounts: ["kubeamber-agents/*"]
  - name: holmesgpt
    serviceAccounts: ["holmesgpt/holmesgpt-holmes-service-account"]
  - name: workstation-agents
    groups: ["kubeamber:agents"]
```

## `spec.thresholds`

| Field | Default | Description |
|---|---|---|
| `hold` | `30` | Lowest score that is held for human approval. 1-100. |
| `deny` | `80` | Lowest score that is denied outright. Must be at least `hold`, at most `101`. Set `101` to never deny on score alone. |

## `spec.protectedNamespaces`

`[]glob`. Changes in these namespaces (or to these Namespace objects) score
+25. Typical: `["kube-system", "kube-public", "prod", "prod-*"]`.

## `spec.rules`

Ordered list. The first rule whose `match` selects the change applies, and
later rules are not consulted.

| Field | Type | Description |
|---|---|---|
| `name` | string, required | Reported as the deciding rule. |
| `match.agents` | []glob | Agent profile names. Each must exist. |
| `match.operations` | []string | `CREATE`, `UPDATE`, `DELETE`, `CONNECT`. |
| `match.kinds` | []glob | Kind, e.g. `Deployment`, `ClusterRoleBinding`. Subresource requests use the parent kind (`deployments/scale` is `Deployment`, `pods/exec` is `Pod`). |
| `match.namespaces` | []glob | Namespace of the change; for a Namespace object, its own name. |
| `match.names` | []glob | Object name. |
| `action` | `allow` \| `hold` \| `deny`, required | |
| `reason` | string | Shown to the agent. Default: `matched rule "<name>"`. |

Empty `match` fields match everything. An `allow` rule overrides the risk
score only. Freeze windows, guards and self-protection still apply, so an
allowed sandbox change is still held during a freeze or while a guard fires.

```yaml
rules:
  - name: no-rbac-changes
    match: {kinds: [Role, RoleBinding, ClusterRole, ClusterRoleBinding]}
    action: deny
    reason: agents may not change RBAC
  - name: scratch-namespaces
    match: {namespaces: ["sandbox-*"]}
    action: allow
```

## `spec.freezeWindows`

Periods in which agent changes are held or denied. A window is either weekly
or one-off, not both.

| Field | Description |
|---|---|
| `name` | Required. |
| `days` | Weekly: `Mon`..`Sun` (case-insensitive; the first three letters are used). Empty means every day. |
| `start`, `end` | Weekly: `HH:MM`, required. `end` may be `24:00`. If `end` is before `start` the window runs overnight and belongs to the day it starts: `Fri 22:00-06:00` covers Friday 22:00 to Saturday 06:00. |
| `from`, `until` | One-off: RFC 3339 timestamps, both required, `until` after `from`. Active in `[from, until)`. |
| `timeZone` | IANA name, default `UTC`. Applies to weekly windows. |
| `namespaces` | []glob. Empty means all namespaces. |
| `action` | `hold` (default) or `deny`. |

```yaml
freezeWindows:
  - name: friday-evening
    days: [Fri]
    start: "17:00"
    end: "24:00"
    timeZone: Asia/Kolkata
    namespaces: ["prod", "prod-*"]
  - name: black-friday
    from: "2026-11-27T00:00:00Z"
    until: "2026-11-30T00:00:00Z"
    action: deny
```

`kubeamber check --at <RFC3339>` evaluates windows at a chosen time.

## `spec.approvals`

| Field | Default | Description |
|---|---|---|
| `ttl` | `15m` | How long an approval stays usable. The agent must retry within this time. |
| `pendingTTL` | `1h` | How long a held change waits for a decision. Also how long a rejection keeps blocking identical retries. A change linked to a ticket by `kubeamber bridge` waits until the ticket's planned window closes instead. |
| `allowOwnerApproval` | `false` | Let the person who launched an agent (its `kubeamber.dev/owner`, set by `kubeamber agent-kubeconfig`) approve that agent's held changes. Leave it off unless your team is one person: approving your own agent's change is approving your own change. |
| `requireExternal` | `false` | Approvals must come from the change-management system through `kubeamber bridge`. `kubeamber approve` is refused; `kubeamber reject` still works, because rejecting fails safe. See [ServiceNow](integrations.md#servicenow-change-management). |

An approval recorded from a ticket is usable until the ticket's planned window
closes, rather than for `ttl`, and only after the window opens.

Durations use Go syntax (`90s`, `15m`, `2h`).

## `spec.guards.prometheus`

Escalates agent changes while a PromQL query returns at least one series, for
example while an SLO burns its error budget.

| Field | Default | Description |
|---|---|---|
| `url` | required | Prometheus-compatible base URL; KubeAmber calls `GET <url>/api/v1/query`. |
| `timeout` | `2s` | Per-query timeout. |
| `onError` | `hold` | Verdict when a query fails or returns a non-vector: `allow` (ignore), `hold` or `deny`. |
| `checks[].name` | required | |
| `checks[].query` | required | Must return an instant vector (or matrix). Fires when non-empty. |
| `checks[].namespaces` | all | []glob; the check runs only for changes in these namespaces. |
| `checks[].action` | `hold` | `hold` or `deny`. |
| `checks[].reason` | `its query returned results` | Shown to the agent. |

Results are cached for 15 seconds per query, so a burst of agent requests
costs one query. Guards are skipped when the verdict is already `deny`, and
are not evaluated by `kubeamber check` or `kubeamber mcp`.

```yaml
guards:
  prometheus:
    url: http://prometheus-operated.monitoring:9090
    checks:
      - name: checkout-fast-burn
        query: 'slo:sli_error:ratio_rate1h{service="checkout"} > 14.4 * 0.001'
        namespaces: ["prod"]
        reason: checkout is burning its error budget; a human should approve changes until it recovers
```

## Validation errors

`kubeamber check --policy` loads and validates the file the same way the webhook
does, so CI can lint a policy change before it is rolled out:

```sh
kubeamber check --policy policy.yaml -f examples/manifests/checkout.yaml --agent sre-agent
```
