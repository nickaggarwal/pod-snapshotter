#!/usr/bin/env bash
# Build, publish and deploy pod-snapshotter, then prove the cluster matches the
# chart.
#
# This exists because the cluster it was written against had no Helm release at
# all: everything had been kubectl-applied and then hand-patched, so
# charts/.../values.yaml was documentation rather than state. Three real bugs
# came out of that gap -- a CRIU image whose marker said it was installed when
# it was not, an agent too old to understand the tuning annotations it was
# being sent, and a manager tag of "latest" that could not be compared against
# anything. Each was found by hand, late, after a measurement had already been
# taken against the wrong binary.
#
# So the verify step is not a postscript here, it is the point. Deploying is
# easy; knowing what is deployed is what was missing.
#
#   ./hack/deploy.sh --check              what differs, change nothing
#   ./hack/deploy.sh --build              build+push images, then deploy+verify
#   ./hack/deploy.sh                      deploy the pinned tags, then verify
#   ./hack/deploy.sh --test               ...and run the end-to-end test
#
set -euo pipefail

REGISTRY="${REGISTRY:-stargzrepo.azurecr.io}"
NAMESPACE="${NAMESPACE:-pod-snapshotter}"
RELEASE="${RELEASE:-pod-snapshotter}"
CHART="$(cd "$(dirname "$0")/.." && pwd)/charts/pod-snapshotter"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"

DO_BUILD=false; DO_CHECK=false; DO_TEST=false; DO_ADOPT=false
for a in "$@"; do
  case "$a" in
    --build) DO_BUILD=true ;;
    --check) DO_CHECK=true ;;
    --test)  DO_TEST=true ;;
    --adopt) DO_ADOPT=true ;;
    -h|--help) sed -n '2,25p' "$0"; exit 0 ;;
    *) echo "unknown flag $a" >&2; exit 2 ;;
  esac
done

say()  { printf '\n\033[1m== %s\033[0m\n' "$*"; }
ok()   { printf '   \033[32mok\033[0m   %s\n' "$*"; }
bad()  { printf '   \033[31mBAD\033[0m  %s\n' "$*"; FAILED=$((FAILED+1)); }
info() { printf '        %s\n' "$*"; }
FAILED=0

# The tags are read out of values.yaml rather than passed in, so that the chart
# stays the single source of truth for what version means what. A build that
# invents its own tag would recreate the drift this script exists to catch.
tagof() { # $1 = top-level key
  awk -v k="$1" '
    $0 ~ "^"k":" {inb=1; next}
    inb && /^[a-z]/ {inb=0}
    inb && /^    tag:/ {print $2; exit}' "$CHART/values.yaml"
}
TAG_MANAGER=$(tagof manager); TAG_AGENT=$(tagof agent); TAG_CRIU=$(tagof criu)
[ -n "$TAG_MANAGER$TAG_AGENT$TAG_CRIU" ] || { echo "could not read tags from values.yaml" >&2; exit 1; }

say "chart pins"
info "manager $TAG_MANAGER   agent $TAG_AGENT   criu $TAG_CRIU"

# ---------------------------------------------------------------- build
if $DO_BUILD; then
  say "go vet + unit tests"
  (cd "$ROOT" && go vet ./... && go test ./...) || { echo "tests failed; not building" >&2; exit 1; }

  say "building and pushing images"
  # ACR build rather than local docker: the nodes are amd64 and the usual
  # workstation here is not, and a silently-arm64 image fails at pull time on
  # the node instead of at build time where it belongs.
  for c in manager:Dockerfile.manager agent:Dockerfile.agent criu:Dockerfile.criu; do
    name=${c%%:*}; file=${c##*:}
    case $name in manager) t=$TAG_MANAGER;; agent) t=$TAG_AGENT;; criu) t=$TAG_CRIU;; esac
    info "$name -> $REGISTRY/pod-snapshotter/$name:$t"
    az acr build --registry "${REGISTRY%%.*}" --image "pod-snapshotter/$name:$t" \
      --file "$ROOT/$file" --platform linux/amd64 "$ROOT" >/dev/null
    ok "$name:$t pushed"
  done
fi

# ---------------------------------------------------------------- deploy
render() {
  helm template "$RELEASE" "$CHART" \
    --namespace "$NAMESPACE" \
    --set imageRegistry="$REGISTRY" \
    --set criu.enabled=true \
    "$@"
}

