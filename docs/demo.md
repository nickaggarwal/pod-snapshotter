# Demo: snapshot & restore a vLLM pod

> This walkthrough is the **v1** path: checkpoint a live serving pod into a
> tar. It still works, and it is the shortest way to see the mechanism. For
> anything real — and for any model whose engine actually initializes CUDA —
> use the **v2** path at the bottom of this page: a `SnapshotBuild` that
> checkpoints the workload at a quiesce point of its own choosing. Dumping a
> live vLLM process holds resources CRIU cannot dump; see
> [prerequisites.md](prerequisites.md#what-must-not-be-in-the-image).

Goal: measure cold start vs restore-from-snapshot for a small vLLM model on
one GPU node. Prereqs: [prerequisites.md](prerequisites.md) all green
(`kubectl get node <n> -o jsonpath='{.metadata.annotations.podsnapshot\.io/prereqs}'` → `ok`).

## 1. Cold start (baseline)

```bash
kubectl apply -f config/samples/vllm-pod.yaml
time kubectl wait pod/vllm-demo --for=condition=Ready --timeout=15m
# Note the time: image pull + weight load to VRAM + CUDA warmup. This is
# the cost we're eliminating.
curl -s http://$(kubectl get pod vllm-demo -o jsonpath='{.status.podIP}'):8000/v1/models
```

## 2. Snapshot

```bash
kubectl apply -f config/samples/podsnapshot_vllm.yaml
kubectl get podsnapshot vllm-demo-snap -w
# Pending → Checkpointing (cuda-checkpoint drains CUDA, copies VRAM to host,
# CRIU dumps; the pod KEEPS RUNNING afterwards) → Checkpointed → Uploading
# (agent streams the tar to /mnt/fuse) → Completed
kubectl get podsnapshot vllm-demo-snap -o jsonpath='{.status.artifact}' | jq
# {"uri":"fuse:///snapshots/default/vllm-demo-snap/vllm.tar","sizeBytes":...,"sha256":"..."}
```

The artifact is now on the fuse-client distributed tier: local NVMe on the
snapshot node, write-through to cloud storage. Any node can restore it.

## 3. Delete the pod

```bash
kubectl delete pod vllm-demo --wait
```

## 4. Restore from the tar

```bash
kubectl apply -f config/samples/podrestore_vllm.yaml
time kubectl wait pod/vllm-demo-restore-restored --for=condition=Ready --timeout=10m
kubectl get podrestore vllm-demo-restore -w
# Pending → Preparing (placeholder pod scheduled, GPU allocated)
#         → PreWarming (tar streamed through the fuse cache to local NVMe)
#         → Restoring (runc restore + CRIU CUDA plugin reattach)
#         → Running
```

The placeholder pod turns Ready when the **restored** vLLM answers its
readiness probe — the restored process shares the pod's network namespace.

```bash
curl -s http://$(kubectl get pod vllm-demo-restore-restored -o jsonpath='{.status.podIP}'):8000/v1/completions \
  -H 'Content-Type: application/json' \
  -d '{"model":"Qwen/Qwen2.5-0.5B-Instruct","prompt":"The capital of France is","max_tokens":8}'
```

Compare the `time` from step 4 with step 1.

## 5. Cross-node restore

Add `spec.nodeName: <other-gpu-node>` to the PodRestore and repeat. First
restore on a cold node streams the tar from peers/cloud (watch
`status.prewarmBytes`); repeat restores hit that node's NVMe cache.

## 6. Teardown

```bash
kubectl delete podrestore vllm-demo-restore   # agent kills the restored
                                              # container, unpins, then the
                                              # placeholder pod is deleted
kubectl delete podsnapshot vllm-demo-snap     # artifact retained (deletionPolicy)
```


---

## v2: build once, restore many

The v1 flow above snapshots whatever the pod happens to be holding. The v2
flow asks the workload to reach a checkpoint-safe point first, and treats the
result as a build output for the revision rather than a picture of one pod
([design-v2.md](design-v2.md)).

### 1. Put the reference shim on the cluster

```bash
kubectl create configmap snapshot-shim \
  --from-file=snapshot-shim.py=../hack/snapshot-shim.py
```

The shim wraps `vllm serve`: it initializes the engine, releases the KV
cache's physical pages with `sleep(level=1)`, writes
`/snapshot/ready-for-checkpoint`, and blocks. Everything that cannot be
dumped — the HTTP listener, io_uring rings, the distributed runtime — is
created only after it observes `/snapshot/restore-complete`.

### 2. Build the artifact

```bash
kubectl apply -f ../config/samples/snapshotbuild_vllm.yaml
kubectl get snapshotbuild qwen14b -w
# Pending → Building (pod scheduled, weights loading)
#         → Snapshotting (Quiescing → Checkpointing → Uploading)
#         → Completed, build pod deleted
```

Watch the quiesce point land in the build pod's log:

```
Sleep mode freed 71.16 GiB memory, 1.31 GiB memory is still in use.
[snapshot-shim] quiesce point reached: wrote /snapshot/ready-for-checkpoint
[snapshot-shim] blocking for /snapshot/restore-complete — the checkpoint happens here
```

The artifact is a directory, not a tar:

```bash
kubectl get snapshotbuild qwen14b -o jsonpath='{.status.artifact}' | jq
# {"uri":"fuse:///snapshots/builds/qwen2.5-14b-instruct-v1/","format":"dir",
#  "sizeBytes":...,"fileCount":...}
```

`MANIFEST` is written last and is what makes the prefix readable; a restore
treats a prefix without one as absent.

### 3. Restore it — as many times as you like

```bash
kubectl apply -f ../config/samples/podrestore_build.yaml
time kubectl wait pod/qwen14b-restore-restored --for=condition=Ready --timeout=10m
```

No untar: `runc restore --image-path` points straight at the artifact, and
pre-warm only pulls its bytes into the node's NVMe tier. The agent writes
`restore-complete` into the pod's rendezvous volume just before CRIU resumes
the process, so the shim's poll loop sees it on its first spin, calls
`wake_up()`, and only then starts serving.

### 4. Check the restore is *correct*, not just Ready

A readiness probe passes with a corrupt KV mapping. Compare greedy output
against a cold-started replica:

```bash
IP=$(kubectl get pod qwen14b-restore-restored -o jsonpath='{.status.podIP}')
curl -s http://$IP:8000/v1/completions -H 'Content-Type: application/json' \
  -d '{"model":"Qwen/Qwen2.5-14B-Instruct","prompt":"The capital of France is",
       "max_tokens":8,"temperature":0}'
```

It should match token for token.
