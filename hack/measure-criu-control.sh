#!/usr/bin/env bash
# Restore the same artifact twice -- once on patched CRIU, once on the distro
# binary -- and print the difference.
#
# Every restore number in docs/design-v2.md §6 and in the blog was taken on the
# fork. That makes them measurements of "our stack", not of the patches: the
# transport work underneath (no staging copy, NVMe bypass, prefetch at 4) moved
# far more time than the patches did, and it moved it for both binaries.
# Without an arm that runs stock CRIU on the finished transport, "the fork is
# worth X" is an attribution rather than a result.
#
# The revert is deliberately the supported path rather than a hand-edit of the
# host: criu.uninstall=true makes the installer `rm` its own binary, the node
# falls back to /usr/sbin/criu on PATH, and putting it back is the same
# DaemonSet with the flag off. Nothing else on the node is touched.
#
# This changes the CRIU binary for EVERY checkpoint and restore on every node
# the DaemonSet selects, for as long as it runs. It needs a free GPU and a
# quiet cluster.
#
#   ./hack/measure-criu-control.sh <podrestore-yaml> <node>
set -euo pipefail

MANIFEST="${1:?usage: measure-criu-control.sh <podrestore-yaml> <node>}"
NODE="${2:?usage: measure-criu-control.sh <podrestore-yaml> <node>}"
NS="${NS:-pod-snapshotter}"
RELEASE="${RELEASE:-pod-snapshotter}"
REGISTRY="${REGISTRY:-stargzrepo.azurecr.io}"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
CHART="$ROOT/charts/pod-snapshotter"

say() { printf '\n\033[1m== %s\033[0m\n' "$*"; }

# Which criu the host resolves, asked of the host. The chart's tag says what
# was shipped; PATH says what runc will actually exec, and only the second one
# is what a measurement runs against.
host_criu() {
  local p
  p=$(kubectl -n "$NS" get pods --no-headers -o custom-columns=:metadata.name,:spec.nodeName \
        | awk -v n="$NODE" '$1 ~ /^pod-snapshotter-criu-/ && $2==n {print $1; exit}')
  [ -n "$p" ] || { echo "no criu pod on $NODE" >&2; return 1; }
  kubectl -n "$NS" exec "$p" -- chroot /host /bin/sh -c 'command -v criu; criu --version' 2>/dev/null
}

# Roll the installer with uninstall on or off, then wait for the host to agree.
# Waiting on the DaemonSet rollout is not enough: the pod reports Ready before
# the install script has finished, and a restore started in that window runs
# against whichever binary happened to still be there.
set_criu() { # $1 = true|false (uninstall)
  helm template "$RELEASE" "$CHART" --namespace "$NS" \
    --set imageRegistry="$REGISTRY" --set criu.enabled=true --set criu.uninstall="$1" \
    | kubectl apply -n "$NS" -f - >/dev/null
  kubectl -n "$NS" rollout restart ds/pod-snapshotter-criu >/dev/null
  kubectl -n "$NS" rollout status ds/pod-snapshotter-criu --timeout=600s >/dev/null

  local want i out
  if [ "$1" = "true" ]; then want=/usr/sbin/criu; else want=/usr/local/sbin/criu; fi
  for i in $(seq 1 60); do
    out=$(host_criu || true)
    if [ "$(echo "$out" | head -1)" = "$want" ]; then
      echo "$out" | sed 's/^/     /'
      return 0
    fi
    sleep 5
  done
  echo "host on $NODE never resolved criu to $want:" >&2
  echo "$out" >&2
  return 1
}

# Always put the fork back, including on Ctrl-C or a failed arm. Leaving a
# shared node on the distro binary is a worse outcome than not having the
# measurement: the next restore would silently be a different experiment.
cleanup() {
  echo
  echo "restoring the patched CRIU before exiting"
  set_criu false || echo "FAILED to reinstall patched CRIU on $NODE -- fix by hand" >&2
}
trap cleanup EXIT

say "before: what $NODE resolves"
host_criu | sed 's/^/     /'

say "arm 1 of 2 -- patched CRIU (the fork)"
"$ROOT/hack/measure-restore.sh" "$MANIFEST" "$NODE" "/tmp/criu-control-patched"

say "reverting $NODE to the distro CRIU"
set_criu true

say "arm 2 of 2 -- stock CRIU 4.2.1"
"$ROOT/hack/measure-restore.sh" "$MANIFEST" "$NODE" "/tmp/criu-control-stock"

say "result -- same artifact, same node, same transport, cold both times"
for a in patched stock; do
  f="/tmp/criu-control-$a/summary.txt"
  echo "  $a:"
  grep -E 'restored|serving|CRIU proper' "$f" 2>/dev/null | sed 's/^/    /' || echo "    (no result)"
done

# The wall clock and CRIU's own clock answer different questions and only the
# second one is attributable to the binary. Everything outside restore.log --
# image pull, sandbox setup, prewarm, the engine re-taking its KV cache -- is
# identical across the arms by construction, so a delta in the wall clock that
# is not also in CRIU's clock is noise in the harness, not a property of the
# patches. Print both deltas; a claim about the fork rests on the second.
say "delta -- what the patches are worth"
python3 - "$@" <<'EOF'
import re, sys
def read(a):
    try: t = open(f"/tmp/criu-control-{a}/summary.txt").read()
    except OSError: return {}
    g = lambda p: (lambda m: float(m.group(1)) if m else None)(re.search(p, t))
    return {"wall": g(r'serving .*in (\d+)s'),
            "criu": g(r'CRIU proper \(restore.log\) ([\d.]+)s')}
p, s = read("patched"), read("stock")
for k, label in (("criu", "CRIU proper "), ("wall", "wall to serving")):
    a, b = p.get(k), s.get(k)
    if a and b:
        print(f"  {label}  patched {a:8.1f}s   stock {b:8.1f}s   "
              f"{b - a:+.1f}s  ({b / a:.2f}x)")
    else:
        print(f"  {label}  incomplete (patched={a}, stock={b})")
EOF
