#!/usr/bin/env bash
# Time a restore end to end, from a cold page cache, and record the disk I/O
# it took to get there.
#
# The companion to measure-snapshot.sh, and it exists for the same reason:
# the interesting number is not "did it come back" but "what did it cost".
# A restore that reaches Ready having read the artifact twice is a different
# result from one that read it once, and nothing in the CRD says which
# happened.
#
# The page cache is dropped before the clock starts. Without that the second
# run of any pair is measuring RAM, and the comparison is meaningless -- a
# 56 GB artifact fits comfortably in 216 GB of node memory, so a warm rerun
# looks like a tenfold improvement that no user will ever see.
#
#   ./hack/measure-restore.sh <podrestore-yaml> <node> [outdir]
set -uo pipefail

MANIFEST="${1:?usage: measure-restore.sh <podrestore-yaml> <node> [outdir]}"
NODE="${2:?usage: measure-restore.sh <podrestore-yaml> <node> [outdir]}"
OUT="${3:-/tmp/measure-restore-$(basename "${MANIFEST%.yaml}")}"
NS="${NS:-default}"
AGENT_NS="${AGENT_NS:-pod-snapshotter}"
INTERVAL="${INTERVAL:-2}"

mkdir -p "$OUT"

NAME=$(python3 -c "
import json,sys
raw=open('$MANIFEST').read()
try: print(json.loads(raw)['metadata']['name'])
except Exception:
    for l in raw.splitlines():
        if l.strip().startswith('name:'): print(l.split(':',1)[1].strip()); break
")
[ -n "$NAME" ] || { echo "could not read the PodRestore name from $MANIFEST" >&2; exit 1; }

AGENT=$(kubectl -n "$AGENT_NS" get pods -l app=pod-snapshotter-agent \
  --field-selector "spec.nodeName=$NODE" -o jsonpath='{.items[0].metadata.name}')
[ -n "$AGENT" ] || { echo "no agent pod on $NODE" >&2; exit 1; }

sample_disks() {
  kubectl -n "$AGENT_NS" exec "$AGENT" -- \
    awk '$3=="sda"||$3=="nvme0n1" {print $3"\t"$6"\t"$10}' /host/proc/diskstats 2>/dev/null || true
}

# Cold start means cold. Anything else measures the node's RAM.
echo "dropping the page cache on $NODE"
kubectl -n "$AGENT_NS" exec "$AGENT" -- nsenter -t 1 -m -p -- \
  sh -c 'sync; echo 3 > /proc/sys/vm/drop_caches' || {
    echo "could not drop the page cache; refusing to report a warm number as cold" >&2
    exit 1
  }

kubectl -n "$NS" delete podrestore "$NAME" --ignore-not-found --wait=true --timeout=180s >/dev/null 2>&1
: > "$OUT/diskstats.tsv"
: > "$OUT/phases.tsv"
printf 't\tdevice\tsectors_read\tsectors_written\n' >> "$OUT/diskstats.tsv"
printf 't\tphase\tmessage\n' >> "$OUT/phases.tsv"

echo "creating $NAME"
kubectl -n "$NS" create -f "$MANIFEST" >/dev/null
START=$(date +%s)
LAST=""
RUNNING_AT=""
READY_AT=""

# Two clocks, because they answer different questions. The controller reaching
# Running means CRIU handed the process back; the pod going Ready means the
# engine inside it answers /health. The gap between them is real -- a restored
# vLLM has to re-take its KV cache allocation before it will serve -- and a
# number that quotes only the first one is not a cold-start time.
POD_LABEL=$(python3 -c "
import json
d=json.load(open('$MANIFEST'))
l=d['spec']['podTemplate']['metadata']['labels']
k=next(iter(l)); print(f'{k}={l[k]}')
" 2>/dev/null || true)

while :; do
  T=$(( $(date +%s) - START ))
  sample_disks | while IFS=$'\t' read -r dev r w; do
    [ -n "$dev" ] && printf '%s\t%s\t%s\t%s\n' "$T" "$dev" "$r" "$w" >> "$OUT/diskstats.tsv"
  done

  read -r phase msg < <(kubectl -n "$NS" get podrestore "$NAME" \
    -o jsonpath='{.status.phase}{"\t"}{.status.message}' 2>/dev/null || true)

  ready=""
  if [ -n "$POD_LABEL" ]; then
    ready=$(kubectl -n "$NS" get pods -l "$POD_LABEL" --no-headers 2>/dev/null | awk '{print $2; exit}')
  fi

  cur="${phase:-?}/${ready:-?}"
  if [ "$cur" != "$LAST" ]; then
    printf '%s\t%s\t%s\n' "$T" "${phase:-?} ${ready:-}" "${msg:-}" >> "$OUT/phases.tsv"
    printf '%6ss  %-12s %-5s %s\n' "$T" "${phase:-?}" "${ready:-}" "${msg:-}"
    LAST="$cur"
  fi

  [ -z "$RUNNING_AT" ] && [ "$phase" = "Running" ] && RUNNING_AT=$T
  if [ "$ready" = "1/1" ]; then READY_AT=$T; break; fi
  [ "${phase:-}" = "Failed" ] && break
  [ "$T" -gt 1800 ] && { echo "gave up after 1800s"; break; }
  sleep "$INTERVAL"
done

# Copy CRIU's own log off the node NOW, in the same iteration that produced it.
# The agent GCs the whole work dir when the PodRestore is deleted, and this
# script deletes before it creates -- so the next run of any sweep destroys the
# previous run's log. Without this the wall clock survives and the "Restoring
# finished successfully" line, which is the only number that isolates CRIU from
# everything around it, does not.
UID_=$(kubectl -n "$NS" get podrestore "$NAME" -o jsonpath='{.metadata.uid}' 2>/dev/null || true)
if [ -n "$UID_" ]; then
  for f in criu-work/restore.log criu-work/runc-restore.log; do
    kubectl -n "$AGENT_NS" exec "$AGENT" -- \
      cat "/var/lib/pod-snapshotter/restores/$UID_/$f" \
      > "$OUT/$(basename "$f")" 2>/dev/null || rm -f "$OUT/$(basename "$f")"
  done
fi

# CRIU stamps every line with seconds since it started, so the last line is its
# own elapsed time -- independent of image pull, sandbox setup, prewarm, and the
# engine's post-restore warmup, all of which sit inside the wall clock.
criu_secs() {
  [ -s "$OUT/restore.log" ] || return 0
  # "Restore finished successfully", not "Restoring" -- CRIU's own wording, and
  # the last stamped line before it hands control back.
  sed -n 's/^(\([0-9.]*\)).*Restore finished successfully.*/\1/p' "$OUT/restore.log" | tail -1
}

{
  echo "restore: $NAME  ($MANIFEST)"
  echo "node:    $NODE"
  echo "ended:   $(date -u +%Y-%m-%dT%H:%M:%SZ)"
  [ -n "$RUNNING_AT" ] && echo "restored (CRIU done) in ${RUNNING_AT}s from a cold page cache"
  [ -n "$READY_AT" ] && echo "serving (/health ok)  in ${READY_AT}s from a cold page cache"
  cs=$(criu_secs); [ -n "$cs" ] && echo "CRIU proper (restore.log) ${cs}s"
  echo
  echo "phases (t seconds from create):"
  cat "$OUT/phases.tsv"
  echo
  echo "disk I/O over the run (GB, 512-byte sectors):"
  awk -F'\t' 'NR>1 {
      if (!(($2) in first_r)) { first_r[$2]=$3; first_w[$2]=$4 }
      last_r[$2]=$3; last_w[$2]=$4
    }
    END {
      printf "  %-10s %12s %12s\n", "device", "read GB", "write GB"
      for (d in last_r)
        printf "  %-10s %12.1f %12.1f\n", d,
          (last_r[d]-first_r[d])*512/1e9, (last_w[d]-first_w[d])*512/1e9
    }' "$OUT/diskstats.tsv"
  echo
  echo "artifact:"
  kubectl -n "$NS" get podrestore "$NAME" -o jsonpath='  uri {.status.artifactURI}{"\n"}  node {.status.targetNode}{"\n"}  prewarm {.status.prewarmBytes}{"\n"}'
} | tee "$OUT/summary.txt"
