# Contributing to KubeAmber

Thanks for helping. KubeAmber is early, so design discussion is as valuable as
code. Open an issue before starting anything large, so we can agree on the
approach first.

## Developer Certificate of Origin

Every commit must be signed off under the
[Developer Certificate of Origin](https://developercertificate.org/), as in
all CNCF projects. The sign-off certifies that you wrote the change or have
the right to submit it under the project's license.

```sh
git commit -s -m "risk: score NetworkPolicy deletes higher"
```

This adds `Signed-off-by: Your Name <you@example.com>` using your git
identity. A DCO check runs on every pull request. To fix a missed sign-off:

```sh
git commit --amend -s --no-edit          # last commit
git rebase --signoff main                # every commit on the branch
```

## Development setup

You need Go 1.26 (the `go` command downloads the toolchain named in `go.mod`
if yours is older). For the end-to-end demo you also need Docker, kind and
kubectl.

```sh
make build        # bin/kubeamber
make test         # unit tests with the race detector
make lint         # go vet and golangci-lint
make kind-demo    # kind cluster + KubeAmber + scripted agent scenarios
```

Useful without a cluster:

```sh
bin/kubeamber check -f examples/manifests/scale-to-zero.yaml \
  --old examples/manifests/live-checkout-prod.yaml \
  --policy examples/policy.yaml --agent sre-agent
```

## Where things live

| Path | Contents |
|---|---|
| `pkg/policy` | Policy document, validation, agent matching, freeze windows |
| `pkg/risk` | Risk scoring. Keep [docs/risk-model.md](docs/risk-model.md) in sync. |
| `pkg/decide` | Verdict engine |
| `pkg/fingerprint` | Change fingerprints for approvals |
| `internal/webhook` | Admission handler, TLS, webhook configuration sync |
| `internal/changerequest` | ChangeRequest store |
| `internal/mcpserver` | MCP tools |
| `cmd/kubeamber` | CLI |
| `deploy/` | Kustomize manifests |
| `docs/` | Documentation |

## Pull requests

- One logical change per pull request, with tests. Scoring and approval
  changes need table-driven test cases that show the before and after.
- Changes to risk factors, verdict semantics, the approval protocol or the
  `kubeamber.dev` API are user-visible. Update the docs and add a `CHANGELOG.md`
  entry.
- Keep the decision path deterministic: no network calls in `pkg/risk` or
  `pkg/decide`, and no model calls anywhere in it.
- Error and verdict messages are read by AI agents. Make them specific, and
  say what to do next.
- `make test lint` must pass. CI also runs the kind end-to-end test.
- A maintainer reviews within a few working days. Approval from one maintainer
  is enough to merge, except for changes covered by
  [GOVERNANCE.md](GOVERNANCE.md#decision-making), which need wider agreement.

## Reporting bugs and security issues

Use the issue templates for bugs and feature requests. Report
vulnerabilities privately, as described in [SECURITY.md](SECURITY.md), not in
public issues.

## Code of conduct

Everyone taking part follows the [CNCF Code of Conduct](CODE_OF_CONDUCT.md).
