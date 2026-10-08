The RebootNode CRD is copied without modification from NVIDIA NVSentinel v1.26.0,
commit `a5b9e90fb3729d72f9ebc1bddcbf4eb518c0b3f5`:

https://github.com/NVIDIA/NVSentinel/blob/a5b9e90fb3729d72f9ebc1bddcbf4eb518c0b3f5/distros/kubernetes/nvsentinel/charts/janitor/crds/janitor.dgxc.nvidia.com_rebootnodes.yaml

Its original NVIDIA copyright and Apache 2.0 license header are preserved.
The envtest suite installs this exact CRD to test compatibility and real
Kubernetes concurrency semantics. It is test data, not an installed chart CRD.
