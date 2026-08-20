# Design v2 — aligning with NVIDIA Dynamo Snapshot

Status: **§3–§5 implemented and verified on A100; §6 not started.** v1 (tar
artifacts, live-pod checkpoints) keeps working unchanged — `artifactFormat`
defaults to `tar`, and a pod without the quiesce annotation takes the v1 path.

| Workstream | State |
|---|---|
| §3 image directories | shipped — `artifactFormat: dir`, MANIFEST commit, parallel pre-warm, restore reads the images in place |
| §4 quiesce/resume | shipped — `podsnapshot.io/quiesce`, `Quiescing` phase, [hack/snapshot-shim.py](../hack/snapshot-shim.py) |
| §5 build artifacts | shipped — `SnapshotBuild` CRD, `PodRestore.spec.buildRef`, compatibility-constrained placement |
| §6a `--stream` restore | not started — `criu-image-streamer` is not on the node images |
| §6b forked CRIU | not started, deliberately conditional |
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
  measured against each other.

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
| 5 | Fork CRIU only if §4 measured short (§6b) | not started |

### Benchmark table to fill in

Every phase lands with this row measured on the A100 pool, end-to-end (CR
created → placeholder pod Ready), not CRIU-restore-only:

| Phase | Artifact size | Prefetch | Untar | CRIU restore | Resume | Total |
|---|---|---|---|---|---|---|
| v1 baseline | | | | | n/a | |
| +§3 dirs | | | *gone* | | n/a | |
| +§4 quiesce | | | — | | | |
| +§6a stream | | | — | | | |

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
