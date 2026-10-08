# NVSentinel provider for Mistral Compute

An independent NVSentinel CSP provider that reboots Mistral Compute bare-metal nodes through the Compute REST API. Install it alongside NVSentinel; no NVSentinel fork is required.

This initial implementation supports `SendRebootSignal` and `IsNodeReady`. Other CSP RPCs return `Unimplemented`. Breakfix integration is a separate future addition.

```text
NVSentinel quarantine + drain
  → RebootNode → Janitor
  → authenticated TLS gRPC → this provider
  → Compute reboot API
  → changed Kubernetes boot ID + Ready → Janitor completes
```

The provider requires an administrator-managed, protected Node label mapping the Kubernetes Node to its Compute bare-metal host name. It verifies the live RebootNode request, the cordoned Node, and its immutable UID before acting. A durable Kubernetes ConfigMap reservation precedes the REST request, so retries and replicas adopt the same request. This prevents automatic repeat submissions on the same boot; it does **not** provide exactly-once delivery. A crash or ambiguous HTTP outcome can require operator recovery.

## Install

Use NVSentinel **v1.26.0** for the configuration documented here, an existing Kubernetes cluster, and a Compute service-account credential allowed to read and update nodes in the selected project and region. The deployment also needs a CA-issued TLS certificate trusted by Janitor. cert-manager can issue it, or you can supply an existing TLS Secret.

The cluster **must use the Node authorizer and enable the NodeRestriction admission plugin**. The protected label namespace prevents a compromised kubelet from changing its mapping to reboot unrelated hardware. Confirm these API-server settings before installation; the provider cannot verify admission configuration. See [Kubernetes node isolation guidance](https://kubernetes.io/docs/concepts/scheduling-eviction/assign-pod-node/#node-isolation-restriction).

The repository, image, and chart start private. Creating or publishing packages does not make them public. Provision GitHub package read access and an image pull Secret before installation.

1. Create an existing Secret named `compute-api-token`, with key `token`, from your secret manager or a protected token file. Do not put the token in Helm values or Git.

   ```sh
   kubectl -n nvsentinel create secret generic compute-api-token \
     --from-file=token=/secure/compute-token
   ```

2. Set the API URL, project, region, issuer, and private registry Secret in a copy of [the provider values example](docs/provider-values.yaml). Its example endpoint is deliberately non-routable.

3. Authenticate Helm to GHCR using a credential with `read:packages`. The cluster also needs its own image pull Secret, such as `ghcr-pull`, containing a Docker registry configuration for an identity allowed to read the image. These credentials are separate from the Compute credential.

   ```sh
   printf '%s' "$GHCR_READ_TOKEN" | helm registry login ghcr.io \
     --username "$GHCR_USER" --password-stdin
   kubectl -n nvsentinel create secret generic ghcr-pull \
     --type=kubernetes.io/dockerconfigjson \
     --from-file=.dockerconfigjson=/secure/ghcr-config.json
   ```

4. Install a published version. Before the first release, use `./charts/nvsentinel-provider-mistral` as the chart path and add `--set image.tag=edge` after the main-branch image has finished publishing.

   ```sh
   helm upgrade --install nvsentinel-provider-mistral \
     oci://ghcr.io/mistralai/charts/nvsentinel-provider-mistral \
     --version 0.1.0 --namespace nvsentinel \
     --values values.local.yaml --wait
   ```

5. As a cluster administrator, verify each node mapping before adding the required protected label. Compute nodepool details expose `status.machines[].nodeName` and `bareMetalHostName`; use that association. The Compute `Node.id` and the Kubernetes `spec.providerID` are different identifiers.

   ```sh
   kubectl label node <kubernetes-node-name> \
     nvsentinel.mistral.ai.node-restriction.kubernetes.io/node-name=<verified-compute-baremetalhost-name>
   ```

6. Merge [the NVSentinel values example](docs/nvsentinel-values.yaml) into your existing, version-pinned release configuration. Review [TLS and routing](docs/integration.md) first. Installing this chart alone does not redirect Janitor.

Keep one installation and one journal namespace for each set of Kubernetes Nodes. Multiple replicas of that installation share the journal safely. Preserve the same namespace and ConfigMaps across upgrades and reinstalls.

## Configuration and operations

- [Integration, TLS, routing, and permissions](docs/integration.md)
- [Recovery, journal retention, and troubleshooting](docs/operations.md)
- [Release and package publishing](docs/releasing.md)
- [Helm defaults](charts/nvsentinel-provider-mistral/values.yaml)

The Compute contract is an HTTPS `POST /v1/projects/{project}/nodes/{bareMetalHostName}/reboot?region={region}` with a Bearer credential and no body. HTTP 204 acknowledges initiation only. Completion requires the original Kubernetes Node UID, a different nonempty boot ID, and `Ready=True`. The provider does not treat `poweredOn` or an accepted HTTP request as proof of a completed reboot.

## Develop and verify

Use Go 1.27.0, Helm 3.18.6+, Python 3, and a container builder. Tests use synthetic credentials and an in-process HTTP server; envtest starts an isolated local API server. They do not contact Compute or reboot real nodes.

```sh
python3 -m venv .venv
.venv/bin/pip install -r charts/tests/requirements.txt
make PYTHON=.venv/bin/python check
make build
docker build -t nvsentinel-provider-mistral:dev .
```

`make check` runs formatting, vet, race-enabled Go tests, real API-server concurrency tests with pinned envtest assets, and Helm rendering checks. `make test` runs the Go suite; integration tests need `KUBEBUILDER_ASSETS` and are otherwise skipped. `make test-integration` installs and selects those assets explicitly.

Runtime configuration uses `COMPUTE_ENDPOINT`, `COMPUTE_PROJECT`, `COMPUTE_REGION`, `COMPUTE_TOKEN_FILE`, `POD_NAMESPACE`, `TLS_CERT_FILE`, and `TLS_KEY_FILE`. Optional values are `NODE_NAME_LABEL`, `AUTH_AUDIENCE`, `AUTH_SERVICE_ACCOUNT`, `LISTEN_ADDRESS`, `HEALTH_ADDRESS`, `HTTP_TIMEOUT`, and `RPC_TIMEOUT`. For local testing, `KUBECONFIG` selects the test API server. TLS and service-account authentication remain required.

Licensed under [Apache 2.0](LICENSE).
