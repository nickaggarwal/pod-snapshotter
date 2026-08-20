package restore

import (
	"context"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// CRIURestoreLogName is the log file runc tells CRIU to write inside the
// work path. It is CRIU's own diagnostic output and the only place the real
// reason for a failed restore appears; runc's wrapper error is always the
// uninformative "criu failed: type RESTORE errno 0".
const CRIURestoreLogName = "restore.log"

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
	// TCPClose passes --tcp-established=false to runc restore: refuse to
	// preserve established TCP connections (escape hatch when peers are
	// gone; runc.conf's tcp-established handles the normal case).
	TCPClose bool
	// Env is added to the environment runc, and through it CRIU, runs
	// with. runc execs criu directly and nsenter does not sanitize the
	// environment, so this is how the restore read-path tunables of the
	// patched CRIU (hack/criu/patches) are set per-restore rather than
	// per-node.
	Env map[string]string
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

// command builds: nsenter -t 1 -m -p -- runc [globalArgs] <args>.
// Global flags (--root, --log, --log-format) must precede the subcommand.
func (h *HostRunc) command(ctx context.Context, globalArgs []string, args ...string) *exec.Cmd {
	full := append([]string{
		"-t", strconv.Itoa(h.NsenterTarget), "-m", "-p", "--",
		h.RuncBinary, "--root", h.RuncRoot,
	}, globalArgs...)
	full = append(full, args...)
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
		"--detach",
	}
	// --tcp-close is not in all runc builds; --tcp-established=false is the
	// portable spelling.
	if opts.TCPClose {
		args = append(args, "--tcp-established=false")
	}
	// NOTE: no --lsm-profile flag. AppArmor rejects changeprofile into
	// "unconfined" (EINVAL), so profile overrides cannot help here; instead
	// the CHECKPOINTED pod must run AppArmor-unconfined
	// (securityContext.appArmorProfile: Unconfined, k8s >= 1.30) so the
	// CRIU image records no LSM state. See docs/prerequisites.md.
	args = append(args, opts.ContainerID)

	cmd := h.command(ctx, []string{"--log", logFile, "--log-format", "json"}, args...)
	if len(opts.Env) > 0 {
		cmd.Env = os.Environ()
		for _, k := range slices.Sorted(maps.Keys(opts.Env)) {
			cmd.Env = append(cmd.Env, k+"="+opts.Env[k])
		}
	}
	// The detached container inherits runc's stdio: never hand it in-process
	// pipes (CombinedOutput) — the restored workload keeps the write end open
	// forever and the agent would block on EOF long after runc exits. Point
	// stdio at a file instead; it doubles as the restored workload's log.
	outPath := filepath.Join(opts.WorkPath, "restore-output.log")
	outFile, err := os.OpenFile(outPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return 0, fmt.Errorf("opening restore output log: %w", err)
	}
	cmd.Stdout = outFile
	cmd.Stderr = outFile
	cmd.Stdin = nil
	runErr := cmd.Run()
	outFile.Close()
	if runErr != nil {
		// Tail only: the output file doubles as the restored workload's log
		// and can be arbitrarily large; this error lands in the CR status.
		//
		// criuLogTail is the one that actually says why a restore failed —
		// runc only ever reports "criu failed: type RESTORE errno 0". It is
		// listed last so that if a status message is truncated somewhere
		// downstream, the least useful text is what gets cut.
		return 0, fmt.Errorf("runc restore failed: %w\noutput tail: %s\nrunc log tail: %s\n%s",
			runErr,
			strings.TrimSpace(tailFile(outPath, 4096)),
			tailFile(logFile, 4096),
			CRIULogTail(opts.WorkPath, 8192, opts.ImagePath))
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
	_ = h.command(killCtx, nil, "kill", containerID, "KILL").Run()

	delCtx, cancel2 := context.WithTimeout(ctx, 30*time.Second)
	defer cancel2()
	out, err := h.command(delCtx, nil, "delete", "--force", containerID).CombinedOutput()
	if err != nil && !strings.Contains(string(out), "does not exist") {
		return fmt.Errorf("runc delete %s: %w (%s)", containerID, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// State implements RuncRunner.
func (h *HostRunc) State(ctx context.Context, containerID string) (bool, error) {
	out, err := h.command(ctx, nil, "state", containerID).CombinedOutput()
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
	return string(buf)
}

// CRIULogTail returns the tail of the CRIU log runc asked for, or — when
// there is no such file — an inventory of the work directory so a failed
// restore is still diagnosable after the fact.
//
// runc names the log "restore.log" and hands CRIU an fd to the work dir, so
// that is normally where it lands; alsoLook covers the builds that resolve it
// against the images directory instead. It is checked for by name rather than
// globbed because a partial write is still worth reading.
func CRIULogTail(workPath string, n int64, alsoLook ...string) string {
	for _, dir := range append([]string{workPath}, alsoLook...) {
		if dir == "" {
			continue
		}
		if t := tailFile(filepath.Join(dir, CRIURestoreLogName), n); t != "" {
			return "criu " + filepath.Join(dir, CRIURestoreLogName) + " tail:\n" + strings.TrimSpace(t)
		}
	}
	entries, err := os.ReadDir(workPath)
	if err != nil {
		return fmt.Sprintf("no criu %s and work dir %s unreadable: %v", CRIURestoreLogName, workPath, err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		size := int64(-1)
		if fi, err := e.Info(); err == nil {
			size = fi.Size()
		}
		names = append(names, fmt.Sprintf("%s(%d)", e.Name(), size))
	}
	slices.Sort(names)
	return fmt.Sprintf("no criu %s in %s; work dir holds: %s", CRIURestoreLogName, workPath, strings.Join(names, " "))
}
