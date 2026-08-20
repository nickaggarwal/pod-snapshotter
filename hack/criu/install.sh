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

if [ -f "$MARKER" ] && [ "$(cat "$MARKER")" = "$VERSION" ]; then
    echo "patched CRIU $VERSION already installed"
    exec sleep infinity
fi

echo "installing patched CRIU $VERSION"
tar -C "$HOST_ROOT" -xzf /criu-dist.tar.gz

if [ ! -f "${HOST_ROOT}/usr/lib/criu/cuda_plugin.so" ]; then
    echo "node has no CUDA plugin; installing the one built with this CRIU"
    mkdir -p "${HOST_ROOT}/usr/lib/criu"
    cp /cuda_plugin.so "${HOST_ROOT}/usr/lib/criu/cuda_plugin.so"
fi

# Refuse to leave a binary behind that the host cannot actually run: a missing
# shared library here would otherwise surface as a failed restore much later.
if ! hostrun '/usr/local/sbin/criu --version'; then
    echo "ERROR: the patched CRIU does not run on this host; rolling back" >&2
    rm -f "${HOST_ROOT}/usr/local/sbin/criu"
    exit 1
fi

printf '%s' "$VERSION" > "$MARKER"

echo "--- what the host resolves now ---"
hostrun 'command -v criu; criu --version'

exec sleep infinity
