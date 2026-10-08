# Install with Kubernetes OIDC workload identity

[README](../README.md) · [Permissions](permissions.md) · [Configuration](configuration.md)

This guide installs the provider and connects NVSentinel **v1.26.0** to Mistral Compute. OIDC workload identity avoids a long-lived Compute API key in the cluster. It requires a Compute deployment that supports service-account OIDC trusts and OAuth2 token exchange.

## 1. Check prerequisites

You need:

- Kubernetes with NVSentinel v1.26.0 and its `RebootNode` CRD installed.
- The **Node authorizer and NodeRestriction admission plugin** enabled. The provider relies on them to protect the node-to-machine mapping label.
- A Kubernetes ServiceAccount issuer whose discovery document and JWKS are reachable from Compute over **public HTTPS**, with publicly resolvable addresses and valid certificates. Projected tokens must be unexpired, signed with **RS256**, and include a signing-key `kid`.
- A Compute project/region containing the target bare-metal hosts and the [bootstrap permissions](permissions.md#bootstrap-permissions) to create the role, account, and trust.
- A CA-issued provider certificate trusted by Janitor. Use an existing cert-manager issuer or an existing TLS Secret.
- Helm 3.18.6+, `kubectl`, `curl`, and `jq` on the installer machine.

Inspect the cluster's discovery metadata:

```sh
kubectl get --raw /.well-known/openid-configuration \
  | jq '{issuer, jwks_uri, id_token_signing_alg_values_supported}'
```

Use the exact issuer; do not replace it with the Kubernetes API address or a convenient alias. A successful `kubectl` read only proves access from your machine. Compute must independently retrieve discovery and signing keys. Issuers such as `https://kubernetes.default.svc` and endpoints resolving to private addresses are rejected. If the issuer is unsuitable, ask the cluster administrator to provision a supported public issuer/discovery setup, or use the [API-key alternative](#alternative-use-an-api-key). Publishing issuer metadata does not require making authenticated Kubernetes API operations public.

For a public issuer, verify discovery from a network comparable to Compute's:

```sh
curl --fail --silent --show-error \
  "$KUBERNETES_ISSUER/.well-known/openid-configuration" \
  | jq '{issuer, jwks_uri}'
```

Set `KUBERNETES_ISSUER` to the inspected value before this command. Maintain public JWKS availability and key rotation for the lifetime of the trust.

## 2. Create the Compute identity

Follow [Create the Compute role and account](permissions.md#create-the-compute-role-and-account), then [Create an OIDC trust](permissions.md#create-an-oidc-trust). The defaults in this guide are:

| Setting | Value |
| --- | --- |
| Provider namespace | `nvsentinel` |
| Provider Kubernetes ServiceAccount | `nvsentinel-provider-mistral` |
| Compute service account | `nvsentinel-provider` |
| Compute role | `nvsentinel-rebooter`, scoped to the project |
| Compute role permission | `nodes: {level: update, scope: all}` |
| Trust subject | `system:serviceaccount:nvsentinel:nvsentinel-provider-mistral` |
| Workload-token audience | `mistral-compute` |
| Trust purpose | `api` |

Keep the returned `oidc-trust.json`; its `clientId` and `tokenUrl` go into the provider configuration. No client secret is issued or needed. Reuse the same namespace and Kubernetes ServiceAccount name across upgrades, or update the trust deliberately.

## 3. Prepare the namespace and TLS

Create the namespace if it does not already exist:

```sh
kubectl create namespace nvsentinel --dry-run=client -o yaml | kubectl apply -f -
```

<details>
<summary>Registry authentication for restricted packages</summary>

If the package requires authentication, use a GHCR credential with package read access for Helm and a separate image-pull Secret for the cluster:

```sh
printf '%s' "$GHCR_READ_TOKEN" | helm registry login ghcr.io \
  --username "$GHCR_USER" --password-stdin

kubectl -n nvsentinel create secret generic ghcr-pull \
  --type=kubernetes.io/dockerconfigjson \
  --from-file=.dockerconfigjson=/secure/ghcr-config.json
```

Populate these credentials using your secret manager. The Docker configuration must contain credentials for an identity allowed to read the image. Set `imagePullSecrets: [{name: ghcr-pull}]` in your values. Helm login does not authenticate Kubernetes image pulls; registry credentials are separate from Compute workload identity.

</details>

Use your existing organizational CA-backed issuer where possible. If you need a dedicated CA and have cert-manager installed, the [optional CA bootstrap](provider-ca.yaml) creates a CA and issuer in `nvsentinel`:

```sh
kubectl apply -f docs/provider-ca.yaml
```

This example assumes you have cloned the repository. Read [TLS trust and CA distribution](integration.md#tls-trust) before applying it, especially if Janitor also talks to another CSP provider. Janitor v1.26.0 uses a shared CA bundle across its provider connections.

An existing certificate is also supported. Its Secret must contain `tls.crt` and `tls.key`; the certificate SAN must cover the provider's service DNS. Janitor needs the public CA bundle in a Secret containing `ca.crt` in **Janitor's namespace**.

## 4. Install the provider

Copy the full example and fill in the API endpoint, project, region, trust metadata, issuer, and exact Janitor ServiceAccount identity:

```sh
cp docs/provider-values.yaml values.local.yaml
```

The important OIDC settings are:

```yaml
compute:
  endpoint: https://compute.example.invalid
  project: example
  region: example-region
  auth:
    mode: workloadIdentity
    workloadIdentity:
      clientId: "<oidc-trust.json clientId>"
      tokenUrl: "<oidc-trust.json tokenUrl>"
      audience: mistral-compute
      expirationSeconds: 3600
  tokenSecret:
    name: ""

serviceAccount:
  name: nvsentinel-provider-mistral

timeouts:
  rpcSeconds: null
```

The chart projects a dedicated, renewable token into `/var/run/secrets/compute-identity/token`. Its subject is the provider ServiceAccount, and its audience must match the Compute trust. The Kubernetes API token and Janitor's inbound token are separate credentials with different purposes.

After choosing a **published** version, install its OCI chart:

```sh
export PROVIDER_VERSION=0.1.0 # Use a version whose image and chart are published.

helm upgrade --install nvsentinel-provider-mistral \
  oci://ghcr.io/mistralai/charts/nvsentinel-provider-mistral \
  --version "$PROVIDER_VERSION" \
  --namespace nvsentinel \
  --values values.local.yaml --wait
```

For production, select an immutable image digest with `image.digest`.

Check the deployment:

```sh
kubectl -n nvsentinel rollout status deployment/nvsentinel-provider-mistral
kubectl -n nvsentinel logs deployment/nvsentinel-provider-mistral --tail=50
```

Pod readiness confirms process readiness. It does not prove a successful OIDC exchange or permission to reboot; perform the next check before enabling routing.

## 5. Verify OIDC without rebooting a node

This check mints a short-lived token for the same ServiceAccount and audience, exchanges it, and reads effective permissions. Run it as an installer allowed to create `serviceaccounts/token` for the provider account. It creates no reboot request.

Set `COMPUTE_API_URL` and `COMPUTE_PROJECT` to your values; use the `oidc-trust.json` returned during setup. Run this block in Bash. Temporary credentials are kept in an owner-only directory and removed on exit; do not enable shell tracing.

```bash
(
  set -euo pipefail
  umask 077
  verify_dir=$(mktemp -d)
  trap 'rm -rf "$verify_dir"' EXIT

  client_id=$(jq -er '.clientId' oidc-trust.json)
  token_url=$(jq -er '.tokenUrl' oidc-trust.json)

  kubectl -n nvsentinel create token nvsentinel-provider-mistral \
    --audience=mistral-compute --duration=10m \
    | tr -d '\n' > "$verify_dir/subject.jwt"

  curl --fail --silent --show-error \
    --header 'Content-Type: application/x-www-form-urlencoded' \
    --data-urlencode 'grant_type=urn:ietf:params:oauth:grant-type:token-exchange' \
    --data-urlencode "client_id=$client_id" \
    --data-urlencode 'subject_token_type=urn:ietf:params:oauth:token-type:jwt' \
    --data-urlencode "subject_token@$verify_dir/subject.jwt" \
    "$token_url" > "$verify_dir/exchange.json"

  jq -er 'select(.token_type | ascii_downcase == "bearer") |
    .access_token | select(type == "string" and length > 0) |
    "Authorization: Bearer " + .' \
    "$verify_dir/exchange.json" > "$verify_dir/compute.header"

  curl --fail --silent --show-error \
    --header "@$verify_dir/compute.header" \
    --get --data-urlencode "project=$COMPUTE_PROJECT" \
    "$COMPUTE_API_URL/v1/me/permissions" \
    | jq '{subject, permissions}'
)
```

If you changed the namespace, ServiceAccount name, or audience, update them in this check too. Expect subject `sa:nvsentinel-provider` and a permission with `resourceType: "node"`, `action: "update"`, and `resourceIds: ["*"]`. A successful exchange proves authentication; that permission entry confirms project access. Neither result validates the machine mapping or proves an actual reboot can complete.

At runtime, the provider exchanges the projected JWT using the same form fields. It caches the returned access token in memory, refreshes before expiry, and rereads the projected token when refreshing. No refresh token or client secret is used.

## 6. Map Kubernetes Nodes to Compute hosts

As a trusted cluster administrator, get the actual association from Compute nodepool details: `status.machines[].nodeName` corresponds to the Kubernetes Node, and `bareMetalHostName` is the reboot API target. Do not infer the target from `Node.id`, `spec.providerID`, or a similar hostname.

After checking the association, add the protected label:

```sh
kubectl label node <kubernetes-node-name> \
  nvsentinel.mistral.ai.node-restriction.kubernetes.io/node-name=<verified-baremetalhost-name>
```

NodeRestriction prevents kubelets from changing this mapping. The provider validates the label's protected namespace but cannot detect whether admission protection is enabled. See [trusted node mapping](integration.md#trusted-node-mapping).

## 7. Connect Janitor

Merge [the NVSentinel values example](nvsentinel-values.yaml) into your existing version-pinned NVSentinel release configuration. It sets:

- `janitor.config.controllers.rebootNode.cspProviderHost` to the provider service.
- TLS verification and Janitor's trusted CA Secret.
- Janitor's projected token audience to match the provider's **inbound** `auth.audience`.
- The termination controller disabled and `REPLACE_VM` removed, because this provider implements reboot only.

Use `helm get values` to inspect the current NVSentinel release before applying your merged configuration through your normal deployment process. The default provider expects caller `system:serviceaccount:nvsentinel:janitor`; inspect the actual Janitor workload's `serviceAccountName` and namespace, and set `auth.serviceAccount` accordingly. The Compute trust must continue to name the provider ServiceAccount.

Review existing `COMPONENT_RESET` policy as described in [routing](integration.md#routing). Install the provider and complete the identity/mapping checks before routing maintenance requests to it. A real end-to-end reboot should be a planned maintenance exercise on a chosen test node.

## Alternative: use an API key

The same project role and Compute service account can authenticate with an API key when a supported OIDC issuer is unavailable. Create the key using your Compute administrator client; the project endpoint is:

```http
POST /v1/projects/{project}/service-accounts/nvsentinel-provider/api-keys
Content-Type: application/json

{"name":"nvsentinel-provider","expiresAt":"<RFC3339 UTC expiry within organization policy>"}
```

The plaintext credential is returned once as `.key`. Store it through your secret manager in an owner-only file, then create an existing Kubernetes Secret:

```sh
kubectl -n nvsentinel create secret generic compute-api-token \
  --from-file=token=/secure/compute-token
```

Use [the API-key values example](provider-values-api-key.yaml). It selects `compute.auth.mode: apiKey` and references that Secret; the runtime rereads its file before requests so Secret rotation is supported. No OIDC trust or public Kubernetes issuer is needed for this mode.

When switching modes, clear the unused credential configuration. For API key → OIDC, set `compute.tokenSecret.name: ""` and `timeouts.rpcSeconds: null` (or a sufficiently large explicit timeout). For OIDC → API key, clear `clientId`, `tokenUrl`, and `audience`. The chart rejects mixed credentials. Keep the same namespace, project, region, endpoint, node mapping, and journals across the switch.

## Troubleshooting and next steps

For OIDC, check the exact issuer, RS256 key, audience, ServiceAccount subject, enabled trust with `purpose: api`, and project role binding. If any changed after installation, update the corresponding configuration deliberately. Trust revocation stops future exchanges; an already-issued access token may remain valid until expiry.

Read [operations and recovery](operations.md) before enabling automation. Credential exchange failure before a new reservation can be retried. After a reservation exists, any ambiguous outcome—including a revoked cached access token rejected by the reboot endpoint—requires the documented recovery path and must not trigger an automatic second reboot.
