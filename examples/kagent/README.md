# KubeAmber with kagent

This walkthrough puts KubeAmber in front of [kagent](https://kagent.dev), the
Kubernetes-native agent framework, and asks a kagent agent for three changes: a
routine one, a risky one and a dangerous one. Every screenshot below comes from
one run on a kind cluster, in the order shown.

| Change the agent was asked for | Light | What happened |
|---|---|---|
| Label `checkout` in `shop` | ALLOW, risk 15 | Applied at once |
| Scale `checkout` in `prod` to 10 replicas | HOLD, risk 50 | Held as a ChangeRequest, approved by a person, then applied |
| Delete the `prod` namespace | DENY, risk 100 | Refused; the agent proposed something safer |

## How it fits together

kagent agents don't call the Kubernetes API themselves. They call tools on
kagent's tool server (`kagent-tools`), which runs `kubectl` as the service
account `kagent/kagent-tools`. That service account is the identity KubeAmber
reviews.

```mermaid
flowchart LR
  user([You, in the kagent UI]) --> agent[kagent agent]
  agent -- MCP tool call --> tools["kagent-tools<br/>ServiceAccount kagent/kagent-tools"]
  tools -- kubectl --> api[kube-apiserver]
  api -- "agents only" --> ka[KubeAmber webhook]
  ka -- "ALLOW / HOLD / DENY" --> api
  approver([Approver]) -- kubeamber approve --> cr[(ChangeRequest)]
  ka -. creates, checks .-> cr
```

In kagent v0.10.3 that service account is bound to a ClusterRole granting
every verb on every resource. KubeAmber doesn't take that power away. It
decides which of those changes need a person first.

## Before you start

- [kind](https://kind.sigs.k8s.io/), `kubectl`, `helm` and `jq`.
- The `kubeamber` CLI from the
  [v0.2.0 release](https://github.com/kubeamber/kubeamber/releases/tag/v0.2.0).
- An API key for a model provider kagent supports. This example uses OpenAI
  (`gpt-4.1-mini`); see `providers` in [kagent-values.yaml](kagent-values.yaml)
  to use another.

Run the commands from the root of this repository.

## 1. Create a cluster and install KubeAmber

```sh
kind create cluster --name kubeamber-kagent

helm install kubeamber oci://ghcr.io/kubeamber/charts/kubeamber --version 0.2.0 \
  --namespace kubeamber-system --create-namespace \
  -f examples/kagent/kubeamber-values.yaml --wait
```

[kubeamber-values.yaml](kubeamber-values.yaml) changes one thing in the
chart's default policy: the agent is `kagent/kagent-tools`. Don't match
`kagent/*`. The kagent controller runs in the same namespace and would then be
reviewed as an agent when it manages its own resources.

## 2. Install kagent

```sh
kubectl create namespace kagent
kubectl -n kagent create secret generic kagent-openai \
  --from-literal=OPENAI_API_KEY="$OPENAI_API_KEY"

helm install kagent-crds oci://ghcr.io/kagent-dev/kagent/helm/kagent-crds \
  --version 0.10.3 --namespace kagent
helm install kagent oci://ghcr.io/kagent-dev/kagent/helm/kagent \
  --version 0.10.3 --namespace kagent -f examples/kagent/kagent-values.yaml

kubectl -n kagent wait --for=condition=Ready agent/k8s-agent --timeout=10m
```

[kagent-values.yaml](kagent-values.yaml) keeps only kagent's built-in
Kubernetes agent (`k8s-agent`), to keep the download small. The kagent
controller restarts a few times while its bundled PostgreSQL starts; that is
expected.

## 3. Add a workload and an approver

```sh
kubectl create namespace shop
kubectl create namespace prod
kubectl apply -n shop -f examples/manifests/checkout.yaml
kubectl apply -n prod -f examples/manifests/checkout.yaml

kubectl create clusterrolebinding alice-approver \
  --clusterrole=kubeamber-approver --user=alice@example.com
```

`prod` is a protected namespace in the default policy. Alice stands in for
whoever approves agent changes on your team.

## 4. Open the kagent UI

```sh
kubectl -n kagent port-forward svc/kagent-ui 8082:8080
```

Open <http://localhost:8082>, skip the wizard and choose **kagent/k8s-agent**.

## Walkthrough

### A routine change goes through

Ask: *Add the label team=payments to the checkout deployment in the shop
namespace.*

![kagent's k8s-agent labels the checkout deployment; the label is added](images/01-allow.png)

The agent asked for confirmation, then called `k8s_label_resource`. KubeAmber
scored the change 15 (a workload is modified, outside protected namespaces)
and allowed it. Below the hold threshold (30), KubeAmber only adds a warning,
and the change is applied as if KubeAmber weren't there.

### A risky change is held, but the built-in agent can't see why

Ask: *Scale the checkout deployment in the prod namespace to 6 replicas. You
have my go-ahead.*

![kagent's k8s-agent gets "exit status 1" from three different tools and asks about permissions](images/02-stock-agent.png)

KubeAmber held the change: risk 40, because `prod` is protected (+25). The
agent never saw that explanation. kagent-tools v0.3.0 returns only
`exit status 1` when `kubectl` fails, and keeps `kubectl`'s error output in its
own log. Here is the message it held back:

```text
Error from server (Forbidden): admission webhook "agents.kubeamber.dev" denied the request:
kubeamber: HOLD (risk 40/100, replicas 3 → 6): risk 40/100 is at or above the hold threshold (30).
Factors: +10 modifies a resource, +5 changes a workload, +25 namespace "prod" is protected.
Held for human approval as change request cr-671cf823-tqrcc (expires in 1h0m0s).
Ask an operator to run `kubeamber approve cr-671cf823-tqrcc`, then retry this exact change.
```

Without that message, the agent tried other routes: a merge patch, then an
annotation, then `kubectl apply` of an edited manifest. KubeAmber held all
three. The patch and the apply produce the same change, so they share one
ChangeRequest; the annotation is a different change, so it got its own.
KubeAmber judges the change that would be made, not the tool that makes it.

Note also that the go-ahead in the prompt changed nothing. Talking to the
agent is not an approval.

Reject what the built-in agent left behind before going on:

```sh
kubeamber list
kubeamber --as alice@example.com reject <name> --reason "retrying with the KubeAmber-aware agent"
```

### A KubeAmber-aware agent explains the hold

[kubeamber-agent.yaml](kubeamber-agent.yaml) defines a second agent,
`k8s-agent-kubeamber`, with the same model and tools as `k8s-agent`. Its
instructions add what kagent-tools can't pass on: what the three lights mean,
how to find the agent's own ChangeRequest when a change fails, and not to work
around a hold. It is a separate Agent, so a `helm upgrade` of kagent won't
replace it.

```sh
kubectl apply -f examples/kagent/kubeamber-agent.yaml
kubectl -n kagent wait --for=condition=Ready agent/k8s-agent-kubeamber --timeout=5m
```

Choose **kagent/k8s-agent-kubeamber** in the UI and ask: *Traffic is about to
spike. Scale the checkout deployment in the prod namespace to 10 replicas.*

![The KubeAmber-aware agent reports the hold, its ChangeRequest, risk 50 and the four factors, and waits](images/03-hold.png)

The agent made the change once, found its ChangeRequest and stopped. It gave
the name, the risk score (50: the same factors as before, plus 10 for affecting
about 10 pods) and the command an approver must run.

### A person approves

As an approver (here, the `--as` flag stands in for Alice's own login):

```sh
kubeamber list
kubectl get changerequest <name> -o jsonpath='{range .spec.factors[*]}{@}{"\n"}{end}'
kubeamber --as alice@example.com approve <name> --reason "planned scale-up for the traffic spike"
```

![The approver lists the pending change, reads its factors and approves it](images/04-approver.png)

Alice needs the `kubeamber-approver` role. The approval covers this one change:
the agent can make it once, unchanged. (For agents set up with
`kubeamber agent-kubeconfig`, the agent's owner also can't approve its changes.)

### The agent retries once

Tell the agent: *Alice approved \<name\>. Go ahead.*

![The agent repeats the identical change and reports that checkout now runs 10 replicas](images/05-approved.png)

KubeAmber matched the retry to the approved ChangeRequest and let it through.
`checkout` in `prod` now runs 10 replicas, and the ChangeRequest is used up.

### A dangerous change is refused

Ask the same agent: *We are moving off this cluster. Delete the prod namespace.*

![The agent reports that KubeAmber refused to delete the prod namespace and suggests a safer approach](images/06-deny.png)

Deleting a protected namespace scores 100, so KubeAmber denied it outright. A
denial creates no ChangeRequest, so the agent worked out from its instructions
that the change was refused rather than held. It didn't retry, and it suggested
removing workloads one at a time instead. It can't quote KubeAmber's reason
until kagent-tools passes error output through.

### What KubeAmber recorded

Every decision is in KubeAmber's log, as well as in the Kubernetes audit log
and OpenTelemetry, if you enable them:

```sh
kubectl -n kubeamber-system logs -l app.kubernetes.io/name=kubeamber --tail=-1 \
  | jq -r 'select(.msg == "agent change reviewed") | [.time[11:19], .outcome, .operation, .target, .score, .changeRequest] | @tsv' \
  | sort | column -t -s $'\t'
```

![KubeAmber's decision log for the run: one allow, four holds, one approved retry, one deny](images/07-decision-log.png)

## Things to know

- **All kagent agents share one identity.** Every agent that uses
  `kagent-tool-server` reaches Kubernetes as `kagent/kagent-tools`, so
  KubeAmber sees them as one agent (`kagent`). To tell agents apart, run a
  separate tool server for each, under its own service account, and list each
  account in the policy.
- **The tool server can do anything.** KubeAmber's built-in protection still
  applies: agents can't change KubeAmber, its policy or admission webhooks, and
  the default policy denies RBAC changes by agents. Narrowing kagent-tools' own
  RBAC is still worth doing.
- **Error text doesn't reach the agent yet.** kagent-tools v0.3.0 drops
  `kubectl`'s output when a command fails. Once it passes that output on, the
  built-in agent will see KubeAmber's explanation directly, and the extra
  instructions in kubeamber-agent.yaml become optional.
- **Model output varies.** Wording, follow-up questions and the order of tool
  calls change from run to run. KubeAmber's scoring doesn't: under the same
  policy, the same change always gets the same score.

## Clean up

```sh
kind delete cluster --name kubeamber-kagent
```

## Captured with

| | Version |
|---|---|
| KubeAmber | v0.2.0 |
| kagent | 0.10.3 (kagent-tools 0.3.0) |
| Kubernetes | v1.34.0 on kind |
| Model | OpenAI gpt-4.1-mini |
| Date | 21 October 2026 |
