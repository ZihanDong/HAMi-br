# 壁仞 SVI（硬件切分 GPU）支持

HAMi 可调度使用 **SVI**（Scalable Virtualization Interface，可伸缩虚拟化接口）
切分的**壁仞**GPU。SVI 是对整卡的粗粒度、按模式的硬件切分，由管理员在带外配置：

| SVI 模式 (`brsmi gpu set -s`) | 实例数 | 单实例显存（166，64GB） | kubelet 资源名 |
| ----------------------------- | ------ | ----------------------- | ----------------------- |
| `0` / 关闭                    | 1      | ~64 GB                  | `birentech.com/gpu`     |
| `1`                           | 2      | ~32 GB                  | `birentech.com/1-2-gpu` |
| `2`                           | 4      | ~16 GB                  | `birentech.com/1-4-gpu` |

每个实例都是硬件隔离的，暴露为 `/dev/biren/card_<N>`。显存/算力**不超分**，因此
HAMi 把每个实例当作**独占的整设备**（`Count=1`）来调度，与 Iluvatar / 整卡模式
一致。同一节点可混用多种粒度（部分整卡、部分 1/2、部分 1/4）。

## 架构

```
 pod（schedulerName: hami-scheduler，runtimeClassName: biren，
      请求 birentech.com/gpu | 1-2-gpu | 1-4-gpu）
        │
        ▼
 hami-scheduler ── 读取节点上的 hami.io/node-<commonWord>-register
   （kube-scheduler + HAMi extender，启动参数 --enable-biren）
        │   Filter：在请求的粒度中挑选一个空闲实例
        │   Bind  ：写入 hami.io/<commonWord>-devices-allocated，绑定 pod
        ▼
 kubelet → 壁仞 device-plugin（厂商提供，独立 DaemonSet）Allocate
        → 注入 /dev/biren/card_N 与 BR_PHY_CARDS（CDI，runtimeClass "biren"）
```

HAMi 负责**放置与计数**；**厂商 device-plugin** 负责上报 `birentech.com/*` 资源并
执行真正的设备分配/注入。二者通过节点注解 `hami.io/node-<commonWord>-register`
桥接——它告诉调度器每个节点上有哪些 SVI 实例。

调度器后端：`pkg/device/biren`（粒度 `BirenGPU`、`Biren-1of2`、`Biren-1of4`），
在 `pkg/scheduler/config/config.go` 中注册。

## 前置条件

- 已部署**厂商壁仞 device-plugin**，在 GPU 节点上上报 `birentech.com/gpu` /
  `1-2-gpu` / `1-4-gpu`，并存在名为 `biren` 的 `RuntimeClass`（它通过 CDI 注入
  `/dev/biren/card_N`）。
- 已按需配置 SVI：`sudo brsmi gpu set -s <0|1|2> -i <id>`。

## 1. 编译镜像

```bash
# 仅编译二进制
make build                          # -> bin/scheduler
# 或构建完整镜像
make docker IMG_NAME=hami IMG_TAG=biren-svi
```

Biren 后端是“纯调度”的，容器内无需任何库，调度器二进制是唯一依赖。最小镜像就是
一个 glibc 基础镜像 + `PATH` 上的 `bin/scheduler`。在没有 Docker daemon 时可用
[`crane`](https://github.com/google/go-containerregistry) 无守护进程构建：

```bash
mkdir -p root/usr/local/bin && cp bin/scheduler root/usr/local/bin/
tar -C root -cf layer.tar usr
crane append -b ubuntu:24.04 -f layer.tar -t <registry>/hami/hami:biren-svi
```

## 2. 用 Helm 部署

在 chart 中开启 Biren 后端：

```bash
helm install hami charts/hami -n hami-system --create-namespace \
  --set devices.biren.enabled=true \
  --set scheduler.extender.image.registry=<registry> \
  --set scheduler.extender.image.repository=hami/hami \
  --set scheduler.extender.image.tag=biren-svi
```

`devices.biren.enabled=true` 会做三件事（见 `charts/hami/values.yaml` 的
`devices.biren`）：

1. 给 scheduler-extender 容器加上 `--enable-biren=true`；
2. 把 `birentech.com/*` 资源追加进 kube-scheduler extender 的 `managedResources`
   （否则 kube-scheduler 不会为 Biren pod 调用 HAMi extender）；
3. 在调度器 `device-config` ConfigMap 中注册 `BirenGPU` / `Biren-1of2` /
   `Biren-1of4` 三种粒度。

说明 / 可选项：

- **准入 Webhook 可选**。若不启用，pod 需自行设置 `schedulerName: hami-scheduler`
  （以及 `runtimeClassName: biren`）——extender 的 Filter 直接从容器资源 limits
  计算设备请求，调度本身不依赖 webhook。关闭 webhook
  （`--set scheduler.admissionWebhook.enabled=false`）可避免集群级
  MutatingWebhook，在共享集群上更稳妥。
- Biren 不使用 HAMi 自带的 NVIDIA device-plugin；分配由厂商插件完成，可设
  `--set devicePlugin.enabled=false`。

## 3. 在节点上注册 SVI 设备

调度器读取 `hami.io/node-<commonWord>-register`（一个 `device.DeviceInfo` 的 JSON
数组，每实例 `Count=1`）。每个节点发布一次（更改 SVI 模式后需重新发布）。实例数量
可直接从节点的 kubelet allocatable 推导：

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

（生产环境可用基于 BRML 的发现 DaemonSet 上报真实的每实例 UUID/显存，JSON 结构相同。）

## 4. 申请 SVI 实例

参见 `examples/biren/`：

```yaml
resources:
  limits:
    birentech.com/1-2-gpu: "1"     # 一个 1/2 SVI 实例
    # birentech.com/1-2-gpumem: "30000"   # 可选：单实例最小显存(MiB)过滤
```

`birentech.com/gpu`（整卡）、`birentech.com/1-2-gpu`（1/2）、
`birentech.com/1-4-gpu`（1/4）。Pod 内会得到 `BR_PHY_CARDS=card_N` 和
`/dev/biren/card_N`。

## 验证

被 HAMi 调度的 pod 带有 `hami.io/<commonWord>-devices-allocated` 注解，且
`schedulerName` 为 `hami-scheduler`。在 pod 内运行 `suvs`（壁仞 DCGM）的 `membw`
测试，测得的 HBM 带宽会随 SVI 粒度成比例下降（约为整卡的 ½ / ¼），证明硬件切分
已生效。完整的“构建→部署→验证”脚本见配套 `br-release` 仓库
`setup/kubernets/tests/` 目录（`build-images.sh`、`deploy-hami.sh`、
`run-svi-tests.sh`）。
