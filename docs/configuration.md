# Configuration reference

[OIDC installation](installation.md) · [Permissions](permissions.md) · [Helm defaults](../charts/nvsentinel-provider-mistral/values.yaml)

Use Helm values for a Kubernetes deployment. The chart validates authentication modes, required fields, protected label keys, TLS configuration, and timeout relationships before rendering. Runtime configuration is also validated when starting the binary directly.

## Compute authentication

| Helm value | Meaning / default |
| --- | --- |
| `compute.endpoint` | Required HTTPS API base URL; exclude `/v1/projects/...` |
| `compute.project` | Required project slug |
| `compute.region` | Required region |
| `compute.auth.mode` | `apiKey` (default, compatible with existing values) or `workloadIdentity` |
| `compute.auth.workloadIdentity.clientId` | Required in OIDC mode; public `clientId` returned by the trust |
| `compute.auth.workloadIdentity.tokenUrl` | Required in OIDC mode; exact HTTPS `tokenUrl` returned by the trust |
| `compute.auth.workloadIdentity.audience` | Required in OIDC mode; dedicated audience matching the trust, e.g. `mistral-compute` |
| `compute.auth.workloadIdentity.expirationSeconds` | Projected JWT lifetime, 600–3600 seconds; default `3600` |
| `compute.tokenSecret.name` | Existing Secret name, required in API-key mode and empty in OIDC mode |
| `compute.tokenSecret.key` | Secret key containing the API key; default `token` |
| `serviceAccount.name` | Kubernetes ServiceAccount created by the chart; defaults to the provider resource name |

Choose one mode. OIDC values and an API-key Secret cannot be combined. API-key mode rejects populated `clientId`, `tokenUrl`, or `audience`. `serviceAccount.name` and the release namespace determine the exact OIDC `sub`; update the Compute trust if either changes.

The default mode preserves existing installations. The [recommended example](provider-values.yaml) explicitly selects workload identity; [API-key values](provider-values-api-key.yaml) show the alternative.

### Token exchange and rotation

The chart mounts an audience-bound ServiceAccount JWT separately from the token used to reach the Kubernetes API. Kubelet renews the projected JWT. On demand, the provider posts this form to the trust's token URL:

```text
grant_type=urn:ietf:params:oauth:grant-type:token-exchange
client_id=<trust clientId>
subject_token_type=urn:ietf:params:oauth:token-type:jwt
subject_token=<projected Kubernetes JWT>
```

Compute verifies the issuer, signature, expiry, audience, and configured claim rules, then issues a bearer `access_token` with `expires_in`. The provider caches it only in memory and refreshes before expiry, reading the current projected JWT each time. Concurrent requests within one process share an in-flight refresh; separate replicas have separate caches. No client secret or refresh token is used.

Credential preparation happens before a **new** durable reservation. An exchange failure there can be retried without submitting a reboot. Once reserved, subsequent failures follow [journal recovery rules](operations.md#unconfirmed-outcomes). A 401 from the reboot endpoint never triggers an automatic refresh-and-retry of the reboot POST.

In API-key mode, the provider rereads the mounted credential before each request to observe Secret rotation. TLS serving certificates also reload for new handshakes. Mount whole Secret/projection directories; a `subPath` file mount would prevent Kubernetes rotation updates from appearing.

## Kubernetes, TLS, and routing

| Helm value | Meaning / default |
| --- | --- |
| `auth.serviceAccount` | Exact Janitor caller subject; default `system:serviceaccount:nvsentinel:janitor` |
| `auth.audience` | Inbound Janitor token audience; default `nvsentinel-csp-provider` |
| `tls.certManager.enabled` | Default `true`; references an existing CA-backed issuer |
| `tls.certManager.issuerRef` | Issuer `name`, `kind`, and `group` |
| `tls.existingSecret` | Use with cert-manager disabled; contains `tls.crt` and `tls.key` |
| `clusterDomain` | Kubernetes cluster DNS domain; default `cluster.local` |
| `nodeNameLabel` | Protected mapping key; default `nvsentinel.mistral.ai.node-restriction.kubernetes.io/node-name` |
| `image.tag` / `image.digest` | Image tag (defaults to chart appVersion) or immutable digest |
| `imagePullSecrets` | Existing image-pull Secret references for private GHCR access |
| `replicaCount` | Default `1`; replicas share the namespace's durable journal |
| `resources` | Default request: 100m CPU / 128Mi memory; memory limit: 256Mi |

`auth.*` controls who may call the provider. `compute.auth.*` controls how the provider calls Compute. These are independent settings. Janitor's audience must match `auth.audience`; the projected workload audience must match the Compute trust.

Provider service DNS must match its certificate SAN and Janitor's `cspProviderHost`. For a different namespace, custom release name, or custom `clusterDomain`, adjust [Janitor routing](nvsentinel-values.yaml) and CA distribution. See [integration](integration.md) for the NVSentinel v1.26.0 shared CA-bundle constraint.

## Timeouts

| Setting | API key | Workload identity |
| --- | --- | --- |
| `timeouts.httpSeconds` | 30s default | 30s default, per token-exchange or reboot HTTP call |
| `timeouts.rpcSeconds: null` | Selects 45s | Selects 75s |
| Explicit RPC timeout | Must exceed HTTP timeout | Must exceed twice HTTP timeout |
| Pod shutdown grace | At least 60s and RPC timeout + 10s | At least 60s and RPC timeout + 10s |

Each timeout must be positive and at most 300 seconds. The longer OIDC RPC default allows a token exchange followed by the reboot API request and journal bookkeeping. Janitor's node-maintenance timeout is separate; it must allow the full machine reboot and recovery.

When switching an old deployment to OIDC, clear an explicitly configured 45s RPC timeout with `timeouts.rpcSeconds: null`, or choose a valid longer value.

## Environment variables

The Helm chart supplies these automatically. Use them when running the binary directly:

| Variable | Requirement / default |
| --- | --- |
| `COMPUTE_ENDPOINT` | Required HTTPS API base URL |
| `COMPUTE_PROJECT`, `COMPUTE_REGION` | Required target scope |
| `COMPUTE_AUTH_MODE` | `apiKey` default or `workloadIdentity` |
| `COMPUTE_TOKEN_FILE` | Required only in API-key mode |
| `COMPUTE_OIDC_CLIENT_ID` | Required only in workload-identity mode |
| `COMPUTE_OIDC_TOKEN_URL` | Required only in workload-identity mode; explicit HTTPS token URL |
| `COMPUTE_OIDC_SUBJECT_TOKEN_FILE` | Required only in workload-identity mode; path to rotating JWT |
| `POD_NAMESPACE` | Required journal namespace |
| `TLS_CERT_FILE`, `TLS_KEY_FILE` | Required serving certificate/key files |
| `AUTH_SERVICE_ACCOUNT` | Exact Janitor ServiceAccount subject; default above |
| `AUTH_AUDIENCE` | `nvsentinel-csp-provider` |
| `NODE_NAME_LABEL` | Protected default key above |
| `HTTP_TIMEOUT` | `30s` |
| `RPC_TIMEOUT` | `45s` for API key; `75s` for workload identity |
| `LISTEN_ADDRESS` | `:50051` |
| `HEALTH_ADDRESS` | `:8080` |
| `KUBECONFIG` | Optional for local development; in-cluster credentials otherwise |

The workload audience is configured in the Kubernetes projection and Compute trust; it is not an extra environment variable or token-exchange form parameter. TLS verification and Janitor authentication are required in both modes.
