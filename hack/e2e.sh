#!/usr/bin/env bash
# End-to-end test: build a snapshot of a real workload, restore it, and check
# that the restored process still computes the right answer.
#
# The last check is the one that matters. A restore that reaches Ready is not a
# restore that worked -- a CRIU image can restore into a process whose CUDA
# context is subtly wrong, and it will serve confident garbage. So the pass
# condition here is a generated completion with known content, not a phase.
#
# Times are recorded but not asserted on: this is a correctness gate, and a
# threshold that fails on a slow day teaches people to ignore it. The numbers
# go in the summary for a human to look at.
set -uo pipefail

NS="${NS:-default}"
NAME="${E2E_NAME:-e2e-snap}"
MODEL="${E2E_MODEL:-/mnt/fuse/models/Qwen2.5-14B-Instruct}"
SERVED="${E2E_SERVED:-Qwen/Qwen2.5-14B-Instruct}"
REV="${E2E_REV:-e2e-$(date +%s)}"
TIMEOUT_BUILD="${E2E_TIMEOUT_BUILD:-3600}"
TIMEOUT_RESTORE="${E2E_TIMEOUT_RESTORE:-900}"
KEEP="${E2E_KEEP:-false}"
# Which side runs the dump. Empty leaves it to the CRD's default (kubelet);
# "agent" takes the direct path. Settable so the same correctness gate can be
# run against both, which is the only way to know the two produce artifacts
# that restore identically.
CHECKPOINTER="${E2E_CHECKPOINTER:-}"

say() { printf '\n\033[1m-- %s\033[0m\n' "$*"; }
ok()  { printf '   \033[32mok\033[0m   %s\n' "$*"; }
bad() { printf '   \033[31mBAD\033[0m  %s\n' "$*"; }

cleanup() {
  if [ "$KEEP" != "true" ]; then
    kubectl -n "$NS" delete podrestore "$NAME-restore" --ignore-not-found --wait=false >/dev/null 2>&1
    kubectl -n "$NS" delete snapshotbuild "$NAME" --ignore-not-found --wait=false >/dev/null 2>&1
  fi
}
trap cleanup EXIT

# The shim is what makes a vLLM engine dumpable: it releases the CUDA
# allocator's memory and then writes the rendezvous file the agent waits for.
kubectl -n "$NS" get configmap snapshot-shim >/dev/null 2>&1 || {
  kubectl -n "$NS" create configmap snapshot-shim --from-file=snapshot-shim.py="$(dirname "$0")/snapshot-shim.py" >/dev/null
  ok "created the snapshot-shim configmap"
}

say "snapshot build ($REV)"
cat <<YAML | kubectl -n "$NS" apply -f - >/dev/null
apiVersion: podsnapshot.io/v1alpha1
kind: SnapshotBuild
metadata:
  name: $NAME
spec:
  revision: $REV
  artifactFormat: dir
${CHECKPOINTER:+  checkpointer: $CHECKPOINTER}
  timeoutSeconds: $TIMEOUT_BUILD
  podTemplate:
    metadata:
      labels: {app: $NAME}
      annotations:
        podsnapshot.io/quiesce: presence-file
        podsnapshot.io/quiesce-dir: /snapshot
        podsnapshot.io/quiesce-timeout: 1800s
    spec:
      tolerations: [{key: nvidia.com/gpu, operator: Exists, effect: NoSchedule}]
      containers:
        - name: vllm
          image: vllm/vllm-openai:v0.9.2
          command: [python3, -u, /shim/snapshot-shim.py, --quiesce-dir, /snapshot, --]
          args:
            - --model=$MODEL
            - --served-model-name=$SERVED
            - --port=8000
            - --gpu-memory-utilization=0.90
            - --max-model-len=4096
          env: [{name: HF_HUB_OFFLINE, value: "1"}]
          ports: [{containerPort: 8000}]
          resources: {limits: {nvidia.com/gpu: 1}}
          volumeMounts:
            - {mountPath: /shim, name: shim}
            - {mountPath: /snapshot, name: snapshot}
            - {mountPath: /mnt/fuse, name: fuse, mountPropagation: HostToContainer}
            - {mountPath: /dev/shm, name: shm}
      volumes:
        - {name: shim, configMap: {name: snapshot-shim}}
        - {name: snapshot, emptyDir: {}}
        - {name: fuse, hostPath: {path: /mnt/fuse, type: Directory}}
        - {name: shm, emptyDir: {medium: Memory, sizeLimit: 16Gi}}
