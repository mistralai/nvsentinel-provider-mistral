# Operations and recovery

[Installation guide](installation.md) · [Configuration](configuration.md)

## Credential lifecycle

With workload identity, kubelet renews the audience-bound projected JWT, and the provider exchanges it for an access token through the configured Compute trust. Access tokens are cached only in memory and refreshed before expiry. Trust issuer, subject, audience, and `purpose: api` must remain valid. See [configuration](configuration.md#token-exchange-and-rotation).

Credential preparation runs before a new reboot reservation. If the projected token cannot be read or the exchange fails at that point, the call can be retried after fixing authentication. Existing reservations and completion checks do not depend on a new exchange. A failure after reservation, including a 401 from the reboot endpoint, retains the journal and suppresses automatic resubmission.

Disabling a trust prevents future exchanges. Already-issued access tokens may remain valid until their expiry. Keep issuer/JWKS availability and signing-key rotation coordinated with the cluster administrator. API-key installations should rotate the mounted Secret according to organization policy.

## Reboot journal

Each Node UID has a namespaced ConfigMap named `nvsentinel-reboot-<uid-hash>`. Its `journal.json` stores the original Node UID, verified Compute target, configuration scope, boot IDs, RebootNode UID bindings, and opaque operation references. It contains no credentials. Kubernetes optimistic concurrency lets replicas share this journal.

The provider records a reservation **before** sending the REST request. A repeat RPC, another RebootNode for the same boot, or another replica adopts the existing reservation. Journal records and the `nvsentinel.mistral.ai/node-uid` RebootNode annotation must survive retries. The ConfigMaps intentionally have no owner reference to a RebootNode and survive its deletion.

The journal binds operations to the Compute endpoint, project, region, and mapped bare-metal host. Changing that scope or the node-name label for an existing Node UID fails closed. Treat a scope migration as an operator-reviewed change, not a routine retry.

Use the same journal namespace across upgrades and reinstalls. Do not run separate provider installations against the same Nodes with different journal namespaces: their reservations would not coordinate. Back up the journal with cluster state, restrict ConfigMap write access, and keep it outside automated age-based cleanup.

## Unconfirmed outcomes

`reserved` means the durable reservation exists. It does not prove that the REST request was sent: the process may have stopped immediately before or after sending it. `submitted` records HTTP acceptance. `unconfirmed` records an HTTP/transport error whose external outcome is not trusted. `complete` records that the original Node returned with a different boot ID and `Ready=True`.

The Compute reboot endpoint has no idempotency key or queryable reboot operation ID. Consequently, this provider suppresses automatic resubmission even if the server rejected the request. This sacrifices automatic recovery after an ambiguous result to avoid a second disruptive reboot. It is not an exactly-once guarantee.

When an operation stalls:

1. Suspend new automated reboot activity for the affected node using your NVSentinel maintenance procedure. Preserve the journal and RebootNode annotation.
2. Inspect the provider logs, RebootNode, journal, and current Node UID/boot ID. Separately confirm the Compute operation with the provider's console or support/audit system. HTTP 204 alone proves only acceptance.
3. If the original Node has completed its reboot and returned Ready, let Janitor observe completion. A provider restart or repeat RPC will adopt the saved operation.
4. If the request was never sent or cannot finish, a qualified operator must decide whether to perform a controlled reboot through the normal maintenance procedure. A confirmed new boot allows the existing operation to complete. Do not clear the journal to force an automated resend.

There is intentionally no automatic reset endpoint or journal deletion command. Removing an operation record, changing a recorded boot ID, or replacing the journal namespace removes duplicate protection. If manual data repair is unavoidable, stop the provider, reconcile the external state, retain an audit copy, and review the change before resuming.

The journal retains history and stops accepting new records before the ConfigMap size limit. There is no automatic history pruning in this initial release. Capacity exhaustion requires reviewed maintenance. Journals for permanently retired Node UIDs may be archived only after all related RebootNodes and active external operations have been reconciled.

## Troubleshooting

| Symptom | Check |
| --- | --- |
| TLS verification fails | Service DNS matches the certificate SAN; Janitor's CA bundle trusts the leaf chain; the Secret is in Janitor's namespace. |
| Permission denied at gRPC | Projected token audience and exact Janitor ServiceAccount subject match the chart values; TokenReview RBAC exists. |
| Missing node-name mapping | A trusted administrator must populate `nvsentinel.mistral.ai.node-restriction.kubernetes.io/node-name` from the verified Compute/Kubernetes association. Confirm Node authorizer and NodeRestriction are enabled. Do not infer the mapping from a similar hostname. |
| Reboot precondition fails | The RebootNode is live and matches the Node; the Node is cordoned and has a UID and boot ID; no replacement or mapping drift occurred. |
| Credential preparation fails | For OIDC, check projected token readability, configured token URL/client ID, public issuer/JWKS, RS256 signing key, expiry, audience, exact ServiceAccount subject, and enabled trust with purpose `api`. Use the [read-only verification](installation.md#5-verify-oidc-without-rebooting-a-node). For API keys, check the mounted Secret and validity. |
| Exchange succeeds but access is denied | Verify the Compute account's single project binding grants `nodes: {level: update, scope: all}`. Read `/v1/me/permissions` with the `project` query parameter. The API currently has no separate reboot-only permission. |
| Compute rejects the reboot request | Check token validity, project/region/target configuration, and node update permission. Correct the cause, then follow unconfirmed-outcome recovery; a rejected reboot POST is never automatically resubmitted. |
| Same boot remains Ready | This is not evidence of a completed reboot. A different boot ID is required. |
| New boot is not Ready | Janitor waits; investigate node recovery without submitting another reboot. |
| Private image cannot pull | Check the imagePullSecret and GHCR package access for its identity. Helm registry login only authenticates the installer. |

Uninstalling the Helm chart leaves runtime-created journal ConfigMaps in place. Reinstall into the same namespace, with the same configuration scope, to preserve protection.
