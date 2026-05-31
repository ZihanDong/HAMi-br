#!/usr/bin/env bash
# Package the HAMi-br scheduler (with the Biren SVI backend) into a
# self-contained, offline installer tarball — mirroring the BirenTech
# device-plugin package (k8s_device_plugin_*.tar.gz: image tar + deploy yaml).
#
# Output bundle layout (hami-br-svi-<version>/):
#   hami-svi.tar           ctr-importable scheduler image (base + scheduler binary)
#   hami-scheduler.yaml    rendered deploy (namespace, RBAC, ConfigMaps, Deployment,
#                          Service) — `kubectl apply -f` it; no Helm needed on target
#   register-svi-devices.sh  publishes hami.io/node-<flavor>-register on a node
#   install.info           image ref + metadata
#   README.md
#
# Consumed by br-release set-node-mode.sh --vgpu (ctr import + kubectl apply +
# register). Built daemonless with `crane`, so no Docker is required.
#
# Usage:
#   SCHED_BIN=/path/to/scheduler ./hack/package-hami-svi.sh
#   (or run `make build` first so bin/scheduler exists)
#
# Env:
#   SCHED_BIN            compiled scheduler binary (default: bin/scheduler)
#   VERSION             package version tag (default: from VERSION file or "dev")
#   HAMI_IMAGE          image ref baked into the tar + yaml
#                       (default: 10.50.36.126:32000/hami/hami:biren-svi)
#   BASE_IMAGE          glibc base (default: ubuntu:24.04)
#   KUBE_SCHEDULER_IMAGE  kube-scheduler sidecar image
#                       (default: registry.k8s.io/kube-scheduler:v1.30.0)
#   KUBE_VERSION        Helm --kube-version for rendering (default: 1.30.0)
#   OUT_DIR             output directory (default: ./_package)
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CHART="${REPO_ROOT}/charts/hami"

SCHED_BIN="${SCHED_BIN:-${REPO_ROOT}/bin/scheduler}"
VERSION="${VERSION:-$( [ -f "${REPO_ROOT}/VERSION" ] && cat "${REPO_ROOT}/VERSION" || echo dev )}"
HAMI_IMAGE="${HAMI_IMAGE:-10.50.36.126:32000/hami/hami:biren-svi}"
BASE_IMAGE="${BASE_IMAGE:-ubuntu:24.04}"
KUBE_SCHEDULER_IMAGE="${KUBE_SCHEDULER_IMAGE:-registry.k8s.io/kube-scheduler:v1.30.0}"
KUBE_VERSION="${KUBE_VERSION:-1.30.0}"
OUT_DIR="${OUT_DIR:-${REPO_ROOT}/_package}"

CRANE="${CRANE:-$(command -v crane || echo /tmp/crane)}"
HELM="${HELM:-$(command -v helm || echo helm)}"

info(){ printf '\033[0;36m[pkg]\033[0m %s\n' "$*"; }
die(){ printf '\033[0;31m[pkg] ERROR:\033[0m %s\n' "$*" >&2; exit 1; }

[ -x "$SCHED_BIN" ] || die "scheduler binary not found at $SCHED_BIN (run 'make build' or set SCHED_BIN)"
[ -d "$CHART" ]     || die "chart not found at $CHART"
command -v "$HELM" >/dev/null 2>&1 || die "helm not found"
if [ ! -x "$CRANE" ] && ! command -v crane >/dev/null 2>&1; then
  info "downloading crane"
  curl -sSL "https://github.com/google/go-containerregistry/releases/download/v0.20.2/go-containerregistry_Linux_x86_64.tar.gz" \
    | tar -xz -C /tmp crane && CRANE=/tmp/crane && chmod +x "$CRANE"
fi

