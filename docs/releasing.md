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

Pull requests that change the build run the same workflow without
publishing.

## Cutting a release

1. Move the `Unreleased` entries in `CHANGELOG.md` under the new version and
   date.
2. Update the image tag in `deploy/webhook.yaml` and
   `deploy/servicenow-bridge/deployment.yaml`, and `version` and `appVersion`
   in `charts/kubeamber/Chart.yaml` (plus the image in its
   `artifacthub.io/images` annotation), to the new version.
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

SBOM and provenance:

```sh
docker buildx imagetools inspect ghcr.io/kubeamber/kubeamber:v0.2.0 --format '{{ json .SBOM }}'
docker buildx imagetools inspect ghcr.io/kubeamber/kubeamber:v0.2.0 --format '{{ json .Provenance }}'
```
