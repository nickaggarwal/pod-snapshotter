# Manual GPU test plan

Unit tests cover everything that doesn't need a kernel, a container runtime,
or a GPU. The following can only be validated on a real GPU node (e.g. one
AKS `Standard_NC24ads_A100_v4` node with the prerequisites installed).

## GS — GPU snapshot path

| ID | Scenario | Pass criteria |
|----|----------|---------------|
| GS-1 | Snapshot a running vLLM pod (small model) | PodSnapshot → Completed; tar in /mnt/fuse; pod still serving afterwards |
| GS-2 | Snapshot under load (concurrent inference requests) | checkpoint succeeds; in-flight requests fail/retry cleanly; post-snapshot requests OK |
| GS-3 | Snapshot a multi-GB VRAM model | no kubelet timeout with spec.timeoutSeconds raised; artifact size ≈ VRAM + heap |
| GS-4 | Snapshot on a node missing cuda-checkpoint | manager blocks with NodeReady=False (prereq annotation) |
| GS-5 | kubelet feature gate off | Failed with actionable 404 message |

## GR — GPU restore path

| ID | Scenario | Pass criteria |
|----|----------|---------------|
| GR-1 | Restore on the same node | placeholder Ready; completions API answers; latency ≪ cold start |
| GR-2 | Restore on a different (identical) node | same as GR-1; prewarmBytes > 0 on first attempt |
| GR-3 | Restore with artifact only in cloud tier (evict NVMe first) | pre-warm streams from cloud; restore succeeds |
| GR-4 | Delete PodRestore while Running | runc container killed, bundle cleaned, unpinned, pod deleted |
| GR-5 | Restored process crashes | PodRestore flips to Failed ("no longer running") |
| GR-6 | Restore with `podsnapshot.io/tcp-close: "true"` | restore succeeds with established sockets closed |
| GR-7 | GPU device missing in restore spec vs node | fails fast with "GPU device nodes missing" |
| GR-8 | Agent restart mid-Running | checkAlive resumes against existing runc state; no duplicate restore |
| GR-9 | NVMe bypass, tier complete | agent logs `restoring from the node NVMe cache tier` **and** `skipping pre-warm`; `--image-path` points under `--nvme-cache-root`, not the mount; restore succeeds and generation matches |
| GR-10 | NVMe bypass, tier partial (delete one image file) | declines silently and pre-warms through the mount; restore still succeeds |
| GR-11 | NVMe bypass, file size disagrees with the manifest | declines with a logged error naming the file; restore falls back to the mount rather than restoring wrong bytes |
| GR-12 | `--nvme-cache-root` unset | no bypass attempted; behavior identical to GR-1 |

## QB — quiesce & build artifacts (v2)

Covers [design-v2.md](design-v2.md) §3–§5. Needs the reference shim on the
pod: `kubectl create configmap snapshot-shim
--from-file=snapshot-shim.py=hack/snapshot-shim.py`.

| ID | Scenario | Pass criteria |
|----|----------|---------------|
| QB-1 | `SnapshotBuild` of a vLLM pod behind the shim | phase Quiescing → Snapshotting → Completed; `status.quiesce.readyAt` set; build pod deleted afterwards |
| QB-2 | Directory artifact layout | prefix holds `checkpoint/`, `spec.dump`, MANIFEST (plus `shm-diff.tar` when the container left anything in `/dev/shm`); no `.part` files left behind |
| QB-3 | MANIFEST is the commit marker | delete the MANIFEST → a restore reports the artifact as absent and retries, never as partial |
| QB-4 | Restore from `buildRef`, no untar | pod Ready; the agent's work dir holds only `bundle/` and `criu-work/` — no copy of the images |
| QB-5 | **Generation correctness after `wake_up()`** | same prompt at `temperature: 0` gives byte-identical output to a cold-started replica. A readiness probe passes with a corrupt KV mapping; this is the check that does not |
| QB-6 | Shim never reports ready | PodSnapshot fails with `QuiesceTimeout` naming the container and the annotation, not an opaque CRIU error |
| QB-7 | Restore onto a node with a different driver/CRIU | placeholder pod stays Pending (compat-hash selector), or fails with "node X cannot restore this artifact: …" — never a `runc restore` failure |
| QB-8 | Re-upload the same revision after the artifact shrinks | every file matches its manifest size (the mount ignores `O_TRUNC`; the agent unlinks first) |
| QB-9 | Parallel pre-warm | `status.prewarmBytes` equals the manifest total; wall time below the single-stream tar path for the same bytes |
| QB-10 | Build pod name collision with a terminating pod | new build waits for the stale pod instead of adopting it (dumping an already-checkpointed container fails inside containerd) |

## CF — patched CRIU (v2 §6b)

Only meaningful with `criu.enabled=true`. Every case runs against the same
installed binary — the tuning annotations are what varies — so a failure can
be attributed to the patches rather than to the rebuild.

