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
	// Checkpoint runs `runc checkpoint`, writing CRIU images to
	// opts.ImagePath.
	Checkpoint(ctx context.Context, opts CheckpointOpts) error
	// Kill sends SIGKILL to the container and deletes its runc state.
	Kill(ctx context.Context, containerID string) error
	// State returns whether the container exists and is running.
	State(ctx context.Context, containerID string) (running bool, err error)
}

// CRIUDumpLogName is the log file runc tells CRIU to write during a dump.
// As with restore, runc's own error is uninformative and this is where the
// reason lives.
const CRIUDumpLogName = "dump.log"

// CheckpointOpts are the inputs to `runc checkpoint`.
//
// The container is one the CRI runtime created and owns, so this runs against
// the runtime's own runc state root, not ours (see HostRunc.CRIRuncRoot).
type CheckpointOpts struct {
	ContainerID string
	// ImagePath is where CRIU writes its images. Pointing this straight at
	// the destination is the whole point of dumping from the agent: the
	// kubelet checkpoint API writes a tar, which then has to be read back and
	// expanded before any restore can use it (docs/design-v2.md §3).
	ImagePath string
	WorkPath  string
	// LeaveRunning keeps the container alive after the dump. The quiesce
	// protocol resumes the workload afterwards, so the default (false, i.e.
	// leave it stopped) is only right when the pod is being discarded.
	LeaveRunning bool
	// PreDump runs a memory pre-dump pass instead of a full dump.
	PreDump bool
	// ParentPath is a previous dump to diff against (with PreDump).
	ParentPath string
	// Env is added to runc's environment, and through it CRIU's -- the dump
	// side equivalent of RestoreOpts.Env.
	Env map[string]string
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
	// CRIRuncRoot is the CRI runtime's OWN runc state root, used only for
	// checkpointing containers the runtime created. Restores use RuncRoot;
	// dumps must use this one, because a container is only visible in the
	// state root of the runc that created it.
	CRIRuncRoot string
}

// NewHostRunc builds the default host runner.
func NewHostRunc() *HostRunc {
	return &HostRunc{
		NsenterTarget: 1,
		RuncBinary:    "runc",
		RuncRoot:      "/run/pod-snapshotter/runc",
		CRIRuncRoot:   "/run/containerd/runc/k8s.io",
	}
}

// command builds: nsenter -t 1 -m -p -- runc [globalArgs] <args>.
// Global flags (--root, --log, --log-format) must precede the subcommand.
func (h *HostRunc) command(ctx context.Context, globalArgs []string, args ...string) *exec.Cmd {
	return h.commandIn(ctx, h.RuncRoot, globalArgs, args...)
}

// commandIn is command() against an explicit runc state root. Checkpointing
// needs the CRI runtime's root, since that is the only place a container the
// runtime created is visible.
func (h *HostRunc) commandIn(ctx context.Context, root string, globalArgs []string, args ...string) *exec.Cmd {
	full := append([]string{
		"-t", strconv.Itoa(h.NsenterTarget), "-m", "-p", "--",
		h.RuncBinary, "--root", root,
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

// Checkpoint implements RuncRunner: `runc checkpoint` against the CRI
// runtime's state root, writing CRIU images straight to opts.ImagePath.
//
// This is the dump-side counterpart to Restore, and it exists to get out from
// under the kubelet checkpoint API. That API returns a tar, so the bytes are
// written by CRIU, read back and written again by the kubelet's tar, then
// read back and written a third time when the agent expands them into the
// directory layout a restore can actually use. For a 56 GB artifact that is
// 112 GB read and 168 GB written to publish 56 GB. Dumping here writes it
// once, in the layout the restore already expects.
//
// Two things the kubelet does that this deliberately does not: it garbage
// collects old checkpoint tars, and it enforces its own request timeout. The
// first is moot -- there is no tar to collect. The second was a problem, not
// a feature: a dump that takes longer than runtimeRequestTimeout fails at the
// API layer while CRIU is still running.
func (h *HostRunc) Checkpoint(ctx context.Context, opts CheckpointOpts) error {
	if err := os.MkdirAll(opts.WorkPath, 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(opts.ImagePath, 0o755); err != nil {
		return err
	}
	logFile := filepath.Join(opts.WorkPath, "runc-checkpoint.log")

	args := []string{
		"checkpoint",
		"--image-path", opts.ImagePath,
		"--work-path", opts.WorkPath,
	}
	if opts.LeaveRunning {
		args = append(args, "--leave-running")
	}
	if opts.PreDump {
		args = append(args, "--pre-dump")
	}
	if opts.ParentPath != "" {
		args = append(args, "--parent-path", opts.ParentPath)
	}
	args = append(args, opts.ContainerID)

	cmd := h.commandIn(ctx, h.CRIRuncRoot, []string{"--log", logFile, "--log-format", "json"}, args...)
	if len(opts.Env) > 0 {
		cmd.Env = os.Environ()
		for _, k := range slices.Sorted(maps.Keys(opts.Env)) {
			cmd.Env = append(cmd.Env, k+"="+opts.Env[k])
		}
	}
	// Unlike Restore, nothing detaches here: runc exits when the dump is
	// done, so capturing output in-process cannot deadlock on a surviving
	// writer.
	out, runErr := cmd.CombinedOutput()
	if runErr != nil {
		return fmt.Errorf("runc checkpoint failed: %w\noutput: %s\nrunc log tail: %s\n%s",
			runErr,
			strings.TrimSpace(string(out)),
			tailFile(logFile, 4096),
			criuDumpLogTail(opts.WorkPath, 8192))
	}
	return nil
}

// criuDumpLogTail returns the tail of CRIU's own dump log, which is where the
// real reason for a failed dump appears.
func criuDumpLogTail(workPath string, n int64) string {
	t := tailFile(filepath.Join(workPath, CRIUDumpLogName), n)
	if t == "" {
		return ""
	}
	return "criu dump.log tail: " + t
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
