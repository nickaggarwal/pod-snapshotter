#!/bin/sh
# Installs pod-snapshotter's patched CRIU onto the host, beside the distro
# package rather than over it: /usr/local/sbin precedes /usr/sbin on the
# default PATH, so runc picks up the patched binary, and uninstalling is `rm`.
#
# The CUDA plugin is NOT replaced. The fork is based on the exact CRIU version
# the node already runs, so the plugin ABI is unchanged and the patched binary
# loads the node's existing /usr/lib/criu/cuda_plugin.so — the GPU-critical
# piece stays byte-identical to what has already been validated there. A plugin
# is only dropped in if the node has none at all.
#
# Runs as a privileged DaemonSet with the host root bind-mounted at $HOST_ROOT.
set -eu

HOST_ROOT="${HOST_ROOT:-/host}"
MARKER="${HOST_ROOT}/usr/local/sbin/.criu-pod-snapshotter"
VERSION="${CRIU_BUILD_VERSION:-unknown}"

hostrun() { chroot "$HOST_ROOT" /bin/sh -c "$1"; }

if [ "${UNINSTALL:-false}" = "true" ]; then
    rm -f "${HOST_ROOT}/usr/local/sbin/criu" "$MARKER"
    echo "removed the patched CRIU; the distro package takes over again"
    hostrun 'command -v criu; criu --version' || true
    exec sleep infinity
fi

# The marker records what is installed. It is compared against the version
# *and* against the binary's own gitid, because those can disagree: a
# `kubectl set image` that does not also update CRIU_BUILD_VERSION leaves the
# marker matching while the image underneath has changed, and the installer
# would skip the very rollout it was asked to do. Trusting the built binary
# rather than the env var alone makes that fail safe.
BUILT_ID="$(sed -n 's/^GitID: //p' /criu-gitid 2>/dev/null || true)"
WANT="${VERSION}${BUILT_ID:+ ${BUILT_ID}}"

if [ -f "$MARKER" ] && [ "$(cat "$MARKER")" = "$WANT" ]; then
    echo "patched CRIU $WANT already installed"
    exec sleep infinity
fi

# Wait for the host to finish its own provisioning before touching it. On a
# freshly scaled-up node this DaemonSet can land minutes before the distro
# criu package and its dependencies do, and the patched binary links against
# the same shared libraries that package pulls in (libnet, libnl, libbsd...).
# Installing into that window and failing the run-check reads as "this build
# is broken on this host" when the truth is "ask again in a minute" -- so wait
# for the host to look provisioned, and only then lay anything down.
i=0
while [ $i -lt 60 ]; do
    hostrun 'command -v criu >/dev/null 2>&1' && break
    [ $i -eq 0 ] && echo "waiting for the host's distro criu to arrive before installing over it"
    i=$((i + 1))
    sleep 10
done
if [ $i -ge 60 ]; then
    echo "host still has no distro criu after 10 minutes; installing anyway" >&2
fi

echo "installing patched CRIU $WANT"
tar -C "$HOST_ROOT" -xzf /criu-dist.tar.gz

PLUGIN_ADDED=false
if [ ! -f "${HOST_ROOT}/usr/lib/criu/cuda_plugin.so" ]; then
    echo "node has no CUDA plugin; installing the one built with this CRIU"
    mkdir -p "${HOST_ROOT}/usr/lib/criu"
    cp /cuda_plugin.so "${HOST_ROOT}/usr/lib/criu/cuda_plugin.so"
    PLUGIN_ADDED=true
fi

# Refuse to leave a binary behind that the host cannot actually run: a missing
# shared library here would otherwise surface as a failed restore much later.
if ! hostrun '/usr/local/sbin/criu --version'; then
    echo "ERROR: the patched CRIU does not run on this host; rolling back" >&2
    rm -f "${HOST_ROOT}/usr/local/sbin/criu"
    # Undo the plugin too. A rollback that leaves it behind hands the distro
    # binary a plugin it was not validated with, which is a worse state than
    # the one we found -- and it makes the next attempt skip the copy.
    [ "$PLUGIN_ADDED" = true ] && rm -f "${HOST_ROOT}/usr/lib/criu/cuda_plugin.so"
    exit 1
fi

printf '%s' "$WANT" > "$MARKER"

echo "--- what the host resolves now ---"
hostrun 'command -v criu; criu --version'

exec sleep infinity
