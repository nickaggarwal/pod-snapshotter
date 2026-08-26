# Stock vs patched CRIU, 2026-08-25

The control for `docs/design-v2.md` §6e. Every restore figure in §6 before this
was taken on the fork, which made "the fork is worth X" an attribution rather
than a result.

Run by `hack/measure-criu-control.sh /tmp/restore-kubenvme.yaml
aks-gpuckpt-13588264-vmss00000g`. Same artifact
(`qwen2.5-14b-instruct-kubenvme`, 56.4 GB on node NVMe), same node, same
transport, `drop_caches` before each arm. Between the arms the DaemonSet was
rolled with `criu.uninstall=true`, which removes `/usr/local/sbin/criu` and
lets the host fall back to its distro package -- CRIU 4.2.1, the exact upstream
base the fork is built from -- then rolled back with the flag off.

|                  | CRIU proper | wall to serving | nvme0n1 read |
|------------------|-------------|-----------------|--------------|
| patched (6c683e4)| 35.9 s      | 43 s            | 56.4 GB      |
| stock 4.2.1      | 72.6 s      | 79 s            | 56.4 GB      |
|                  | **2.02x**   | 1.84x           | identical    |

`*-restore-milestones.log` is CRIU's own log filtered to the memfd phase and
the completion line; the full logs are ~3 MB each and are not committed. They
are what shows the mechanism: 205 memfd inodes restored across 8 threads in
0.003 s (patched) versus serially over 56.8 s (stock), with the 16 s tail after
that phase identical in both arms.
