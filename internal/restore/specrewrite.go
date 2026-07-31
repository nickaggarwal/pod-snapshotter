package restore

import (
	"encoding/json"
	"fmt"
	"os"
	"path"
	"regexp"
	"strings"

	rspec "github.com/opencontainers/runtime-spec/specs-go"
)

// SandboxTarget describes the placeholder pod's sandbox the restored
// container must join.
type SandboxTarget struct {
	// PausePID is the host PID of the sandbox pause process (or the keeper
	// container's init when resolving via the keeper).
	PausePID int
	// PodUID of the placeholder pod (new pod).
	PodUID string
	// OldPodUID of the checkpointed pod, used to remap kubelet volume paths.
	OldPodUID string
	// CgroupsPath for the restored container, e.g. a systemd slice spec
	// "kubepods-besteffort-pod<uid>.slice:snap:<name>" or a cgroupfs path.
	CgroupsPath string
	// RootfsPath overrides the spec root path (the prepared rootfs).
	RootfsPath string
	// SandboxID of the NEW placeholder sandbox. Mount sources under
	// containerd's per-sandbox dirs (.../sandboxes/<64-hex>/hostname, shm,
	// resolv.conf) are rewritten from the old sandbox id to this one.
	SandboxID string
	// KeeperMounts (destination -> source) from the NEW keeper container's
	// OCI config. Last-resort remap for runtime-generated sources (e.g.
	// /run/nvidia-ctk-hook<uuid> files) that died with the old pod.
	KeeperMounts map[string]string
	// ExtraMounts: every mount CRIU recorded in the checkpointed container
	// (from dump.log). Entries missing from the spec (hook-injected driver
	// libs etc.) get bind entries added so restore can map them.
	ExtraMounts []ExtraMount
	// HostRoot is where the node's root filesystem is visible to THIS
	// process (e.g. /host in the agent container). Host paths in the spec
	// stay unprefixed — runc resolves them in the host mount namespace —
	// but existence checks here must go through HostRoot.
	HostRoot string
}

// sandboxDirRe matches containerd per-sandbox path components.
var sandboxDirRe = regexp.MustCompile(`(/sandboxes/)[0-9a-f]{64}(/)`)

// nsPath returns /proc/<pid>/ns/<kind>.
func nsPath(pid int, kind string) string {
	return fmt.Sprintf("/proc/%d/ns/%s", pid, kind)
}

