# Service accounts and permissions

[Installation guide](installation.md) · [Configuration](configuration.md)

There are three identities. Keep their names and permissions separate when configuring the installation.

| Identity | Example | Purpose |
| --- | --- | --- |
| Janitor Kubernetes ServiceAccount | `system:serviceaccount:nvsentinel:janitor` | Calls the provider over authenticated TLS gRPC |
| Provider Kubernetes ServiceAccount | `system:serviceaccount:nvsentinel:nvsentinel-provider-mistral` | Reads Kubernetes state, writes the journal, and supplies the OIDC subject token |
| Compute service account | `nvsentinel-provider` | Holds the project-scoped permission to update/reboot Compute nodes |

The Helm chart creates the provider's Kubernetes account and RBAC. An administrator creates the Compute account and OIDC trust separately. An OIDC trust authenticates a Kubernetes subject **as** that Compute account; the account's role bindings determine access.

## Runtime Compute permission

The reboot endpoint is:

```http
POST /v1/projects/{project}/nodes/{bareMetalHostName}/reboot?region={region}
Authorization: Bearer <Compute access token>
```

It requires **project-scoped `nodes` level `update`, scope `all`**. The `update` level includes lower levels such as `read`. The current API checks reboot against the project-wide node resource, so a restricted list of individual node IDs does not satisfy this permission.

This grants other node update operations in the same project as well. The current API has no separate `nodes.reboot` permission. Use a dedicated account with a single project binding to keep that scope explicit; it does not need access to clusters, secrets, users, service accounts, or role administration.

The provider sends a configured region on every reboot request. The project binding is the authorization boundary; configuring a region in this provider is not an additional restriction on the Compute credential itself.

## Bootstrap permissions

These belong to the installation operator, not to the running provider.

| Operation | Required operator access |
| --- | --- |
| Create the custom role | Organization-level `roles` create, plus permission to grant its node privileges; use an organization owner |
| Create the project service account | Project `service_accounts` create, plus all permissions being assigned; a project administrator can do this once the role exists |
| Create or change the account's OIDC trust/API key | Project `service_accounts` update, plus all permissions held by the target account |
| Install the Helm chart | Create namespace resources and cluster RBAC, with authority to bind the permissions below |
| Verify OIDC using `kubectl create token` | Create `serviceaccounts/token` for the provider ServiceAccount; this is an operator check, not an extra provider permission |
| Set machine mappings | Update protected Node labels as a trusted administrator/provisioning controller |

Compute prevents privilege escalation when roles or service-account credentials are created. Being an organization administrator alone does not guarantee permission to create a role containing project-node privileges. Keep the provider account bound only to its intended project; project credential-management endpoints reject accounts with bindings outside that project.

## Create the Compute role and account

Use your authenticated Compute administration client, or the equivalent REST requests below. Run these once, using existing objects on subsequent installations. Examples assume:

```sh
export COMPUTE_API_URL=https://compute.example.invalid
export COMPUTE_PROJECT=example
export COMPUTE_ADMIN_HEADER_FILE=/secure/compute-admin.header
```

Replace the example URL and project. The protected header file contains `Authorization: Bearer <administrator credential>` and should be readable only by its owner. It is used only for bootstrap; never mount it into the provider or commit it. Avoid shell tracing while handling credentials.

As an **organization owner**, create a reusable project role:

```sh
curl --fail-with-body --silent --show-error \
  --header "@$COMPUTE_ADMIN_HEADER_FILE" \
  --header 'Content-Type: application/json' \
  --data-binary @- \
  "$COMPUTE_API_URL/v1/roles" <<'JSON'
{
  "name": "nvsentinel-rebooter",
  "scope": "project",
  "description": "NVSentinel provider node reboot access",
  "permissions": {
    "nodes": {"level": "update", "scope": "all"}
  }
}
JSON
```

As a **project administrator** (or that owner), create the account with its single project binding:

