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

var criuVersionRe = regexp.MustCompile(`Version:\s*(\d+)\.(\d+)`)

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
	failures := p.run(ctx)

	value := "ok"
	if len(failures) > 0 {
		value = strings.Join(failures, ",")
	}

	var node corev1.Node
	if err := p.Client.Get(ctx, types.NamespacedName{Name: p.NodeName}, &node); err != nil {
		logger.Error(err, "getting node")
		return
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
func (p *PrereqChecker) run(ctx context.Context) []string {
	var failures []string

	// Fuse mount visible to the agent.
	if p.FuseMount != "" {
		if fi, err := os.Stat(p.FuseMount); err != nil || !fi.IsDir() {
			failures = append(failures, "fuse-mount-missing")
		}
	}

	if p.SkipHostChecks {
		return failures
	}

	// CRIU version.
	if out, err := p.hostCommand(ctx, "criu", "--version"); err != nil {
		failures = append(failures, "criu-missing")
	} else if maj, min, ok := parseCriuVersion(out); !ok || maj < 4 || (maj == 4 && min < 1) {
		failures = append(failures, "criu-version-lt-4.1")
	}

	// CRIU CUDA plugin (well-known install paths).
	hasGPU := p.hostFileExists("/dev/nvidiactl")
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
	}

	// runc.conf directives.
	confPath := "/etc/criu/runc.conf"
	if p.HostRoot != "" {
		confPath = p.HostRoot + confPath
	}
	if raw, err := os.ReadFile(confPath); err != nil {
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

func (p *PrereqChecker) hostFileExists(path string) bool {
	if p.HostRoot != "" {
		path = p.HostRoot + path
	}
	_, err := os.Stat(path)
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