// RewriteSpec loads spec.dump from bundleDir, rewrites it to join the target
// sandbox, and writes it as <bundle>/config.json. Rules:
//
//   - network/ipc/uts namespaces: join the sandbox by path (the restored
//     workload must share the placeholder pod's IP so probes/services work).
//   - pid namespace: FRESH (path cleared). runc restore creates a new pidns
//     and CRIU recreates the exact in-namespace PIDs via ns_last_pid — this
//     sidesteps the "CRIU needs the original host PID" problem entirely.
//   - cgroupsPath: moved under the new pod so GPU/memory accounting lands in
//     the placeholder pod's cgroup subtree.
//   - kubelet pod-scoped mount sources (/var/lib/kubelet/pods/<oldUID>/...)
//     are remapped to the new pod UID (serviceaccount token, hosts,
//     termination-log, emptyDirs).
//   - /dev/nvidia* device bind mounts are preserved verbatim with rprivate
//     propagation (the blog-documented requirement for cuda-checkpoint).
//   - root.path is pointed at the prepared rootfs.
func RewriteSpec(specDumpPath, outPath string, target SandboxTarget) (*rspec.Spec, error) {
	raw, err := os.ReadFile(specDumpPath)
	if err != nil {
		return nil, fmt.Errorf("reading spec.dump: %w", err)
	}
	var spec rspec.Spec
	if err := json.Unmarshal(raw, &spec); err != nil {
		return nil, fmt.Errorf("parsing spec.dump: %w", err)
	}
	if spec.Linux == nil {
		return nil, fmt.Errorf("spec.dump has no linux section")
	}

	// Namespaces.
	var namespaces []rspec.LinuxNamespace
	for _, ns := range spec.Linux.Namespaces {
		switch ns.Type {
		case rspec.NetworkNamespace:
			ns.Path = nsPath(target.PausePID, "net")
		case rspec.IPCNamespace:
			ns.Path = nsPath(target.PausePID, "ipc")
		case rspec.UTSNamespace:
			ns.Path = nsPath(target.PausePID, "uts")
		case rspec.PIDNamespace:
			ns.Path = "" // fresh pidns; CRIU restores in-namespace PIDs
		case rspec.MountNamespace:
			ns.Path = "" // fresh mount ns over the prepared rootfs
		}
		namespaces = append(namespaces, ns)
	}
	spec.Linux.Namespaces = namespaces

	if target.CgroupsPath != "" {
		spec.Linux.CgroupsPath = target.CgroupsPath
	}

	// Device cgroup: spec.dump carries only the CRI's default-deny rule —
	// the runtime grants device access at create time (CDI/device plugin),
	// outside the dumped spec. Replaying the deny-all here loads a BPF
	// cgroup_device program that blocks /dev/nvidia*, and cuda-checkpoint
	// then fails with "no CUDA-capable device is detected". Allow all
	// devices; real isolation still comes from the mount list (only the
	// dumped device nodes are bind-mounted in).
	if spec.Linux.Resources == nil {
		spec.Linux.Resources = &rspec.LinuxResources{}
	}
	spec.Linux.Resources.Devices = []rspec.LinuxDeviceCgroup{
		{Allow: true, Access: "rwm"},
	}

	// The kubelet-applied AppArmor profile (cri-containerd.apparmor.d) is
	// per-container-instance state the restore environment can't re-enter
	// (CRIU pie: "can't write lsm profile -22"). Run unconfined — the
	// keeper pod's own confinement still bounds the sandbox.
	if spec.Process != nil {
		spec.Process.ApparmorProfile = ""
		spec.Process.SelinuxLabel = ""
	}
	if target.RootfsPath != "" {
		if spec.Root == nil {
			spec.Root = &rspec.Root{}
		}
		spec.Root.Path = target.RootfsPath
	}

	// Mount rewrites.
	oldPodPrefix := "/var/lib/kubelet/pods/" + target.OldPodUID + "/"
	newPodPrefix := "/var/lib/kubelet/pods/" + target.PodUID + "/"
	for i := range spec.Mounts {
		m := &spec.Mounts[i]
		if target.OldPodUID != "" && target.PodUID != "" && strings.HasPrefix(m.Source, oldPodPrefix) {
			m.Source = newPodPrefix + strings.TrimPrefix(m.Source, oldPodPrefix)
		}
		// containerd sandbox-scoped files (hostname, resolv.conf, shm).
		if target.SandboxID != "" {
			m.Source = sandboxDirRe.ReplaceAllString(m.Source, "${1}"+target.SandboxID+"${2}")
		}
		// Kubelet volume dirs with generated names (projected serviceaccount
		// tokens like kube-api-access-<rand>) differ per pod instance; remap
		// to the new pod's actual dir when the rewritten source is missing.
		if strings.HasPrefix(m.Source, newPodPrefix) {
			m.Source = resolveVolumeDir(m.Source)
		}
		// Any remaining bind source that vanished with the old pod: borrow
		// the NEW keeper's source for the same destination.
		if m.Type == "bind" || hasBindOption(m.Options) {
			if _, err := os.Stat(m.Source); err != nil {
				if src, ok := target.KeeperMounts[m.Destination]; ok && src != "" {
					m.Source = src
				}
			}
		}
		// NVIDIA device nodes must stay private so CRIU's CUDA plugin can
		// manage the mappings (cuda-checkpoint requirement).
		if isNvidiaDevice(m.Destination) {
			m.Options = ensureOption(m.Options, "rprivate")
		}
	}

	// Hook-injected mounts (NVIDIA toolkit driver libs, ctk hook tmpfs
	// files): present in the checkpointed mount namespace but absent from
	// spec.dump because a runtime hook — not the OCI spec — created them.
	// Add bind entries so runc hands CRIU an external mapping for each.
	// Mounts under /proc are skipped (runc rejects them; CRIU restores
	// nested proc binds once their source mounts are mappable).
	inSpec := map[string]bool{}
	for _, m := range spec.Mounts {
		inSpec[m.Destination] = true
	}
	for _, em := range target.ExtraMounts {
		if inSpec[em.ContainerPath] || strings.HasPrefix(em.ContainerPath, "/proc/") {
			continue
		}
		// Mounts without host backing (tmpfs, e.g. the nvidia-ctk hook's
		// /run/nvidia-ctk-hook*) are NOT external: CRIU recreates them with
		// contents from the image. They only need a directory mountpoint in
		// the rootfs (PrepareHookMountpoints).
		if em.HostPath == "" {
			continue
		}
		if _, err := os.Stat(path.Join(target.HostRoot, em.HostPath)); err != nil {
			continue
		}
		spec.Mounts = append(spec.Mounts, rspec.Mount{
			Destination: em.ContainerPath,
			Type:        "bind",
			Source:      em.HostPath,
			Options:     []string{"rbind", "rprivate", "ro", "nosuid", "nodev"},
		})
		inSpec[em.ContainerPath] = true
	}

	if err := writeJSON(outPath, &spec); err != nil {
		return nil, err
	}
	return &spec, nil
}

