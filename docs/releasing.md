# CI, packages, and releases

[README](../README.md) · [Contributing](../CONTRIBUTING.md) · [Install](installation.md)

GitHub Actions validates changes and publishes the container image and Helm chart to GitHub Container Registry (GHCR).

## Pull-request checks

The reusable [CI workflow](../.github/workflows/ci.yaml) runs on every pull request and again before publishing. It checks:

- Go formatting, module consistency, vet, and race-enabled tests.
- Reboot handling against a local Kubernetes API server and the pinned NVSentinel CRD; OIDC exchanges against synthetic servers.
- Helm lint and rendered manifests for both authentication modes, plus GitHub Actions workflow syntax.
- Container builds for Linux amd64 and arm64.

See [contributing](../CONTRIBUTING.md#local-checks) for local commands. Tests require no Compute credentials and do not reboot live nodes. Pull-request jobs have read-only repository access and do not publish packages.

## Publish triggers

The [publishing workflow](../.github/workflows/publish.yaml) starts after a push to `main` or a `v*` tag. It first runs the reusable CI workflow, then publishes only if validation succeeds.

| Trigger | Image tags | Helm chart |
| --- | --- | --- |
| `main` push | `edge` and `sha-<short commit>` | Not published |
| `vMAJOR.MINOR.PATCH` tag | `MAJOR.MINOR.PATCH` and `sha-<short commit>` | Matching version |

Package locations:

- Image: `ghcr.io/mistralai/nvsentinel-provider-mistral`
- OCI chart: `oci://ghcr.io/mistralai/charts/nvsentinel-provider-mistral`

Tags must use the exact `vMAJOR.MINOR.PATCH` form, and both `version` and `appVersion` in [Chart.yaml](../charts/nvsentinel-provider-mistral/Chart.yaml) must match without the leading `v`. The workflow rejects mismatches and prerelease tags.

Publishing uses GitHub's `GITHUB_TOKEN` with `contents: read` and `packages: write`. It does not use the provider's Compute identity. GHCR manages package access separately from repository access; restricted packages require read access for installers and cluster pull identities.

## Release procedure

1. Update both chart versions in a pull request, including any documentation and compatibility changes.
2. Merge after the checks and review pass.
3. Create and push the matching tag from the intended commit on `main`. This example uses `0.1.0`; substitute the chosen version throughout:

   ```sh
   git switch main
   git pull --ff-only
   git tag -a v0.1.0 -m 'Release v0.1.0'
   git push origin v0.1.0
   ```

4. Wait for **Publish packages** to succeed, then verify both artifacts:

   ```sh
   docker buildx imagetools inspect ghcr.io/mistralai/nvsentinel-provider-mistral:0.1.0
   helm show chart oci://ghcr.io/mistralai/charts/nvsentinel-provider-mistral --version 0.1.0
   ```

5. Record the image digest for deployment. Announce the version after its image and chart are both available. Create a GitHub Release separately if release notes are needed; the workflow publishes packages only.

Do not move an existing version tag. `edge` is a development tag; production installations should pin an immutable image digest.

## Build provenance and maintenance

BuildKit publishes build provenance and an SBOM with each multiarchitecture image. Separate image signatures and signed Helm provenance files are not configured.

Actions are pinned to commit SHAs and base images to digests. [Dependabot](../.github/dependabot.yml) proposes weekly GitHub Actions, Docker, and Go-module updates. Review those updates through the same CI before merging.
