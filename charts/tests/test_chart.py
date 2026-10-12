"""Validate rendered Kubernetes contracts without contacting a cluster."""

import pathlib
import subprocess
import unittest

import yaml

ROOT = pathlib.Path(__file__).resolve().parents[2]
CHART = ROOT / "charts/nvsentinel-provider-mistral"


def render(*values, succeeds=True, fixture="ci-values.yaml", namespace="nvsentinel"):
    command = [
        "helm", "template", "test", str(CHART), "--namespace", namespace,
        "--values", str(ROOT / "charts" / fixture),
    ]
    for value in values:
        command += ["--set", value]
    result = subprocess.run(command, capture_output=True, text=True, check=False)
    if not succeeds:
        if result.returncode == 0:
            raise AssertionError("unsafe configuration rendered successfully")
        return result.stderr
    if result.returncode:
        raise AssertionError(result.stderr)
    return [doc for doc in yaml.safe_load_all(result.stdout) if doc]


def object_of(objects, kind):
    return next(obj for obj in objects if obj["kind"] == kind)


class ChartTest(unittest.TestCase):
    def test_hardened_deployment_and_rotating_credentials(self):
        objects = render("imagePullSecrets[0].name=private-registry")
        pod = object_of(objects, "Deployment")["spec"]["template"]["spec"]
        security = pod["securityContext"]
        self.assertTrue(security["runAsNonRoot"])
        self.assertEqual(security["runAsUser"], 65532)
        self.assertEqual(security["seccompProfile"]["type"], "RuntimeDefault")
        container = pod["containers"][0]
        self.assertFalse(container["securityContext"]["allowPrivilegeEscalation"])
        self.assertTrue(container["securityContext"]["readOnlyRootFilesystem"])
        self.assertEqual(container["securityContext"]["capabilities"]["drop"], ["ALL"])
        self.assertEqual(pod["imagePullSecrets"], [{"name": "private-registry"}])
        self.assertFalse(pod["automountServiceAccountToken"])
        self.assertEqual(pod["terminationGracePeriodSeconds"], 60)
        self.assertTrue(all("subPath" not in mount for mount in container["volumeMounts"]))
        volumes = {volume["name"]: volume for volume in pod["volumes"]}
        token = volumes["compute-token"]["secret"]
        self.assertEqual(token["secretName"], "synthetic-compute-token")
        self.assertEqual(token["items"], [{"key": "token", "path": "token"}])
        projection = volumes["kube-api-access"]["projected"]["sources"][0]
        self.assertEqual(projection["serviceAccountToken"]["expirationSeconds"], 3600)
        env = {item["name"]: item for item in container["env"]}
        self.assertEqual(env["COMPUTE_AUTH_MODE"]["value"], "apiKey")
        self.assertEqual(env["RPC_TIMEOUT"]["value"], "45s")
        self.assertEqual(env["COMPUTE_TOKEN_FILE"]["value"], "/var/run/secrets/compute/token")
        self.assertFalse(any(name.startswith("COMPUTE_OIDC_") for name in env))
        self.assertNotIn("compute-identity", volumes)
        self.assertNotIn("audience", projection["serviceAccountToken"])
        self.assertEqual(env["POD_NAMESPACE"]["valueFrom"]["fieldRef"]["fieldPath"], "metadata.namespace")
        self.assertEqual(env["AUTH_SERVICE_ACCOUNT"]["value"], "system:serviceaccount:nvsentinel:janitor")
        self.assertEqual(env["NODE_NAME_LABEL"]["value"],
                         "nvsentinel.mistral.ai.node-restriction.kubernetes.io/node-name")
        self.assertEqual(container["readinessProbe"]["httpGet"]["path"], "/readyz")
        self.assertEqual(container["livenessProbe"]["httpGet"]["path"], "/healthz")
        self.assertEqual(object_of(objects, "Service")["spec"]["ports"][0]["port"], 50051)
        self.assertFalse(any(obj["kind"] == "Secret" for obj in objects))

    def test_workload_identity_uses_a_distinct_renewable_token(self):
        objects = render(fixture="ci-workload-identity-values.yaml")
        pod = object_of(objects, "Deployment")["spec"]["template"]["spec"]
        container = pod["containers"][0]
        env = {item["name"]: item["value"] for item in container["env"] if "value" in item}
        self.assertEqual(env["COMPUTE_AUTH_MODE"], "workloadIdentity")
        self.assertEqual(env["COMPUTE_OIDC_CLIENT_ID"], "synthetic-trust-client")
        self.assertEqual(env["COMPUTE_OIDC_TOKEN_URL"], "https://compute.example.invalid/oauth2/token")
        self.assertEqual(env["COMPUTE_OIDC_SUBJECT_TOKEN_FILE"], "/var/run/secrets/compute-identity/token")
        self.assertEqual(env["RPC_TIMEOUT"], "75s")
        self.assertEqual(pod["terminationGracePeriodSeconds"], 85)
        self.assertNotIn("COMPUTE_TOKEN_FILE", env)
        self.assertFalse(any("CLIENT_SECRET" in name for name in env))
        volumes = {volume["name"]: volume for volume in pod["volumes"]}
        self.assertNotIn("compute-token", volumes)
        identity = volumes["compute-identity"]["projected"]
        self.assertEqual(identity["defaultMode"], 0o440)
        self.assertEqual(identity["sources"], [{"serviceAccountToken": {
            "audience": "mistral-compute", "expirationSeconds": 3600, "path": "token"
        }}])
        api_token = volumes["kube-api-access"]["projected"]["sources"][0]["serviceAccountToken"]
        self.assertNotIn("audience", api_token)
        mounts = {mount["name"]: mount for mount in container["volumeMounts"]}
        self.assertEqual(mounts["compute-identity"]["mountPath"], "/var/run/secrets/compute-identity")
        self.assertTrue(mounts["compute-identity"]["readOnly"])
        self.assertNotIn("subPath", mounts["compute-identity"])
        self.assertNotEqual(mounts["compute-identity"]["mountPath"], mounts["kube-api-access"]["mountPath"])
        self.assertFalse(pod["automountServiceAccountToken"])
        self.assertFalse(any(obj["kind"] == "Secret" for obj in objects))
        api_objects = render()
        for kind in ("ClusterRole", "Role"):
            self.assertEqual(object_of(objects, kind)["rules"], object_of(api_objects, kind)["rules"])

    def test_workload_identity_service_account_subject_stays_explicit(self):
        objects = render("serviceAccount.name=compute-rebooter", "fullnameOverride=renamed-provider",
                         fixture="ci-workload-identity-values.yaml", namespace="provider-system")
        pod = object_of(objects, "Deployment")["spec"]["template"]["spec"]
        self.assertEqual(pod["serviceAccountName"], "compute-rebooter")
        self.assertEqual(object_of(objects, "ServiceAccount")["metadata"]["name"], "compute-rebooter")
        for kind in ("RoleBinding", "ClusterRoleBinding"):
            self.assertEqual(object_of(objects, kind)["subjects"], [{
                "kind": "ServiceAccount", "name": "compute-rebooter", "namespace": "provider-system"
            }])

    def test_auth_modes_are_exclusive_and_trust_metadata_is_required(self):
        for values in [
            ("compute.auth.mode=unknown",),
            ("compute.auth.workloadIdentity.clientId=unused-client",),
            ("compute.auth.workloadIdentity.tokenUrl=https://compute.example.invalid/oauth2/token",),
            ("compute.auth.workloadIdentity.audience=unused-audience",),
        ]:
            with self.subTest(mode="apiKey", values=values):
                render(*values, succeeds=False)
        for values in [
            ("compute.tokenSecret.name=unexpected-api-key",),
            ("compute.auth.workloadIdentity.clientId=",),
            ("compute.auth.workloadIdentity.tokenUrl=",),
            ("compute.auth.workloadIdentity.audience=",),
            ("compute.auth.workloadIdentity.tokenUrl=http://compute.example.invalid/oauth2/token",),
            ("compute.auth.workloadIdentity.tokenUrl=https://user@compute.example.invalid/oauth2/token",),
            ("compute.auth.workloadIdentity.tokenUrl=https://compute.example.invalid/oauth2/token?query=value",),
            ("compute.auth.workloadIdentity.tokenUrl=https://compute.example.invalid/oauth2/token#fragment",),
            ("compute.auth.workloadIdentity.expirationSeconds=599",),
            ("compute.auth.workloadIdentity.expirationSeconds=3601",),
            ("compute.auth.workloadIdentity.clientSecret=unsupported",),
            ("serviceAccount.name=invalid:name",),
            ("timeouts.rpcSeconds=60",),
            ("timeouts.rpcSeconds=45",),
            ("timeouts.httpSeconds=40",),
        ]:
            with self.subTest(mode="workloadIdentity", values=values):
                render(*values, succeeds=False, fixture="ci-workload-identity-values.yaml")

    def test_least_privilege_rbac(self):
        objects = render()
        rules = object_of(objects, "ClusterRole")["rules"]
        actual = {(tuple(rule["apiGroups"]), tuple(rule["resources"])): set(rule["verbs"]) for rule in rules}
        self.assertEqual(actual, {
            (("",), ("nodes",)): {"get"},
            (("janitor.dgxc.nvidia.com",), ("rebootnodes",)): {"get", "patch"},
            (("authentication.k8s.io",), ("tokenreviews",)): {"create"},
        })
        self.assertEqual(object_of(objects, "Role")["rules"], [
            {"apiGroups": [""], "resources": ["configmaps"], "verbs": ["get", "create", "update"]}
        ])

    def test_network_policy_allows_only_janitor_grpc_ingress(self):
        objects = render(namespace="provider-system")
        policy = object_of(objects, "NetworkPolicy")["spec"]
        deployment = object_of(objects, "Deployment")["spec"]
        self.assertEqual(policy, {
            "podSelector": deployment["selector"],
            "policyTypes": ["Ingress"],
            "ingress": [{
                "from": [{
                    "namespaceSelector": {"matchLabels": {"kubernetes.io/metadata.name": "nvsentinel"}},
                    "podSelector": {"matchLabels": {"app.kubernetes.io/name": "janitor"}},
                }],
                "ports": [{"protocol": "TCP", "port": 50051}],
            }],
        })
        self.assertEqual(policy["podSelector"]["matchLabels"], {
            "app.kubernetes.io/name": "nvsentinel-provider-mistral",
            "app.kubernetes.io/instance": "test",
        })

    def test_network_policy_tracks_caller_namespace_and_custom_labels(self):
        objects = render(
            "auth.serviceAccount=system:serviceaccount:janitor-system:custom-janitor",
            r"networkPolicy.callerPodSelector.matchLabels.app\.kubernetes\.io/name=custom-janitor",
            r"networkPolicy.callerPodSelector.matchLabels.app\.kubernetes\.io/instance=sentinel",
            "fullnameOverride=reboot-provider", namespace="provider-system",
        )
        policy = object_of(objects, "NetworkPolicy")
        self.assertEqual(policy["metadata"]["name"], "reboot-provider")
        self.assertEqual(policy["spec"]["ingress"][0]["from"], [{
            "namespaceSelector": {"matchLabels": {"kubernetes.io/metadata.name": "janitor-system"}},
            "podSelector": {"matchLabels": {
                "app.kubernetes.io/name": "custom-janitor",
                "app.kubernetes.io/instance": "sentinel",
            }},
        }])

    def test_network_policy_can_be_managed_separately(self):
        objects = render("networkPolicy.enabled=false")
        self.assertFalse(any(obj["kind"] == "NetworkPolicy" for obj in objects))

    def test_network_policy_rejects_invalid_or_unrestricted_selectors(self):
        for values in [
            ("networkPolicy.callerPodSelector.matchLabels=null",),
            (r"networkPolicy.callerPodSelector.matchLabels.app\.kubernetes\.io/name=null",),
            (r"networkPolicy.callerPodSelector.matchLabels.app\.kubernetes\.io/name=true",),
            ("networkPolicy.callerPodSelector.matchExpressions[0].key=app",),
            ("auth.serviceAccount=system:serviceaccount:invalid/namespace:janitor",),
            ("auth.serviceAccount=system:serviceaccount:Invalid:janitor",),
            ("auth.serviceAccount=system:serviceaccount:" + "a" * 64 + ":janitor",),
        ]:
            with self.subTest(values=values):
                render(*values, succeeds=False)

    def test_certificate_matches_service_and_alternative_tls_secret(self):
        objects = render("clusterDomain=example.internal", "fullnameOverride=reboot-provider")
        cert = object_of(objects, "Certificate")["spec"]
        self.assertEqual(cert["dnsNames"], [
            "reboot-provider.nvsentinel.svc", "reboot-provider.nvsentinel.svc.example.internal"
        ])
        self.assertEqual(cert["issuerRef"]["name"], "synthetic-ca")
        self.assertEqual(cert["privateKey"]["rotationPolicy"], "Always")
        external = render("tls.certManager.enabled=false", "tls.existingSecret=existing-tls")
        self.assertFalse(any(obj["kind"] == "Certificate" for obj in external))
        volumes = object_of(external, "Deployment")["spec"]["template"]["spec"]["volumes"]
        self.assertEqual(next(v for v in volumes if v["name"] == "provider-tls")["secret"]["secretName"], "existing-tls")

    def test_digest_and_replicas(self):
        digest = "sha256:" + "a" * 64
        objects = render("image.digest=" + digest, "replicaCount=2")
        deployment = object_of(objects, "Deployment")["spec"]
        self.assertEqual(deployment["replicas"], 2)
        self.assertEqual(deployment["template"]["spec"]["containers"][0]["image"],
                         "ghcr.io/mistralai/nvsentinel-provider-mistral@" + digest)

    def test_grace_period_tracks_rpc_timeout(self):
        objects = render("timeouts.rpcSeconds=120")
        pod = object_of(objects, "Deployment")["spec"]["template"]["spec"]
        self.assertEqual(pod["terminationGracePeriodSeconds"], 130)
        env = {item["name"]: item for item in pod["containers"][0]["env"]}
        self.assertEqual(env["RPC_TIMEOUT"]["value"], "120s")

    def test_workload_timeout_override_covers_both_http_requests(self):
        objects = render("timeouts.httpSeconds=20", "timeouts.rpcSeconds=45",
                         fixture="ci-workload-identity-values.yaml")
        pod = object_of(objects, "Deployment")["spec"]["template"]["spec"]
        self.assertEqual(pod["terminationGracePeriodSeconds"], 60)
        env = {item["name"]: item for item in pod["containers"][0]["env"]}
        self.assertEqual(env["HTTP_TIMEOUT"]["value"], "20s")
        self.assertEqual(env["RPC_TIMEOUT"]["value"], "45s")

    def test_mapping_label_requires_protected_namespace(self):
        for label in [
            "node-restriction.kubernetes.io/node-name",
            "example.com.node-restriction.kubernetes.io/Compute_Name",
        ]:
            with self.subTest(label=label):
                objects = render("nodeNameLabel=" + label)
                container = object_of(objects, "Deployment")["spec"]["template"]["spec"]["containers"][0]
                env = {item["name"]: item for item in container["env"]}
                self.assertEqual(env["NODE_NAME_LABEL"]["value"], label)
        for label in [
            "nvsentinel.mistral.ai/node-name",
            "node-name",
            "notnode-restriction.kubernetes.io/node-name",
            "node-restriction.kubernetes.io.example.com/node-name",
            "example.com/node-restriction.kubernetes.io",
            "example_com.node-restriction.kubernetes.io/node-name",
        ]:
            with self.subTest(label=label):
                render("nodeNameLabel=" + label, succeeds=False)

    def test_rejects_missing_credentials_plaintext_endpoint_or_tls(self):
        for values in [
            ("compute.tokenSecret.name=",),
            ("compute.endpoint=http://compute.example.invalid",),
            ("tls.certManager.enabled=false",),
            ("tls.certManager.issuerRef.name=",),
            ("tls.existingSecret=conflicting-tls",),
            ("auth.serviceAccount=janitor",),
            ("timeouts.rpcSeconds=30",),
        ]:
            with self.subTest(values=values):
                render(*values, succeeds=False)


if __name__ == "__main__":
    unittest.main()
