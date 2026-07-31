package agent

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	snapv1 "pod-snapshotter/api/v1alpha1"
)

// PrereqChecker verifies node prerequisites for GPU checkpoint/restore and
// publishes the result as the podsnapshot.io/prereqs node annotation
// ("ok" or a comma-separated list of failing checks). The manager refuses to
// checkpoint pods on nodes without "ok".
//
// Checks (via nsenter into the host mount namespace):
//  1. criu >= 4.1 on the host (4.0 introduced the CUDA plugin; 4.1 fixes)
//  2. criu CUDA plugin present
//  3. cuda-checkpoint binary on the host PATH
//  4. NVIDIA driver >= 570 (nvidia-smi)
//  5. /etc/criu/runc.conf contains tcp-established and link-remap
//  6. fuse mount present
//
// GPU checks are skipped (not failed) on nodes without /dev/nvidiactl so the
// system remains usable for CPU-only checkpoint trials.
type PrereqChecker struct {
	Client    client.Client
	NodeName  string
	FuseMount string
	// HostRoot is where the host filesystem is visible (e.g. /host), used for
	// file checks; command checks go through nsenter.
	HostRoot string
	// Interval between checks (default 5m).
	Interval time.Duration
	// SkipHostChecks disables nsenter-based checks (tests / non-Linux dev).
	SkipHostChecks bool
}

var (
	criuVersionRe = regexp.MustCompile(`Version:\s*(\d+)\.(\d+)`)
	// nvidia-container-runtime config: mode = "cdi" in the
	// [nvidia-container-runtime] section (nvidia-ctk config --set writes it
	// with this exact shape).
	nvidiaCDIModeRe = regexp.MustCompile(`(?m)^\s*mode\s*=\s*"cdi"`)
)

// Start runs the checker loop until ctx is done. Blocks; run in a goroutine
// or via mgr.Add.
func (p *PrereqChecker) Start(ctx context.Context) error {
	interval := p.Interval
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	// First check immediately.
	p.checkAndPublish(ctx)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			p.checkAndPublish(ctx)
		}
	}
}

func (p *PrereqChecker) checkAndPublish(ctx context.Context) {
	logger := log.FromContext(ctx).WithName("prereq")

	var node corev1.Node
	if err := p.Client.Get(ctx, types.NamespacedName{Name: p.NodeName}, &node); err != nil {
		logger.Error(err, "getting node")
		return
	}

	failures := p.run(ctx, &node)

	value := "ok"
	if len(failures) > 0 {
		value = strings.Join(failures, ",")
	}
	if node.Annotations[snapv1.PrereqsAnnotation] == value {
		return
	}
	patch := client.MergeFrom(node.DeepCopy())
	if node.Annotations == nil {
		node.Annotations = map[string]string{}
	}
	node.Annotations[snapv1.PrereqsAnnotation] = value
	if err := p.Client.Patch(ctx, &node, patch); err != nil {
		logger.Error(err, "patching node annotation")
		return
	}
	logger.Info("published prereq status", "value", value)
}

