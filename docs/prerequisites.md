# Node & cluster prerequisites

pod-snapshotter builds on the CRIU + cuda-checkpoint stack described in
[GPU snapshots for reducing ML inference cold starts](https://nilesh-agarwal.com/gpu-snapshots-for-reducing-ml-inference-cold-starts-2/).
Every node that snapshots or restores GPU pods needs the following. The agent
verifies these every 5 minutes and publishes the result as the node
annotation `podsnapshot.io/prereqs` (`ok` or a CSV of failing checks); the
manager refuses to checkpoint pods on nodes not marked `ok`.

## 1. Kubernetes / kubelet

- Kubernetes ≥ 1.30 (`ContainerCheckpoint` feature gate is beta and
  default-on; on older versions enable it in the kubelet config).
- The manager's ServiceAccount needs `nodes/proxy` and `nodes/checkpoint`
  `get,create` RBAC (the Helm chart installs this) — the kubelet authorizes
  checkpoint calls via SubjectAccessReview on the `nodes/checkpoint`
  subresource (verified on AKS 1.32).
- Kubelet serving certs: if your cluster uses self-signed kubelet certs
  (most managed clusters), set `manager.kubeletInsecureTLS=true` (chart
  default) — the same trade-off metrics-server documents.

## 2. Container runtime — containerd ≥ 2.0 is a HARD requirement

- containerd ≥ 2.0 (CRI checkpoint support) with runc, **or** CRI-O ≥ 1.25.

  containerd 1.7 does **not** implement the CRI `CheckpointContainer` RPC.
  On a 1.7 node the kubelet accepts the checkpoint request and then fails
  with (verified on AKS Ubuntu 22.04, containerd 1.7.30):

  ```
  rpc error: code = Unimplemented desc = method CheckpointContainer not implemented
  ```

  **On AKS**: Ubuntu 22.04 node pools ship containerd 1.7; Ubuntu 24.04
  node pools ship containerd 2.x. Put checkpoint/restore workloads on an
  Ubuntu 24.04 pool:

  ```bash
  az aks nodepool add -g <rg> --cluster-name <cluster> -n ckpt2404 \
    --node-count 1 --node-vm-size <size> --os-sku Ubuntu2404 --mode User \
    --labels pool=ckpt2404
  ```

  For GPU pools the same applies — pick `--os-sku Ubuntu2404` (or AzureLinux
  3) so the runtime is containerd 2.x.
- runc must find CRIU configured at `/etc/criu/runc.conf`:

  ```
  tcp-established
  link-remap
  ```

  `tcp-established` preserves open sockets across checkpoint; `link-remap`
  handles deleted-but-open files (common with `/dev/shm` usage). The agent
  checks for both lines but never writes host config.

## 3. CRIU

> The Helm chart installs CRIU for you: the `nodeSetup` DaemonSet (enabled by
> default, `nodeSetup.enabled=false` to opt out) installs CRIU from
> `ppa:criu/ppa` on Ubuntu nodes (4.2+ with the CUDA plugin; the distro's own
> 24.04 package is still 3.x), downloads NVIDIA's `cuda-checkpoint` binary,
> writes `/etc/criu/runc.conf`, and switches the NVIDIA Container Toolkit to
> CDI mode. On non-Ubuntu nodes it falls back to the distro package — GPU
> checkpointing then needs a custom node image with CRIU ≥ 4.x.

- CRIU ≥ 4.1 (4.0 introduced the NVIDIA CUDA plugin; 4.1 has fixes).
- The CUDA plugin (`cuda_plugin.so`) installed in CRIU's plugin dir
  (`/usr/lib/criu/`, `/usr/lib64/criu/`, or `/usr/local/lib/criu/`).
- `criu check` should pass on the host.

## 4. NVIDIA

- Driver ≥ 570 (the blog recommends ≥ 570; the CUDA plugin requires ≥ 550).
- [`cuda-checkpoint`](https://github.com/NVIDIA/cuda-checkpoint) binary on
  the host `PATH` (it's a standalone binary from NVIDIA's GitHub).
- NVIDIA Container Toolkit in **CDI mode** (`nvidia-ctk cdi generate
  --output=/etc/cdi/nvidia.yaml` + `nvidia-container-runtime.mode=cdi`; the
  `nodeSetup` DaemonSet configures both). In the default "legacy" mode the
  toolkit's prestart hook injects driver-library bind mounts and a
  `/run/nvidia-ctk-hook*` params-masking tmpfs that are invisible to the OCI
  spec, and CRIU fails the checkpoint with `No mapping for N:(null)
  mountpoint`. CDI puts every mount in the spec where CRIU can see it.
- Device nodes (`/dev/nvidia0`, `/dev/nvidiactl`, `/dev/nvidia-uvm`) must
  appear as **private** bind mounts in the container for cuda-checkpoint to
  track CPU↔GPU mappings (pod-snapshotter enforces `rprivate` on them when
  rewriting the restore spec).
- Device cgroup at restore: the dumped OCI spec contains only the CRI's
  default-deny devices rule — the runtime grants GPU access at container
  create time, outside the spec. pod-snapshotter therefore rewrites the
  restore spec with an allow-all device cgroup (`a *:* rwm`); without it the
  restored process hits `no CUDA-capable device is detected` even though the
  device nodes exist. Isolation still comes from the mount list: only the
  originally-dumped device nodes are bind-mounted into the container.

## 5. fuse-client

- The [fuse-client](../../fuse-client) DaemonSet running with its mount at
  `/mnt/fuse` (configurable via `agent.fuseMount`).
- Its HTTP API reachable (default `127.0.0.1:8081` on each node via
  hostNetwork, and a `fuse-client` Service for the manager).
- Optional but recommended: the fuse-client agent socket
  (`/var/run/fuse-client/agent.sock`, flag `-enable-agent-server`) for
  artifact pinning. Without it restores still work — just unpinned.

## Workload requirements

Pods you intend to checkpoint must run **AppArmor-unconfined** (verified on
AKS Ubuntu nodes): the kubelet's default `cri-containerd.apparmor.d`
confinement is recorded in the CRIU image, and the restore cannot re-enter
it (`can't write lsm profile -22` — AppArmor forbids `changeprofile` from an
out-of-band runc). On Kubernetes ≥ 1.30:

```yaml
spec:
  securityContext:
    appArmorProfile:
      type: Unconfined
```

## Restore environment matching

CRIU/cuda-checkpoint restores require the target to match the source:

| Must match | Why |
|---|---|
| Container image | rootfs + rootfs-diff.tar assume it |
| GPU model | device state layout |
| NVIDIA driver version | CUDA plugin restores driver state |
| CRIU version | image format compatibility |
| OS/kernel (close) | CRIU kernel feature dependencies |

Homogeneous GPU node pools satisfy this naturally. Use
`spec.nodeSelector`/`spec.nodeName` on the PodRestore to steer placement.

## Failure annotations reference

| Annotation value | Fix |
|---|---|
| `criu-missing` / `criu-version-lt-4.1` | install/upgrade CRIU on the node |
| `criu-cuda-plugin-missing` | install cuda_plugin.so into CRIU's plugin dir |
| `cuda-checkpoint-missing` | put the NVIDIA binary on the host PATH |
| `nvidia-driver-lt-570` | upgrade the driver |
| `criu-runc-conf-missing` / `runc-conf-no-*` | write `/etc/criu/runc.conf` (see §2) |
| `fuse-mount-missing` | fix the fuse-client DaemonSet / mount path |
