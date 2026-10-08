# Integration with NVSentinel

NVSentinel v1.26.0 supports a per-controller CSP provider address. Configure `janitor.config.controllers.rebootNode.cspProviderHost` to use this service. Keep the existing quarantine, drain, and RebootNode workflow. The provider accepts requests only from the exact configured Janitor ServiceAccount, verified through Kubernetes TokenReview with the configured audience.

## Routing

The [example](nvsentinel-values.yaml) routes reboots to this provider and disables the termination controller and `REPLACE_VM` action. This is an explicit choice for a reboot-only installation. The provider returns `Unimplemented` for termination and other unsupported CSP methods.

Existing `COMPONENT_RESET` policy remains unchanged by the example. The stock v1.26.0 fault-remediation values map that action to `RebootNode`; deployments can override it with `GPUReset`. Review the effective values so the physical action matches your policy.

No health-event webhook directly reboots a machine. Janitor calls this provider after its normal maintenance workflow. The provider requires a live matching RebootNode, a cordoned Node with a nonempty boot ID, and an administrator-managed protected provider-name label. It pins the RebootNode to the Node UID before submitting the operation. Deleting and recreating a Node with the same name does not authorize the old request to act on its replacement.

## Trusted node mapping

Enable the Kubernetes Node authorizer and NodeRestriction admission plugin before installing the provider. The default mapping key is `nvsentinel.mistral.ai.node-restriction.kubernetes.io/node-name`. NodeRestriction prevents kubelets from adding, removing, or changing labels in `node-restriction.kubernetes.io` or any subdomain of that namespace. Without that admission protection, a compromised worker could redirect its reboot request to another machine in the configured Compute scope.

Only trusted administrators or provisioning controllers should populate the mapping from the verified Kubernetes-to-Compute association. Both runtime configuration and the chart reject unprotected `NODE_NAME_LABEL` / `nodeNameLabel` overrides. Custom keys must use the exact `node-restriction.kubernetes.io` namespace or a domain ending in `.node-restriction.kubernetes.io`. The provider validates the key but cannot determine whether the cluster has enabled NodeRestriction; that is an installation prerequisite.

## TLS trust

The server certificate must include the exact hostname used in `cspProviderHost`. With the default release namespace and cluster domain, that name is `nvsentinel-provider-mistral.nvsentinel.svc.cluster.local`. Set the chart's `clusterDomain` if your cluster uses another domain. The chart creates both the `.svc` and full cluster-domain SANs.

Use a CA-backed cert-manager `Issuer` or `ClusterIssuer` whose CA Janitor trusts. The chart references an existing issuer; it does not bootstrap an untrusted self-signed serving certificate. For an external certificate, set `tls.certManager.enabled=false` and `tls.existingSecret`. That Secret must contain `tls.crt` and `tls.key`.

Janitor's CA Secret must contain `ca.crt` and exist in **Janitor's namespace**. A cert-manager certificate issued by a CA Issuer normally has this key in its generated Secret. Otherwise provision a separate Secret containing your approved public CA bundle. The private key does not need to be copied to Janitor.

NVSentinel v1.26.0 has one shared `janitor.config.cspProvider.tls.caSecretName`, even when controllers use different provider hosts. If other controllers retain another provider, the CA bundle must trust every provider they use, or all serving certificates must chain to a shared CA. A new private CA used only for this provider can otherwise break those connections. Keep certificate verification enabled.

To create a dedicated CA with cert-manager, an administrator can apply [this optional bootstrap example](provider-ca.yaml). It creates a self-signed **CA**, then a CA Issuer for leaf certificates. It is suitable when the CA is dedicated to this connection and its public certificate is distributed deliberately. An existing organizational CA is preferable where available.

Certificate files are mounted as directories, allowing Kubernetes Secret rotation. The provider reloads its certificate for new TLS handshakes. Janitor may require a rollout to load a changed CA bundle; check that before CA rotation. The Compute credential file is also reread, so rotating the mounted Secret does not require embedding credentials in the deployment.

## Permissions and network

The Compute credential requires project-scoped `nodes` read/update permissions and region access. Prefer a dedicated service account bound to the intended project. The provider sends the credential only in the HTTPS Authorization header.

The Kubernetes ServiceAccount can get Nodes; get and patch RebootNodes; create TokenReviews; and get, create, and update ConfigMaps in the release namespace. It cannot delete Nodes, cordon or drain them, read arbitrary Secrets, or delete its journal. Secret data arrives through pod volumes.

The provider needs egress to the Kubernetes API and the configured Compute HTTPS endpoint. Janitor needs access to provider TCP 50051. The ClusterIP service exposes only gRPC; HTTP `/healthz` and `/readyz` are pod probes on port 8080. Integrate these requirements into your existing network policy.

## Runtime limits

The chart uses `timeouts.httpSeconds=30` and `timeouts.rpcSeconds=45`. RPC timeout must exceed HTTP timeout; both are limited to 300 seconds. The chart derives a termination grace period of at least 60 seconds and at least RPC timeout plus 10 seconds. Janitor's maintenance timeout is separate and should allow the entire machine reboot and node recovery.

The provider's readiness probe checks process readiness, not the success of a pending node reboot. Janitor's `IsNodeReady` RPC handles the latter.
