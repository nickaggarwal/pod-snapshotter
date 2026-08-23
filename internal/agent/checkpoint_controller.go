package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	snapv1 "pod-snapshotter/api/v1alpha1"
	"pod-snapshotter/internal/artifact"
	"pod-snapshotter/internal/restore"
)

// CheckpointReconciler owns the Checkpointing phase for PodSnapshots that
// asked for checkpointer: agent. It runs `runc checkpoint` against the CRI
// runtime's own container and points CRIU's --image-path straight at the
// artifact directory, so the images are written once, where they will be
// read from.
//
// This is the write side of docs/design-v2.md §3. The read side already
// works this way: a directory artifact is restored in place, with no untar.
// The kubelet checkpoint API is what kept the write side from matching —
// it hands back a tar whatever you do with it, so CRIU writes the bytes, the
// kubelet reads them back to build the archive, and the agent reads the
// archive back to expand it. Three passes over the same 56 GB, and on a
// stock AKS GPU node all of it lands on the OS disk, because that is where
// /var/lib/kubelet is: the NVMe tier the artifact is bound for is a
// different device entirely.
//
// The kubelet path stays as the default and the fallback. It is the only one
// that works for tar artifacts, and it is the one to reach for when a
// runtime's runc state is somewhere this agent cannot see.
type CheckpointReconciler struct {
	client.Client
	NodeName string
	// FuseMount is the node's fuse-client mount point, as this container
	// sees it.
	FuseMount string
	// WorkRoot is node-local scratch; CRIU's work directory (its logs and
	// stats) goes here, never into the artifact.
	WorkRoot string
	// HostRoot is the host's / mounted read-only here.
	HostRoot string
	// Digest computes sha256 for every image file at publish time. Off by
	// default: it means reading all the bytes back immediately after writing
	// them, which is the pass this path exists to remove
	// (artifact.PublishDirOptions.Digest).
	Digest bool

	Resolver SandboxResolver
	Runc     restore.RuncRunner

	// inflight guards against a second dump for a snapshot whose first one is
	// still running — reconciles keep arriving while runc holds the container.
	inflight sync.Map
}

// +kubebuilder:rbac:groups=podsnapshot.io,resources=podsnapshots,verbs=get;list;watch
// +kubebuilder:rbac:groups=podsnapshot.io,resources=podsnapshots/status,verbs=get;update;patch

func (r *CheckpointReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var snap snapv1.PodSnapshot
	if err := r.Get(ctx, req.NamespacedName, &snap); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !r.owns(&snap) {
		return ctrl.Result{}, nil
	}

	key := req.NamespacedName
	if _, running := r.inflight.LoadOrStore(key, struct{}{}); running {
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}
	defer r.inflight.Delete(key)

	return r.checkpoint(ctx, &snap)
}

