# KubeAmber with Chainguard

AI agents change workloads, and changing a workload usually means choosing an
image. An agent that was prompt-injected, or simply wrong, can swap in an image
from an unknown registry, or one whose tag can be moved under it. This example
puts three layers in front of that, all free and with no Chainguard account:

| Layer | Who it applies to | What it checks |
|---|---|---|
| [Chainguard Containers](https://images.chainguard.dev) | the images you choose | Minimal, Wolfi-based images, signed by Chainguard with Sigstore |
| KubeAmber, with [`spec.images`](../../docs/policy.md#specimages) | agents only | Where an image comes from, and whether it is pinned by digest; holds anything else for a human |
| [Sigstore policy-controller](https://docs.sigstore.dev/policy-controller/overview/) | everyone in an opted-in namespace | That an image is genuinely signed by the expected identity |

KubeAmber itself is built the same way: its runtime image is
`cgr.dev/chainguard/static`, its release workflow verifies that base image's
signature, and digestabot keeps the digest current
([docs/releasing.md](../../docs/releasing.md)).

The outputs below come from a run on kind with KubeAmber built from `main`.

## Prerequisites

- A cluster with KubeAmber installed, built from a release or `main` that has
  `spec.images`, for example `make kind-deploy` from the repository root.
- `kubectl`, `helm`, and [cosign](https://docs.sigstore.dev/cosign/system_config/installation/) for part 3.
- A `shop` namespace: `kubectl create namespace shop`.

## Part 1: agents deploy only Chainguard images

Add the images block to KubeAmber's policy. With kustomize, replace the
policy ConfigMap with [policy.yaml](policy.yaml), which is the default policy
plus `spec.images`:

```sh
kubectl -n kubeamber-system create configmap kubeamber-policy \
  --from-file=policy.yaml=examples/chainguard/policy.yaml --dry-run=client -o yaml | kubectl apply -f -
```

With Helm, add [kubeamber-values.yaml](kubeamber-values.yaml):

```sh
helm upgrade kubeamber oci://ghcr.io/kubeamber/charts/kubeamber -n kubeamber-system \
  --reuse-values -f examples/chainguard/kubeamber-values.yaml
```

The block that matters:

```yaml
images:
  trusted: ["cgr.dev/chainguard/*", "ghcr.io/kubeamber/*"]
  requireDigest: true
```

KubeAmber reloads the policy within about a minute (the kubelet's ConfigMap
sync). Then create the agent: [agent.yaml](agent.yaml) is a service account in
`kubeamber-agents` that may manage Deployments in `shop` and nothing else.

```sh
kubectl apply -f examples/chainguard/agent.yaml
alias agent='kubectl --as=system:serviceaccount:kubeamber-agents:web-agent'
```

**The agent deploys Docker Hub's nginx by tag** ([web-dockerhub.yaml](web-dockerhub.yaml)):
held, with every reason.

```console
$ agent apply -f examples/chainguard/web-dockerhub.yaml
Error from server (Forbidden): error when creating "examples/chainguard/web-dockerhub.yaml": admission webhook
"agents.kubeamber.dev" denied the request: kubeamber: HOLD (risk 50/100): risk 50/100 is at or above the hold
threshold (30). Factors: +5 creates a resource, +5 changes a workload, +30 runs images from outside the trusted
repositories (nginx:1.27), +10 runs images not pinned by digest (nginx:1.27). Held for human approval as change
request cr-fe0066aa-xc2bm (expires in 1h0m0s). Ask an operator to run `kubeamber approve cr-fe0066aa-xc2bm`, then
retry this exact change.
```

**The agent deploys Chainguard's nginx, pinned by digest**
([web-chainguard.yaml](web-chainguard.yaml)): allowed, with the score as a
warning.

```console
$ agent apply -f examples/chainguard/web-chainguard.yaml
Warning: kubeamber: ALLOW (risk 10/100): risk 10/100 is below the hold threshold (30). Factors: +5 creates a resource, +5 changes a workload
deployment.apps/web created
```

The image runs as non-root user 65532 and serves on port 8080.

**The agent swaps the running image for Docker Hub's**: held. The change
summary shows exactly what would change.

```console
$ agent -n shop set image deploy/web web=nginx:1.27
error: failed to patch image update to pod template: admission webhook "agents.kubeamber.dev" denied the request:
kubeamber: HOLD (risk 65/100, image web: cgr.dev/chainguard/nginx:latest@sha256:16d0e55f… → nginx:1.27): risk 65/100
is at or above the hold threshold (30). Factors: +10 modifies a resource, +5 changes a workload, +10 changes container
image (…), +30 runs images from outside the trusted repositories (nginx:1.27), +10 runs images not pinned by digest
(nginx:1.27). Held for human approval as change request cr-7c6cf8d7-gkn2r (expires in 1h0m0s). …
```

An approver sees both held changes:

```console
$ kubeamber list
NAME               PHASE    AGENT      CHANGE                      RISK  TICKET  AGE  SUMMARY
cr-7c6cf8d7-gkn2r  Pending  sre-agent  UPDATE Deployment shop/web  65    -       17s  image web: cgr.dev/chainguard/nginx:latest@sha256:… → nginx:1.27
cr-fe0066aa-xc2bm  Pending  sre-agent  CREATE Deployment shop/web  50    -       17s
```

Agents using `kubeamber mcp` can ask first: the `kubeamber_policy` tool
returns `trustedImages` and `requireDigest`, and `kubeamber_preview` scores a
manifest before it is applied. In CI, `kubeamber check` gives the same verdicts
offline.

## Part 2: check the images are genuine

KubeAmber checks where an image comes from, not that it is genuine. Sigstore's
policy-controller verifies signatures at admission, for every identity, in the
namespaces you opt in.

```sh
helm repo add sigstore https://sigstore.github.io/helm-charts
helm install policy-controller sigstore/policy-controller --version 0.10.8 \
  -n cosign-system --create-namespace -f examples/chainguard/policy-controller/values.yaml --wait
kubectl apply -f examples/chainguard/policy-controller/image-policies.yaml
kubectl label namespace shop policy.sigstore.dev/include=true
```

[image-policies.yaml](policy-controller/image-policies.yaml) requires images
from `cgr.dev/chainguard` to carry Chainguard's signature: keyless, from
`https://github.com/chainguard-images/images/.github/workflows/release.yaml@refs/heads/main`
with issuer `https://token.actions.githubusercontent.com`.
[values.yaml](policy-controller/values.yaml) sets `no-match-policy: warn`, so
images no policy covers are admitted with a warning.

Now the Chainguard image is verified for everyone, and the agent's Docker Hub
image is still held. Note what changed in the agent's hold:

```console
$ agent apply -f examples/chainguard/web-dockerhub.yaml
Warning: no matching policies: spec.template.spec.containers[0].image
Warning: index.docker.io/library/nginx:1.27@sha256:6784fb08…
Error from server (Forbidden): … admission webhook "agents.kubeamber.dev" denied the request: kubeamber: HOLD
(risk 40/100): … +30 runs images from outside the trusted repositories (index.docker.io/library/nginx:1.27@sha256:6784fb08…) …
```

policy-controller's mutating webhook resolves tags to digests before validating
webhooks run, so KubeAmber sees a pinned image and the digest factor no longer
applies. The stored spec really is pinned, which is what `requireDigest` asks
for. The repository is still untrusted, so the change is still held.

A person is not held by KubeAmber, but is still covered by policy-controller:

```console
$ kubectl -n shop create deployment human-web --image=nginx:1.27 --dry-run=server
Warning: no matching policies: spec.template.spec.containers[0].image
Warning: index.docker.io/library/nginx:1.27@sha256:6784fb08…
deployment.apps/human-web created (server dry run)
```

To see the signature check refuse something, add a policy that expects a
different signer for Chainguard's nginx:

```console
$ kubectl -n shop create deployment human-cg --image=cgr.dev/chainguard/nginx:latest --dry-run=server
error: failed to create deployment: admission webhook "policy.sigstore.dev" denied the request: validation failed:
failed policy: wrong-signer-demo: … signature keyless validation failed for authority authority-0 for
cgr.dev/chainguard/nginx@sha256:16d0e55f…: no matching signatures: none of the expected identities matched what was in
the certificate, got subjects [https://github.com/chainguard-images/images/.github/workflows/release.yaml@refs/heads/main] …
```

## Part 3: KubeAmber's own supply chain

Verify the base image `Dockerfile` pins (also `make verify-base-images`):

```sh
cosign verify "$(grep -o 'cgr.dev/chainguard/static[^ ]*' Dockerfile)" \
  --certificate-identity https://github.com/chainguard-images/images/.github/workflows/release.yaml@refs/heads/main \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

Verify a KubeAmber release image:

```sh
cosign verify ghcr.io/kubeamber/kubeamber:<version> \
  --certificate-identity-regexp '^https://github.com/kubeamber/kubeamber/\.github/workflows/release\.yaml@refs/tags/v' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

KubeAmber's releases are signed with cosign 3, which stores the signature as
an OCI 1.1 referrer in the new bundle format. cosign 3 verifies it.
policy-controller 0.13.1 (chart 0.10.8) reports `no signatures found` for it,
so this example doesn't add a ClusterImagePolicy for `ghcr.io/kubeamber`.
Chainguard's images use the older signature format, which policy-controller
reads.

## Part 4: build KubeAmber from source with melange and apko

The release image comes from the repository's `Dockerfile`. Chainguard's own
toolchain can build the same binary without a Dockerfile:
[melange](https://github.com/chainguard-dev/melange) compiles it into a
signed Wolfi package, and [apko](https://github.com/chainguard-dev/apko)
assembles a minimal image from packages and writes its SBOM. Both run from
their free images. [melange.yaml](apko/melange.yaml) builds with Wolfi's
`go-1.26`, the release `go.mod` asks for. Set `package.version` to the version
you build.

```sh
cd examples/chainguard/apko
ARCH=$(uname -m | sed 's/arm64/aarch64/')

# A throwaway key to sign the local package (git-ignored).
docker run --rm -v "$PWD":/work -w /work cgr.dev/chainguard/melange keygen

# melange builds in a sandbox, so it needs --privileged.
docker run --rm --privileged -v "$PWD":/work -v "$(git rev-parse --show-toplevel)":/src:ro -w /work \
  cgr.dev/chainguard/melange build melange.yaml --source-dir /src --signing-key melange.rsa --arch "$ARCH"

docker run --rm -v "$PWD":/work -w /work \
  cgr.dev/chainguard/apko build apko.yaml kubeamber:apko kubeamber.tar --arch "$ARCH"
docker load < kubeamber.tar
```

On Apple silicon the loaded image is `kubeamber:apko-arm64` (on Intel,
`kubeamber:apko-amd64`). It is about 16 MB, runs as user 65532, and has no
shell; [apko.yaml](apko/apko.yaml) lists everything in it, and the SBOM agrees:

```console
$ jq -r '.packages[].name' sbom-aarch64.spdx.json | grep -v -e '\.yaml$' -e '^sha256' | sort -u
alpine/ca-certificates
ca-certificates-bundle
kubeamber
wolfi
wolfi-baselayout

$ docker run --rm -v "$(git rev-parse --show-toplevel)/examples":/examples:ro kubeamber:apko-arm64 \
    check --policy /examples/chainguard/policy.yaml -f /examples/chainguard/web-dockerhub.yaml --agent sre-agent
HOLD   CREATE Deployment shop/web  risk 50/100
…
```

Tested on Apple silicon (aarch64).

## Optional: with a Chainguard account

Nothing above needs an account. If your organisation has one:

- **Chainguard Console** ([console.chainguard.dev](https://console.chainguard.dev)):
  manage the images your organisation is entitled to, their versions and pull
  tokens. The free Catalog Starter plan includes five images of your choice,
  with previous versions; it needs a business email.
- **Versioned images.** The free public images are `:latest` only. With an
  entitlement you can pin a version, for example a Go 1.26 builder that matches
  `go.mod`, in place of `golang:1.26` in KubeAmber's build stage. Set up pulls
  with `chainctl` (see Chainguard's documentation for pull tokens in CI), and
  point digestabot's `verify-signatures` rule at your organisation's signing
  identity.
- **Chainguard Libraries** (Java, Python, JavaScript; Go in beta) and
  **Chainguard Actions** are paid products. KubeAmber's own build doesn't use
  them, so contributors and forks need no account.

## Clean up

```sh
kubectl -n shop delete deployment web --ignore-not-found
kubectl delete -f examples/chainguard/agent.yaml
helm uninstall policy-controller -n cosign-system
kubectl label namespace shop policy.sigstore.dev/include-
```

Restore your own KubeAmber policy afterwards (`kubectl apply -k deploy`, or
`helm upgrade` without the example's values).
