package restore

import (
	"encoding/json"
	"fmt"
	"os"
	"path"
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
}

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
		// NVIDIA device nodes must stay private so CRIU's CUDA plugin can
		// manage the mappings (cuda-checkpoint requirement).
		if isNvidiaDevice(m.Destination) {
			m.Options = ensureOption(m.Options, "rprivate")
		}
	}

	if err := writeJSON(outPath, &spec); err != nil {
		return nil, err
	}
	return &spec, nil
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
