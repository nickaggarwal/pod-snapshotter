package restore

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// RuncRunner abstracts runc invocations so the restore controller is testable
// without a Linux host.
type RuncRunner interface {
	// Restore runs `runc restore` detached and returns the restored init's
	// host PID.
	Restore(ctx context.Context, opts RestoreOpts) (pid int, err error)
	// Kill sends SIGKILL to the container and deletes its runc state.
	Kill(ctx context.Context, containerID string) error
	// State returns whether the container exists and is running.
	State(ctx context.Context, containerID string) (running bool, err error)
}

// RestoreOpts are the inputs to `runc restore`.
type RestoreOpts struct {
	ContainerID string
	BundleDir   string // contains config.json (the rewritten spec)
	ImagePath   string // CRIU images (checkpoint/ dir from the tar)
	WorkPath    string // CRIU work/log dir
	// TCPClose maps to CRIU --tcp-close: restore with established TCP
	// connections closed instead of preserved (escape hatch when peers are
	// gone; runc.conf's tcp-established handles the normal case).
	TCPClose bool
}

// HostRunc executes the node's runc/criu through nsenter into the host mount
// namespace, so the agent container never has to ship binaries matching the
// node's driver/CRIU versions.
//
// Requires the agent pod: privileged, hostPID: true.
type HostRunc struct {
	// NsenterTarget is the PID whose mount ns to enter (1 = host init).
	NsenterTarget int
	// RuncBinary on the host (default "runc"; resolved on the host's PATH).
	RuncBinary string
	// RuncRoot is runc's state root. MUST be a namespace distinct from the
	// CRI runtime's (containerd uses /run/containerd/runc/k8s.io) so our
	// containers never collide with kubelet-managed ones.
	RuncRoot string
}

// NewHostRunc builds the default host runner.
func NewHostRunc() *HostRunc {
	return &HostRunc{
		NsenterTarget: 1,
		RuncBinary:    "runc",
		RuncRoot:      "/run/pod-snapshotter/runc",
	}
}

func (h *HostRunc) command(ctx context.Context, args ...string) *exec.Cmd {
	full := append([]string{
		"-t", strconv.Itoa(h.NsenterTarget), "-m", "-p", "--",
		h.RuncBinary, "--root", h.RuncRoot,
	}, args...)
	return exec.CommandContext(ctx, "nsenter", full...)
}

// Restore implements RuncRunner.
func (h *HostRunc) Restore(ctx context.Context, opts RestoreOpts) (int, error) {
	pidFile := filepath.Join(opts.WorkPath, "restored.pid")
	logFile := filepath.Join(opts.WorkPath, "runc-restore.log")
	if err := os.MkdirAll(opts.WorkPath, 0o755); err != nil {
		return 0, err
	}

	args := []string{
		"restore",
		"--bundle", opts.BundleDir,
		"--image-path", opts.ImagePath,
		"--work-path", opts.WorkPath,
		"--pid-file", pidFile,
		"--log", logFile,
		"--log-format", "json",
		"--detach",
	}
	if opts.TCPClose {
		args = append(args, "--tcp-close")
	}
	args = append(args, opts.ContainerID)

	cmd := h.command(ctx, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return 0, fmt.Errorf("runc restore failed: %w\noutput: %s\n%s", err, strings.TrimSpace(string(out)), tailFile(logFile, 4096))
	}

	raw, err := os.ReadFile(pidFile)
	if err != nil {
		return 0, fmt.Errorf("runc restore succeeded but pid file unreadable: %w", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		return 0, fmt.Errorf("parsing pid file: %w", err)
	}
	return pid, nil
}

// Kill implements RuncRunner.
func (h *HostRunc) Kill(ctx context.Context, containerID string) error {
	// Best-effort kill, then delete state.
	killCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	_ = h.command(killCtx, "kill", containerID, "KILL").Run()

	delCtx, cancel2 := context.WithTimeout(ctx, 30*time.Second)
	defer cancel2()
	out, err := h.command(delCtx, "delete", "--force", containerID).CombinedOutput()
	if err != nil && !strings.Contains(string(out), "does not exist") {
		return fmt.Errorf("runc delete %s: %w (%s)", containerID, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// State implements RuncRunner.
func (h *HostRunc) State(ctx context.Context, containerID string) (bool, error) {
	out, err := h.command(ctx, "state", containerID).CombinedOutput()
	if err != nil {
		if strings.Contains(string(out), "does not exist") {
			return false, nil
		}
		return false, fmt.Errorf("runc state %s: %w (%s)", containerID, err, strings.TrimSpace(string(out)))
	}
	return strings.Contains(string(out), `"status": "running"`) || strings.Contains(string(out), `"status":"running"`), nil
}

func tailFile(p string, n int64) string {
	f, err := os.Open(p)
	if err != nil {
		return ""
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return ""
	}
	off := int64(0)
	if fi.Size() > n {
		off = fi.Size() - n
	}
	buf := make([]byte, fi.Size()-off)
	if _, err := f.ReadAt(buf, off); err != nil {
		return ""
	}
	return "criu log tail:\n" + string(buf)
}
