# Proposal 0001: Change-management integration

| | |
|---|---|
| Status | Implementing (phase 1) |
| Author | @sathpal |
| Created | 2026-10-21 |
| Discussion | GitHub issue linked from the pull request |

## Summary

Connect KubeAmber's held changes (amber) to the change-management systems
enterprises already use, starting with ServiceNow. When an AI agent's change is
held, KubeAmber opens a change ticket, waits for the organisation's own approval
process to decide, lets the agent apply exactly that change inside the approved
window, and closes the ticket with evidence of what happened.

## Motivation

Regulated organisations will not let AI agents change production unless agent
changes go through the same change process as human ones. Today an operator
approves a KubeAmber ChangeRequest with `kubeamber approve`. That is a clean
two-person rule, but it lives outside the system of record that auditors,
change managers and change advisory boards (CABs) use.

Auditors ask the same questions of every production change. Who asked for it,
and why? Who approved it? When was it allowed to happen? What actually changed?
KubeAmber already captures each answer (the agent, its stated intent, the
approver, the fingerprint of the exact change, a trace link). This proposal
puts those answers into the ticket where auditors look for them.

Relevant controls include SOC 2 CC8.1 (change management), ISO/IEC 27001:2022
Annex A 8.32 (change management) and PCI DSS v4.0 requirement 6.5.1 (change
control procedures). This mapping is indicative; organisations and their
auditors decide what satisfies a control.

## Goals

- A held agent change opens a ticket in the change-management system,
  automatically and exactly once.
- The organisation's own approval process decides. KubeAmber honours an
  approval only for that exact change, once, inside the ticket's planned window.
- The ticket ends up with an implementation record (what was applied, when, by
  which agent) or is cancelled if the change never happened.
- Organisations can require that approvals come from the change system, so
  that local `kubeamber approve` cannot bypass it.
- No change to the admission path's latency or availability: the webhook never
  calls an external system.
- Integration credentials are least-privilege and never stored in the policy.
  Secret contents never leave the cluster.

## Non-goals (phase 1)

- Agents working under a human-raised, pre-approved ticket (see phase 2).
- Creating standard changes for allowed (green) agent changes.
- Bidirectional sync after approval, for example re-holding a change that a
  CAB cancels after approving it. Phase 1 honours the approval until its window
  closes.
- Providers other than ServiceNow. The interface is designed for them.

## Design

### Components

```
 agent ──► kube-apiserver ──► KubeAmber webhook ──(amber)──► ChangeRequest cr-…
                                                                 │
                         ┌───────────── kubeamber bridge ◄───────┘ watches
                         │  (own Deployment and service account)
                         ▼
              change-management system (ServiceNow)
                 CHG0031234: New → Assess → Authorize → Scheduled → Implement → Review
```

A new subcommand, `kubeamber bridge`, runs as its own Deployment under the
service account `kubeamber-bridge`. It reconciles ChangeRequests with tickets
through a **provider**. The webhook is unchanged in its role: it decides
locally and asks nothing of the outside world. It gains rules about what the
bridge may write.

### ChangeRequest API additions (`kubeamber.dev/v1alpha1`)

```yaml
spec:
  approval:
    decision: Approved
    by: servicenow:jane.cab            # provider-prefixed approver identity
    reason: CHG0031234 approved in ServiceNow
    at: "2026-10-21T10:04:00Z"
    source: servicenow                 # new: empty for in-cluster approvals
    externalRef: CHG0031234            # new
status:
  phase: Approved
  external:                            # new: written by the bridge
    system: servicenow
    id: 9d385017c611228701d22104cc95c371   # sys_id
    number: CHG0031234
    url: https://example.service-now.com/change_request.do?sys_id=…
    state: Scheduled
    plannedStart: "2026-10-21T10:00:00Z"
    plannedEnd: "2026-10-22T10:00:00Z"
    syncedAt: "2026-10-21T10:04:10Z"
    closed: false
```

### Lifecycle

1. **Hold.** The webhook holds the agent's change and creates a Pending
   ChangeRequest, as today.
