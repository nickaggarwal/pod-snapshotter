# Design v2 — aligning with NVIDIA Dynamo Snapshot

Status: **§3–§5 and §6b implemented and verified on A100.** v1 (tar
artifacts, live-pod checkpoints) keeps working unchanged — `artifactFormat`
defaults to `tar`, and a pod without the quiesce annotation takes the v1 path.

| Workstream | State |
|---|---|
| §3 image directories | shipped — `artifactFormat: dir`, MANIFEST commit, parallel pre-warm, restore reads the images in place |
| §4 quiesce/resume | shipped — `podsnapshot.io/quiesce`, `Quiescing` phase, [hack/snapshot-shim.py](../hack/snapshot-shim.py) |
| §5 build artifacts | shipped — `SnapshotBuild` CRD, `PodRestore.spec.buildRef`, compatibility-constrained placement |
| §6a `--stream` restore | not started — `criu-image-streamer` is not on the node images |
| §6b forked CRIU | shipped — v4.2.1 + 3 patches, opt-in DaemonSet, per-restore tunables |
| §7 weight decoupling | not started |

Measured numbers are in [§8](#8-rollout). What running §4 against a real vLLM
engine actually cost is in [§10](#10-what-the-workload-had-to-give-up), and
the two fuse-mount behaviors that shaped §3 are in [§11](#11-storage-behavior-that-shaped-the-design).

NVIDIA published
[Dynamo Snapshot](https://developer.nvidia.com/blog/nvidia-dynamo-snapshot-fast-startup-for-inference-workloads-on-kubernetes/)
in the same problem space as this repo: CRIU + `cuda-checkpoint` to kill LLM
cold starts on Kubernetes. Their architecture is close enough to ours that the
differences are directly actionable, and several of them are *also* the fix for
problems we already hit (the io_uring dump wall, the multi-GB tar, the 10-minute
`runtimeRequestTimeout` pressure).

This document records those differences and the plan to close them.

---

## 1. Where we differ today

| Dimension | Dynamo Snapshot | pod-snapshotter today |
|---|---|---|
| Checkpoint driver | Agent calls CRIU / `runc checkpoint` directly, images written straight to storage | kubelet `/checkpoint` API → **CRI tar**; restore must untar before CRIU starts |
| Workload cooperation | Quiesce/resume hooks; checkpoint taken at a chosen safe point | Checkpoint a live serving pod as-is (hence the io_uring wall) |
| KV cache | Physically unmapped before dump: 190 GiB → 6 GiB (Qwen3-0.6B, B200) | Whole live process including all device memory |
| CRIU read path | Patched: threaded memfd restore + native AIO (`io_submit`, 128-deep, `O_DIRECT`) | Upstream single-threaded `preadv` |
| Weights | Decoupled into a separate artifact (GMS), restored in parallel with the process | Inside the CRIU image, serialized behind it |
| Artifact lifecycle | Built once, restored many times | One tar per live pod checkpoint |

Their published restore numbers, for calibration — these are *CRIU restore
only*, not end-to-end pod ready:

| Model | CRIU image | Upstream CRIU | Patched CRIU | Speedup |
|---|---|---|---|---|
| Qwen3-0.6B | 6.2 GiB | 6.8 s | 2.4 s | 2.8× |
| Qwen3-8B | 26 GiB | 24 s | 4.7 s | 5.1× |
| gpt-oss-120b | 129 GiB | 119 s | 15 s | 7.9× |

With GMS (weights split out and restored over a striped-NVMe channel in
parallel with CRIU) they report gpt-oss-120b ready in **under 5 s**, ~21×
better than cold start.

Our own restore-path breakdown is dominated by a step Dynamo simply does not
have: **untar**. `internal/restore/bundle.go` `Unpack()` walks the whole CRI
archive and writes every CRIU image file out again before `runc restore` has
read a single page. On a multi-GB artifact that is seconds of pure copy, on the
critical path, single-threaded, before the work we actually care about begins.

That makes the payoff order below start somewhere other than where the blog
starts.

---

## 2. Target architecture

```
BUILD TIME (once per image × model × GPU SKU)     SCALE-UP (every 0→1)
─────────────────────────────────────────────     ────────────────────
SnapshotBuild CR                                  PodRestore CR
  │                                                 │ (artifact = build output)
  ▼ one-shot Job on a GPU node                      ▼ manager
engine init (weights, warmup, CUDA graphs)        placeholder pod scheduled
  │                                                 │  (keeper holds sandbox + GPU)
  ▼ shim: llm.sleep(level=1) / torch_memory_saver   │
KV physical pages released (cuMemUnmap +          ┌─┴─ overlapped ─┐
  cuMemRelease), VA reservation kept              │                │
  │                                          prefetch image   placeholder
  ▼ touch /snapshot/ready-for-checkpoint      dir → node NVMe   reaches Ready
  ▼ blocks on /snapshot/restore-complete           └─┬─ join ─┘
  │                                                 ▼ node agent
  ▼ node agent: cuda-checkpoint + criu dump       runc restore --image-path <dir>
IMAGE DIRECTORY on node NVMe                        │  (no untar; parallel reads)
  │  images/<revision>/{checkpoint/,spec.dump,…}    ▼
  ▼ sync dir (not tar) to fuse/S3                 shim observes restore-complete
                                                    ▼ wake_up() → KV remapped
                                                    ▼ frontend + distributed runtime
                                                  readiness probe → Ready
```

Four workstreams, in payoff order.

---

## 3. Workstream 1 — image directories, not tars

**Payoff: removes the untar step entirely from the restore critical path.**

`runc restore --image-path <dir>` already takes a directory — we hand it
`<work>/checkpoint` *after* materializing it from the tar. The tar exists only
because the kubelet checkpoint API returns one.

### Checkpoint side

Keep the kubelet path as a fallback, but add a direct path that writes CRIU
images straight to node NVMe:

```
/var/lib/kubelet/pod-snapshotter/images/<revision>/
  checkpoint/        CRIU image files (incl. CUDA plugin dumps)
  spec.dump          original OCI runtime spec
  config.dump        runtime metadata
  rootfs-diff.tar    filesystem writes (stays a tar — it is small and is
                     applied to the rootfs, not read by CRIU)
  dump.log
```

Produced either by `crictl checkpoint` with a directory output, or by the agent
calling `runc checkpoint --image-path <dir>` directly against the CRI container
(the Dynamo "no runtime modifications" approach — it is also how we escape the
kubelet's `runtimeRequestTimeout`).

### Transport

Sync the **directory**, not a tar. `fuse:///snapshots/<ns>/<name>/<ctr>/` as a
prefix rather than a `.tar` object. Per-file objects are what make the next two
things possible:

- **Parallel prefetch** — N files fetched concurrently through fuse-client
  instead of one sequential stream. Today `prewarm()` in
  `internal/agent/restore_controller.go` does a single `io.CopyBuffer` over one
  huge file; a directory lets us fan out.
- **Overlapped prefetch** — start pulling the image directory to node NVMe at
  *pod-schedule* time, concurrently with placeholder pod startup, instead of
  strictly after it.

### Code changes

| File | Change |
|---|---|
| `internal/restore/bundle.go` | Split `Unpack()`: new `Open(imageDir, rootfsDir)` builds a `Bundle` from an existing directory; `Unpack()` becomes tar→dir + `Open()`, kept for v1 artifacts |
| `internal/artifact/uri.go` | Accept directory URIs (trailing `/` or a `kind: dir` marker); `DefaultURI()` emits a prefix |
| `internal/agent/restore_controller.go` | `prewarm()` walks the prefix and fetches files concurrently; `restore()` passes the prefetched dir as `ImagePath` with no unpack |
| `internal/agent/upload_controller.go` | Upload a directory tree (atomic per-file `.part` + rename, then a `MANIFEST` written last as the commit marker) |
| `api/v1alpha1/podsnapshot_types.go` | `ArtifactStatus` gains `Format: tar\|dir`, `FileCount`, per-file digests in a manifest object |

**Compatibility:** `Format` defaults to `tar` when absent, so existing artifacts
and existing `PodRestore`s keep working unchanged.

### What shipped, and where it differs from the plan above

- A directory URI is one with a trailing slash. `artifact.URI` carries `Dir`,
  and `CommitPath()` returns the MANIFEST for a directory and the tar itself
  for a tar, so the manager can Stat one path either way.
- `restore.Unpack()` split into `Open(imageDir, workDir, rootfsDir)` over an
  existing directory plus the tar path that feeds it. `Open` only ever reads
  the artifact; everything it writes (the rewritten OCI config, CRIU's work
  dir) goes to node-local scratch.
- **Per-file `.part` + rename was dropped.** On the fuse-client mount, rename
  copies without unlinking the source — every artifact tar on the cluster had
  a full-size `.tar.part` beside it — so per-file temp files would double the
  cost of every artifact for no benefit: the MANIFEST is what commits the
  tree, and a reader that ignores an uncommitted prefix does not care whether
  individual files are torn. Image files are written straight to their final
  names; `RenamePublish` handles the one rename that remains. See §11.
- The expansion also captures the container's `/dev/shm` as `shm-diff.tar`.
  It is pod-scoped and external to the CRIU image, so nothing else carries
  it, and CRIU needs it back — same idea as `rootfs-diff.tar`.
- Pre-warm defaults to reading the files through the mount (fuse-client
  promotes every miss to node NVMe) and `runc restore --image-path` then
  points straight at the artifact: no copy anywhere on the restore path.
  `--stage-image-local` copies to node NVMe instead, so the two can be
  measured against each other. They were, and staging lost badly — it wrote
  to the OS disk rather than the NVMe (§6c). Better still is
  `--nvme-cache-root`, which skips pre-warm entirely when fuse-client has
  already promoted the artifact and points `--image-path` at the cache tier
  on the device.

---

## 4. Workstream 2 — quiesce/resume hooks

**Payoff: fixes the io_uring dump failure, and drops KV cache from the image.**

This is the single highest-leverage change to *correctness*, not just speed.
Today we dump a live serving process, which means we dump whatever it happens to
be holding: io_uring rings (CRIU cannot dump them), a listening frontend socket
with live peers, NCCL/RDMA registrations, and a KV cache full of nothing useful.

### Presence-file protocol

Exactly Dynamo's design. An entrypoint shim wraps the engine:

1. Initialize the engine — load weights, warm kernels, capture CUDA graphs.
2. Release KV physical memory: `llm.sleep(level=1)` (vLLM) or
   `torch_memory_saver` region release (SGLang).
3. `touch /snapshot/ready-for-checkpoint`.
4. Block polling for `/snapshot/restore-complete`.
5. — *checkpoint happens here, asynchronously, while the process sits in the
   poll loop* —
6. On restore, CRIU resumes execution at the exact instruction inside the poll
   loop. The file is now present.
7. `wake_up()` — KV physical pages re-mapped into the reserved VA range.
8. *Only now* start the HTTP frontend and the distributed runtime.

### Why this fixes io_uring

Step 8 is the lever. Everything un-dumpable — the frontend's io_uring-backed
event loop, uvloop's rings, NCCL communicators, RDMA registrations, TCP
listeners — is created *after* the quiesce point, so it does not exist in the
image at all. It is constructed fresh on the resume side, on a node where it can
actually be constructed correctly. This is the same reason Dynamo lists
quiesce/resume as the prerequisite for their multi-GPU/multi-node work.

### Why this shrinks the image

At the quiesce point the KV cache has never served a request. vLLM/SGLang
allocate it through the CUDA VMM API (`cuMemCreate` + `cuMemMap`), so the
physical pages can be dropped with `cuMemUnmap` + `cuMemRelease` while the
**virtual address reservation stays put** — which is what keeps the captured
CUDA graphs valid across the restore. Dynamo measures 190 GiB → 6 GiB on a B200
for Qwen3-0.6B; the ratio scales with how much of the GPU the KV cache was
allowed to claim.

`sleep(level=1)` is the supported, no-custom-code version of this. Note that it
also offloads weights to host RAM — which for our purposes is roughly neutral,
since `cuda-checkpoint` moves device memory to host during dump anyway. If we
want to go below what `sleep()` gives us, the lower-level route is driving
`cuMemUnmap`/`cuMemRelease` on the KV pool directly and keeping weights resident
on the device for `cuda-checkpoint` to capture. That is an optimization, not the
first cut.

### Interface

The shim is workload-side, so it must be a contract, not a code dependency:

```yaml
# on the pod being snapshotted
metadata:
  annotations:
    podsnapshot.io/quiesce: "presence-file"
    podsnapshot.io/quiesce-dir: "/snapshot"        # default
    podsnapshot.io/quiesce-timeout: "600s"
```

- Manager waits for `<dir>/ready-for-checkpoint` (polled via the agent, which
  can already see container filesystems) before issuing the checkpoint, instead
  of waiting on the readiness probe.
- Agent creates `<dir>/restore-complete` inside the restored container's rootfs
  immediately **before** `runc restore` returns control — the file must be
  visible the moment the poll loop next spins.
- `<dir>` must be a writable `emptyDir`; it is captured in the image, so the
  agent writes into the restored container's view of it.

We ship a reference shim (`hack/snapshot-shim.py`) covering vLLM, and document
the protocol for anything else. Workloads without the annotation take the v1
path unchanged.

### What shipped

`PodSnapshot` gains a `Quiescing` phase between `Pending` and
`Checkpointing`. The manager resolves the contract off the pod's annotations
and sets the deadline; the node agent polls for the presence file and hands
the snapshot back to the manager when it appears. The agent reads the
rendezvous directory through the container init's mount namespace
(`/proc/<pid>/root/...`) — an `emptyDir` is a mount *inside* the container and
is not visible under its bundle rootfs, which is the one thing about this that
is not obvious.

On restore the agent writes the resume file through the *keeper* container's
mount namespace: the keeper mounts the same `emptyDir` the restored workload
will, and the spec rewriter has already remapped that volume onto the new pod.
The contract is also recorded in the artifact MANIFEST, so a quiesced
checkpoint is self-describing and a `PodRestore` does not have to repeat the
annotations.

`llm.sleep(level=1)` measured on an A100 80GB, vLLM 0.9.2:

| Model | Device memory before | After | Freed |
|---|---|---|---|
| Qwen2.5-1.5B-Instruct | 48.6 GiB | 0.98 GiB | 47.6 GiB |
| Qwen2.5-14B-Instruct | 72.5 GiB | 1.31 GiB | 71.2 GiB |

The ratio tracks how much of the GPU the KV cache was allowed to claim, as
Dynamo's does. Note what it does *not* do: `sleep(level=1)` offloads weights
to host RAM rather than dropping them, so they move from the device side of
the image to the host side rather than leaving it. That is why the artifacts
below are still roughly weights-sized — and why §7 is the thing that would
actually shrink them.

---

## 5. Workstream 3 — snapshots as build artifacts

**Payoff: makes quiesce usable at all, and removes the checkpoint timeout
pressure.**

Quiesce-then-checkpoint is destructive: a replica that has released its KV cache
and parked in a poll loop is not serving. So it cannot be a live serving pod.

The fix is to stop thinking of a snapshot as "a picture of this pod" and start
thinking of it as "a build output for this revision":

- **One artifact per `(image, model, GPU SKU, driver, CRIU version)`**, built by
  a one-shot Job at revision-publish time, on a node matching that SKU.
- Every 0→1 scale-up restores that same image. N restores, one build.
- The artifact is immutable and content-addressed; the tuple above is exactly
  the environment-matching table already in
  [prerequisites.md](prerequisites.md#restore-environment-matching), so it
  doubles as the compatibility key we check before scheduling a restore.

### New CRD: `SnapshotBuild`

```go
type SnapshotBuildSpec struct {
    // PodTemplate for the build pod — the real workload plus the shim.
    PodTemplate corev1.PodTemplateSpec

    // Revision identifies the artifact; also the directory name under the
    // image root. Immutable.
    Revision string

    // Compatibility is the environment tuple restores are matched against.
    // Filled in from the build node if unset.
    Compatibility *CompatibilityKey   // gpuModel, driverVersion, criuVersion, imageDigest

    // ArtifactURI defaults to fuse:///snapshots/builds/<revision>/
    ArtifactURI string
}
```

`PodRestore` gains `spec.buildRef` alongside `artifactURI` and `snapshotRef`.
The restore controller refuses to schedule onto a node whose
`podsnapshot.io/prereqs` compatibility tuple does not match the build's —
today that mismatch is a runtime failure deep inside `runc restore`.

### What shipped

`SnapshotBuild` runs a bare Pod rather than a Job: the checkpoint is taken of
one specific running container, and a Job's restart semantics only get in the
way of a pod that is deliberately parked in a poll loop and never exits. The
build drives an ordinary `PodSnapshot` against that pod, so the quiesce wait
is the same code path as everywhere else, and deletes the pod once the
artifact exists.

The compatibility tuple is published by the agent as the node annotation
`podsnapshot.io/compat` plus a label `podsnapshot.io/compat-hash` — annotations
cannot be selected on, and the GPU model contains spaces so it cannot be a
label value. A `PodRestore` with `buildRef` copies the build's tuple into its
status, and `BuildPlaceholderPod` adds the hash to the pod's `nodeSelector`.
The scheduler then never places the pod somewhere the artifact cannot restore;
a pinned `spec.nodeName`, which bypasses the scheduler, is checked directly
and fails with a specific message instead.

### Consequences

- The `runtimeRequestTimeout: 10m` problem disappears: the checkpoint no longer
  runs inside a kubelet API call on a serving pod's critical path. It runs in a
  Job that can take as long as it takes.
- `PodSnapshot` (checkpoint a live pod) stays, for debugging and for
  non-quiescible workloads. It is no longer the primary path.
- Autoscaler integration becomes trivial: a scale-up controller creates a
  `PodRestore` pointing at the current revision's build. Whatever swaps the
  workload container for a keeper and annotates the pod with the artifact
  reference now points at a build-time URI rather than a per-pod tar — the
  mechanism in `internal/controller/placeholder_pod.go` is unchanged.

---

## 6. Workstream 4 — restore-side I/O parallelism

**Payoff: the 2.8×–7.9× in the table above. Only reachable after workstream 1.**

Dynamo's numbers come from a patched CRIU that is **not upstream yet**. Two
routes, and they are not exclusive:

### 6a. `criu-image-streamer` (no patches, available today)

`criu restore --stream` reads the image through multi-pipe parallel streams and
skips the filesystem round-trip. It attacks the same bottleneck the AIO patch
does — storage bandwidth left on the floor by a one-read-at-a-time restore —
without a CRIU fork to maintain. This is the default we should ship.

Requires image-directory artifacts (workstream 1) and a `runc restore` build
that forwards the flag; verify against the node's runc before enabling, and fall
back silently if absent.

### 6b. Forked CRIU (only if 6a is not enough)

Two changes, both in CRIU's restore read path:

- **`criu/pagemap.c`** — replace the sequential `read_local_page`/`preadv` loop
  with an `io_submit`/`io_getevents` sliding window (build the `iocb` job list
  up front, keep ~128 reads in flight, backfill on completion). Use `O_DIRECT`
  when the backing filesystem supports it, to avoid a page-cache copy on a
  one-pass streaming read; fall back to buffered I/O with sequential readahead
  on NFS.
- **`criu/shmem.c`** — enumerate unique shmem/memfd objects first, then restore
  them from a thread pool instead of the serial create → resize → map → read →
  next loop. This is the path that matters for us specifically: vLLM and SGLang
  park GPU allocations in pinned CPU shadow buffers that appear to CRIU as
  memfds, so on a GPU checkpoint this *is* most of the image.

**Track upstream before writing any of this.** NVIDIA says both are pending
merge; a rebase onto CRIU v4.3+ may hand us the whole thing for free, and a
private fork means owning driver/plugin compatibility for every node image.
Concretely: check the CRIU tree at the start of this workstream, and only fork
if the patches are still unmerged *and* 6a measured short.

### What shipped

The upstream check came back the wrong way. The AIO work **is** merged
(checkpoint-restore/criu#3022 and #3066) but there is no release carrying it:
the newest tag is still v4.2.1, which is exactly what the node images run. So
the choice was not fork-vs-upstream, it was fork-vs-wait. We forked.

The fork is a real fork, published at
[github.com/nickaggarwal/criu](https://github.com/nickaggarwal/criu), branch
`pod-snapshotter/v4.2.1-restore-parallelism`. It branches off the upstream
`v4.2.1` tag (commit `9539417`) and carries four commits, each a reviewable,
rebasable change on its own:

| Commit | Mirrored patch | What it changes |
|---|---|---|
| `be64855` | `0001-make-shared-restore-state-thread-safe.patch` | `criu/bfd.c`, `criu/log.c` — locks the two shared statics the pools reach |
| `1398bff` | `0002-pagemap-native-aio-async-page-reads.patch` | `criu/pagemap.c` — the async read path gets an `io_submit`/`io_getevents` sliding window |
| `53e7e72` | `0003-shmem-parallel-restore.patch` | `criu/shmem.c`, `criu/mem.c` — a thread pool that creates, sizes and fills shmem objects concurrently |
| `2090e7f` | `0004-memfd-parallel-inode-restore.patch` | `criu/memfd.c` — the same pool shape for memfd inodes |

`add24ee` on top is `POD-SNAPSHOTTER.md`, which describes the branch to anyone
who finds the fork without this repo, and is the commit `Dockerfile.criu`
pins.

The same four changes are mirrored into `hack/criu/patches/*.patch` so the
series stays readable from this repo without cloning anything, and so it can
be re-cut against a newer upstream base. The mirror is kept identical to the
published commits — same files, same hunk counts.

[Dockerfile.criu](../Dockerfile.criu) builds from the fork, pinned to a
**commit** rather than the branch tip: what shipped to a node has to be
reconstructible later, and a moving branch would make the image tag ambiguous.
It then asserts that `Makefile.versions` still reads exactly `4.2.1` before
building, so a future rebase onto a different upstream base fails at build
time rather than shipping a binary whose CRIU image format silently disagrees
with the node's CUDA plugin.

**The CUDA plugin is deliberately not replaced.** The fork is based on the
exact CRIU version the node already runs, so the plugin ABI is unchanged and
the patched binary loads the node's existing
`/usr/lib/criu/cuda_plugin.so`. The GPU-critical piece — the one whose
interaction with the driver was validated the hard way — stays byte-identical.
A freshly built plugin ships alongside and is only laid down on a node that
has none.

Installation goes to `/usr/local/sbin`, which precedes `/usr/sbin` on the
default PATH, so runc picks up the patched binary without the distro package
being touched. Uninstalling is `rm`. The installer
([hack/criu/install.sh](../hack/criu/install.sh), run by an opt-in DaemonSet
gated on `criu.enabled`) refuses to leave a binary behind that the host cannot
execute: it runs `criu --version` under `chroot` and rolls back on failure,
because a missing shared library would otherwise surface as a failed restore
minutes later on a different code path.

### Per-restore tuning, not per-node

All three patches read their settings from the environment, and
`runc restore` is given that environment per restore
([internal/restore/runc.go](../internal/restore/runc.go)):

| Annotation | Env | Meaning |
|---|---|---|
| `podsnapshot.io/criu-aio-depth` | `CRIU_AIO_DEPTH` | reads in flight; `0`/`1` = stock serial `preadv` loop |
| `podsnapshot.io/criu-shmem-threads` | `CRIU_SHMEM_RESTORE_THREADS` | shmem/memfd objects restored concurrently; `1` = stock serial loop |
| `podsnapshot.io/criu-image-io-mode` | `CRIU_IMAGE_IO_MODE` | `writeback` (default) or `direct` for `O_DIRECT` |

That shape was chosen for one reason: **A/B on the same binary.** Setting
depth `0` and threads `1` makes both patches inert, so a regression can be
attributed to the patches rather than to the rebuild, without reinstalling
anything on the node. It also means a stock CRIU can be handed the same
annotations and will simply ignore them, which is why the agent sets them
unconditionally rather than probing for the fork first.

### What the pools got wrong the first time

The first build of the pools (`v4.2.1-ps3`) failed **every** restore it was
enabled for, and the failure is worth recording because the fix is not local
to the patch — it is a property of where CRIU's restore runs.

```
148: Error (criu/util.c:1014): Unable to change [10]/ ownership to (0, 0): Bad file descriptor
148: Error (criu/memfd.c:471): Can't set permissions ... of memfd:/dev/zero: Bad file descriptor
148: Error (criu/mem.c:1475): `- Can't open vma
```

`[10]` is the tell: `cr_fchpermat()` prints its `dirfd`, and 10 was a
descriptor the pool had opened in CRIU's **main process** during
`prepare_memfd_inodes()`. But `memfd_open_inode_nocache()` runs inside the
**forked tasks**, which have already rebuilt their fd tables
(`setup_newborn_fds` → `close_old_fds`). By the time the fd number was read it
named nothing, or some unrelated file. Passing an fd *number* through shared
memory only works while everyone still shares an fd *table*.

The memfd pass now publishes through `fdstore_add()` — the mechanism CRIU
already uses for exactly this boundary, a datagram socket every forked task
inherits. `memfd_open_inode()` already prefers `fdstore_id` when set, so a
prepared inode needs no new lookup path; it simply arrives already cached.

**The shmem pass keeps a plain descriptor, and that is correct, not an
oversight.** `shmem_restore_parallel()` is called from `open_vmas()` — already
inside the task that will consume the fd — so it never crosses a fork. The
asymmetry between the two passes *is* the bug, so each side now states which
one it is and why.

Reading the rest of the shared state for the same class of mistake turned up
two races that had not fired yet, either of which would have corrupted a
restore in a much harder-to-attribute way than a bad fd:

- `criu/bfd.c` keeps a free list of read buffers that every worker reaches via
  `open_page_read → open_image_at → bfdopenr → buf_get`. Two workers could
  take the same buffer, or race the refill.
- `criu/log.c` formats every `pr_*` call into one shared static buffer.

Both are now locked (patch 0001). The early-outs in `vprint_on_level()` stay
*outside* the lock, so a filtered-out debug line still costs nothing — the
pools make `pr_debug` a hot path in a way it was not before.

The general lesson for anything else added to this fork: CRIU's restore
crosses a fork boundary partway through, and a patch that adds concurrency
before that point cannot hand anything fd-shaped to the far side except
through the fdstore.

### What the async patch may not do

`process_async_reads_aio()` deliberately declines the AIO path when
`opts.auto_dedup` is on — dedup rewrites the image as it reads, and the
sliding window would have several reads outstanding against a file that is
being punched underneath them.

Making shmem and memfd page reads `PR_ASYNC` — so the pool and the AIO window
compose — was tried and reverted. CRIU merges adjacent async read jobs, and
merging across objects produced a single read spanning past the end of an
object's pages image: `AIO read returned 0 at 268435456, 1 iovs left`, then
`BUG at criu/pagemap.c:963`. The two patches now cover disjoint ground: AIO
accelerates the private-VMA read path, the pools accelerate shmem/memfd. For
the 14B artifact that split is lopsided — 205 of the 208 page images belong to
memfds — so the pools are the half that matters and the AIO window is nearly
idle. On a workload whose memory is mostly private anonymous VMAs it would be
the other way round.

### 6c. The transport ceiling, and how it came down

Halving CRIU restore exposed what was underneath it. 452 s end-to-end for a
52 GiB artifact is ~0.25 GB/s sustained, on *both* phases. PCIe on this node
would move that in about three seconds. So the patched CRIU was no longer the
constraint — getting the bytes to it was — and three things were paying for
that:

1. **The bytes moved twice.** Pre-warm copied the artifact from the fuse mount
   onto node-local storage, then CRIU read that copy. Directory artifacts
   exist precisely so this copy is unnecessary (`PrefetchOpts.StageDir`
   empty = restore in place); the copy was a hand-applied
   `--stage-image-local=true` on the live DaemonSet, not the chart default.

2. **Prefetch ran at the wrong concurrency.** Measured on a warm client, read
   throughput peaks at 4 concurrent files (~1.3 GB/s) and *falls* on either
   side: 542 MB/s at 2, 642 MB/s at 8. The live setting was 2 — chosen not
   because it was fast but because 8 had OOMKilled the fuse client, so the
   workaround for a memory bug became a throughput ceiling.

3. **The client idles near its limit.** `client-fmjpg` sat at 14.98 GiB of a
   16 GiB limit (`memory.peak` 16.03 GiB — already over), of which 6.5 GiB is
   unreclaimable anon. It does not OOM because readers allocate much; it OOMs
   because a burst has ~1 GiB to land in. Raising parallelism without fixing
   that just moves the failure.

**What `/proc/diskstats` said.** Sampling sectors-read per device across a
restore is what actually located the problem, and it was not where any of the
three guesses pointed. During a staging restore the node read **266 MB/s from
`sda`** — the 256 GB OS disk — and **0 B/s from `nvme0n1`**, the 894 GB local
NVMe. The staging copy was writing to and reading back from the wrong device
entirely. Dropping it moved the reads onto `nvme0n1` and cut end-to-end from
452 s to 253 s.

**The mount itself is the remaining ~7×.** With staging gone, measuring the
same bytes two ways: through the FUSE mount, 500-690 MB/s; straight off
`nvme0n1`, 2.6 GB/s single-stream `O_DIRECT` and 4519 MiB/s at four streams.
fuse-client already promotes everything it serves onto a node-local cache tier
laid out as a faithful 1:1 mirror of the artifact prefix (verified: 632 files
on both sides, matching sizes, no `.nvme-stream` temp files). So the bytes are
*already on the device* — reading them back through the userspace filesystem
that put them there is pure tax.

`NVMeCache` (`internal/artifact/nvmecache.go`) skips it. When the tier holds
every file the manifest lists at the size the manifest says, `runc restore`
gets `--image-path` pointed at the cache directory and the mount is out of the
read path. Pre-warm gets the same check: if the bytes are resident there is
nothing to warm.

| | pre-warm | CRIU restore | total to Ready |
|---|---|---|---|
| staging on, P=2 | 225 s | 225 s | 452 s |
| staging on, P=4 | 220 s | 224 s | 446 s |
| staging off, P=4 | 104 s | 147 s | 253 s |
| **NVMe bypass** | **2 s** | **35 s** | **40 s** |

Every row was verified by generation, not by readiness probe:
`"The capital of France is"` → `" Paris. The capital of Spain is Madrid."`

Two things about that table are worth stating plainly. The 11× end-to-end is
almost entirely transport, not CRIU: the patched binary is identical across
the last three rows. And the manifest check is what makes the bypass safe — a
partial mirror declines and falls back to the mount, a size that disagrees
with the manifest is an error rather than a silently wrong restore.

**What is still unmeasured: AIO.** The `criu-aio-depth` A/B is not resolved.
Two runs back to back gave 22 s with AIO at 128 and 23 s with it off — but
`/proc/diskstats` showed **zero sectors read from either device** across the
one of those pairs that was instrumented, because the node has 226 GB of RAM
and the 52 GiB artifact was entirely in page cache from the run before. A read
path cannot be benchmarked against reads that never reach a device, so that
pair is a null result and not evidence about AIO either way. Isolating it
needs the artifact evicted from page cache between runs
(`POSIX_FADV_DONTNEED` over the cache directory, or a node that has not served
this artifact yet). The 35 s row in the table is the first restore after the
bypass landed and is the conservative number of the three; how much of it
reached the device was not instrumented, so treat 35 s as the honest figure
and 22 s as a cache-warm best case rather than a second data point.

**And `restore.log` says the read path is no longer the majority of it.**
CRIU's own clock on that run, with the memfd pool confirmed active
(`Restoring 205 memfd inodes on 8 threads`):

| Phase | Wall | Share |
|---|---|---|
| CRIU proper — images read, memory mapped, tasks built | 8.9 s | 42% |
| `cuda_plugin` resuming devices on the GPU worker | 12.4 s | 58% |
| **total** | **21.3 s** | |

That 12.4 s is a single gap in the log between `cuda_plugin: resuming devices`
and the next line: one `cuda-checkpoint` call, no I/O of ours in it at all. It
reproduces — a second run gave 8.7 s / 12.4 s / 21.4 s against the first run's
8.9 s / 12.4 s / 21.3 s. It is the largest single item left in a restore, it
is inside NVIDIA's plugin rather than in CRIU or in us, and neither the CRIU
fork nor any transport work touches it.

So the read path is now 8.9 s of a 40 s end-to-end — and the ordering of what
to attack next changes accordingly. More AIO depth, `--stream`, faster
devices: all of them divide into the 42%, and the resume half sets a floor
they cannot cross. §7's weight decoupling is the one item on the list that
plausibly moves the GPU side too, by shrinking what has to be resumed rather
than by reading it faster.

(One caveat on that split: it comes from the cache-warm run, the only one with
a `restore.log` still on the node. On the 35 s run the CRIU-proper half would
be larger and the GPU-resume half about the same, since the latter is not
I/O-bound — so the read path's share is somewhere between 42% and roughly
65%, not lower.)

### Against the blog

The blog's table is *CRIU restore only*, so the honest comparison is against
our CRIU-proper number, not against end-to-end:

| | image | CRIU restore | GB/s |
|---|---|---|---|
| Dynamo, Qwen3-0.6B | 6.2 GiB | 2.4 s | 2.77 |
| Dynamo, Qwen3-8B | 26 GiB | 4.7 s | 5.94 |
| Dynamo, gpt-oss-120b | 129 GiB | 15 s | 9.23 |
| **ours, Qwen2.5-14B** | **52.5 GiB** | **8.8 s** | **6.41** |

6.41 GB/s lands between their 8B and 120b rows, on a different model, a
different GPU (A100 vs B200) and an artifact that is roughly 2× the weights
because `sleep(level=1)` offloads to host RAM instead of dropping. On the
metric the blog actually publishes, the read path is there.

Two honest asterisks. That 8.8 s is the cache-warm run, so it is a
memory-bandwidth figure rather than a storage one — the blog does not say
which theirs is either. And their end-to-end story includes GMS, which we have
not built: their "under 5 s ready" for gpt-oss-120b is weights restored over a
separate channel in parallel, not a faster CRIU. Our end-to-end is 40 s, and
the gap between 8.8 s and 40 s is the 12.4 s of `cuda_plugin` plus pod
scheduling and container setup around it — which is where the remaining work
is, and none of it is CRIU's read path.

Note also that the artifact is roughly 2× the model weights: vLLM's
`sleep(level=1)` offloads weights to host RAM rather than dropping them, so
they are captured in the image instead of being re-read from the weight
store. §7 is the structural answer to that half.

Item (3) above is untouched and still real: fuse-client's per-file range
budgets mean N concurrent readers reserve N × (1 GiB chunk cache + 512 MiB
prefetch), which is why P=8 OOMKilled it. The bypass routes around that rather
than fixing it, and the fix belongs in that repo.

---

## 7. Later — weight decoupling

Dynamo's GMS splits model weights out of the CRIU image so the two restore in
parallel over independent channels (GPUDirect Storage, peer-GPU RDMA/NVLink,
striped NVMe). Their split, from the blog:

| Model | Single CRIU image | Core process | Weights |
|---|---|---|---|
| Qwen3-0.6B | 6.2 GiB | 4.3 GiB | 1.2 GiB |
| Qwen3-8B | 26 GiB | 4.8 GiB | 15 GiB |
| gpt-oss-120b | 129 GiB | 6.7 GiB | 74 GiB |

Note the core process is nearly constant while the weights dominate — which is
the whole argument for splitting them.

Not for this cycle: it needs a CUDA driver patch that is not shipped, and the
core-process artifact being roughly constant means workstreams 1–4 capture most
of the available win first. Worth noting that fuse-cache's tiering is a decent
fit for the weight channel when we do get there — weights are the *shared*,
identical-across-revisions part of the artifact, which is exactly what a
distributed cache is good at.

---

## 8. Rollout

| Phase | Contents | State |
|---|---|---|
| 1 | Image directories end-to-end (§3) | **done** |
| 2 | Quiesce/resume + reference shim (§4) | **done** |
| 3 | `SnapshotBuild` + build-time artifacts (§5) | **done** |
| 4 | `--stream` restore, measured (§6a) | not started — `criu-image-streamer` is not on the node images, and the restore is not currently CRIU-read-bound (see below) |
| 5 | Fork CRIU only if §4 measured short (§6b) | **done** — [nickaggarwal/criu](https://github.com/nickaggarwal/criu), branch `pod-snapshotter/v4.2.1-restore-parallelism` |

### Benchmark table to fill in

Every phase lands with this row measured on the A100 pool, end-to-end (CR
created → placeholder pod Ready), not CRIU-restore-only:

| Phase | Artifact size | Prefetch | Untar | CRIU restore | Resume | Total |
|---|---|---|---|---|---|---|
| v1 baseline | | | | | n/a | |
| +§3 dirs | | | *gone* | | n/a | |
| +§4 quiesce | | | — | | | |
| +§6a stream | | | — | | | |
| +§6b fork, patches inert | 52 GiB | 231 s | *gone* | 445 s | — | 677 s |
| +§6b fork, shmem pool ×8 | 52 GiB | 225 s | *gone* | 225 s | — | 452 s |
| +no staging copy | 52 GiB | 104 s | *gone* | 147 s | — | 253 s |
| +NVMe bypass | 52 GiB | **2 s** | *gone* | **35 s** | — | **40 s** |

**677 s → 40 s, 17×**, on the same 52 GiB artifact and the same node, with the
restored engine verified by generation at every step. Of the 637 s removed,
the CRIU fork accounts for 225 s (35%) and transport for 412 s (65%) —
199 s from deleting the staging copy and 213 s from bypassing the mount. The
fork was the thing we set out to build; the larger half turned out to be two
configuration mistakes underneath it, which is worth remembering the next time
a slow restore looks like it needs a patch.

The "patches inert" row is the control, not a separate build: it is the same
`v4.2.1-ps4` binary run with `criu-aio-depth: 0` and `criu-shmem-threads: 1`,
which is what makes the last two rows comparable at all. The pool row is the
same binary again with `criu-shmem-threads: 8` and AIO still off — CRIU
restore halves (−49%), end-to-end drops a third (−33%), and the restored
engine answers correctly (`"The capital of France is"` → `" Paris."`).

The first two rows read the artifact through a fuse-client that was, at the
time, pinned to `--prefetch-parallelism=2` and staging a node-local copy. The
last two are where those minutes went — see §6c.

`docs/testplan-gpu.md` gains the matching cases: directory-artifact restore,
quiesce-point checkpoint of a vLLM pod, resume-side wake_up correctness (KV
re-mapped, CUDA graphs still valid, first token correct), and build-artifact
restore onto a node that never ran the build.

---

## 9. Risks

- **The shim is workload-side.** It is a contract we cannot enforce, and a
  wrong implementation fails at checkpoint time with an unhelpful CRIU error.
  Mitigation: the manager validates the presence file appears within
  `quiesce-timeout` and reports a specific condition, rather than letting the
  checkpoint fail opaquely.
- **`wake_up()` correctness is not covered by "the pod went Ready".** A
  readiness probe passes with a corrupt KV mapping. The GPU test plan needs an
  actual generation-correctness check after restore, compared against the same
  prompt on a cold-started replica.
- **Directory artifacts have no atomic commit.** A tar was atomic by rename;
  a tree is not. Hence the MANIFEST-written-last rule in §3 — restores must
  treat a prefix without a complete MANIFEST as absent, not as partial.
- **Forking CRIU means owning node images.** §6b is deliberately last and
  deliberately conditional.


---

## 10. What the workload had to give up

§4 says the shim is "a contract we cannot enforce". Running it against a real
vLLM engine is what showed how much that contract actually contains. Five
things had to change before an engine was both dumpable and restorable, and
only the first is specific to vLLM:

1. **NVML holds its own `/dev/nvidiactl`.** `cuda-checkpoint` hands back the
   descriptors the CUDA *runtime* owns; NVML's is not one of them, so it is
   still open when CRIU walks the process:
   `Can't dump file 9 of that type [20666] (chr 195:255)`. Any library that
   calls `nvmlInit` without a matching `nvmlShutdown` — PyTorch's device-count
   probe does — leaves one behind.

2. **`fork` spreads the problem.** vLLM's V1 async path always runs EngineCore
   in its own process and defaults to forking it, so the worker inherits the
   frontend's nvidiactl fd. In the child that descriptor belongs to no library
   at all, so `cuda-checkpoint` does not release it either. vLLM already forces
   `spawn` when the parent has initialized CUDA; NVML alone does not count,
   which is the gap.

3. **Anything bound to the pod IP.** `Can't bind inet socket back: Cannot
   assign requested address`. The engine's ZMQ endpoints and the
   `torch.distributed` TCPStore are created during engine init — before the
   quiesce point — so deferring the HTTP frontend does not help. They have to
   be on loopback, which exists identically in every pod. Same for outbound
   connections: vLLM's usage reporting keeps a TLS session open to a host on
   the public internet, and nothing can bring that back elsewhere.

4. **Deleted-but-mapped files on `/dev/shm`.** glibc's `sem_open` creates a
   temp file, mmaps it, links it to the caller's name and unlinks the temp —
   so every POSIX semaphore is permanently mapped from a deleted path, and any
   Python `multiprocessing` primitive produces one. CRIU cannot ghost a
   *mapped* deleted file that still has a link, so it demands `link-remap`;
   and `link-remap` hard-links the inode into `/dev/shm` during the dump,
   drops the link when the dump ends, and expects it back at restore, which a
   fresh pod's tmpfs cannot provide. The way out is to drop the last link
   before the dump: `nlink` hits zero and CRIU writes a ghost file, content
   and all, into the image.

5. **`/dev/shm` itself is not in the image.** It is a pod-scoped external
   mount, so the agent captures it as `shm-diff.tar` alongside
   `rootfs-diff.tar` and lays it back down over the restored pod's tmpfs.

The generalizable shape: the quiesce point buys you control over what is
*created* after it, and nothing at all over what engine initialization already
did. Everything in the list above exists by the time weights are loaded. A
future protocol version could plausibly check for these before declaring
ready — scan `/proc/self/maps` and `/proc/net/tcp` for the known-bad shapes and
refuse to write `ready-for-checkpoint` — rather than letting the checkpoint
fail minutes later with a CRIU error nobody can read.

The io_uring wall §4 predicted did not appear at the quiesce point, for the
reason §4 gives: uvicorn is never started before the dump, so its event loop
is not in the image. The shim also pins uvicorn to the `asyncio` loop rather
than uvloop, so that a *restored* process is still dumpable.

## 11. Storage behavior that shaped the design

Two things about the fuse-client mount, both measured, both of which changed
the §3 implementation:

- **`rename` copies without unlinking the source.** Every artifact tar on the
  cluster had a full-size `.tar.part` sitting beside it — silently doubling
  the storage cost of every snapshot taken since v1 shipped. This is why the
  plan's per-file `.part` + rename was dropped for directory artifacts (the
  MANIFEST is the commit marker, so per-file atomicity buys nothing) and why
  `RenamePublish` explicitly removes the source after the one rename that
  remains.

- **`O_TRUNC` is ignored.** Re-uploading a revision left every file that had
  shrunk carrying the previous upload's tail, and pre-warm then read a longer
  image than the manifest described. The agent now unlinks before every
  overwrite. The manifest's per-file sizes are what caught this — a tar
  artifact would have carried the corruption into `runc restore`.

Neither is a bug report against fuse-cache so much as a reminder that an
artifact store is not a POSIX filesystem just because it is mounted like one.
