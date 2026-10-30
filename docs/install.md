# Installation

KubeAmber has two parts:

- **The webhook**, installed once per cluster. It reviews the changes agents
  make.
- **The `kubeamber` CLI**, for the people who approve held changes. It also
  gives agents their own identities, previews changes for agents over MCP, and
  scores manifests in CI.

This page installs both, then covers what comes next: telling KubeAmber who
your agents are, choosing who approves, changing the policy, upgrading,
uninstalling and fixing common problems. To watch KubeAmber hold a change
before you plan a real rollout, follow the [Quickstart](../README.md#quickstart)
on a kind cluster first.

## Before you start

- **Kubernetes 1.30 or later.** KubeAmber relies on webhook match conditions,
  generally available since 1.30. Check with `kubectl version`.
- **Cluster-admin rights** for the install. It creates a webhook
  configuration, a ValidatingAdmissionPolicy, a CRD and ClusterRoles.
- **Helm 3.8 or later**, or only kubectl for the
  [plain manifests](#with-plain-manifests).
- **Nodes that can pull from `ghcr.io`.** For a private registry, copy
  `ghcr.io/kubeamber/kubeamber:v0.3.1` there and set the chart's
  `image.repository` (and `imagePullSecrets` if it needs credentials).
- **One installation per cluster.** The webhook configuration, the policy and
  the CRD are cluster-wide.

The webhook runs two replicas, each requesting 20m of CPU and 32Mi of memory.

## Install the webhook

### With Helm

```sh
helm install kubeamber oci://ghcr.io/kubeamber/charts/kubeamber --version 0.3.1 \
  --namespace kubeamber-system --create-namespace --wait
```

When you change a setting later, put it in a values file, such as
`kubeamber-values.yaml`, and pass that file to every `helm upgrade`. The
chart's [values.yaml](../charts/kubeamber/values.yaml) describes every
setting.

### With plain manifests

```sh
kubectl apply -k "https://github.com/kubeamber/kubeamber/deploy?ref=v0.3.1"
kubectl -n kubeamber-system rollout status deploy/kubeamber
```

To use your own policy, keep it in a kustomize overlay rather than editing the
cluster, so the next apply doesn't undo your changes. Start `policy.yaml` from
[deploy/policy.yaml](../deploy/policy.yaml) and put this next to it:

```yaml
# kustomization.yaml
resources:
  - https://github.com/kubeamber/kubeamber/deploy?ref=v0.3.1
configMapGenerator:
  - name: kubeamber-policy
    namespace: kubeamber-system
    behavior: replace
    files: [policy.yaml]
    options:
      disableNameSuffixHash: true
```

Then run `kubectl apply -k .` in that directory. The ServiceNow bridge and the
notifier have their own kustomizations in [deploy/](../deploy/); see
[Integrations](integrations.md).

### With Argo CD or Flux

While it runs, KubeAmber writes two fields of its webhook configuration: the
CA bundle it serves with, and the match condition it generates from the
policy. A tool that re-applies the manifest resets both, and until KubeAmber
writes them back (within 30 seconds) agents' changes are not reviewed. Tell
your GitOps tool to leave them alone.

**Argo CD** renders the chart with `helm template`, which can't read the live
fields back. Add this to the Application:

```yaml
spec:
  ignoreDifferences:
    - group: admissionregistration.k8s.io
      kind: ValidatingWebhookConfiguration
      name: kubeamber
      jqPathExpressions:
        - .webhooks[].clientConfig.caBundle
        - .webhooks[].matchConditions
  syncPolicy:
    syncOptions:
      - RespectIgnoreDifferences=true
```

**Flux:** install the chart with a HelmRelease rather than the plain
manifests. Flux runs Helm against the cluster, so the chart reads the live
fields back as a `helm upgrade` does. If you turn on the HelmRelease's drift
detection, ignore the same fields:

```yaml
spec:
  driftDetection:
    mode: enabled
    ignore:
      - target: {kind: ValidatingWebhookConfiguration, name: kubeamber}
        paths:
          - /webhooks/0/clientConfig/caBundle
          - /webhooks/0/matchConditions
          - /webhooks/1/clientConfig/caBundle
```

## Check that it works

```sh
kubectl -n kubeamber-system get pods
kubectl get validatingwebhookconfiguration kubeamber \
  -o jsonpath='{.webhooks[0].matchConditions[0].expression}{"\n"}'
```

You should see two pods `Running`, and an expression that names your agents,
starting with `request.userInfo.username.matches('^system:serviceaccount:kubeamber-agents:.*$')`
for the default policy. If it prints `false`, KubeAmber hasn't written its
match condition yet: wait a few seconds, and if it stays `false`, read the
logs with `kubectl -n kubeamber-system logs deploy/kubeamber`.

To watch it allow, hold and deny real changes from a test agent, follow
step 3 of the [Quickstart](../README.md#quickstart).

## Install the CLI

Install it wherever someone approves held changes. Like kubectl, it uses your
current kubeconfig context, or `--kubeconfig` and `--context`, including the
sign-in plugins of EKS, AKS and GKE kubeconfigs.

**macOS and Linux:**

```sh
VERSION=0.3.1
OS=$(uname -s | tr '[:upper:]' '[:lower:]')
ARCH=$(uname -m | sed 's/x86_64/amd64/; s/aarch64/arm64/')
NAME="kubeamber_${VERSION}_${OS}_${ARCH}"
curl -fsSLO "https://github.com/kubeamber/kubeamber/releases/download/v${VERSION}/${NAME}.tar.gz"
curl -fsSLO "https://github.com/kubeamber/kubeamber/releases/download/v${VERSION}/checksums.txt"
shasum -a 256 --ignore-missing -c checksums.txt
tar -xzf "${NAME}.tar.gz"
sudo install -m 0755 "${NAME}/kubeamber" /usr/local/bin/kubeamber   # or anywhere on your PATH
kubeamber version
```

**Windows:** download `kubeamber_0.3.1_windows_amd64.zip` (or `_arm64.zip`)
from the [release page](https://github.com/kubeamber/kubeamber/releases/tag/v0.3.1),
unzip it, and put `kubeamber.exe` in a folder on your `PATH`.

**With Go 1.26 or later:**
`go install github.com/kubeamber/kubeamber/cmd/kubeamber@v0.3.1`.

**Verify the signature.** The release workflow signs `checksums.txt` with
Sigstore keyless signing. With [cosign](https://docs.sigstore.dev/cosign/system_config/installation/):

```sh
curl -fsSLO "https://github.com/kubeamber/kubeamber/releases/download/v${VERSION}/checksums.txt.sigstore.json"
cosign verify-blob checksums.txt --bundle checksums.txt.sigstore.json \
  --certificate-identity-regexp '^https://github.com/kubeamber/kubeamber/\.github/workflows/release\.yaml@refs/tags/v' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

[Verifying a release](releasing.md#verifying-a-release) covers the image and
the chart too.

## Tell KubeAmber who your agents are

KubeAmber reviews only the identities in the policy's `spec.agents`. People,
controllers and CI pipelines are never sent to it. The default policy treats
two kinds of identity as agents:

- every service account in the `kubeamber-agents` namespace (the agent profile
  `sre-agent`);
- every user in the group `kubeamber:agents` (the profile
  `workstation-agents`).

| Your agent | What to do |
|---|---|
| Runs in the cluster under a service account: kagent, HolmesGPT, your own operator | Add that service account to `spec.agents` (see [kagent](integrations.md#kagent) and [HolmesGPT](integrations.md#holmesgpt)), or run the agent under a service account in `kubeamber-agents`. |
| Runs on someone's machine: Claude Code, Codex, Cursor, kubectl-ai | Give it an identity of its own with [`kubeamber agent-kubeconfig`](integrations.md#give-each-agent-its-own-identity). Never give an agent your own kubeconfig: KubeAmber would see you, not the agent. |
| Runs in a cloud service with a cloud identity: Azure, AWS, Google | Match the identity the cloud maps it to; see [agents from any provider](integrations.md#agents-from-any-provider). |

To see which identity an agent uses, run `kubectl auth whoami` with its
credentials. To check that a policy treats that identity as an agent:

```sh
kubeamber check -f some-manifest.yaml --policy policy.yaml --as <username> --group <group>
```

It prints a verdict for an agent, and `"<username>" is not an agent` for
anyone else.

Agents that preview changes over MCP also need to read change requests, to
follow up on their held changes; see
[Claude Code and other MCP clients](integrations.md#claude-code-and-other-mcp-clients).

## Choose who approves

People approve from their own kubeconfig, and RBAC decides who may: bind the
`kubeamber-approver` ClusterRole to the people or group on call.
`kubeamber-viewer` lets others see change requests without deciding them.

```sh
kubectl create clusterrolebinding kubeamber-approvers \
  --clusterrole=kubeamber-approver --group=<your on-call group>
```

Approvers then use the CLI:

```sh
kubeamber list                                   # changes waiting for a decision
kubeamber approve cr-3fa9c2d1-x7k2q --reason "incident 4411, approved by on-call"
kubeamber reject  cr-3fa9c2d1-x7k2q --reason "scale to 1 instead"
```

- No agent can approve anything. The identity that made a change can't
  approve it, and neither can the person who launched the agent with
  `kubeamber agent-kubeconfig` (`approvals.allowOwnerApproval` relaxes this
  for one-person teams).
- An approval lets the agent retry exactly that change, once, within 15
  minutes (`approvals.ttl`). A held change nobody decides expires after an
  hour (`approvals.pendingTTL`).

Rather than watching `kubeamber list`, have held changes posted to
[Slack, Teams or a webhook](integrations.md#notifications-in-slack-teams-or-any-webhook),
or raised in [ServiceNow](integrations.md#servicenow-change-management).

## Change the policy

The policy says which identities are agents, which namespaces are protected,
the hold and deny thresholds, rules, freeze windows and approval times. The
[policy reference](policy.md) describes every field.

**With Helm,** the policy is the chart's `policy` value. Put your changes in
your values file:

```yaml
# kubeamber-values.yaml
policy:
  spec:
    protectedNamespaces: ["kube-system", "kube-public", "prod", "prod-*", "payments"]
```

```sh
helm upgrade kubeamber oci://ghcr.io/kubeamber/charts/kubeamber --version 0.3.1 \
  --namespace kubeamber-system -f kubeamber-values.yaml
```

Helm merges your file into the default policy: fields you leave out keep their
defaults, and a list you set, such as `protectedNamespaces` or `rules`,
replaces the default list. `helm get values kubeamber -n kubeamber-system --all`
shows the result.

If you installed with Helm, change the policy only through your values
file. An edit made directly to the `kubeamber-policy` ConfigMap is lost at a
later upgrade, or makes the upgrade fail with a conflict (Helm 4).

**With the plain manifests,** edit `policy.yaml` in your overlay and run
`kubectl apply -k .` again. `kubectl -n kubeamber-system edit configmap
kubeamber-policy` works for a quick experiment, but the next apply overwrites
it.

KubeAmber reloads the policy without restarting, within about a minute:
Kubernetes takes that long to update the file each replica reads. Check that
both replicas accepted it:

```sh
kubectl -n kubeamber-system logs -l app.kubernetes.io/name=kubeamber --prefix --tail=-1 | grep policy
```

`policy reloaded` means the new policy is in force on that replica.
`policy reload failed; keeping the previous policy` means it was rejected, for
example for an unknown field, and the error says why; the previous policy
still applies.

## Upgrade

Read the [changelog](../CHANGELOG.md) first: before 1.0, a minor version may
change the policy format.

**With Helm,** pass your values file again:

```sh
helm upgrade kubeamber oci://ghcr.io/kubeamber/charts/kubeamber --version <new version> \
  --namespace kubeamber-system -f kubeamber-values.yaml
```

If you used `--set` instead of a file, add `--reset-then-reuse-values` (Helm
3.14 or later) to keep those settings. Avoid `--reuse-values`: it also keeps
the old version's defaults, so a new version's settings are missing and the
upgrade can fail with `nil pointer evaluating interface {}.enabled`. Agents
stay reviewed throughout a Helm upgrade.

**With the plain manifests,** change `ref=` to the new version in your overlay
and apply it again. The apply resets the webhook's match condition, so agents'
changes are not reviewed until KubeAmber restores it, within 30 seconds.

Upgrade the CLI on approvers' machines too.

## Uninstall

**With Helm:**

```sh
helm uninstall kubeamber --namespace kubeamber-system
```

From then on, agents' changes are no longer reviewed. Two things are kept on
purpose, so that a reinstall carries on where it left off. Delete them if you
don't need them:

```sh
kubectl delete crd changerequests.kubeamber.dev   # every change request
kubectl delete namespace kubeamber-agents         # agents' service accounts: ends their access
kubectl delete namespace kubeamber-system         # the TLS Secret KubeAmber created
kubectl delete clusterrolebinding kubeamber-approvers --ignore-not-found
```

**With the plain manifests:**

```sh
kubectl delete -k "https://github.com/kubeamber/kubeamber/deploy?ref=v0.3.1"
```

This deletes everything, including the change requests and the
`kubeamber-agents` namespace.

## Platform notes

- **kind, k3d, minikube, k3s:** nothing special, on Kubernetes 1.30 or later.
- **EKS, AKS, GKE:** map your cloud identities to agent profiles; see
  [agents from any provider](integrations.md#agents-from-any-provider).
- **Private GKE clusters:** the control plane can reach nodes only on ports
  443 and 10250 by default. Add a firewall rule allowing it to reach the
  webhook pods on TCP port 9443, or agents' changes fail with
  `failed calling webhook`.
- **AKS:** `az aks stop` refuses to run while KubeAmber's webhook is
  installed, because its rule covers every resource; AKS doesn't take into
  account the match condition that limits it to agents. Uninstall KubeAmber
  with Helm before stopping the cluster, and install it again after starting
  it. Change requests are kept.
- **OpenShift:** not tested yet. Reports are welcome in
  [GitHub issues](https://github.com/kubeamber/kubeamber/issues).

## Troubleshooting

| What you see | Why | What to do |
|---|---|---|
| An agent's change goes through with no `kubeamber:` warning | KubeAmber doesn't treat that identity as an agent, or its match condition isn't written yet | Run `kubectl auth whoami` with the agent's credentials and compare it with `spec.agents`. Check the match condition as in [Check that it works](#check-that-it-works). |
| Agents get `failed calling webhook "agents.kubeamber.dev"` | The API server can't reach KubeAmber: its pods are down, a network policy blocks it, or a firewall does | `kubectl -n kubeamber-system get pods,endpoints`. Only agents are affected; people's changes still work. |
| An approval is refused with `the identity that requested a change cannot approve it` or `you launched this agent` | You made the change, or launched the agent that made it | Ask someone else with the `kubeamber-approver` role. You can still reject it. |
| `kubeamber approve` fails with `forbidden` | You don't have the `kubeamber-approver` role | Bind it as in [Choose who approves](#choose-who-approves). |
| The agent's retry is held again after an approval | The retry isn't exactly the approved change, or the approval expired or was used | `kubeamber list -A` shows each request's phase. Retry the identical change, or decide the new request. |
| A policy change has no effect | KubeAmber rejected it, or `helm upgrade` ran without your values file | Look for `policy reload failed` in the logs, as in [Change the policy](#change-the-policy). |
| `helm upgrade` fails with `nil pointer evaluating interface {}.enabled` | `--reuse-values` kept the old version's defaults | Upgrade with `-f kubeamber-values.yaml` or `--reset-then-reuse-values`. |
| macOS won't run `kubeamber` because it can't verify the developer | The binary was downloaded with a browser, which marks it as quarantined | Run `xattr -d com.apple.quarantine kubeamber`, or download it with `curl` as above. |
