# Releases and private packages

The repository and packages are private initially. The workflows do not change repository or package visibility. Administrators must grant intended consumers and the repository's `GITHUB_TOKEN` access under the organization's package policy. A public release is a separate decision.

Pull requests run formatting, vet, race-enabled unit and real Kubernetes API-server tests, Helm lint/render assertions, and an amd64/arm64 container build without publishing. Main-branch and tag pushes run the same checks before publishing.

`main` publishes an `edge` image and a commit-based image tag. A tag of the form `vMAJOR.MINOR.PATCH` publishes a versioned image and Helm chart. Both `version` and `appVersion` in `Chart.yaml` must equal the tag without `v`; the workflow rejects a mismatch. The initial version is `0.1.0`.

Package locations:

- Image: `ghcr.io/mistralai/nvsentinel-provider-mistral`
- OCI chart: `oci://ghcr.io/mistralai/charts/nvsentinel-provider-mistral`

To prepare a release, update both chart versions in a reviewed change, run `make check`, merge through the repository's normal review process, then create the matching version tag. The image and chart must both finish publishing before installation instructions advertise that version. Use an immutable image digest for production rollouts.

GitHub Actions references and base images are pinned by digest/commit. Dependabot proposes updates. Publishing uses `GITHUB_TOKEN` with `packages: write`; pull-request jobs receive read-only repository access. BuildKit publishes image provenance and SBOM attestations. These attestations describe the container build; the Helm archive is versioned OCI content and is not advertised as a signed chart provenance file.

Changing a repository from private to public does not guarantee that an existing GHCR package changes visibility. Review repository, image, and chart package visibility independently when a public launch is authorized.