if ! $DO_CHECK; then
  say "deploying"
  # CRDs first and separately: helm does not upgrade anything in crds/, and a
  # new status field that the manager writes but the API server does not know
  # about is dropped silently, which looks like a controller bug.
  #
  # Server-side, because these CRDs no longer fit in the client-side
  # last-applied-configuration annotation (262144 bytes) once the podTemplate
  # schemas are inlined -- a plain `kubectl apply` fails with "Too long" on
  # the annotation rather than on anything wrong with the CRD.
  kubectl apply --server-side --force-conflicts -f "$CHART/crds/" >/dev/null
  ok "CRDs applied"

  if ! helm -n "$NAMESPACE" status "$RELEASE" >/dev/null 2>&1; then
    if $DO_ADOPT; then
      # Adopt hand-applied objects into the release rather than making the
      # operator delete a working deployment to install one.
      say "adopting existing objects into a Helm release"
      for kind_name in \
        "daemonset/pod-snapshotter-agent" "daemonset/pod-snapshotter-criu" \
        "daemonset/pod-snapshotter-node-setup" "deployment/pod-snapshotter-manager" \
        "serviceaccount/pod-snapshotter-manager" "serviceaccount/pod-snapshotter-agent"; do
        kubectl -n "$NAMESPACE" get "$kind_name" >/dev/null 2>&1 || continue
        kubectl -n "$NAMESPACE" annotate --overwrite "$kind_name" \
          meta.helm.sh/release-name="$RELEASE" meta.helm.sh/release-namespace="$NAMESPACE" >/dev/null
        kubectl -n "$NAMESPACE" label --overwrite "$kind_name" app.kubernetes.io/managed-by=Helm >/dev/null
        info "adopted $kind_name"
      done
    else
      echo "   no Helm release named $RELEASE in $NAMESPACE." >&2
      echo "   The cluster was bootstrapped by hand. Re-run with --adopt to take" >&2
      echo "   ownership of the existing objects, or install into a clean namespace." >&2
      exit 1
    fi
  fi

  render | kubectl apply -n "$NAMESPACE" -f - >/dev/null
  ok "manifests applied"

  # A rebuild reuses the tag, so the rendered pod spec is byte-identical to
  # what is already live and `kubectl apply` is a no-op -- the nodes go on
  # running the layers they cached under that tag. Nothing above catches it:
  # the tag matches, the rollout is complete, and the binary is old.
  #
  # So when this run pushed images, restart explicitly. On a plain deploy of
  # already-published tags there is nothing new to pull, and skipping the
  # restart keeps it non-disruptive.
  if $DO_BUILD; then
    say "restarting workloads onto the images just pushed"
    for w in deploy/pod-snapshotter-manager ds/pod-snapshotter-agent ds/pod-snapshotter-criu; do
      kubectl -n "$NAMESPACE" rollout restart "$w" >/dev/null
    done
    ok "restart requested"
  fi

  say "waiting for rollout"
  kubectl -n "$NAMESPACE" rollout status deploy/pod-snapshotter-manager --timeout=300s
  for ds in pod-snapshotter-agent pod-snapshotter-criu; do
    kubectl -n "$NAMESPACE" rollout status ds/$ds --timeout=600s
  done
fi

# ---------------------------------------------------------------- verify
say "verifying what is actually running"

want_manager="$REGISTRY/pod-snapshotter/manager:$TAG_MANAGER"
want_agent="$REGISTRY/pod-snapshotter/agent:$TAG_AGENT"
want_criu="$REGISTRY/pod-snapshotter/criu:$TAG_CRIU"