2. **Open.** The bridge sees a Pending ChangeRequest with no `status.external`.
   It looks for an existing ticket whose `correlation_id` is the ChangeRequest
   name, which makes a bridge restart safe. If there is none, it creates a
   normal change (or an emergency change, if configured). The ticket holds:
   - a short description (agent, operation, target, summary);
   - a description (intent, risk score and every reason, fingerprint, trace
     link, ChangeRequest name);
   - the planned window, set to now plus the configured window;
   - the configuration item mapped from the namespace;
   - any static fields set in the bridge configuration.

   The bridge then writes `status.external`.
3. **Wait.** While the ticket is undecided, the agent's retries are answered
   with the ticket number and link, so the agent can tell its user where
   approval is pending. A linked Pending request expires at the ticket's
   planned end, not at `approvals.pendingTTL`, because CAB cycles take longer
   than an hour.
4. **Decide.** The bridge polls the ticket.
   - `approval=approved` with the ticket in Scheduled, Implement or later: it
     records `spec.approval` (source `servicenow`, approvers from
     `sysapproval_approver`) and sets the phase to Approved.
   - `approval=rejected`, or the ticket cancelled: it records a rejection.
5. **Apply.** The agent retries the identical change. The webhook allows it
   once, and only between the window's planned start and end. Before the window
   opens, the agent is told when to retry.
6. **Close.**
   - Consumed: the bridge writes work notes (what was applied, when, by which
     agent, fingerprint, trace) and moves the ticket through Implement to
     Review, or to Closed if configured, with close code `successful`.
   - Expired, or rejected in the cluster: the bridge cancels the ticket with a
     work note.

   Either way it sets `status.external.closed`.

### Who may write what (enforced by the webhook)

| Writer | May | May not |
|---|---|---|
| KubeAmber webhook identity | create ChangeRequests, mark them Consumed | |
| Bridge (`kubeamber-bridge`) | set `status.external` once (system, id, number and url are then immutable); update its state, window, sync time and closed flag; record a decision whose `source` and `externalRef` match `status.external`, with `by` different from the requester, `at` within 2 minutes of now, before the deadline | change `spec` other than `approval`; mark Consumed; record decisions on requests without a ticket; create or delete |
| Humans | approve or reject Pending requests in the cluster (`kubeamber approve`/`reject`), and delete requests | touch `status.external`; record decisions with a `source`; approve in the cluster when the policy sets `approvals.requireExternal` (rejecting is still allowed, because it fails safe) |
| Agents | nothing | anything |

### Policy

```yaml
spec:
  approvals:
    # Approvals must come from the change-management system. Local
    # `kubeamber approve` is refused; local `kubeamber reject` still works.
    requireExternal: true
```

### Provider interface

```go
type Provider interface {
    Name() string // "servicenow"
    Open(ctx context.Context, cr *changerequest.ChangeRequest) (*changerequest.External, error)
    Sync(ctx context.Context, ext changerequest.External) (Ticket, error)
    Close(ctx context.Context, ext changerequest.External, outcome Outcome, notes string) error
}
```

`Open` must be idempotent for the same ChangeRequest name. `Sync` reports the
ticket's state, planned window and, once there is one, the decision and who
made it. `Close` records the outcome.

### ServiceNow mapping

| KubeAmber | ServiceNow |
|---|---|
| Open | `POST /api/sn_chg_rest/change/{normal,emergency}` (Change Management API) |
| Idempotency | `correlation_id` = ChangeRequest name, `correlation_display` = `KubeAmber` |
| Sync | `GET /api/now/table/change_request/{sys_id}` (`state`, `approval`, `start_date`, `end_date`) |
| Approvers | `GET /api/now/table/sysapproval_approver?sysparm_query=sysapproval={sys_id}^state=approved` |
| Close, implemented | `PATCH /api/now/table/change_request/{sys_id}`: Implement (-1), then Review (0) or Closed (3), `close_code=successful`, `close_notes`, `work_notes` |
| Close, abandoned | `PATCH …`: Canceled (4), `work_notes` |
| Window | `start_date` / `end_date`, in UTC |
| Configuration item | `cmdb_ci` from a namespace-to-CI map in the bridge configuration |

