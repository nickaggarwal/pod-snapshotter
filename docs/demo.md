# Demo: snapshot & restore a vLLM pod

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
