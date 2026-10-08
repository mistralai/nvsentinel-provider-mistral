# Contributing

Contributions are welcome through pull requests. Keep changes focused on the provider integration and preserve NVSentinel's maintenance workflow.

## Local checks

Install Go 1.27.0, Helm 3.18.6 or later, and Python 3, then run:

```sh
python3 -m venv .venv
.venv/bin/pip install -r charts/tests/requirements.txt
make PYTHON=.venv/bin/python check
make workflow-check
make build
```

`make check` downloads pinned envtest tooling and Kubernetes assets, then runs formatting, module consistency, vet, race tests, real API-server tests, and chart checks. `make test` is a quicker Go-only loop; API-server tests are skipped unless `KUBEBUILDER_ASSETS` is set. `make test-integration` selects those assets explicitly.

For packaging changes, also build the container:

```sh
docker buildx build --platform linux/amd64,linux/arm64 .
helm package charts/nvsentinel-provider-mistral --destination dist
```

## Changes to authentication or reboot handling

Cover meaningful failure cases, including token expiry and rotation, rejected exchanges, interrupted requests, and concurrent callers. Use synthetic credentials and local servers; tests must not reboot live nodes.

Preserve the reservation-before-submission guarantee. Credential preparation may run before a new reservation; a reboot request must never be retried automatically after submission is possible. Existing reservations must remain usable during an identity-provider outage.

Update the installation/configuration examples when values or permissions change. Keep the chart, runtime validation, and documentation consistent. Explain the user-visible behavior and relevant validation in the PR description.

See [CI and releases](docs/releasing.md) for package publishing and release steps.
