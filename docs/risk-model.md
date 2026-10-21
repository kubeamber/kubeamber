# Risk model

KubeAmber scores every agent change from 0 (harmless) to 100 (dangerous). The
score is compared with the policy thresholds (default hold at 30, deny at 80).

Design goals:

- **Deterministic.** The same change under the same policy always gets the same
  score. There is no model, sampling or network call in the scorer.
- **Explainable.** Every point comes from a factor with a sentence attached.
  That sentence is what the agent and the approver read.
- **No LLM in the decision path.** An agent that was talked into a bad change by
  a prompt injection cannot talk KubeAmber out of stopping it.
- **Conservative and simple.** Factors are additive and few. Exceptions belong
  in [policy rules](policy.md#specrules), not in hidden scoring logic.

The implementation is [pkg/risk/risk.go](../pkg/risk/risk.go). Factors are
summed and the total is clamped to 0..100.

## Operation

| Operation | Points | Reason |
|---|---|---|
| CREATE | +5 | creates a resource |
| CREATE `pods/eviction` | +25 | evicts a pod (instead of the CREATE base) |
| UPDATE | +10 | modifies a resource |
| DELETE | +30 | deletes a resource |
| CONNECT (`exec`, `attach`, `portforward`) | +35 | opens an interactive session into a running workload |

## Kind

Subresource requests are scored as their parent kind (`deployments/scale` is
a Deployment, `pods/exec` is a Pod).

| Kind | Points |
|---|---|
| Namespace | DELETE +50 (deletes it and everything in it); otherwise +10 |
| CustomResourceDefinition | DELETE +60 (deletes every custom resource of that type); otherwise +35 |
| ClusterRole, ClusterRoleBinding | +45 (cluster-wide permissions; possible privilege escalation) |
| Role, RoleBinding | +35 (namespace permissions; possible privilege escalation) |
| Validating/MutatingWebhookConfiguration, Validating/MutatingAdmissionPolicy (and bindings) | +50 (can switch off cluster safety checks). The API server never sends these objects to webhooks, so this factor appears only in `kubeamber check` and MCP previews; in the cluster, `kubeamber-protect-admission` denies agents outright. |
| Node | +40 |
| PersistentVolume, PersistentVolumeClaim | DELETE +45 (possible data loss); otherwise +10 |
| Secret | +25 |
| ServiceAccount token request (`serviceaccounts/token`) | +45 (whoever holds the token acts as that identity) |
| NetworkPolicy | +20 (network isolation) |
| Service, Ingress, Gateway, HTTPRoute, GRPCRoute | +10 (traffic routing) |
| StatefulSet | +15 (stateful workload) |
| DaemonSet | +15 (runs on every node) |
| Deployment, ReplicaSet, Job, CronJob | +5 |
| ConfigMap | +5 |
| Pod, DELETE, with a controller owner reference | −20 (will be recreated) |

## Workload changes

For Deployments, StatefulSets and ReplicaSets, including through the `scale`
subresource. A missing `spec.replicas` counts as 1.

| Condition | Points |
|---|---|
| UPDATE scales from more than 0 to 0 replicas | +35 (takes it offline) |
| UPDATE cuts replicas by half or more (not to 0) | +15 |
| UPDATE changes a container image | +10 |
| About 10 or more pods affected | +10 |
| About 50 or more pods affected | +20 (instead of +10) |

"Pods affected" is the old replica count for a delete, the larger of old and
new for a scale, the new count for an image change, and the new count for a
create.

## Host access

| Condition | Points |
|---|---|
| A new or changed pod spec (Pod, workload template, CronJob job template) requests `hostNetwork`, `hostPID`, `hostIPC`, a `hostPath` volume, `privileged: true`, or adds `SYS_ADMIN`, `NET_ADMIN`, `SYS_PTRACE` or `ALL` capabilities that the old spec did not have | +40 |

## Scope

| Condition | Points |
|---|---|
| Namespace matches `spec.protectedNamespaces` (for a Namespace object, its own name) | +25 |
| Cluster-scoped resource other than a Namespace | +10 |
| Object (old or new) labelled `kubeamber.dev/critical: "true"` | +20 |

## Worked examples

From [pkg/risk/risk_test.go](../pkg/risk/risk_test.go), with `prod` and
`kube-system` protected and default thresholds:

| Change | Factors | Score | Verdict |
|---|---|---|---|
| Scale `shop/checkout` 3 → 4 | UPDATE 10, Deployment 5 | 15 | allow |
| Scale `prod/checkout` 3 → 0 | UPDATE 10, Deployment 5, to zero 35, protected 25 | 75 | hold |
| Scale subresource `shop/checkout` 5 → 0 | UPDATE 10, Deployment 5, to zero 35 | 50 | hold |
| Cut `shop/checkout` 10 → 4 | UPDATE 10, Deployment 5, halved 15, ~10 pods 10 | 40 | hold |
| Change image in `prod/checkout` | UPDATE 10, Deployment 5, image 10, protected 25 | 50 | hold |
| Delete namespace `shop` | DELETE 30, Namespace delete 50 | 80 | deny |
| Delete namespace `prod` | DELETE 30, Namespace delete 50, protected 25 | 100 (capped) | deny |
| Delete a ReplicaSet-managed pod in `shop` | DELETE 30, managed −20 | 10 | allow |
| Delete a bare pod in `shop` | DELETE 30 | 30 | hold |
| `kubectl exec` into a pod in `prod` | CONNECT 35, protected 25 | 60 | hold |
| Delete a PVC | DELETE 30, PVC delete 45 | 75 | hold |
| Create a ClusterRoleBinding | CREATE 5, ClusterRoleBinding 45, cluster-scoped 10 | 60 | hold (the example policy's RBAC rule makes it deny) |
| Delete a ValidatingWebhookConfiguration | DELETE 30, admission 50, cluster-scoped 10 | 90 | deny |
| Create a privileged pod with `hostNetwork` | CREATE 5, host access 40 | 45 | hold |
| Update a pod that already had `hostPID` | UPDATE 10 | 10 | allow |
| Update a ConfigMap labelled critical | UPDATE 10, ConfigMap 5, critical 20 | 35 | hold |
| Delete a 60-replica Deployment | DELETE 30, Deployment 5, ~60 pods 20 | 55 | hold |
| Evict a pod | eviction 25 | 25 | allow |

## Tuning

- Move the thresholds to change how much reaches a human.
- Use `protectedNamespaces` and the `kubeamber.dev/critical` label to mark what
  matters most.
- Use rules for organisation-specific exceptions: `deny` for things agents
  must never do, `allow` for sandboxes.
- If a factor is wrong for everyone, open an issue. Changes to the scoring
  table are breaking changes and are called out in the changelog.