| ID | Scenario | Pass criteria |
|----|----------|---------------|
| CF-1 | Installer on a node that cannot run the binary | DaemonSet fails, `/usr/local/sbin/criu` is removed, `criu --version` on the host still answers from the distro package |
| CF-2 | `criu.uninstall=true` | marker and binary gone; the next restore succeeds on the packaged CRIU |
| CF-3 | Patches inert (`criu-aio-depth: 0`, `criu-shmem-threads: 1`) | restore succeeds and the workload serves — this is the control for CF-4 and the first thing to run after any rebase |
| CF-4 | Pools on (`criu-shmem-threads: 8`) | restore succeeds; `restore.log` shows `Restoring N memfd inodes on M threads`; time to the first `cuda_plugin: resuming devices` line below CF-3 |
| CF-5 | AIO on (`criu-aio-depth: 128`) | restore succeeds; no `AIO read returned 0` and no `BUG at criu/pagemap.c`. Not a throughput case — see below |
| CF-6 | Pools + AIO together | restore succeeds and serves; generation matches QB-5 |
| CF-8 | Pool raised to `criu-shmem-threads: 24` | restore succeeds and serves; `Restoring N memfd inodes on 24 threads`; CRIU-proper phase below CF-4 and `nvme0n1` read rate above CF-4's |
| CF-9 | `criu-image-io-mode: direct` on `v4.2.1-ps5` | restore succeeds and serves; `restore.log` shows `AIO: N of M submissions used O_DIRECT` with N/M near 1; `nvme0n1` reads roughly equal artifact size even on a warm node |
| CF-10 | AIO depth sweep on `v4.2.1-ps5` with `direct` | `criu-aio-depth` 1 / 16 / 128 at fixed `criu-shmem-threads: 8`: CRIU-proper phase must *fall* with depth. Flat here means the §6d submission-depth model is wrong |
| CF-7 | Stock CRIU given the annotations | ignored, restore unaffected — the agent sets them unconditionally and must not require the fork |

**Compare the CRIU-proper phase, not the CRIU wall.** On the 14B artifact,
`cuda_plugin` resuming devices on the GPU worker takes 12.4 s of a 21.3 s CRIU
restore, and none of the read-path tuning touches it — a patch that halved the
read path would move total CRIU wall by well under a third. The number to
compare across CF-3/4/5/6 is the log timestamp of the first
`cuda_plugin: resuming devices` line, which is where CRIU's own work ends.

**CF-9 is how to get a cold-cache number without a node-wide side effect.**
`CRIU_IMAGE_IO_MODE=direct` bypasses the page cache for CRIU's own reads only,
so it isolates the storage path without `drop_caches` or a
`POSIX_FADV_DONTNEED` sweep touching every other workload on a shared GPU
node. It only became viable once the memfd bytes moved onto the async path:
`pages-%u.img` has no header, so those reads are page-aligned and
`piov_is_aligned()` accepts them. Check the `AIO: N of M submissions used
O_DIRECT` line before trusting the timing — a low N means most jobs fell back
to buffered reads and the run is warm again.

**On `v4.2.1-ps4` and earlier, CF-5 is a safety case, not a performance one.** `criu-aio-depth` cannot
currently change the wall clock on a GPU checkpoint, because the memfd and
shmem restore paths call `read_pages()` with flags `0` and never reach the
async code — see [design-v2.md §6d](design-v2.md). Run CF-5 to prove the AIO
path stays harmless when enabled (it is still live for task-anonymous memory,
3 of 208 images here), and do not read a timing conclusion out of it. On
`v4.2.1-ps5` and later the memfd path does pass `PR_ASYNC`, so CF-5 becomes a
performance case — and CF-10 is the sweep that proves the depth knob is
connected to anything.

**Any case that does claim a throughput number needs a cold page cache.** The
A100 nodes have 226 GB of RAM and the 14B artifact is 52 GiB, so a second
restore of the same artifact reads entirely from page cache — measured: zero
sectors read from either `sda` or `nvme0n1` across a whole restore. That
applies to CF-4, CF-6 and CF-8. Before each, either evict the artifact
(`POSIX_FADV_DONTNEED` over the cache directory) or use a node that has not
served it yet, and confirm with `/proc/diskstats`, or with
`node_disk_read_bytes_total` in Prometheus, that the device actually saw the
reads. The Prometheus route is worth knowing about: it is scraped at 15 s
resolution on every node continuously, so it can answer this question
*retroactively* about a run you forgot to instrument.

CF-3 exists because of a real regression: the first pool build failed every
restore with `Bad file descriptor` from `cr_fchpermat`, and having the inert
control on the same binary is what proved the rebuild innocent and pointed at
the patches. See [design-v2.md §6b](design-v2.md).

## IR — integration & resilience

| ID | Scenario | Pass criteria |
|----|----------|---------------|
| IR-1 | fuse-client agent socket absent | restore proceeds unpinned (log only) |
| IR-2 | fuse-client down during upload | Uploading retries with backoff; recovers when fuse-client returns |
| IR-3 | Artifact sha256 spot-check | sha256 of /mnt/fuse tar matches status.artifact.sha256 |
| IR-4 | Node reboot with stale runc state | agent GC path: PodRestore fails cleanly; no orphaned cgroups |

## Measured

A100 80GB PCIe, driver 580.159.04, CRIU 4.2.1, containerd 2.3.2,
vLLM 0.9.2 behind `hack/snapshot-shim.py`, weights on the fuse-cache mount.
Cold start is container start → engine ready. See
[design-v2.md §8](design-v2.md#8-rollout) for the phase-by-phase breakdown.

| Model | Device memory at dump | Artifact | Cold start | Build (start→artifact) | Restore → Ready |
|-------|----------------------|----------|-----------|-----------------------|-----------------|
| Qwen2.5-1.5B-Instruct | 0.98 GiB (from 48.6) | 6.7 GiB, 396 files | ~90 s | ~11 min | **73 s** |
| Qwen2.5-14B-Instruct | 1.31 GiB (from 72.5) | 52.5 GiB, 640 files | 179 s | 34 min | **395 s** |

The 14B restore is slower than its cold start, and
[design-v2.md §8](design-v2.md#the-uncomfortable-number) explains why: the
artifact is 2× the weights, because `sleep(level=1)` offloads them to host
RAM rather than dropping them. Weight decoupling, not restore-side I/O
parallelism, is what closes that.
