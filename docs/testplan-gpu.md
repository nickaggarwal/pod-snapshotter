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

## IR — integration & resilience

| ID | Scenario | Pass criteria |
|----|----------|---------------|
| IR-1 | fuse-client agent socket absent | restore proceeds unpinned (log only) |
| IR-2 | fuse-client down during upload | Uploading retries with backoff; recovers when fuse-client returns |
| IR-3 | Artifact sha256 spot-check | sha256 of /mnt/fuse tar matches status.artifact.sha256 |
| IR-4 | Node reboot with stale runc state | agent GC path: PodRestore fails cleanly; no orphaned cgroups |

## Timing table to fill in

| Model | Cold start | Snapshot | Restore (warm NVMe) | Restore (cloud) |
|-------|-----------|----------|---------------------|-----------------|
| Qwen2.5-0.5B | | | | |
| Llama-3-8B | | | | |
