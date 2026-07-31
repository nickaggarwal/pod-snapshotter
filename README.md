# pod-snapshotter

A Kubernetes operator that **snapshots running GPU (CUDA) pods and restores
them from tar artifacts** — eliminating multi-gigabyte model cold starts for
ML inference workloads.

Based on the approach in
[GPU snapshots for reducing ML inference cold starts](https://nilesh-agarwal.com/gpu-snapshots-for-reducing-ml-inference-cold-starts-2/):
CRIU 4.1+ (with the NVIDIA CUDA plugin) and NVIDIA `cuda-checkpoint` freeze a
running CUDA container — GPU memory, CUDA contexts, process state — into a
single tar archive. Restore memory-maps that state back instead of paying the
disk→GPU model load and warmup cost.

Artifacts are stored on the
[fuse-cache](https://github.com/nickaggarwal/fuse-cache) distributed cache
filesystem: snapshot tars written to the FUSE mount persist to the cloud
tier (S3/Azure/GCS) automatically, and restores read through the 3-tier
cache — hot artifacts come from local NVMe or peer nodes, cold ones stream
from cloud.

**Verified end-to-end on AKS** (Ubuntu 24.04 GPU pool, A100, containerd 2.3,
CRIU 4.2.1, driver 580): a PyTorch pod holding a ~2 GB CUDA tensor was
checkpointed to a 3 GB tar and restored into a fresh placeholder pod — the
process resumed exactly where it left off (its counter continued from the
checkpointed value) with GPU memory re-attached by the CRIU CUDA plugin.
**Cross-node restore verified too**: the same artifact, checkpointed on one
A100 node, restored onto a *different* node in the pool (identical GPU
model, driver, and CRIU — see the environment-matching table in
[docs/prerequisites.md](docs/prerequisites.md)) — including fully through
the `fuse://` path: snapshot written to the FUSE mount on the source node,
restore reading it through the target node's own mount via the distributed
cache (peer/cloud tiers), no manual artifact copy.

**CPU-only pods work as well** (verified on a D4as_v4 Ubuntu 24.04 pool): the
pipeline is plain CRIU underneath — GPU handling is an extension that no-ops
when the checkpoint has no NVIDIA state. A Python pod with a ~40 MB heap
checkpointed in seconds and restored (via `fuse://`) resuming from the
checkpointed counter. CPU nodes only need containerd ≥ 2.0, CRIU (the
nodeSetup DaemonSet installs it), and the AppArmor-Unconfined workload
setting; there is no driver/GPU matching constraint between nodes.

## How it works

```
SNAPSHOT                                          RESTORE
────────                                          ───────
PodSnapshot CR                                    PodRestore CR (points at tar URI)
  │                                                 │
  ▼ manager                                         ▼ manager
POST kubelet :10250/checkpoint/{ns}/{pod}/{ctr}   creates "placeholder" pod from template
  │  (CRI → containerd → runc → CRIU +              │  (same image, same GPU request,
  │   cuda-checkpoint drains CUDA, copies           │   command = keeper `sleep`)
  │   VRAM to host, CRIU dumps process)             ▼ node agent (DaemonSet)
  ▼                                               pre-warm: stream tar through FUSE cache
tar at /var/lib/kubelet/checkpoints/...           pin artifact against cache eviction
  │                                               unpack tar, rewrite OCI spec:
  ▼ node agent (DaemonSet)                          – join placeholder's net/ipc/uts ns
copy → /mnt/fuse/snapshots/<ns>/<name>/<ctr>.tar    – fresh PID ns (no PID collisions)
  │  (fuse-client persists to cloud tier)           – remap kubelet volume paths
  ▼                                                 – keep /dev/nvidia* binds rprivate
status.artifact = {uri, size, sha256}             runc restore --image-path checkpoint/
                                                    │  (CRIU CUDA plugin reattaches GPU)
                                                    ▼
                                                  restored process serves in the pod's
                                                  netns → readiness probe turns Ready
```

Two CRDs:

- **PodSnapshot** — point at a running pod+container; produces a tar artifact.
- **PodRestore** — point at a tar artifact URI (or a PodSnapshot); produces a
  running pod resuming from the checkpoint.

Two binaries (fuse-client conventions: `cmd/<binary>`, `internal/<domain>`):

| Binary | Runs as | Role |
|---|---|---|
| `manager` | Deployment | PodSnapshot + PodRestore controllers: kubelet checkpoint calls, placeholder pod lifecycle, artifact verification |
| `agent` | privileged DaemonSet (hostPID) | tar upload to fuse mount, artifact pre-warm/pin, `runc restore` via nsenter into the host, node prereq checks |

The kubelet **has no restore endpoint** — that's why restore is done by the
node agent driving the host's `runc restore` directly, joining the restored
container into a kubelet-managed placeholder pod's sandbox (network/IPC/UTS
namespaces, GPU allocation, volumes). A fresh PID namespace sidesteps CRIU's
original-PID requirement.

## Quick start

```bash
# Build
make build           # bin/manager, bin/agent (needs Go >= 1.24)
make test

# Images + deploy
make docker-build
helm install pod-snapshotter charts/pod-snapshotter -n pod-snapshotter --create-namespace

# Snapshot a running GPU pod
kubectl apply -f config/samples/vllm-pod.yaml        # wait until Ready (the slow cold start)
kubectl apply -f config/samples/podsnapshot_vllm.yaml
kubectl get podsnapshot -w                            # Pending → Checkpointing → Uploading → Completed

# Kill it, restore from the tar
kubectl delete pod vllm-demo
kubectl apply -f config/samples/podrestore_vllm.yaml
kubectl get podrestore -w                             # → Running (seconds, not minutes)
```

See [docs/demo.md](docs/demo.md) for the full timed walkthrough and
[docs/prerequisites.md](docs/prerequisites.md) for node setup (CRIU 4.1+,
cuda-checkpoint, driver ≥ 570, runc config, feature gates).

## Artifact URIs

```
fuse:///snapshots/<ns>/<name>/<ctr>.tar   → /mnt/fuse/... on every node (default)
file:///abs/path.tar                      → node-local path (testing)
```

Anything that can produce a CRI checkpoint tar can be restored — point
`spec.artifactURI` at it.

## Node prerequisites (summary)

The agent probes each node and publishes `podsnapshot.io/prereqs: ok` (or the
failing checks) as a node annotation; the manager refuses to checkpoint on
nodes that aren't ready. Checks: CRIU ≥ 4.1 + CUDA plugin, `cuda-checkpoint`
on PATH, NVIDIA driver ≥ 570, `/etc/criu/runc.conf` with `tcp-established` +
`link-remap`, kubelet `ContainerCheckpoint` feature gate (default-on ≥ 1.30),
**containerd ≥ 2.0** (1.7 lacks the CRI `CheckpointContainer` RPC — on AKS
that means Ubuntu 24.04 node pools, not 22.04) or CRI-O ≥ 1.25, fuse-client
mounted at `/mnt/fuse`.

## fuse-client integration

- **Upload**: the agent streams the kubelet tar to the FUSE mount
  (atomic `.part` + rename); fuse-client's write-through persists it to cloud.
- **Pre-warm**: before restore, the agent sequentially reads the tar through
  the mount — fuse-client promotes every miss to local NVMe (peers → cloud).
- **Pin**: the agent creates a pinned session over the fuse-client agent
  socket (`/var/run/fuse-client/agent.sock`) so the artifact can't be evicted
  mid-restore; unpinned at teardown.

pod-snapshotter never modifies fuse-client — it consumes the mount, the HTTP
API (`:8081`), and the session gRPC socket. `proto/agent.proto` is a copy of
fuse-client's (regenerate stubs with `make proto`).

## Development

```bash
make generate manifests   # after editing api/ types
make vet test
make helm-template        # render the chart
```

Tests cover the kubelet client (fake kubelet server), URI parsing, checkpoint
tar unpacking (incl. path-traversal rejection), OCI spec rewriting (namespace
joins, kubelet path remaps, NVIDIA device propagation), atomic upload, and
placeholder pod construction. GPU-dependent behavior (cuda-checkpoint, CRIU
CUDA plugin, real restores) needs a GPU node — see
[docs/testplan-gpu.md](docs/testplan-gpu.md).

## Known limitations (v1)

- containerd only (CRI-O's native checkpoint-image restore is a planned
  alternative backend).
- Restored processes keep established TCP state per `/etc/criu/runc.conf`;
  peers are usually gone after a delete+restore. Set annotation
  `podsnapshot.io/tcp-close: "true"` on the PodRestore to restore with
  established connections closed.
- The restored container is invisible to `kubectl logs`/`exec` (it is a
  sibling runc container in the pod's cgroup, not a kubelet-managed
  container). The placeholder pod's readiness probe is the health signal.
- Restore requires an identical environment: same image, same GPU model,
  same driver/CRIU versions (the standard CRIU/cuda-checkpoint constraint).
