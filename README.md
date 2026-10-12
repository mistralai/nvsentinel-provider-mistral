# NVSentinel provider for Mistral Compute

Reboot Compute nodes from an existing NVSentinel installation.

## Required role

Create a role **in the target project** (`POST /v1/projects/<project>/roles`) with this permission, then assign it to a service account in that project:

```yaml
nodes:
  level: update
  scope: all
```

## OIDC setup

Add an OIDC trust to that service account:

| Setting | Value |
| --- | --- |
| Issuer | Your cluster's public HTTPS OIDC issuer (RS256); discovery and signing keys must be reachable by Compute |
| Audience | `mistral-compute` |
| Subject (`sub`, exact match) | `system:serviceaccount:nvsentinel:nvsentinel-provider-mistral` |
| Purpose | `api` |

Copy the trust's returned `clientId` and `tokenUrl` into the provider's Helm values:

```yaml
compute:
  endpoint: https://<compute-api>
  project: <project>
  region: <region>
  auth:
    mode: workloadIdentity
    workloadIdentity:
      clientId: <trust-client-id>
      tokenUrl: <trust-token-url>
      audience: mistral-compute
```

The example uses namespace `nvsentinel` and Kubernetes service account `nvsentinel-provider-mistral`. Adjust the trust's subject if you change either. No client secret is needed.

See [Helm values](charts/nvsentinel-provider-mistral/values.yaml) for the remaining chart settings.