// run returns the list of failing check names.
func (p *PrereqChecker) run(ctx context.Context, node *corev1.Node) []string {
	var failures []string

	// Container runtime must implement CRI CheckpointContainer: containerd
	// >= 2.0 or CRI-O >= 1.25. containerd 1.7 returns Unimplemented.
	// Read from the Node object — no host access needed.
	if !runtimeSupportsCheckpoint(node.Status.NodeInfo.ContainerRuntimeVersion) {
		failures = append(failures, "runtime-no-checkpoint-support")
	}

	// Fuse mount visible to the agent.
	if p.FuseMount != "" {
		if fi, err := os.Stat(p.FuseMount); err != nil || !fi.IsDir() {
			failures = append(failures, "fuse-mount-missing")
		}
	}

	if p.SkipHostChecks {
		return failures
	}

	hasGPU := p.hostFileExists("/dev/nvidiactl")

	// CRIU version: containerd needs >= 3.16 for CheckpointContainer; GPU
	// checkpointing needs >= 4.1 (4.0 introduced the CUDA plugin, 4.1 fixes).
	// The nodeSetup DaemonSet installs the PPA 4.x package on GPU nodes only,
	// so hold CPU-only nodes to the lower bar.
	if out, err := p.hostCommand(ctx, "criu", "--version"); err != nil {
		failures = append(failures, "criu-missing")
	} else if maj, min, ok := parseCriuVersion(out); !ok {
		failures = append(failures, "criu-version-unparseable")
	} else if hasGPU && (maj < 4 || (maj == 4 && min < 1)) {
		failures = append(failures, "criu-version-lt-4.1")
	} else if maj < 3 || (maj == 3 && min < 16) {
		failures = append(failures, "criu-version-lt-3.16")
	}

	// CRIU CUDA plugin (well-known install paths).
	if hasGPU {
		pluginFound := false
		for _, path := range []string{
			"/usr/lib/criu/cuda_plugin.so",
			"/usr/local/lib/criu/cuda_plugin.so",
			"/usr/lib64/criu/cuda_plugin.so",
		} {
			if p.hostFileExists(path) {
				pluginFound = true
				break
			}
		}
		if !pluginFound {
			failures = append(failures, "criu-cuda-plugin-missing")
		}

		if _, err := p.hostCommand(ctx, "cuda-checkpoint", "--help"); err != nil {
			failures = append(failures, "cuda-checkpoint-missing")
		}

		if out, err := p.hostCommand(ctx, "nvidia-smi", "--query-gpu=driver_version", "--format=csv,noheader"); err != nil {
			failures = append(failures, "nvidia-smi-missing")
		} else if major := driverMajor(out); major > 0 && major < 570 {
			failures = append(failures, "nvidia-driver-lt-570")
		}

		// The toolkit must run in CDI mode: legacy mode injects driver-lib
		// mounts outside the OCI spec and CRIU cannot map them at restore
		// ("No mapping for N:(null) mountpoint"). nodeSetup switches the
		// mode; verify it actually took effect.
		if raw, err := os.ReadFile(p.hostPath("/etc/nvidia-container-runtime/config.toml")); err != nil || !nvidiaCDIModeRe.Match(raw) {
			failures = append(failures, "nvidia-toolkit-not-cdi-mode")
		}
	}

	// runc.conf directives.
	if raw, err := os.ReadFile(p.hostPath("/etc/criu/runc.conf")); err != nil {
		failures = append(failures, "criu-runc-conf-missing")
	} else {
		conf := string(raw)
		if !strings.Contains(conf, "tcp-established") {
			failures = append(failures, "runc-conf-no-tcp-established")
		}
		if !strings.Contains(conf, "link-remap") {
			failures = append(failures, "runc-conf-no-link-remap")
		}
	}

	return failures
}

func (p *PrereqChecker) hostCommand(ctx context.Context, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	full := append([]string{"-t", "1", "-m", "-p", "--", name}, args...)
	out, err := exec.CommandContext(ctx, "nsenter", full...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%s: %w (%s)", name, err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// hostPath prefixes a host filesystem path with the HostRoot mount.
func (p *PrereqChecker) hostPath(path string) string {
	return p.HostRoot + path
}

func (p *PrereqChecker) hostFileExists(path string) bool {
	_, err := os.Stat(p.hostPath(path))
	return err == nil
}

func parseCriuVersion(out string) (major, minor int, ok bool) {
	m := criuVersionRe.FindStringSubmatch(out)
	if len(m) != 3 {
		return 0, 0, false
	}
	major, _ = strconv.Atoi(m[1])
	minor, _ = strconv.Atoi(m[2])
	return major, minor, true
}

// runtimeSupportsCheckpoint parses Node.status.nodeInfo.containerRuntimeVersion
// (e.g. "containerd://1.7.30-2", "containerd://2.0.4", "cri-o://1.29.1") and
// reports whether the runtime implements CRI CheckpointContainer.
func runtimeSupportsCheckpoint(runtimeVersion string) bool {
	name, ver, ok := strings.Cut(runtimeVersion, "://")
	if !ok {
		return false
	}
	parts := strings.SplitN(ver, ".", 3)
	if len(parts) < 2 {
		return false
	}
	major, err1 := strconv.Atoi(parts[0])
	minor, err2 := strconv.Atoi(strings.TrimFunc(parts[1], func(r rune) bool { return r < '0' || r > '9' }))
	if err1 != nil || err2 != nil {
		return false
	}
	switch name {
	case "containerd":
		return major >= 2
	case "cri-o":
		return major > 1 || (major == 1 && minor >= 25)
	default:
		return false
	}
}

func driverMajor(out string) int {
	fields := strings.SplitN(strings.TrimSpace(out), ".", 2)
	if len(fields) == 0 {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(fields[0]))
	if err != nil {
		return 0
	}
	return n
}