```sh
curl --fail-with-body --silent --show-error \
  --header "@$COMPUTE_ADMIN_HEADER_FILE" \
  --header 'Content-Type: application/json' \
  --data-binary @- \
  "$COMPUTE_API_URL/v1/projects/$COMPUTE_PROJECT/service-accounts" <<'JSON'
{
  "name": "nvsentinel-provider",
  "description": "NVSentinel reboot provider",
  "role": "nvsentinel-rebooter"
}
JSON
```

## Create an OIDC trust

Use a dedicated audience such as `mistral-compute` and an exact `sub` rule. The subject must match the **provider's** Kubernetes namespace/account, not Janitor's account.

```sh
export KUBERNETES_ISSUER=https://issuer.example.invalid
export PROVIDER_NAMESPACE=nvsentinel
export PROVIDER_SERVICE_ACCOUNT=nvsentinel-provider-mistral
export COMPUTE_OIDC_AUDIENCE=mistral-compute

jq -n \
  --arg issuer "$KUBERNETES_ISSUER" \
  --arg audience "$COMPUTE_OIDC_AUDIENCE" \
  --arg subject "system:serviceaccount:$PROVIDER_NAMESPACE:$PROVIDER_SERVICE_ACCOUNT" \
  '{
    name: "nvsentinel-kubernetes",
    issuer: $issuer,
    audiences: [$audience],
    claimRules: [{claim: "sub", operator: "equals", value: $subject}],
    purpose: "api",
    enabled: true
  }' > oidc-trust-request.json

curl --fail-with-body --silent --show-error \
  --header "@$COMPUTE_ADMIN_HEADER_FILE" \
  --header 'Content-Type: application/json' \
  --data-binary @oidc-trust-request.json \
  "$COMPUTE_API_URL/v1/projects/$COMPUTE_PROJECT/service-accounts/nvsentinel-provider/oidc-trusts" \
  > oidc-trust.json
```

Replace the issuer with the exact `iss` used by the cluster. Compute validates public HTTPS discovery/JWKS and accepts RS256-signed subject tokens with a `kid`. A private IP, private-resolving hostname, or Kubernetes service DNS issuer will not work. See [issuer prerequisites](installation.md#1-check-prerequisites).

Copy the returned **`clientId`** and **`tokenUrl`** into Helm values. They are public routing metadata, not a client secret. Use `purpose: api`; a node-events trust is not intended for reboot API access. Trust claim rules must all match, and the token must contain one of the configured audiences. The example deliberately restricts the subject to one ServiceAccount.

## Runtime Kubernetes RBAC

The chart grants only these permissions to the provider:

| Scope | API group / resource | Verbs | Why |
| --- | --- | --- | --- |
| Cluster | core / `nodes` | `get` | Verify mapping, UID, cordon state, boot ID, and readiness |
| Cluster | `janitor.dgxc.nvidia.com` / `rebootnodes` | `get`, `patch` | Verify the live request and pin its Node UID |
| Cluster | `authentication.k8s.io` / `tokenreviews` | `create` | Authenticate Janitor's token |
| Provider namespace | core / `configmaps` | `get`, `create`, `update` | Persist and coordinate reboot reservations |

The provider does not need `serviceaccounts/token` creation: kubelet projects and renews its workload token. It does not need Secret reads through the Kubernetes API: TLS and optional API-key credentials are mounted as volumes. Node cordoning/draining remains NVSentinel's responsibility.

The journal role applies to ConfigMaps throughout the provider namespace because record names are created at runtime. Choose a namespace whose ConfigMaps are appropriate for that trust boundary. The provider supports a namespace different from Janitor's; update trust subject, service DNS, and CA distribution together.

## Verify effective access

With the exchanged provider access token, call `GET /v1/me/permissions?project=<project>`. Check the subject is `sa:nvsentinel-provider`, and permissions include:

```json
{
  "resourceType": "node",
  "action": "update",
  "resourceIds": ["*"]
}
```

Omitting `project` evaluates organization context and can give a misleading result. The [installation guide](installation.md#5-verify-oidc-without-rebooting-a-node) includes a complete read-only token-exchange check. It verifies identity and effective permission; it does not test node mapping, regional availability, or a real reboot.
