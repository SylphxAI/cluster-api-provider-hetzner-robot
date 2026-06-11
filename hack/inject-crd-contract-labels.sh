#!/usr/bin/env bash
# Re-inject CAPI contract labels after controller-gen regenerates CRDs.
# controller-gen does not emit metadata.labels; without these labels CAPI
# (GetLatestContractAndAPIVersionFromContract) hard-errors and every CAPHR
# CRD is dead on a cluster rebuilt from git. See platform ADR-226 §3/§6.
set -euo pipefail
cd "$(dirname "$0")/.."
python3 - <<'PY'
import glob, re
labels = (
    "  labels:\n"
    "    cluster.x-k8s.io/provider: infrastructure-hetzner-robot\n"
    "    cluster.x-k8s.io/v1beta1: v1alpha1\n"
    "    cluster.x-k8s.io/v1beta2: v1alpha1\n"
)
for f in sorted(glob.glob("config/crd/bases/infrastructure.cluster.x-k8s.io_*.yaml")):
    s = open(f).read()
    if "cluster.x-k8s.io/v1beta2" in s:
        continue
    new, n = re.subn(r"(?m)^(  name: hetznerrobot)", labels + r"\1", s, count=1)
    assert n == 1, f"no metadata.name anchor in {f}"
    open(f, "w").write(new)
    print(f"labels injected: {f}")
PY
