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

  **`link-remap` lets the dump succeed but cannot make the restore succeed**
  for a deleted file on pod-scoped tmpfs. CRIU hard-links the inode to
  `/dev/shm/link_remap.<n>` during the dump, drops that link when the dump
  finishes, and expects it back at restore time — which it never is, because
  a restored pod gets an empty tmpfs:

  ```
  Error (criu/files-reg.c:2258): Can't link dev/shm/link_remap.337 ->
  dev/shm/sem.l0JKK0: No such file or directory
  ```

  Removing `link-remap` does not help either; CRIU then refuses the dump
  outright (`Can't create link remap for /dev/shm/sem.XXXXXX. Use link-remap
  option.`) because it cannot ghost a *mapped* deleted file that still has a
  link. The workload has to drop the last link itself, which takes `nlink`
  to zero and puts CRIU on the ghost-file path — see
  [Workload requirements](#workload-requirements).

## 3. CRIU

> The Helm chart installs CRIU for you: the `nodeSetup` DaemonSet (enabled by
> default, `nodeSetup.enabled=false` to opt out) installs CRIU from
> `ppa:criu/ppa` on Ubuntu nodes — the PPA is required on **all** Ubuntu
> 24.04 nodes, not just GPU ones: noble dropped `criu` from the archive
> entirely ("no installation candidate" on AKS images). It also downloads
> NVIDIA's `cuda-checkpoint` binary, writes `/etc/criu/runc.conf`, and
> switches the NVIDIA Container Toolkit to CDI mode (GPU nodes only). On
> non-Ubuntu nodes it falls back to the distro package — GPU checkpointing
> then needs a custom node image with CRIU ≥ 4.x.

- GPU nodes: CRIU ≥ 4.1 (4.0 introduced the NVIDIA CUDA plugin; 4.1 fixes),
  with `cuda_plugin.so` in CRIU's plugin dir (`/usr/lib/criu/`,
  `/usr/lib64/criu/`, or `/usr/local/lib/criu/`).
