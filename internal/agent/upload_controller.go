// Package agent contains the node-local controllers run by the
// pod-snapshotter DaemonSet: checkpoint tar upload, restore orchestration,
// and node prerequisite checking.
package agent

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"k8s.io/apimachinery/pkg/api/meta"

	snapv1 "pod-snapshotter/api/v1alpha1"
	"pod-snapshotter/internal/artifact"
)

// UploadReconciler moves kubelet checkpoint tars from
// /var/lib/kubelet/checkpoints to the artifact destination (the fuse-client
// mount for fuse:// URIs). It owns phases Checkpointed -> Uploading ->
// Completed for PodSnapshots on this node.
type UploadReconciler struct {
	client.Client
	NodeName string
	// FuseMount is the node's fuse-client mount point (default /mnt/fuse).
	FuseMount string
	// CheckpointsHostPath is where kubelet tars appear inside the agent
	// container (hostPath mount of /var/lib/kubelet/checkpoints).
	CheckpointsHostPath string
	// VerifyFuse optionally HEADs the uploaded file via the local fuse-client
	// API to confirm visibility; nil disables verification.
	VerifyFuse func(ctx context.Context, fusePath string) (int64, error)
}

// +kubebuilder:rbac:groups=podsnapshot.io,resources=podsnapshots,verbs=get;list;watch
// +kubebuilder:rbac:groups=podsnapshot.io,resources=podsnapshots/status,verbs=get;update;patch

func (r *UploadReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var snap snapv1.PodSnapshot
	if err := r.Get(ctx, req.NamespacedName, &snap); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if snap.Status.NodeName != r.NodeName {
		return ctrl.Result{}, nil
	}

	switch snap.Status.Phase {
	case snapv1.SnapshotPhaseCheckpointed:
		// Claim the upload (optimistic concurrency prevents double-claim).
		snap.Status.Phase = snapv1.SnapshotPhaseUploading
		snap.Status.Message = "agent uploading checkpoint tar to artifact store"
		if err := r.Status().Update(ctx, &snap); err != nil {
			return ctrl.Result{}, client.IgnoreNotFound(err)
		}
		return ctrl.Result{Requeue: true}, nil

	case snapv1.SnapshotPhaseUploading:
		return r.upload(ctx, &snap)

	default:
		_ = logger
		return ctrl.Result{}, nil
	}
}

func (r *UploadReconciler) upload(ctx context.Context, snap *snapv1.PodSnapshot) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	src := snap.Status.KubeletCheckpointPath
	if r.CheckpointsHostPath != "" {
		// Remap the kubelet path into our hostPath mount if they differ.
		src = filepath.Join(r.CheckpointsHostPath, filepath.Base(src))
	}

	uri, err := artifact.Parse(snap.Status.Artifact.URI)
	if err != nil {
		return r.fail(ctx, snap, err.Error())
	}
	dst := uri.HostPath(r.FuseMount)

	info, err := os.Stat(src)
	if err != nil {
		if os.IsNotExist(err) {
			return r.fail(ctx, snap, fmt.Sprintf("kubelet checkpoint tar %s not found on node %s (was it garbage-collected?)", src, r.NodeName))
		}
		return ctrl.Result{}, err
	}

	sum, n, err := copyAtomic(src, dst)
	if err != nil {
		snap.Status.Message = fmt.Sprintf("upload failed, will retry: %v", err)
		if uerr := r.Status().Update(ctx, snap); uerr != nil {
			return ctrl.Result{}, uerr
		}
		return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
	}

	// Confirm fuse-client sees the file (its writeback to the cloud tier is
	// queued from here).
	if uri.Scheme == artifact.SchemeFuse && r.VerifyFuse != nil {
		if _, err := r.VerifyFuse(ctx, uri.FusePath()); err != nil {
			logger.Info("fuse-client verification failed; artifact is on the mount but API HEAD failed", "err", err)
		}
	}

	// Free node disk; the artifact now lives on the distributed tier.
	if err := os.Remove(src); err != nil && !os.IsNotExist(err) {
		logger.Info("could not remove kubelet checkpoint tar", "path", src, "err", err)
	}

	now := metav1.Now()
	snap.Status.Artifact.SizeBytes = n
	snap.Status.Artifact.SHA256 = sum
	snap.Status.Artifact.CreatedAt = metav1.NewTime(info.ModTime())
	snap.Status.Phase = snapv1.SnapshotPhaseCompleted
	snap.Status.Message = ""
	snap.Status.CompletedAt = &now
	setCond(&snap.Status.Conditions, snapv1.ConditionArtifactUploaded, metav1.ConditionTrue, "Uploaded", uri.String())
	setCond(&snap.Status.Conditions, snapv1.ConditionReady, metav1.ConditionTrue, "Completed", "")
	return ctrl.Result{}, r.Status().Update(ctx, snap)
}

func (r *UploadReconciler) fail(ctx context.Context, snap *snapv1.PodSnapshot, msg string) (ctrl.Result, error) {
	snap.Status.Phase = snapv1.SnapshotPhaseFailed
	snap.Status.Message = msg
	setCond(&snap.Status.Conditions, snapv1.ConditionArtifactUploaded, metav1.ConditionFalse, "UploadFailed", msg)
	return ctrl.Result{}, r.Status().Update(ctx, snap)
}

// copyAtomic streams src to dst+".part" with a sha256 tee, fsyncs, then
// renames — an atomic publish on the destination filesystem. Checkpoint tars
// can be tens of GB (VRAM + process memory), so nothing is buffered whole.
func copyAtomic(src, dst string) (sha string, n int64, err error) {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return "", 0, err
	}
	in, err := os.Open(src)
	if err != nil {
		return "", 0, err
	}
	defer in.Close()

	tmp := dst + ".part"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return "", 0, err
	}
	defer func() {
		out.Close()
		if err != nil {
			os.Remove(tmp)
		}
	}()

	h := sha256.New()
	n, err = io.Copy(io.MultiWriter(out, h), in)
	if err != nil {
		return "", 0, err
	}
	if err = out.Sync(); err != nil {
		return "", 0, err
	}
	if err = out.Close(); err != nil {
		return "", 0, err
	}
	if err = os.Rename(tmp, dst); err != nil {
		return "", 0, err
	}
	return fmt.Sprintf("%x", h.Sum(nil)), n, nil
}

func setCond(conds *[]metav1.Condition, t string, s metav1.ConditionStatus, reason, msg string) {
	meta.SetStatusCondition(conds, metav1.Condition{Type: t, Status: s, Reason: reason, Message: msg})
}

// SetupWithManager registers the controller with a predicate narrowing to
// snapshots whose upload this node owns.
func (r *UploadReconciler) SetupWithManager(mgr ctrl.Manager) error {
	nodeOwned := predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool { return r.owns(e.Object) },
		UpdateFunc: func(e event.UpdateEvent) bool { return r.owns(e.ObjectNew) },
		DeleteFunc: func(event.DeleteEvent) bool { return false },
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&snapv1.PodSnapshot{}).
		WithEventFilter(nodeOwned).
		Named("agent-upload").
		Complete(r)
}

func (r *UploadReconciler) owns(obj client.Object) bool {
	snap, ok := obj.(*snapv1.PodSnapshot)
	if !ok {
		return false
	}
	return snap.Status.NodeName == r.NodeName &&
		(snap.Status.Phase == snapv1.SnapshotPhaseCheckpointed || snap.Status.Phase == snapv1.SnapshotPhaseUploading)
}
