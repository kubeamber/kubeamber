# Releasing

Pushing a `v*` tag runs [`.github/workflows/release.yaml`](../.github/workflows/release.yaml)
and [`.github/workflows/chart.yaml`](../.github/workflows/chart.yaml). They
publish the following, with no long-lived signing keys:

- the multi-arch image `ghcr.io/kubeamber/kubeamber:<tag>` (linux/amd64,
  linux/arm64), with an SBOM and SLSA provenance attached, signed with
  Sigstore keyless signing;
- CLI binaries for Linux, macOS and Windows (amd64 and arm64), with
  `checksums.txt` and its Sigstore bundle, as a GitHub release;
- the Helm chart `oci://ghcr.io/kubeamber/charts/kubeamber`, at the tag's
  version without the `v`, signed the same way.

The image and the chart each carry two signatures from the same keyless
identity: cosign 3's bundle, stored as an OCI 1.1 referrer, and the legacy
`.sig` tag. Admission verifiers such as Sigstore policy-controller (tested up
to v0.15.1) only find the legacy one; `cosign verify` accepts either. Releases
up to v0.2.0 have only the bundle.

Pull requests that change the build run the same workflow without
publishing.

## Cutting a release

1. Move the `Unreleased` entries in `CHANGELOG.md` under the new version and
   date.
2. Update the image tag in `deploy/webhook.yaml`,
   `deploy/servicenow-bridge/deployment.yaml` and `deploy/notify/deployment.yaml`,
   `version` and `appVersion` in `charts/kubeamber/Chart.yaml` (plus the image
   in its `artifacthub.io/images` annotation), the status line and install
   commands in `README.md`, the install commands in `docs/install.md` and
   `charts/kubeamber/README.md`, and the supported version in `SECURITY.md`,
   to the new version. `make chart-lint` fails while the install commands
   name another version.
3. Merge that pull request, then tag the merge commit and push the tag:

   ```sh
   git tag -s v0.2.0 -m "v0.2.0"
   git push origin v0.2.0
   ```

4. The first time only: the new packages at
   github.com/orgs/kubeamber/packages (`kubeamber` and `charts/kubeamber`)
   are private. Set each to public under Package settings → Change
   visibility.

## Verifying a release

Image:

```sh
cosign verify ghcr.io/kubeamber/kubeamber:v0.2.0 \
  --certificate-identity-regexp '^https://github.com/kubeamber/kubeamber/\.github/workflows/release\.yaml@refs/tags/v' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

Binaries:

```sh
cosign verify-blob checksums.txt --bundle checksums.txt.sigstore.json \
  --certificate-identity-regexp '^https://github.com/kubeamber/kubeamber/\.github/workflows/release\.yaml@refs/tags/v' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
sha256sum --check --ignore-missing checksums.txt
```

Chart:

```sh
cosign verify ghcr.io/kubeamber/charts/kubeamber:0.2.0 \
  --certificate-identity-regexp '^https://github.com/kubeamber/kubeamber/\.github/workflows/chart\.yaml@refs/tags/v' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

Base image: the runtime image is built on `cgr.dev/chainguard/static`, which
Chainguard signs with Sigstore keyless signing from GitHub Actions. Check the
digest the `Dockerfile` pins:

```sh
cosign verify "$(grep -o 'cgr.dev/chainguard/static[^ ]*' Dockerfile)" \
  --certificate-identity https://github.com/chainguard-images/images/.github/workflows/release.yaml@refs/heads/main \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

SBOM and provenance:

```sh
docker buildx imagetools inspect ghcr.io/kubeamber/kubeamber:v0.2.0 --format '{{ json .SBOM }}'
docker buildx imagetools inspect ghcr.io/kubeamber/kubeamber:v0.2.0 --format '{{ json .Provenance }}'
```

## Keeping dependencies current

Two bots open weekly pull requests, and CI runs on each:

- **Dependabot** ([`.github/dependabot.yml`](../.github/dependabot.yml)): Go
  modules, GitHub Actions (pinned by commit SHA), the `golang` build image and
  the website's Python packages.
- **digestabot** ([`.github/workflows/digestabot.yaml`](../.github/workflows/digestabot.yaml)):
  the digests of `cgr.dev/chainguard` images, in every file that pins one.
  It verifies Chainguard's signature on a new digest before it updates it,
  lists the package changes from the image's SBOM in the run summary, and
  signs off and signs (gitsign) its commits.

The release workflow runs `make verify-base-images` before every image build.
It checks that `Dockerfile` and `hack/Dockerfile.local` pin the same base
image, and verifies Chainguard's signature on it. Run it locally with cosign
installed.

digestabot opens its pull requests with a short-lived token from
[Octo STS](https://github.com/octo-sts/app), because pull requests opened with
`GITHUB_TOKEN` don't run CI. The trust policy
[`.github/chainguard/digestabot.sts.yaml`](../.github/chainguard/digestabot.sts.yaml)
grants only `contents: write` and `pull_requests: write`, only to that workflow
on `main`, for an hour at a time. One-time setup: install the
[Octo STS GitHub App](https://github.com/apps/octo-sts/installations/new) on
the kubeamber organization with **Only select repositories → kubeamber**. Don't
give it the other repositories (such as security advisory forks). The App
itself is granted broad permissions; the trust policy is what limits each
token.