States follow the default change model: New (-5), Assess (-4), Authorize (-3),
Scheduled (-2), Implement (-1), Review (0), Closed (3), Canceled (4).

### Security

- **Credentials.** OAuth client credentials (preferred) or a dedicated
  integration user with basic auth. They are read from files mounted from a
  Kubernetes Secret, so they can be rotated with External Secrets or Vault and
  are never in the policy or the bridge configuration. The ServiceNow role
  should be the minimum that can create and update change requests and read
  approvals (for example `itil` with `sn_change_read`), never `admin`.
- **Data minimisation.** Tickets carry the agent, the operation, the target,
  the summary, the score and reasons, the intent, the fingerprint and a trace
  link. Object contents are never sent, so Secret values and environment
  variables never leave the cluster.
- **Two-person rule.** The ticket approver is recorded as
  `servicenow:<user_name>`. It can never equal the requesting agent's identity.
  Approval happens through the organisation's SSO in ServiceNow.
- **Forgery.** Only the bridge's service account can record external
  decisions, and only against the ticket it linked. That link is immutable
  once set. The webhook enforces this, not the bridge.
- **Evidence.** The Kubernetes audit log already carries
  `agents.kubeamber.dev/change-request`. With the ticket number in the
  ChangeRequest, an auditor can go from ticket to ChangeRequest to audit event
  to trace.

### Failure modes

| Failure | Effect |
|---|---|
| ServiceNow unreachable | The change stays held. The bridge retries every interval and records the error in `status.external.error`. Agents are told the change is waiting. |
| Bridge down | The change stays held. Nothing is approved without it, so this fails safe. |
| Ticket approved after its window | The ChangeRequest expires at the planned end. The agent's next attempt raises a new request and ticket. |
| CAB reschedules the window | The bridge mirrors the new start and end while the request is pending. |
| Duplicate bridge replicas | Run one replica. Opening is idempotent through `correlation_id`, and ChangeRequest updates use optimistic concurrency. |

## Phase 2

- **Ticket-gated agents.** An agent works under a human-raised, approved ticket
  by annotating its changes `kubeamber.dev/change: CHG0031234`. The bridge
  mirrors approved tickets into in-cluster `ChangeWindow` objects (scope:
  namespaces, kinds and window), so the webhook can check them locally.
- **Standard changes.** Allowed agent changes are optionally logged as standard
  changes from a template, for a complete audit trail.
- **Emergency changes.** When an incident is open (PagerDuty, Opsgenie or
  ServiceNow Incident), held changes become emergency changes routed to the
  emergency CAB.
- **More providers.** Jira Service Management, BMC Helix and Freshservice for
  tickets. Slack and Microsoft Teams for lightweight approvals. Argo CD sync
  windows imported as freeze windows.

## Alternatives considered

- **Call ServiceNow from the webhook.** Rejected. Admission must answer in
  seconds and stay available; ServiceNow latency and outages would become
  cluster outages for agents.
- **A ServiceNow Spoke or app that pulls from Kubernetes.** Rejected for phase
  1. It needs network access from ServiceNow into clusters, which most
  organisations forbid, and it ties the design to one vendor. A spoke could be
  added later as an alternative front end.
- **Approve in ServiceNow by running `kubeamber approve` from a ServiceNow
  workflow.** Rejected. It needs Kubernetes credentials stored in ServiceNow,
  and the approver identity would be a shared service account.

## Testing

- Unit tests run the ServiceNow provider against a simulated instance
  (`internal/changes/servicenow/fake`) that implements the endpoints above.
- Bridge tests run against a fake Kubernetes client.
- The kind end-to-end test runs the bridge and the simulated instance. It
  approves in "ServiceNow" and checks that the agent's retry is applied and the
  ticket closed.
- Manual validation against a ServiceNow Personal Developer Instance
  (developer.servicenow.com).

## Open questions

1. Should one ticket cover several held changes from the same agent task, for
   example by grouping on the traceparent's trace ID?
2. How should a ServiceNow approver be mapped to a Kubernetes identity, if an
   organisation wants both to appear in the audit log?
3. Should the bridge move tickets from Scheduled to Implement when the window
   opens, or leave that to ServiceNow workflows?