YAML

t0=$(date +%s)
phase=""
while :; do
  p=$(kubectl -n "$NS" get snapshotbuild "$NAME" -o jsonpath='{.status.phase}' 2>/dev/null)
  [ "$p" != "$phase" ] && [ -n "$p" ] && { printf '        [%4ss] %s\n' "$(( $(date +%s) - t0 ))" "$p"; phase=$p; }
  case "$p" in
    Completed) break ;;
    Failed) bad "build failed: $(kubectl -n "$NS" get snapshotbuild "$NAME" -o jsonpath='{.status.message}')"; exit 1 ;;
  esac
  [ $(( $(date +%s) - t0 )) -gt "$TIMEOUT_BUILD" ] && { bad "build timed out"; exit 1; }
  sleep 10
done
build_s=$(( $(date +%s) - t0 ))
uri=$(kubectl -n "$NS" get snapshotbuild "$NAME" -o jsonpath='{.status.artifact.uri}')
size=$(kubectl -n "$NS" get snapshotbuild "$NAME" -o jsonpath='{.status.artifact.sizeBytes}')
ok "built in ${build_s}s: $uri ($(( size / 1000000000 )) GB)"

say "restore"
cat <<YAML | kubectl -n "$NS" apply -f - >/dev/null
apiVersion: podsnapshot.io/v1alpha1
kind: PodRestore
metadata:
  name: $NAME-restore
spec:
  source: {snapshotBuildRef: {name: $NAME}}
  podTemplate:
    metadata:
      labels: {app: $NAME-restored}
    spec:
      tolerations: [{key: nvidia.com/gpu, operator: Exists, effect: NoSchedule}]
      containers:
        - name: vllm
          image: vllm/vllm-openai:v0.9.2
          ports: [{containerPort: 8000}]
          resources: {limits: {nvidia.com/gpu: 1}}
YAML

t1=$(date +%s)
while :; do
  st=$(kubectl -n "$NS" get pods -l "app=$NAME-restored" --no-headers 2>/dev/null | awk '{print $2; exit}')
  [ "$st" = "1/1" ] && break
  p=$(kubectl -n "$NS" get podrestore "$NAME-restore" -o jsonpath='{.status.phase}' 2>/dev/null)
  [ "$p" = "Failed" ] && { bad "restore failed: $(kubectl -n "$NS" get podrestore "$NAME-restore" -o jsonpath='{.status.message}')"; exit 1; }
  [ $(( $(date +%s) - t1 )) -gt "$TIMEOUT_RESTORE" ] && { bad "restore timed out"; exit 1; }
  sleep 5
done
restore_s=$(( $(date +%s) - t1 ))
ok "restored to Ready in ${restore_s}s"

say "correctness"
# Over the pod network, not kubectl exec: the restored workload is a runc
# container the kubelet does not know about, so an exec lands in the keeper's
# namespaces and cannot see the listener.
ip=$(kubectl -n "$NS" get pods -l "app=$NAME-restored" --no-headers -o custom-columns=:status.podIP | head -1)
gen=$(kubectl -n "$NS" run e2egen-$RANDOM --rm -i --restart=Never --image=curlimages/curl:latest --quiet -- \
  -s -m 120 -X POST "http://$ip:8000/v1/completions" -H 'Content-Type: application/json' \
  -d "{\"model\":\"$SERVED\",\"prompt\":\"The capital of France is\",\"max_tokens\":8,\"temperature\":0}" 2>/dev/null \
  | python3 -c "import sys,json;print(json.load(sys.stdin)['choices'][0]['text'])" 2>/dev/null | head -1)

echo "        generated: '$gen'"
if echo "$gen" | grep -qi "paris"; then
  ok "restored engine generates correct output"
else
  bad "restored engine did not name Paris -- the process is alive but its state is wrong"
  exit 1
fi

say "summary"
printf '        build   %ss\n        restore %ss\n        artifact %s GB\n' \
  "$build_s" "$restore_s" "$(( size / 1000000000 ))"
