# NVSentinel provider for Mistral Compute

Reboot Compute nodes from an existing NVSentinel v1.26.0 installation through its Janitor plugin interface.

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

## Install the provider

Save this as `provider-values.yaml`. Replace the `<...>` values with your Compute settings, the trust's returned `clientId` and `tokenUrl`, and an existing CA-backed cert-manager `ClusterIssuer`:

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

tls:
  certManager:
    issuerRef:
      name: <your-ca-clusterissuer>

auth:
  serviceAccount: system:serviceaccount:nvsentinel:janitor
```

The example uses namespace `nvsentinel`, provider ServiceAccount `nvsentinel-provider-mistral`, and Janitor ServiceAccount `janitor`. If yours differ, update the OIDC subject, `auth.serviceAccount`, and service address below. No client secret is needed.

```sh
helm upgrade --install nvsentinel-provider-mistral \
  oci://ghcr.io/mistralai/charts/nvsentinel-provider-mistral \
  --version 0.1.1 --namespace nvsentinel \
  --values provider-values.yaml --wait
```

## Connect NVSentinel

As a cluster administrator, label each Kubernetes node with its verified Compute `bareMetalHostName`. The cluster must enable the Node authorizer and NodeRestriction admission plugin:

```sh
kubectl label node <kubernetes-node> \
  nvsentinel.mistral.ai.node-restriction.kubernetes.io/node-name=<bareMetalHostName>
```

With NVSentinel's quarantine, drain, and fault-remediation workflow enabled, merge this into your **existing NVSentinel Helm values**, then upgrade that release using your usual deployment process:

```yaml
global:
  janitor:
    enabled: true

janitor:
  config:
    controllers:
      rebootNode:
        enabled: true
        cspProviderHost: nvsentinel-provider-mistral.nvsentinel.svc.cluster.local:50051
    cspProvider:
      tls:
        enabled: true
        insecure: false
        caSecretName: <your-janitor-ca-secret>
      auth:
        enabled: true
        audience: nvsentinel-csp-provider
```

The CA Secret must be in Janitor's namespace, with a `ca.crt` that trusts the provider's issuer **and any other providers Janitor uses**. Janitor's chart creates and mounts the authentication token automatically.

The chart allows Janitor traffic by default; for custom pod labels, set `networkPolicy.callerPodSelector.matchLabels`.

Existing `RebootNode` remediation now calls this provider after NVSentinel quarantines and drains the node. This provider supports reboot only; keep other actions routed to their existing providers.

See [Helm values](charts/nvsentinel-provider-mistral/values.yaml) for the remaining chart settings.

Licensed under the [Apache License 2.0](LICENSE).
