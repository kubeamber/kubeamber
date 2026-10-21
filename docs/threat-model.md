# Threat model

KubeAmber sits at the Kubernetes API and judges **writes made by agent
identities**. This document states what that position can and cannot
protect against. It is the starting point for the security self-assessment
expected of CNCF projects.

## Assets

- Workloads, data (PersistentVolumes), configuration and Secrets in the cluster.
- The cluster's permission model (RBAC) and admission controls.
- KubeAmber's own integrity: its policy, webhook configuration, CRD, RBAC and
  ChangeRequests.

## Actors

- **Agent**: an AI agent acting through its own Kubernetes identity. It may be
  wrong, over-eager, or steered by an attacker through prompt injection (a log
  line, an issue comment or a web page it read).
- **Approver**: a human with RBAC permission to update ChangeRequests.
- **Operator**: the people who install KubeAmber and own its policy.
- **Attacker**: anyone trying to get a harmful change applied through an agent,
  or to get past KubeAmber.

## What KubeAmber defends against

| Threat | Mitigation |
|---|---|
| An agent makes an accidental destructive change (deletes a namespace, scales production to zero, deletes a PVC) | Risk scoring holds or denies it, with reasons. Protected namespaces and critical labels raise the score. |
| A prompt-injected agent tries a harmful change | The decision is deterministic and outside the agent; no text the agent sees or writes changes the score. The agent's stated intent is shown to the approver but does not lower the score. |
| An agent escalates privileges through RBAC (binds itself to `cluster-admin`, edits roles) | RBAC kinds score +35 to +45 plus cluster scope; the example policy denies all RBAC changes by agents. |
| An agent switches off safety controls (deletes webhooks or admission policies, including KubeAmber's) | Webhook configurations never reach webhooks, so the ValidatingAdmissionPolicy `kubeamber-protect-admission` (evaluated by the API server) denies agents any change to them. Built-in self-protection denies changes to KubeAmber's CRD, RBAC, namespace and ChangeRequests, whatever the policy says. See the limit on admission policies below. |
| An agent edits KubeAmber's policy to loosen it | The policy ConfigMap lives in KubeAmber's namespace, which self-protection covers. |
| An agent approves its own held change | Agents are denied any write to ChangeRequests. The requesting identity cannot approve its own request. `spec.approval.by` must equal the caller's username. |
| An approval is reused for another change, or replayed later | Approvals are bound to a fingerprint of identity, operation, target and content. Each is consumed once under optimistic concurrency and expires after `approvals.ttl`. Post-dated approvals (clock skew > 2 minutes) are rejected. |
| Someone forges a change-management approval, or points a request at a different, already approved ticket | Only the `kubeamber-bridge` service account can record external decisions, and only for the ticket it linked; the link is immutable once set. Humans cannot write `status.external` or decisions with a `source`. External approvals are honoured only inside the ticket's planned window. |
| The ServiceNow integration credential leaks | It is read from a mounted Secret, never stored in configuration, and should hold a least-privilege ServiceNow role. On its own it cannot approve a change: an approval also needs the bridge's Kubernetes identity and a ticket linked to a held request. |
| An approved request is tampered with after approval | Only `spec.approval` and the matching `status.phase` may change; the recorded request is immutable. |
| Agents act during a freeze or an incident | Freeze windows and Prometheus guards hold changes. A failing guard query holds by default. |
| KubeAmber is unavailable | Agents are blocked (fail-closed); non-agent traffic is unaffected because match conditions keep it away from the webhook. |

## Non-goals and limits

These are outside what an admission webhook can see or control. Operators
should cover them with other controls.

- **Reads and data exfiltration.** Admission webhooks see writes, not `get`,
  `list` or `watch`. An agent with read access to Secrets can read them. Limit
  reads with RBAC, and watch the audit log.
- **Effects inside workloads.** `exec` sessions are gated when they open
  (CONNECT, +35), but KubeAmber does not see the commands run inside them.
  Anything a running pod does with its own credentials is out of scope.
- **Agents with host or node access.** An agent that can reach the kubelet,
  container runtime, etcd or a node shell bypasses the API server and
  therefore KubeAmber. Do not give agents node access.
- **Identity misuse.** KubeAmber recognises agents by Kubernetes identity. An
  agent that runs with a human's kubeconfig, or can impersonate another user
  (`impersonate` RBAC verb), is judged as that identity. Give every agent its
  own identity and do not grant it impersonation.
- **Changes made by controllers on an agent's behalf.** If an agent commits to
  a GitOps repository, the change is applied by the GitOps controller's
  identity. Gate those changes with `kubeamber check` in CI.
- **Rubber-stamping.** KubeAmber ensures a human decides; it cannot ensure they
  decide well. Keep the hold rate low enough that approvers read requests,
  and restrict the approver role.
- **Agents with RBAC over admission policies.** The API server exempts
  ValidatingAdmissionPolicy objects and their bindings from all admission
  control, including KubeAmber's. An agent allowed to delete the
  `kubeamber-protect-admission` binding can then delete KubeAmber's webhook. The
  demo binds `cluster-admin` to its agent to show the worst case; do not do
  this in a real cluster.
- **Before the first start.** The shipped match conditions are `"false"`.
  Until KubeAmber has started once and synced them from the policy, agents are
  not gated.
- **Cluster administrators.** A human with cluster-admin can delete the
  webhook configuration or change the policy. KubeAmber protects the cluster
  from agents, not from its administrators.
- **Policy quality.** An agent profile that does not match an agent's real
  identity means the agent is never gated. Test with `kubeamber check --as`, and
  look for `gen_ai.agent.name` on decisions.

## Hardening checklist

- Run each agent under its own service account or user, never a shared or human one.
- Bind agents to the narrowest RBAC role that works. KubeAmber is a second layer, not a substitute.
- Give agents no write access to `admissionregistration.k8s.io`, `rbac.authorization.k8s.io` or the `kubeamber-system` namespace.
- Bind `kubeamber-approver` only to the people who should approve, ideally an on-call group. It also aggregates into the built-in `admin` role, so namespace admins can approve; remove the aggregation label if that is too broad.
- Keep `failurePolicy: Fail` on the agents webhook.
- Run at least two webhook replicas.
- Export telemetry and alert on denied and held decisions.
- Review the policy ConfigMap through the same change process as other security configuration.

## Reporting

See [SECURITY.md](../SECURITY.md).
