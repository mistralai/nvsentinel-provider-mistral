# CI, packages, and releases

[README](../README.md) · [Contributing](../CONTRIBUTING.md) · [Install](installation.md)

The repository and packages are private during initial development. Workflows do not change visibility. Administrators must grant intended consumers access under the organization's package policy; package publication alone does not make a package public.

## Pull-request checks

The reusable [CI workflow](../.github/workflows/ci.yaml) runs on every pull request and is called again before publishing. Jobs use pinned Actions on Ubuntu 24.04 with a 20-minute timeout.

| Check | What it verifies | Local command |
| --- | --- | --- |
| Formatting | Go source is formatted | `make fmt-check` |
| Module consistency | Dependencies match imports without modifying module files | `make mod-check` |
| Static analysis | Go vet diagnostics | `make vet` |
| Go and integration tests | Race-enabled tests, including a local Kubernetes API server and pinned NVSentinel CRD | `make test-integration` |
| OIDC tests | Exchange contract, expiry/rotation, concurrent refresh, cancellation, malformed responses, redirects, and no reboot retry | Included in Go tests |
| Helm | Strict lint in both auth modes plus rendered manifests, RBAC, projections, and validation failures | `make PYTHON=.venv/bin/python chart-test` |
| Workflow lint | GitHub Actions syntax and expressions using pinned actionlint | `make workflow-check` |
| Container | Build Linux amd64 and arm64 | `docker buildx build --platform linux/amd64,linux/arm64 .` |

Run `make PYTHON=.venv/bin/python check workflow-check` for all non-container checks after installing the Python dependency described in [contributing](../CONTRIBUTING.md). CI also rejects dependency drift left in `go.mod` or `go.sum`.

Tests use synthetic credentials and local servers. CI has no Compute credentials and never initiates a real node reboot. Pull-request jobs receive read-only repository access and do not publish packages. Additional organization security checks may appear on a PR; they are managed outside this repository.

## Publish triggers

The [publishing workflow](../.github/workflows/publish.yaml) starts after a push to `main` or a `v*` tag. It first runs the reusable CI workflow, then publishes only if validation succeeds.

| Trigger | Image tags | Helm chart |
| --- | --- | --- |
| `main` push | `edge` and `sha-<short commit>` | Not published |
| `vMAJOR.MINOR.PATCH` tag | `MAJOR.MINOR.PATCH` and `sha-<short commit>` | Matching version |

Package locations:

- Image: `ghcr.io/mistralai/nvsentinel-provider-mistral`
- OCI chart: `oci://ghcr.io/mistralai/charts/nvsentinel-provider-mistral`

Tags must use the exact `vMAJOR.MINOR.PATCH` form, and both `version` and `appVersion` in [Chart.yaml](../charts/nvsentinel-provider-mistral/Chart.yaml) must match without the leading `v`. The workflow rejects a mismatch or a prerelease tag. The initial chart version is `0.1.0`; that value does not by itself mean a release exists.

Publishing uses GitHub's `GITHUB_TOKEN` with `contents: read` and `packages: write`. The Compute OIDC identity belongs to the deployed provider and is not used by GitHub Actions. No Compute secret or OIDC client secret belongs in repository Actions secrets.

## Release procedure

1. Update both chart versions in a reviewed PR, with any release documentation and compatibility changes.
2. Run the checks above and merge through the repository's normal review rules.
3. From the intended reviewed commit on `main`, create and push the matching tag:

   ```sh
   git switch main
   git pull --ff-only
   git tag -a v0.1.0 -m 'Release v0.1.0'
   git push origin v0.1.0
   ```

4. Wait for **Publish packages** to succeed. Verify both artifacts are accessible with the intended consumer identity:

   ```sh
   docker buildx imagetools inspect ghcr.io/mistralai/nvsentinel-provider-mistral:0.1.0
   helm show chart oci://ghcr.io/mistralai/charts/nvsentinel-provider-mistral --version 0.1.0
   ```

5. Record the image digest for deployment. Advertise the version only after its image and chart are both available. A GitHub Release page may be created separately; the current workflow publishes packages and does not create that page.

Substitute the actual new version throughout. Avoid moving an existing version tag. `edge` is a development tag; production installations should pin an immutable image digest.

## Build provenance and maintenance

BuildKit publishes image provenance and an SBOM with the multiarchitecture image. These describe the container build; the Helm archive is versioned OCI content and is not advertised as a signed Helm provenance file. Image signing with a separate identity is not configured.

Actions are pinned to commit SHAs and base images to digests. [Dependabot](../.github/dependabot.yml) proposes weekly GitHub Actions, Docker, and Go-module updates. Review those updates through the same CI before merging.

## Private access and public launch

During private development, grant image and chart read access to intended installers and cluster pull identities. The publishing repository must have package write access under organization policy. A denied publish or pull is an access problem; it does not require a Compute credential.

When public launch is authorized, change the repository through the organization's GitOps process and review the visibility of **both** GHCR packages separately. Existing package visibility does not necessarily follow repository visibility. Until then, keep all three private.
