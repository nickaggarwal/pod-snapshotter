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
- The manager's ServiceAccount needs `nodes/proxy` `get,create` RBAC (the
  Helm chart installs this) — the kubelet authorizes checkpoint calls via
  SubjectAccessReview.
- Kubelet serving certs: if your cluster uses self-signed kubelet certs
  (most managed clusters), set `manager.kubeletInsecureTLS=true` (chart
  default) — the same trade-off metrics-server documents.

## 2. Container runtime

- containerd ≥ 2.0 (CRI checkpoint support) with runc, **or** CRI-O ≥ 1.25.
- runc must find CRIU configured at `/etc/criu/runc.conf`:

  ```
  tcp-established
  link-remap
  ```

  `tcp-established` preserves open sockets across checkpoint; `link-remap`
  handles deleted-but-open files (common with `/dev/shm` usage). The agent
  checks for both lines but never writes host config.

## 3. CRIU

- CRIU ≥ 4.1 (4.0 introduced the NVIDIA CUDA plugin; 4.1 has fixes).
- The CUDA plugin (`cuda_plugin.so`) installed in CRIU's plugin dir
  (`/usr/lib/criu/`, `/usr/lib64/criu/`, or `/usr/local/lib/criu/`).
- `criu check` should pass on the host.

## 4. NVIDIA

- Driver ≥ 570 (the blog recommends ≥ 570; the CUDA plugin requires ≥ 550).
- [`cuda-checkpoint`](https://github.com/NVIDIA/cuda-checkpoint) binary on
  the host `PATH` (it's a standalone binary from NVIDIA's GitHub).
- NVIDIA Container Toolkit with CDI (`nvidia-ctk cdi generate ...`), so pods
  get `/dev/nvidia0`, `/dev/nvidiactl`, `/dev/nvidia-uvm` device nodes —
  these must appear as **private** bind mounts in the container for
  cuda-checkpoint to track CPU↔GPU mappings (pod-snapshotter enforces
  `rprivate` on them when rewriting the restore spec).

## 5. fuse-client

- The [fuse-client](../../fuse-client) DaemonSet running with its mount at
  `/mnt/fuse` (configurable via `agent.fuseMount`).
- Its HTTP API reachable (default `127.0.0.1:8081` on each node via
  hostNetwork, and a `fuse-client` Service for the manager).
- Optional but recommended: the fuse-client agent socket
  (`/var/run/fuse-client/agent.sock`, flag `-enable-agent-server`) for
  artifact pinning. Without it restores still work — just unpinned.

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
