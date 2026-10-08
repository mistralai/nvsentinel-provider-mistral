"""Validate rendered Kubernetes contracts without contacting a cluster."""

import pathlib
import subprocess
import unittest

import yaml

ROOT = pathlib.Path(__file__).resolve().parents[2]
CHART = ROOT / "charts/nvsentinel-provider-mistral"


def render(*values, succeeds=True):
    command = [
        "helm", "template", "test", str(CHART), "--namespace", "nvsentinel",
        "--values", str(ROOT / "charts/ci-values.yaml"),
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
        self.assertEqual(env["COMPUTE_TOKEN_FILE"]["value"], "/var/run/secrets/compute/token")
        self.assertEqual(env["POD_NAMESPACE"]["valueFrom"]["fieldRef"]["fieldPath"], "metadata.namespace")
        self.assertEqual(env["AUTH_SERVICE_ACCOUNT"]["value"], "system:serviceaccount:nvsentinel:janitor")
        self.assertEqual(env["NODE_NAME_LABEL"]["value"],
                         "nvsentinel.mistral.ai.node-restriction.kubernetes.io/node-name")
        self.assertEqual(container["readinessProbe"]["httpGet"]["path"], "/readyz")
        self.assertEqual(container["livenessProbe"]["httpGet"]["path"], "/healthz")
        self.assertEqual(object_of(objects, "Service")["spec"]["ports"][0]["port"], 50051)
        self.assertFalse(any(obj["kind"] == "Secret" for obj in objects))

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