# A pod spec may reference the image by tag or by digest. Pinning by digest is
# strictly stronger -- it is the only reference that cannot drift -- so accept
# it when it resolves to the tag the chart pins, and report the tag it stands
# for rather than flagging the safer form as a mismatch.
image_check() {
  local what="$1" workload="$2" repo="$3" tag="$4"
  local want="$REGISTRY/$repo:$tag" got
  got=$(kubectl -n "$NAMESPACE" get "$workload" -o jsonpath='{.spec.template.spec.containers[0].image}' 2>/dev/null || true)
  if [ "$got" = "$want" ]; then
    ok "$what image $got"
    return
  fi
  case "$got" in
    "$REGISTRY/$repo@sha256:"*)
      local pinned tagged
      pinned="${got#*@}"
      tagged=$(az acr repository show -n "${REGISTRY%%.*}" --image "$repo:$tag" --query digest -o tsv 2>/dev/null || true)
      if [ -n "$tagged" ] && [ "$pinned" = "$tagged" ]; then
        ok "$what image pinned by digest to $tag"
      else
        bad "$what image is pinned to ${pinned:0:19}, which is not $tag"
      fi
      ;;
    *) bad "$what image is ${got:-<missing>}, chart says $want" ;;
  esac
}
image_check manager deploy/pod-snapshotter-manager pod-snapshotter/manager "$TAG_MANAGER"
image_check agent   ds/pod-snapshotter-agent       pod-snapshotter/agent   "$TAG_AGENT"
image_check criu    ds/pod-snapshotter-criu        pod-snapshotter/criu    "$TAG_CRIU"

# The tag matching is not evidence the running binary is the one just built.
# These tags are mutable and the pull policy is IfNotPresent, so re-pushing a
# tag a node already has changes nothing: the pod spec is byte-identical, no
# rollout happens, and the node keeps serving the layers it cached. That is
# how a manager three commits old passed every check above while silently
# pruning a spec field it did not know about.
#
# Digests are the invariant. Ask the registry what the tag points at now, and
# the kubelet what it actually pulled.
digest_check() {
  local what="$1" repo="$2" tag="$3" selector="$4"
  local want got pod
  want=$(az acr repository show -n "${REGISTRY%%.*}" --image "$repo:$tag" --query digest -o tsv 2>/dev/null || true)
  if [ -z "$want" ]; then
    info "$what digest not checked (registry unreachable)"
    return
  fi
  pod=$(kubectl -n "$NAMESPACE" get pods --no-headers -o custom-columns=:metadata.name 2>/dev/null | grep "^$selector" | head -1)
  [ -n "$pod" ] || { bad "$what has no running pod to check"; return; }
  got=$(kubectl -n "$NAMESPACE" get pod "$pod" -o jsonpath='{.status.containerStatuses[0].imageID}' 2>/dev/null | sed 's/.*@//')
  if [ "$got" = "$want" ]; then
    ok "$what runs ${want:0:19}"
  else
    bad "$what runs ${got:0:19} but $tag now points at ${want:0:19} -- stale image, force a rollout"
  fi
}
digest_check manager pod-snapshotter/manager "$TAG_MANAGER" pod-snapshotter-manager-
digest_check agent   pod-snapshotter/agent   "$TAG_AGENT"   pod-snapshotter-agent-
digest_check criu    pod-snapshotter/criu    "$TAG_CRIU"    pod-snapshotter-criu-

# Every DaemonSet fully rolled. A partially-rolled criu DaemonSet is the exact
# state that produced a measurement attributed to the wrong binary.
for ds in pod-snapshotter-agent pod-snapshotter-criu; do
  read -r desired ready <<<"$(kubectl -n "$NAMESPACE" get ds $ds -o jsonpath='{.status.desiredNumberScheduled} {.status.numberReady}' 2>/dev/null)"
  [ -n "$desired" ] && [ "$desired" = "$ready" ] && ok "$ds $ready/$desired ready" || bad "$ds ${ready:-0}/${desired:-?} ready"
done

# The chart's image tag is not evidence the host runs the patched binary: the
# installer lays it beside the distro package and rolls back if it will not
# run. Ask the host what `criu` resolves to, and compare the gitid to the
# commit the Dockerfile pins. This is the check that would have caught both
# the marker-drift bug and the libnet.so.1 rollback.
want_gitid=$(awk -F= '/^ARG CRIU_FORK_COMMIT/{print substr($2,1,7)}' "$ROOT/Dockerfile.criu")
for p in $(kubectl -n "$NAMESPACE" get pods --no-headers -o custom-columns=:metadata.name 2>/dev/null | grep '^pod-snapshotter-criu-' || true); do
  node=$(kubectl -n "$NAMESPACE" get pod "$p" -o jsonpath='{.spec.nodeName}')
  out=$(kubectl -n "$NAMESPACE" exec "$p" -- chroot /host /bin/sh -c 'command -v criu; criu --version' 2>/dev/null || true)
  path=$(echo "$out" | head -1)
  gitid=$(echo "$out" | sed -n 's/^GitID: //p')
  if [ "$path" = "/usr/local/sbin/criu" ] && [ "$gitid" = "$want_gitid" ]; then
    ok "$node runs patched CRIU $gitid"
  elif kubectl -n "$NAMESPACE" logs "$p" --tail=20 2>/dev/null | grep -q '^SKIPPED:'; then
    # The installer refuses to leave a binary the host cannot exec, and an
    # older glibc than the build is not something a rollout fixes -- it parks
    # and keeps the distro CRIU. That is the designed outcome on a pool this
    # image was never built for, so reporting it as a failure trains people to
    # ignore a check that is otherwise the one thing standing between a
    # measurement and the wrong binary. Say what happened; do not fail.
    info "$node deliberately skipped: $(kubectl -n "$NAMESPACE" logs "$p" --tail=20 2>/dev/null | sed -n 's/^SKIPPED: //p' | head -1)"
  else
    bad "$node resolves criu to '${path:-?}' gitid '${gitid:-none}' (want /usr/local/sbin/criu $want_gitid)"
  fi
