#!/usr/bin/env bash
# Measure one snapshot end to end: phase timings and per-device disk I/O.
#
# The point of this script is that the interesting number is not the one the
# controller reports. A PodSnapshot says how long it took; it does not say how
# many times the bytes were written to get there, or onto which device. On the
# node this was written against those two questions have very different
# answers depending on who runs the dump -- the kubelet stages its tar under
# /var/lib/kubelet, which is the OS disk, while the artifact is bound for the
# NVMe tier. A wall clock alone hides that entirely.
#
#   ./hack/measure-snapshot.sh <build-name> <node> [outdir]
#
# Creates nothing. Watches an existing SnapshotBuild and writes:
#   <outdir>/diskstats.tsv    t, device, sectors read, sectors written
#   <outdir>/phases.tsv       t, build phase, snapshot phase
#   <outdir>/summary.txt      totals and per-phase durations
set -euo pipefail

BUILD="${1:?usage: measure-snapshot.sh <build-name> <node> [outdir]}"
NODE="${2:?need the node name}"
OUT="${3:-/tmp/measure-$BUILD}"
NS="${NS:-default}"
AGENT_NS="${AGENT_NS:-pod-snapshotter}"
INTERVAL="${INTERVAL:-5}"

mkdir -p "$OUT"

agent_pod() {
  kubectl -n "$AGENT_NS" get pods -l app.kubernetes.io/name=pod-snapshotter-agent \
    --field-selector "spec.nodeName=$NODE" -o jsonpath='{.items[0].metadata.name}'
}
AGENT="$(agent_pod)"
[ -n "$AGENT" ] || { echo "no agent pod on $NODE" >&2; exit 1; }
echo "watching $BUILD on $NODE via $AGENT; writing to $OUT"

# Sample the two devices that matter: the OS disk (where the kubelet stages
# its tar) and the NVMe tier (where the artifact lives). Field 3 is the device
# name, field 6 sectors read, field 10 sectors written; a sector is 512 bytes.
sample_disks() {
  kubectl -n "$AGENT_NS" exec "$AGENT" -- \
    awk '$3=="sda"||$3=="nvme0n1" {print $3"\t"$6"\t"$10}' /host/proc/diskstats 2>/dev/null || true
}

phases() {
  kubectl -n "$NS" get snapshotbuild "$BUILD" \
    -o jsonpath='{.status.phase}{"\t"}{.status.snapshotName}' 2>/dev/null || true
}

: > "$OUT/diskstats.tsv"
: > "$OUT/phases.tsv"
printf 't\tdevice\tsectors_read\tsectors_written\n' >> "$OUT/diskstats.tsv"
printf 't\tbuild_phase\tsnapshot_phase\n' >> "$OUT/phases.tsv"

START=$(date +%s)
LAST_PHASE=""
while :; do
  T=$(( $(date +%s) - START ))

  sample_disks | while IFS=$'\t' read -r dev r w; do
    [ -n "$dev" ] && printf '%s\t%s\t%s\t%s\n' "$T" "$dev" "$r" "$w" >> "$OUT/diskstats.tsv"
  done

  read -r bphase sname < <(phases) || true
  sphase=""
  if [ -n "${sname:-}" ]; then
    sphase=$(kubectl -n "$NS" get podsnapshot "$sname" -o jsonpath='{.status.phase}' 2>/dev/null || true)
  fi
  cur="${bphase:-?}/${sphase:-?}"
  if [ "$cur" != "$LAST_PHASE" ]; then
    printf '%s\t%s\t%s\n' "$T" "${bphase:-?}" "${sphase:-?}" >> "$OUT/phases.tsv"
    printf '%6ss  %s\n' "$T" "$cur"
    LAST_PHASE="$cur"
  fi

  case "${bphase:-}" in
    Completed|Failed) break ;;
  esac
  sleep "$INTERVAL"
done

# ------------------------------------------------------------------ summary
{
  echo "build:  $BUILD"
  echo "node:   $NODE"
  echo "ended:  $(date -u +%Y-%m-%dT%H:%M:%SZ)"
  echo
  echo "phases (t seconds from start of measurement):"
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
  sname=$(kubectl -n "$NS" get snapshotbuild "$BUILD" -o jsonpath='{.status.snapshotName}' 2>/dev/null || true)
  if [ -n "$sname" ]; then
    echo "artifact:"
    kubectl -n "$NS" get podsnapshot "$sname" -o jsonpath='  checkpointer {.spec.checkpointer}{"\n"}  uri {.status.artifact.uri}{"\n"}  bytes {.status.artifact.sizeBytes}{"\n"}  files {.status.artifact.fileCount}{"\n"}'
  fi
} | tee "$OUT/summary.txt"