func (r *CheckpointReconciler) checkpoint(ctx context.Context, snap *snapv1.PodSnapshot) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	uri, err := artifact.Parse(snap.Status.Artifact.URI)
	if err != nil {
		return r.fail(ctx, snap, err.Error())
	}
	if !uri.Dir {
		// The manager should never route a tar here; say so plainly rather
		// than dumping into a path that means something else.
		return r.fail(ctx, snap, fmt.Sprintf(
			"checkpointer %q needs a directory artifact; %s is a tar", snapv1.CheckpointerAgent, uri.String()))
	}

	sandbox, err := r.Resolver.Resolve(ctx, snap.Status.PodUID, snap.Status.Container)
	if err != nil {
		// The pod is running — the manager checked — so this is the CRI being
		// slow, not the container being gone.
		return r.retry(ctx, snap, fmt.Sprintf("resolving container to checkpoint: %v", err))
	}

	// Two things have to be read while the container is still LIVE, because
	// `runc checkpoint` takes the process with it and /proc/<pid> goes with
	// the process. The kubelet path gets both for free — containerd collects
	// them while assembling the tar.
	//
	// The OCI spec is the first: the restore rewrites it into the new
	// sandbox's config.json.
	spec, err := readTaskSpec(sandbox.KeeperContainerID)
	if err != nil {
		return r.fail(ctx, snap, fmt.Sprintf("reading the container's OCI spec: %v", err))
	}
	// The second is the overlay upperdir — where to find the container's
	// writable layer. The archive itself is written after the dump (it is
	// megabytes and the container is quiesced either way), but the path can
	// only be resolved now.
	upperDir, err := restore.OverlayUpperDir(sandbox.KeeperPID)
	if err != nil {
		// Not fatal on its own: a node whose snapshotter is not overlayfs has
		// no upperdir, and the restore reuses the keeper's rootfs unchanged.
		logger.Info("could not resolve the container's writable layer; the artifact will carry no rootfs diff",
			"pid", sandbox.KeeperPID, "err", err)
	}

	dstDir := uri.HostPath(r.FuseMount)
	imageDir := filepath.Join(dstDir, "checkpoint")
	workDir := filepath.Join(r.WorkRoot, "checkpoints", string(snap.UID))

	// A retry must not inherit a half-written tree: CRIU appends to whatever
	// image files it finds, and a stale pages-*.img from a failed dump is
	// indistinguishable from a good one once the MANIFEST is written.
	if err := os.RemoveAll(imageDir); err != nil {
		return ctrl.Result{}, err
	}
	if err := os.RemoveAll(workDir); err != nil {
		return ctrl.Result{}, err
	}

	timeout := time.Duration(snap.Spec.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	dumpCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	logger.Info("running runc checkpoint",
		"container", sandbox.KeeperContainerID, "imagePath", imageDir, "timeout", timeout)
	started := metav1.Now()
	err = r.Runc.Checkpoint(dumpCtx, restore.CheckpointOpts{
		ContainerID: sandbox.KeeperContainerID,
		ImagePath:   imageDir,
		WorkPath:    workDir,
		// The workload is parked in its quiesce poll loop and the build pod
		// is torn down straight after; leaving it stopped is both what the
		// kubelet path does and what makes the dump consistent.
		LeaveRunning: false,
		Env:          criuDumpTuning(snap),
	})
	if err != nil {
		return r.fail(ctx, snap, fmt.Sprintf("runc checkpoint: %v", err))
	}
	logger.Info("dump complete", "elapsed", time.Since(started.Time).Round(time.Second))

	// CRIU's dump.log is a restore input, not a diagnostic: the bundle reads
	// the mount table out of it to reconstruct the container's extra mounts
	// and NVIDIA hook paths. Copy it in beside the images.
	if err := copyInto(filepath.Join(workDir, restore.CRIUDumpLogName), filepath.Join(dstDir, "dump.log")); err != nil {
		return r.fail(ctx, snap, fmt.Sprintf("copying dump.log into the artifact: %v", err))
	}

	var q *artifact.QuiesceInfo
	if snap.Status.Quiesce != nil {
		q = &artifact.QuiesceInfo{Mode: snap.Status.Quiesce.Mode, Dir: snap.Status.Quiesce.Dir}
	}
	m, err := artifact.PublishDir(artifact.PublishDirOptions{
		Dir:        dstDir,
		Meta:       checkpointMeta(snap, sandbox.KeeperContainerID, started),
		Spec:       spec,
		Quiesce:    q,
		Digest:     r.Digest,
		Contribute: r.contribute(upperDir),
	})
	if err != nil {
		return r.fail(ctx, snap, fmt.Sprintf("publishing the artifact: %v", err))
	}

	// Scratch only — the logs are already in the artifact.
	if err := os.RemoveAll(workDir); err != nil {
		logger.Info("could not remove CRIU work dir", "path", workDir, "err", err)
	}

	now := metav1.Now()
	snap.Status.Artifact.Format = uri.Format()
	snap.Status.Artifact.SizeBytes = m.TotalBytes
	snap.Status.Artifact.FileCount = int32(len(m.Files)) // #nosec G115 -- CRIU image counts are small
	snap.Status.Artifact.CreatedAt = now
	snap.Status.Phase = snapv1.SnapshotPhaseCompleted
	snap.Status.Message = ""
	snap.Status.CompletedAt = &now
	setCond(&snap.Status.Conditions, snapv1.ConditionCheckpointCreated, metav1.ConditionTrue, "CheckpointCreated", imageDir)
	setCond(&snap.Status.Conditions, snapv1.ConditionArtifactUploaded, metav1.ConditionTrue, "Published", uri.String())
	setCond(&snap.Status.Conditions, snapv1.ConditionReady, metav1.ConditionTrue, "Completed", "")
	return ctrl.Result{}, r.Status().Update(ctx, snap)
}

// contribute adds the two members that are pod-scoped rather than
// CRIU-scoped, and so travel with the artifact or not at all: the container's
// /dev/shm (CRIU's link-remap targets live there, and the tmpfs dies with the
// pod) and its writable layer. On the kubelet path both arrive inside the tar.
func (r *CheckpointReconciler) contribute(upperDir string) func(string) ([]artifact.ManifestFile, error) {
	return func(dstDir string) ([]artifact.ManifestFile, error) {
		var files []artifact.ManifestFile

		captured, err := restore.CaptureShm(
			filepath.Join(dstDir, "spec.dump"),
			r.HostRoot,
			filepath.Join(dstDir, restore.ShmDiffName),
		)
		if err != nil {
			return nil, fmt.Errorf("capturing %s: %w", restore.ShmPath, err)
		}
		if captured {
			entry, err := artifact.DescribeFile(dstDir, restore.ShmDiffName)
			if err != nil {
				return nil, err
			}
			files = append(files, entry)
		}

		captured, err = restore.CaptureRootfsDiff(
			upperDir, r.HostRoot, filepath.Join(dstDir, restore.RootfsDiffName))
		if err != nil {
			return nil, fmt.Errorf("capturing the container's writable layer: %w", err)
		}
		if captured {
			entry, err := artifact.DescribeFile(dstDir, restore.RootfsDiffName)
			if err != nil {
				return nil, err
			}
			files = append(files, entry)
		}
		return files, nil
	}
}