done

# Node prereqs, as the agent itself reports them. The manager refuses to
# checkpoint on a node without this, so a missing annotation is not cosmetic.
#
# Which nodes are GPU nodes is asked of the scheduler, not guessed from a
# label: the first version of this check looked for nvidia.com/gpu.present and
# silently verified nothing, because this cluster labels them accelerator=nvidia.
# A check that finds no nodes and reports success is worse than no check.
gpu_nodes=$(kubectl get nodes -o json 2>/dev/null | python3 -c '
import json,sys
for n in json.load(sys.stdin)["items"]:
    if n.get("status",{}).get("allocatable",{}).get("nvidia.com/gpu","0") != "0":
        print(n["metadata"]["name"])
')
if [ -z "$gpu_nodes" ]; then
  info "no nodes advertise nvidia.com/gpu; skipping the prereq check"
else
  for n in $gpu_nodes; do
    v=$(kubectl get node "$n" -o jsonpath='{.metadata.annotations.podsnapshot\.io/prereqs}' 2>/dev/null)
    [ "$v" = "ok" ] && ok "$n prereqs=ok" || bad "$n prereqs=${v:-<unset>}"
  done
fi

# An agent on every node that can hold a GPU workload. A node the DaemonSet
# does not select cannot checkpoint, restore, or report prereqs, and nothing
# above would notice -- the missing pod is not a failing pod.
for n in $gpu_nodes; do
  c=$(kubectl -n "$NAMESPACE" get pods --field-selector "spec.nodeName=$n" --no-headers 2>/dev/null | grep -c '^pod-snapshotter-agent-' || true)
  [ "${c:-0}" -ge 1 ] && ok "$n has an agent" || bad "$n has no agent pod"
done

# A rendered-vs-live diff catches everything the specific checks above do not:
# hand-patched args, an env var someone set during a benchmark and forgot.
say "rendered vs live"
if command -v helm >/dev/null && helm plugin list 2>/dev/null | grep -q diff; then
  helm -n "$NAMESPACE" diff upgrade "$RELEASE" "$CHART" --set imageRegistry="$REGISTRY" --set criu.enabled=true 2>/dev/null || true
else
  # No helm-diff plugin: compare the args of the two workloads that carry
  # tuning flags, which is where hand-patching actually happens.
  live=$(kubectl -n "$NAMESPACE" get ds pod-snapshotter-agent -o jsonpath='{.spec.template.spec.containers[0].args}' 2>/dev/null | tr ',' '\n' | tr -d '[]"' | sed 's/^ *//' | sort)
  rendered=$(render 2>/dev/null | python3 "$ROOT/hack/render_args.py" pod-snapshotter-agent 2>/dev/null | sort)
  if [ "$live" = "$rendered" ]; then
    ok "agent args match the chart"
  else
    bad "agent args differ from the chart:"
    diff <(echo "$rendered") <(echo "$live") | sed 's/^/        /' || true
  fi
fi

# ---------------------------------------------------------------- test
if $DO_TEST; then
  say "end-to-end test"
  "$ROOT/hack/e2e.sh" || FAILED=$((FAILED+1))
fi

say "result"
if [ "$FAILED" -eq 0 ]; then
  ok "cluster matches the chart"
else
  bad "$FAILED check(s) failed"
  exit 1
fi