- CPU-only nodes: CRIU ≥ 3.16 suffices (containerd's floor); the agent's
  prereq checker applies the matching threshold per node.
- `criu check` should pass on the host.

### Optional: the patched CRIU

`criu.enabled=true` adds a second, opt-in DaemonSet that installs
pod-snapshotter's CRIU fork — upstream v4.2.1 plus the restore read-path
patches in `hack/criu/patches` (see [design-v2.md §6b](design-v2.md)). It
lands in `/usr/local/sbin`, which precedes `/usr/sbin` on the default PATH, so
runc picks it up while the distro package stays where it is; `criu.uninstall=true`
removes it and the node falls straight back.

It is off by default because it replaces the binary every checkpoint and
restore on that node goes through. Nothing else in the chart depends on it:
the tuning annotations below are ignored by a stock CRIU, so a cluster can run
with it enabled on some nodes and not others.

| Annotation on a PodRestore | Effect |
|---|---|
| `podsnapshot.io/criu-aio-depth` | AIO reads in flight; `0`/`1` = stock serial loop |
| `podsnapshot.io/criu-shmem-threads` | shmem/memfd objects restored at once; `1` = stock serial loop |
| `podsnapshot.io/criu-image-io-mode` | `writeback` (default) or `direct` |

Setting the first two to their off values makes the patches inert without
reinstalling anything, which is the supported way to check whether a restore
problem is the fork's fault.

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

- The [fuse-cache](https://github.com/nickaggarwal/fuse-cache) client
  DaemonSet running with its mount at `/mnt/fuse` (configurable via
  `agent.fuseMount`). It must be scheduled on the GPU nodes too (add the
  GPU pool to its node affinity and a `nvidia.com/gpu` toleration).
  Size its memory limit for your artifact sizes — multi-GB checkpoint
  tars buffer 4 MB chunks in flight on both the write (cloud persist)
  and read (range-read) paths; an OOM-kill tears down the FUSE mount on
  that node (verified: 4 Gi and 8 Gi limits both OOMed on 3 GB tars;
  16 Gi held). Directory artifacts help here too: the largest object in
  flight is one CRIU image file rather than the whole checkpoint.
- Two behaviors of the mount that pod-snapshotter works around, both
  measured on the cluster — worth knowing if you write to it yourself:
  `rename` copies without unlinking the source (every artifact tar had a
  full-size `.tar.part` beside it until `RenamePublish` started cleaning
  up), and `O_TRUNC` is ignored (re-uploading a revision left files that
  had shrunk carrying the previous upload's tail, so the agent unlinks
  before every overwrite).
- Its HTTP API reachable (default `127.0.0.1:8081` on each node via
  hostNetwork, and a `fuse-client` Service for the manager).
- Optional but recommended: the fuse-client agent socket
  (`/var/run/fuse-client/agent.sock`, flag `-enable-agent-server`) for
  artifact pinning. Without it restores still work — just unpinned.
- Optional and worth it: point `agent.nvmeCacheRoot` at the client's
  node-local cache tier (default `/mnt/fuse-nvme0n1/fuse-cache`) so restores
  read the CRIU images straight off the device instead of back through the
  FUSE mount. The client already promotes everything it serves onto that tier
  as a 1:1 mirror of the artifact prefix, and reading it back through
  userspace costs about 7× — 500-690 MB/s through the mount against 2.6 GB/s
  single-stream and 4.5 GB/s at four streams on the raw device. Measured
  end-to-end on the 14B artifact: 253 s → **40 s**.

  Two things to get right. It is a **host** path, not a path inside the agent
  container: the agent execs runc through `nsenter -t 1 -m`, so runc resolves
  `--image-path` against the host's mount namespace. And the bypass only
  engages when the tier holds every file the artifact's MANIFEST lists at the
  size it lists — a partial mirror declines and falls back to the mount, and a
  size that disagrees is an error rather than a silently wrong restore. Leave
  it unset to disable.

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

`SnapshotBuild` sets this on the build pod for you.

### What must not be in the image

A process is dumpable only if nothing it holds is un-dumpable, and
restorable only if nothing it holds is bound to the pod it was dumped from.
Every item below was hit checkpointing vLLM 0.9.2 on an A100; the reference
shim ([hack/snapshot-shim.py](../hack/snapshot-shim.py)) handles all of them,
and any other workload has to handle them too.

| Must not be in the image | Why | How the shim handles it |
|---|---|---|
| An NVML handle on `/dev/nvidiactl` | `cuda-checkpoint` releases the descriptors the CUDA *runtime* owns. NVML's is not one, so it survives into the dump: `Can't dump file 9 of that type [20666] (chr 195:255)` | unwinds `nvmlInit`'s refcount in every process, frontend and engine worker |
| Descriptors inherited across `fork` | A forked engine worker inherits the frontend's nvidiactl fd. It then belongs to no library in the child, so `cuda-checkpoint` does not release it either. vLLM only forces `spawn` when the parent has initialized CUDA — NVML alone does not count | `VLLM_WORKER_MULTIPROC_METHOD=spawn` |
| A socket bound to the pod IP | The restored pod has a different IP: `Can't bind inet socket back: Cannot assign requested address`. vLLM's engine ZMQ endpoints and the `torch.distributed` TCPStore are both created during engine init, so they are in the image however late the frontend starts | `VLLM_HOST_IP=127.0.0.1`, `GLOO_SOCKET_IFNAME=lo` — loopback exists identically in every pod |
| An established connection to anything outside the pod | Nothing can bring it back on another node. vLLM's usage reporting holds one open to the public internet | `VLLM_NO_USAGE_STATS=1`, `DO_NOT_TRACK=1` |
| A deleted-but-mapped file on `/dev/shm` with a surviving link | glibc's `sem_open` leaves every POSIX semaphore mapped from a deleted path, so any Python `multiprocessing` primitive produces one. See the `link-remap` note in §2 | unlinks the surviving names, taking `nlink` to zero so CRIU writes a ghost file instead |
| io_uring rings, NCCL communicators, TCP listeners | CRIU cannot dump an io_uring ring at all, and the rest are bound to the node | created only *after* the resume point, so they are never in the image |

The last row is the whole reason for the quiesce protocol — see
[design-v2.md §4](design-v2.md#4-workstream-2--quiesceresume-hooks).

### Quiesce/resume contract

A workload opts in by annotating the pod:

```yaml
metadata:
  annotations:
    podsnapshot.io/quiesce: "presence-file"
    podsnapshot.io/quiesce-dir: "/snapshot"      # default
    podsnapshot.io/quiesce-timeout: "900s"       # default 10m
```

The rendezvous directory must be a writable volume (an `emptyDir`) mounted
into the container: it is captured in the image, and the agent writes into
the *restored* pod's copy of it.

The workload then:

1. initializes — weights loaded, kernels warm, CUDA graphs captured;
2. releases what it can afford to lose (`llm.sleep(level=1)` for vLLM);
3. creates `<quiesce-dir>/ready-for-checkpoint`;
4. blocks polling for `<quiesce-dir>/restore-complete` — **the checkpoint
   happens here**, while the process sits in the loop;
5. on restore, finds the file already present, reacquires what it released
   (`wake_up()`), and only *then* opens its listening sockets.

The manager waits for step 3 (phase `Quiescing`) instead of the readiness
probe, and fails with `QuiesceTimeout` if it never comes. The agent writes
the resume file immediately before `runc restore` returns, so the poll loop
sees it the first time it spins.

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

Cross-node restore is verified on AKS: an artifact checkpointed on one A100
node of a pool restores cleanly on another node of the same pool (same VMSS
image → same GPU/driver/CRIU). Note that `file://` artifacts are node-local
— for cross-node restores use the `fuse://` scheme
([fuse-cache](https://github.com/nickaggarwal/fuse-cache) distributed
cache), which is verified end-to-end: snapshot written to the source node's
mount, restore streaming it through the target node's mount from the
peer/cloud tiers.

## Failure annotations reference

| Annotation value | Fix |
|---|---|
| `criu-missing` / `criu-version-lt-4.1` | install/upgrade CRIU on the node |
| `criu-cuda-plugin-missing` | install cuda_plugin.so into CRIU's plugin dir |
| `cuda-checkpoint-missing` | put the NVIDIA binary on the host PATH |
| `nvidia-driver-lt-570` | upgrade the driver |
| `criu-runc-conf-missing` / `runc-conf-no-*` | write `/etc/criu/runc.conf` (see §2) |
| `fuse-mount-missing` | fix the fuse-client DaemonSet / mount path |

The agent also publishes the node's restore-compatibility tuple as
`podsnapshot.io/compat` (`gpu=…;driver=…;criu=…`) plus a label
`podsnapshot.io/compat-hash`. A `PodRestore` with `spec.buildRef` confines
its placeholder pod to nodes carrying the build's hash, so a mismatched node
is rejected by the scheduler rather than by `runc restore`.
