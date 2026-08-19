# Design v2 — aligning with NVIDIA Dynamo Snapshot

Status: **proposed**. This document supersedes nothing yet; v1 (tar artifacts,
live-pod checkpoints) keeps working while these land incrementally.

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

We ship a reference shim (`hack/snapshot-shim.py`) covering vLLM and SGLang, and
document the protocol for anything else. Workloads without the annotation take
the v1 path unchanged.

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

| Phase | Contents | Unblocks |
|---|---|---|
| 1 | Image directories end-to-end (§3) | Removes untar; prerequisite for §6 |
| 2 | Quiesce/resume + reference shim (§4) | Fixes io_uring; shrinks image |
| 3 | `SnapshotBuild` + build-time artifacts (§5) | Makes §4 usable in production |
| 4 | `--stream` restore, measured (§6a) | Parallel reads |
| 5 | Fork CRIU only if §4 measured short (§6b) | The rest of the 7.9× |

Phases 1 and 2 are independent and can land in parallel. Phase 3 depends on 2.

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