PKG_NAME="hami-br-svi-${VERSION}"
STAGE="${OUT_DIR}/${PKG_NAME}"
ksreg="${KUBE_SCHEDULER_IMAGE%:*}"; kstag="${KUBE_SCHEDULER_IMAGE##*:}"
extreg="${HAMI_IMAGE%/*/*}"; extrepo="$(echo "${HAMI_IMAGE#*/}" | cut -d: -f1)"; exttag="${HAMI_IMAGE##*:}"

rm -rf "$STAGE"; mkdir -p "$STAGE"
info "packaging ${PKG_NAME}  (image ${HAMI_IMAGE})"

# 1) image tar (base + scheduler binary at /usr/local/bin/scheduler), daemonless
info "building image tar via crane"
tmp="$(mktemp -d)"; mkdir -p "$tmp/root/usr/local/bin"
install -m0755 "$SCHED_BIN" "$tmp/root/usr/local/bin/scheduler"
tar -C "$tmp/root" -cf "$tmp/layer.tar" usr
"$CRANE" append -b "$BASE_IMAGE" -f "$tmp/layer.tar" -t "$HAMI_IMAGE" -o "$STAGE/hami-svi.tar" --insecure
rm -rf "$tmp"

# 2) rendered deploy yaml (webhook + nvidia device-plugin disabled; biren enabled)
info "rendering hami-scheduler.yaml via helm template (kube ${KUBE_VERSION})"
"$HELM" template hami "$CHART" -n hami-system --kube-version "$KUBE_VERSION" \
  --set devices.biren.enabled=true \
  --set scheduler.admissionWebhook.enabled=false \
  --set devicePlugin.enabled=false \
  --set scheduler.kubeScheduler.image.registry="$ksreg" \
  --set scheduler.kubeScheduler.image.repository=kube-scheduler \
  --set scheduler.kubeScheduler.image.tag="$kstag" \
  --set scheduler.kubeScheduler.image.pullPolicy=IfNotPresent \
  --set scheduler.extender.image.registry="$extreg" \
  --set scheduler.extender.image.repository="$extrepo" \
  --set scheduler.extender.image.tag="$exttag" \
  --set scheduler.extender.image.pullPolicy=IfNotPresent \
  > "$STAGE/hami-scheduler.yaml"
grep -q "kind: Deployment" "$STAGE/hami-scheduler.yaml" || die "rendered yaml missing Deployment"

# 3) self-contained node SVI registration script (no external deps)
cat > "$STAGE/register-svi-devices.sh" <<'REG'
#!/usr/bin/env bash
# Publish hami.io/node-<flavor>-register (JSON []DeviceInfo, Count=1 each) for the
# Biren SVI instances the vendor device plugin advertises on a node, and clear any
# stale node-handshake-* so a (re)started scheduler re-ingests. Counts come from
# the node's kubelet allocatable. Usage: ./register-svi-devices.sh [node]
set -euo pipefail
KC="${KUBECONFIG:+--kubeconfig $KUBECONFIG}"
NODE="${1:-$(hostname -s)}"
python3 - "$NODE" ${KUBECONFIG:-} <<PY
import json, subprocess, sys
node=sys.argv[1]; kc=(["--kubeconfig",sys.argv[2]] if len(sys.argv)>2 and sys.argv[2] else [])
alloc=json.loads(subprocess.check_output(["kubectl","get","node",node,"-o","jsonpath={.status.allocatable}"]+kc))
for res,(cw,mem) in {"birentech.com/gpu":("BirenGPU",65536),
    "birentech.com/1-2-gpu":("Biren-1of2",32768),
    "birentech.com/1-4-gpu":("Biren-1of4",16384)}.items():
    n=int(alloc.get(res,"0"))
    if n<=0: continue
    devs=[{"id":f"{cw}-{node}-{i}","index":i,"count":1,"devmem":mem,"devcore":100,
           "type":cw,"numa":0,"mode":"biren-svi","health":True} for i in range(n)]
    subprocess.check_call(["kubectl","annotate","node",node,
        f"hami.io/node-{cw}-register={json.dumps(devs,separators=(',',':'))}","--overwrite"]+kc)
    print(f"  registered {cw}: {n} instances")
PY
for cw in BirenGPU Biren-1of2 Biren-1of4; do
  kubectl annotate node "$NODE" "hami.io/node-handshake-${cw}-" $KC >/dev/null 2>&1 || true
done
echo "SVI devices registered on $NODE"
REG
chmod +x "$STAGE/register-svi-devices.sh"

# 4) metadata + README
cat > "$STAGE/install.info" <<EOF
package=${PKG_NAME}
hami_image=${HAMI_IMAGE}
kube_scheduler_image=${KUBE_SCHEDULER_IMAGE}
kube_version=${KUBE_VERSION}
built=$(date -u +%Y-%m-%dT%H:%M:%SZ)
EOF
cat > "$STAGE/README.md" <<EOF
# HAMi-br Biren SVI scheduler plugin — ${PKG_NAME}

Offline installer for the HAMi scheduler with the Biren SVI backend.

Contents:
- \`hami-svi.tar\`         scheduler image (\`${HAMI_IMAGE}\`) — \`ctr -n k8s.io images import\`
- \`hami-scheduler.yaml\`  full deploy — \`kubectl apply -f\`
- \`register-svi-devices.sh\`  publish node SVI registration

Install (or use \`set-node-mode.sh biren --vgpu\` which does this for you):
\`\`\`
sudo ctr -n k8s.io images import hami-svi.tar
kubectl apply -f hami-scheduler.yaml
kubectl -n hami-system rollout status deploy/hami-scheduler
./register-svi-devices.sh <gpu-node>
\`\`\`
Requires the vendor Biren device plugin already running (advertising
birentech.com/{gpu,1-2-gpu,1-4-gpu}). Pods request a flavor with
\`schedulerName: hami-scheduler\`.
EOF

# 5) bundle
TARBALL="${OUT_DIR}/${PKG_NAME}.tar.gz"
tar -C "$OUT_DIR" -czf "$TARBALL" "$PKG_NAME"
info "done:"
ls -la "$STAGE"
echo
info "bundle: $TARBALL"