// PrepareHookMountpoints pre-creates DIRECTORY mountpoints in the rootfs for
// the hook's tmpfs mounts. CRIU recreates the tmpfs (with contents) itself;
// it just needs a directory to mount on. A stale FILE at the path (from the
// toolkit hook's own bind or an earlier restore attempt) is removed.
func PrepareHookMountpoints(rootfsDir string, hookMounts []string) error {
	for _, hm := range hookMounts {
		if !strings.HasPrefix(hm, nvidiaHookPrefix) {
			continue
		}
		dst := path.Join(rootfsDir, hm)
		if fi, err := os.Stat(dst); err == nil && !fi.IsDir() {
			if err := os.Remove(dst); err != nil {
				return fmt.Errorf("removing stale file %s: %w", dst, err)
			}
		}
		if err := os.MkdirAll(dst, 0o755); err != nil {
			return fmt.Errorf("creating mountpoint dir %s: %w", dst, err)
		}
	}
	return nil
}

func hasBindOption(opts []string) bool {
	for _, o := range opts {
		if o == "bind" || o == "rbind" {
			return true
		}
	}
	return false
}

// resolveVolumeDir returns src if it exists; otherwise, for paths like
// .../volumes/kubernetes.io~projected/kube-api-access-abcde, it looks for a
// sibling dir sharing the name's prefix up to the last '-' (the generated
// suffix) and returns it when the match is unique.
func resolveVolumeDir(src string) string {
	if _, err := os.Stat(src); err == nil {
		return src
	}
	dir, base := path.Split(src)
	dash := strings.LastIndex(base, "-")
	if dash <= 0 {
		return src
	}
	prefix := base[:dash+1]
	entries, err := os.ReadDir(dir)
	if err != nil {
		return src
	}
	var match string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), prefix) {
			if match != "" {
				return src // ambiguous; leave unchanged
			}
			match = e.Name()
		}
	}
	if match == "" {
		return src
	}
	return dir + match
}

// ValidateGPUDevices checks that every /dev/nvidia* mount in the spec exists
// on the node — a restore onto a node without the devices fails later with a
// much worse error.
func ValidateGPUDevices(spec *rspec.Spec) error {
	var missing []string
	for _, m := range spec.Mounts {
		if isNvidiaDevice(m.Destination) {
			if _, err := os.Stat(m.Source); err != nil {
				missing = append(missing, m.Source)
			}
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("GPU device nodes missing on this node: %s", strings.Join(missing, ", "))
	}
	return nil
}

func isNvidiaDevice(dest string) bool {
	base := path.Base(dest)
	return strings.HasPrefix(dest, "/dev/") && (strings.HasPrefix(base, "nvidia") || base == "nvidia-uvm" || base == "nvidiactl")
}

func ensureOption(opts []string, opt string) []string {
	for _, o := range opts {
		if o == opt {
			return opts
		}
	}
	// Propagation options are mutually exclusive; strip competing ones.
	if strings.Contains(opt, "private") || strings.Contains(opt, "shared") || strings.Contains(opt, "slave") {
		filtered := opts[:0]
		for _, o := range opts {
			switch o {
			case "shared", "rshared", "slave", "rslave", "private", "rprivate", "unbindable", "runbindable":
			default:
				filtered = append(filtered, o)
			}
		}
		opts = filtered
	}
	return append(opts, opt)
}

func writeJSON(p string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p, data, 0o600)
}
