# Biren SVI (hardware-partitioned GPU) support

HAMi can schedule **Biren** GPUs partitioned with **SVI** (Scalable
Virtualization Interface). SVI is a coarse, mode-based hardware partition of a
whole GPU, configured out-of-band by the administrator:

| SVI mode (`brsmi gpu set -s`) | instances | per-instance mem (166, 64GB) | kubelet resource |
| ----------------------------- | --------- | ---------------------------- | ----------------------- |
| `0` / disabled                | 1         | ~64 GB                       | `birentech.com/gpu`     |
| `1`                           | 2         | ~32 GB                       | `birentech.com/1-2-gpu` |
| `2`                           | 4         | ~16 GB                       | `birentech.com/1-4-gpu` |

Each instance is hardware-isolated and exposed as `/dev/biren/card_<N>`. There
is no memory/core oversubscription, so HAMi schedules each instance as a
**whole, exclusive device** (`Count=1`), mirroring the Iluvatar / whole-card
pattern. A node can mix flavors (some GPUs whole, some halved, some quartered).

## Architecture

```
 pod (schedulerName: hami-scheduler, runtimeClassName: biren,
      requests birentech.com/gpu | 1-2-gpu | 1-4-gpu)
        │
        ▼
 hami-scheduler ── reads hami.io/node-<commonWord>-register on the node
   (kube-scheduler + HAMi extender, started with --enable-biren)
        │   Filter: picks a free instance of the requested flavor
        │   Bind  : writes hami.io/<commonWord>-devices-allocated, binds pod
        ▼
 kubelet → Biren device-plugin (vendor, runs as its own DaemonSet) Allocate
        → injects /dev/biren/card_N + BR_PHY_CARDS (CDI, runtimeClass "biren")
```

HAMi performs **placement and accounting**; the **vendor Biren device-plugin**
advertises the `birentech.com/*` resources and performs the real device
allocation / injection. The two are bridged by node annotations
(`hami.io/node-<commonWord>-register`) that tell the scheduler which SVI
instances exist on each node.

Scheduler backend: `pkg/device/biren` (flavors `BirenGPU`, `Biren-1of2`,
`Biren-1of4`), registered in `pkg/scheduler/config/config.go`.

## Prerequisites

- The **vendor Biren device-plugin** deployed and advertising
  `birentech.com/gpu` / `1-2-gpu` / `1-4-gpu` on the GPU nodes, plus a
  `RuntimeClass` named `biren` (it injects `/dev/biren/card_N` via CDI).
- SVI configured on the GPUs as desired: `sudo brsmi gpu set -s <0|1|2> -i <id>`.

## 1. Build the image

```bash
# binary only
make build                          # -> bin/scheduler
# or the full image
make docker IMG_NAME=hami IMG_TAG=biren-svi
```

The scheduler binary is the only thing needed for the Biren backend (the
backend is scheduling-only; no in-container library is required). A minimal
image is just a glibc base plus `bin/scheduler` on `PATH`, e.g. with
[`crane`](https://github.com/google/go-containerregistry) when no Docker daemon
is available:

```bash
mkdir -p root/usr/local/bin && cp bin/scheduler root/usr/local/bin/
tar -C root -cf layer.tar usr
crane append -b ubuntu:24.04 -f layer.tar -t <registry>/hami/hami:biren-svi
```

## 2. Deploy with Helm

Enable the Biren backend in the chart:

```bash
helm install hami charts/hami -n hami-system --create-namespace \
  --set devices.biren.enabled=true \
  --set scheduler.extender.image.registry=<registry> \
  --set scheduler.extender.image.repository=hami/hami \
  --set scheduler.extender.image.tag=biren-svi
```

`devices.biren.enabled=true` does three things (see `charts/hami/values.yaml`
key `devices.biren`):

1. adds `--enable-biren=true` to the scheduler-extender container;
2. appends the `birentech.com/*` resources to the kube-scheduler extender's
   `managedResources` (so kube-scheduler invokes the HAMi extender for Biren
   pods);
3. registers the `BirenGPU` / `Biren-1of2` / `Biren-1of4` flavors in the
   scheduler `device-config` ConfigMap.

Notes / options:

- The **admission webhook is optional**. If you do not enable it, pods must set
  `schedulerName: hami-scheduler` (and `runtimeClassName: biren`) themselves —
  the extender's Filter computes device requests directly from the container
  resource limits, so the webhook is not required for scheduling. Disabling the
  webhook (`--set scheduler.admissionWebhook.enabled=false`) avoids a
  cluster-wide MutatingWebhook, which is convenient on a shared cluster.
- HAMi's NVIDIA device-plugin is **not** used for Biren; the vendor plugin
  handles allocation, so you can set `--set devicePlugin.enabled=false`.

## 3. Register SVI devices on the node

The scheduler reads `hami.io/node-<commonWord>-register` (a JSON array of
`device.DeviceInfo`, `Count=1` per instance). Publish it once per node (and
again after changing SVI modes). Derive the per-flavor instance counts from the
node's kubelet allocatable, e.g.:

```bash
NODE=<gpu-node>
python3 - "$NODE" <<'PY'
import json, subprocess, sys
node = sys.argv[1]
alloc = json.loads(subprocess.check_output(
    ["kubectl","get","node",node,"-o","jsonpath={.status.allocatable}"]))
for res,(cw,mem) in {
    "birentech.com/gpu":("BirenGPU",65536),
    "birentech.com/1-2-gpu":("Biren-1of2",32768),
    "birentech.com/1-4-gpu":("Biren-1of4",16384)}.items():
    n=int(alloc.get(res,"0"))
    if n<=0: continue
    devs=[{"id":f"{cw}-{node}-{i}","index":i,"count":1,"devmem":mem,
           "devcore":100,"type":cw,"numa":0,"mode":"biren-svi","health":True}
          for i in range(n)]
    subprocess.check_call(["kubectl","annotate","node",node,
        f"hami.io/node-{cw}-register={json.dumps(devs,separators=(',',':'))}",
        "--overwrite"])
PY
```

(Production alternative: a BRML-based discovery DaemonSet that reports real
per-instance UUIDs/memory; the JSON shape is the same.)

## 4. Request SVI instances

See `examples/biren/`:

```yaml
resources:
  limits:
    birentech.com/1-2-gpu: "1"     # one half-GPU SVI instance
    # birentech.com/1-2-gpumem: "30000"   # optional min per-instance MiB filter
```

Use `birentech.com/gpu` (whole), `birentech.com/1-2-gpu` (half) or
`birentech.com/1-4-gpu` (quarter). The pod gets `BR_PHY_CARDS=card_N` and
`/dev/biren/card_N`.

## Verifying

A pod scheduled by HAMi carries `hami.io/<commonWord>-devices-allocated` and
runs on a node where `kubectl get node <n> -o jsonpath='{.spec...}'` shows the
`hami-scheduler`. A `suvs` (Biren DCGM) `membw` run inside the pod measures HBM
bandwidth that scales with the SVI fraction (≈ ½ / ¼ of a whole GPU),
confirming the partition is enforced. A full reproducible build → deploy →
validate harness lives in the companion `br-release` repo under
`setup/kubernets/tests/` (`build-images.sh`, `deploy-hami.sh`,
`run-svi-tests.sh`).