// checkpointMeta reconstructs the config.dump the CRI archive would have
// carried. The restore reads two things out of it — the container id and the
// pod UID encoded in the CRI name — so those two are what must be right.
func checkpointMeta(snap *snapv1.PodSnapshot, containerID string, at metav1.Time) *artifact.CheckpointMeta {
	return &artifact.CheckpointMeta{
		ID: containerID,
		// containerd's CRI name shape, which restore.OldPodUID parses:
		// <container>_<pod>_<namespace>_<podUID>_<attempt>
		Name: fmt.Sprintf("%s_%s_%s_%s_0",
			snap.Status.Container, snap.Spec.PodName, snap.Namespace, snap.Status.PodUID),
		Runtime:        "io.containerd.runc.v2",
		CheckpointedAt: at.Time,
	}
}

// readTaskSpec reads the live OCI spec from the container's containerd task
// bundle. This is the same config.json runc itself was handed, which is what
// makes it the right thing to record as spec.dump.
func readTaskSpec(containerID string) (json.RawMessage, error) {
	if containerID == "" {
		return nil, fmt.Errorf("no container id")
	}
	path := filepath.Join(
		"/run/containerd/io.containerd.runtime.v2.task/k8s.io", containerID, "config.json")
	raw, err := os.ReadFile(path) // #nosec G304 -- path built from a CRI-issued container id
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	if !json.Valid(raw) {
		return nil, fmt.Errorf("%s is not valid JSON", path)
	}
	return json.RawMessage(raw), nil
}

// copyInto copies src to dst. Used for dump.log, which is megabytes, not
// gigabytes — the image files are never copied anywhere.
func copyInto(src, dst string) error {
	raw, err := os.ReadFile(src) // #nosec G304 -- path built from our own work dir
	if err != nil {
		return err
	}
	return os.WriteFile(dst, raw, 0o644)
}

// criuDumpTuning turns podsnapshot.io/criu-dump-env-<var> annotations into
// environment for the dump: the suffix, uppercased with dashes as
// underscores, is the variable name.
//
// Deliberately a passthrough rather than the named map criuTuning uses on the
// restore side. Every knob our CRIU fork adds is read-path only, so there is
// no dump-side equivalent to name -- and naming a fixed set here would imply
// the dump has tunables it does not have. What this is for is reaching a
// stock CRIU variable during an experiment without shipping an agent.
func criuDumpTuning(snap *snapv1.PodSnapshot) map[string]string {
	env := map[string]string{}
	for k, v := range snap.Annotations {
		name, ok := strings.CutPrefix(k, "podsnapshot.io/criu-dump-env-")
		if !ok || name == "" || v == "" {
			continue
		}
		env[strings.ToUpper(strings.ReplaceAll(name, "-", "_"))] = v
	}
	if len(env) == 0 {
		return nil
	}
	return env
}

func (r *CheckpointReconciler) retry(ctx context.Context, snap *snapv1.PodSnapshot, msg string) (ctrl.Result, error) {
	snap.Status.Message = msg
	if err := r.Status().Update(ctx, snap); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
}

func (r *CheckpointReconciler) fail(ctx context.Context, snap *snapv1.PodSnapshot, msg string) (ctrl.Result, error) {
	snap.Status.Phase = snapv1.SnapshotPhaseFailed
	snap.Status.Message = msg
	setCond(&snap.Status.Conditions, snapv1.ConditionCheckpointCreated, metav1.ConditionFalse, "CheckpointFailed", msg)
	setCond(&snap.Status.Conditions, snapv1.ConditionReady, metav1.ConditionFalse, "Failed", msg)
	return ctrl.Result{}, r.Status().Update(ctx, snap)
}

// SetupWithManager registers the controller, narrowed to snapshots this node
// must dump itself.
func (r *CheckpointReconciler) SetupWithManager(mgr ctrl.Manager) error {
	mine := predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool { return r.ownsObj(e.Object) },
		UpdateFunc: func(e event.UpdateEvent) bool { return r.ownsObj(e.ObjectNew) },
		DeleteFunc: func(event.DeleteEvent) bool { return false },
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&snapv1.PodSnapshot{}).
		WithEventFilter(mine).
		Named("agent-checkpoint").
		Complete(r)
}

func (r *CheckpointReconciler) ownsObj(obj client.Object) bool {
	snap, ok := obj.(*snapv1.PodSnapshot)
	return ok && r.owns(snap)
}

func (r *CheckpointReconciler) owns(snap *snapv1.PodSnapshot) bool {
	return snap.Status.NodeName == r.NodeName &&
		snap.Status.Phase == snapv1.SnapshotPhaseCheckpointing &&
		snap.Spec.Checkpointer == snapv1.CheckpointerAgent &&
		snap.Status.Artifact != nil && snap.Status.Artifact.URI != ""
}
