package agent

import (
	"context"
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

	snapv1 "pod-snapshotter/api/v1alpha1"
	"pod-snapshotter/internal/artifact"
	"pod-snapshotter/internal/cri"
	"pod-snapshotter/internal/restore"
)

// SandboxResolver abstracts CRI lookups (testable without a runtime).
type SandboxResolver interface {
	Resolve(ctx context.Context, podUID, keeperContainerName string) (*cri.SandboxInfo, error)
}

// Pinner abstracts the fuse-client session pin API.
type Pinner interface {
	Pin(ctx context.Context, volumeID, rootPath string) error
	Unpin(ctx context.Context, volumeID string) error
}

// RestoreReconciler is the node-local half of PodRestore: it owns the
// PreWarming and Restoring phases (for restores targeting this node) and the
// teardown of restored runc containers.
type RestoreReconciler struct {
	client.Client
	NodeName  string
	FuseMount string
	// WorkRoot is node-local scratch for bundles (default
	// /var/lib/pod-snapshotter/restores).
	WorkRoot string
	// HostRoot is where the host's / is mounted (read-only) here.
	HostRoot string

	Resolver SandboxResolver
	Runc     restore.RuncRunner
	// Pinner is optional; restores proceed unpinned when nil or on error.
	Pinner Pinner
}

// +kubebuilder:rbac:groups=podsnapshot.io,resources=podrestores,verbs=get;list;watch
// +kubebuilder:rbac:groups=podsnapshot.io,resources=podrestores/status,verbs=get;update;patch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch

func (r *RestoreReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var pr snapv1.PodRestore
	if err := r.Get(ctx, req.NamespacedName, &pr); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Teardown on deletion, regardless of phase.
	if !pr.DeletionTimestamp.IsZero() {
		return r.teardown(ctx, &pr)
	}

	if pr.Status.TargetNode != r.NodeName {
		return ctrl.Result{}, nil
	}

	switch pr.Status.Phase {
	case snapv1.RestorePhasePreWarming:
		return r.prewarm(ctx, &pr)
	case snapv1.RestorePhaseRestoring:
		return r.restore(ctx, &pr)
	case snapv1.RestorePhaseRunning:
		return r.checkAlive(ctx, &pr)
	default:
		return ctrl.Result{}, nil
	}
}

// prewarm streams the artifact through the FUSE mount so its bytes land in
// the node's NVMe tier (fuse-client promotes read-through misses), and pins
// the artifact prefix against eviction for the duration of the restore.
func (r *RestoreReconciler) prewarm(ctx context.Context, pr *snapv1.PodRestore) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	uri, err := artifact.Parse(pr.Status.ArtifactURI)
	if err != nil {
		return r.fail(ctx, pr, err.Error())
	}

	if r.Pinner != nil && boolOrTrue(pr.Spec.Pin) && uri.Scheme == artifact.SchemeFuse {
		rootPath := filepath.Dir(uri.Path)
		if err := r.Pinner.Pin(ctx, pinVolumeID(pr), rootPath); err != nil {
			logger.Info("pinning failed; continuing unpinned", "err", err)
		}
	}

	hostPath := uri.HostPath(r.FuseMount)
	f, err := os.Open(hostPath)
	if err != nil {
		if os.IsNotExist(err) {
			return r.retryTransient(ctx, pr, fmt.Sprintf("artifact %s not yet visible on node %s; retrying", uri.String(), r.NodeName))
		}
		return ctrl.Result{}, err
	}
	defer f.Close()

	// Sequential read in large chunks: each miss is fetched from peers/cloud
	// and promoted to local NVMe by fuse-client.
	buf := make([]byte, 8<<20)
	n, err := io.CopyBuffer(io.Discard, f, buf)
	if err != nil {
		return r.fail(ctx, pr, fmt.Sprintf("pre-warm read failed: %v", err))
	}

	pr.Status.PrewarmBytes = n
	setCond(&pr.Status.Conditions, snapv1.ConditionPreWarmed, metav1.ConditionTrue, "PreWarmed", fmt.Sprintf("%d bytes", n))
	// Hand back to the manager: if the placeholder pod doesn't exist yet the
	// manager creates it (Preparing); if it does, we move straight to
	// Restoring on the next agent pass.
	if pr.Status.PodUID == "" {
		pr.Status.Phase = snapv1.RestorePhasePreparing
		pr.Status.Message = "pre-warm complete; preparing placeholder pod"
	} else {
		pr.Status.Phase = snapv1.RestorePhaseRestoring
		pr.Status.Message = "pre-warm complete; restoring"
	}
	return ctrl.Result{}, r.Status().Update(ctx, pr)
}

// restore performs: resolve sandbox -> unpack tar -> rewrite spec ->
// runc restore joining the sandbox -> record PID.
func (r *RestoreReconciler) restore(ctx context.Context, pr *snapv1.PodRestore) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	if pr.Status.RestoredContainerID != "" {
		// Already restored (idempotency after agent restart): verify below.
		return r.checkAlive(ctx, pr)
	}

	uri, err := artifact.Parse(pr.Status.ArtifactURI)
	if err != nil {
		return r.fail(ctx, pr, err.Error())
	}
	tarPath := uri.HostPath(r.FuseMount)
	if _, err := os.Stat(tarPath); err != nil {
		return r.retryTransient(ctx, pr, fmt.Sprintf("artifact not readable at %s: %v; retrying", tarPath, err))
	}

	keeperName := pr.Spec.Container
	if keeperName == "" && len(pr.Spec.PodTemplate.Spec.Containers) > 0 {
		keeperName = pr.Spec.PodTemplate.Spec.Containers[0].Name
	}
	sandbox, err := r.Resolver.Resolve(ctx, pr.Status.PodUID, keeperName)
	if err != nil {
		return r.retryTransient(ctx, pr, fmt.Sprintf("resolving sandbox: %v; retrying", err))
	}

	// The keeper (and with it /proc/<pid>/root) can vanish between resolve
	// and unpack if the placeholder pod is replaced; that's transient.
	if sandbox.KeeperRootfs != "" {
		if _, err := os.Stat(sandbox.KeeperRootfs); err != nil {
			return r.retryTransient(ctx, pr, fmt.Sprintf("keeper rootfs %s not ready: %v; retrying", sandbox.KeeperRootfs, err))
		}
	}

	workDir := filepath.Join(r.WorkRoot, string(pr.UID))
	// Clean any partial state from a prior attempt before unpacking.
	_ = os.RemoveAll(workDir)
	// The keeper runs the same image as the checkpointed container; its live
	// rootfs (with rootfs-diff.tar applied) is the restored workload's root.
	bundle, err := restore.Unpack(tarPath, workDir, sandbox.KeeperRootfs)
	if err != nil {
		return r.fail(ctx, pr, fmt.Sprintf("unpacking checkpoint: %v", err))
	}

	oldPodUID := restore.OldPodUID(bundle.ConfigDump)

	if len(bundle.NvidiaHookMounts) > 0 {
		if err := restore.PrepareHookMountpoints(sandbox.KeeperRootfs, bundle.NvidiaHookMounts); err != nil {
			return r.fail(ctx, pr, fmt.Sprintf("preparing hook mountpoints: %v", err))
		}
	}

	// Place the restored container under the placeholder POD's cgroup so it
	// inherits the pod's limits. An absolute cgroupfs path is required — a
	// relative/systemd-style one makes runc nest under the agent's cgroup,
	// whose small memory limit OOM-kills CRIU mid-restore.
	cgPath := sandbox.PodCgroupPath + "/snap-" + pr.Name

	spec, err := restore.RewriteSpec(
		filepath.Join(workDir, "spec.dump"),
		bundle.SpecPath,
		restore.SandboxTarget{
			PausePID:     sandbox.PausePID,
			PodUID:       pr.Status.PodUID,
			OldPodUID:    oldPodUID,
			CgroupsPath:  cgPath,
			RootfsPath:   sandbox.KeeperRootfs,
			SandboxID:    sandbox.SandboxID,
			KeeperMounts: sandbox.KeeperMounts,
			ExtraMounts:  bundle.ExtraMounts,
			HostRoot:     r.HostRoot,
		},
	)
	if err != nil {
		return r.fail(ctx, pr, fmt.Sprintf("rewriting spec: %v", err))
	}
	if err := restore.ValidateGPUDevices(spec); err != nil {
		return r.fail(ctx, pr, err.Error())
	}

	cid := "snap-" + string(pr.UID)
	tcpClose := pr.Annotations["podsnapshot.io/tcp-close"] == "true"
	logger.Info("invoking runc restore", "container", cid, "bundle", filepath.Dir(bundle.SpecPath))
	pid, err := r.Runc.Restore(ctx, restore.RestoreOpts{
		ContainerID: cid,
		BundleDir:   filepath.Dir(bundle.SpecPath),
		ImagePath:   bundle.CheckpointDir,
		WorkPath:    filepath.Join(workDir, "criu-work"),
		TCPClose:    tcpClose,
	})
	if err != nil {
		return r.fail(ctx, pr, fmt.Sprintf("runc restore: %v", err))
	}

	now := metav1.Now()
	pr.Status.RestoredContainerID = cid
	pr.Status.RestoredPID = int32(pid) // #nosec G115 -- PIDs fit in int32
	pr.Status.Phase = snapv1.RestorePhaseRunning
	pr.Status.Message = ""
	pr.Status.RestoredAt = &now
	setCond(&pr.Status.Conditions, snapv1.ConditionRestored, metav1.ConditionTrue, "Restored", fmt.Sprintf("pid %d", pid))
	return ctrl.Result{}, r.Status().Update(ctx, pr)
}

// checkAlive verifies the restored container still runs; flags Failed if it
// died (the keeper pod may still look healthy).
func (r *RestoreReconciler) checkAlive(ctx context.Context, pr *snapv1.PodRestore) (ctrl.Result, error) {
	if pr.Status.RestoredContainerID == "" {
		return ctrl.Result{}, nil
	}
	running, err := r.Runc.State(ctx, pr.Status.RestoredContainerID)
	if err != nil {
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}
	if !running {
		return r.fail(ctx, pr, "restored container is no longer running")
	}
	return ctrl.Result{RequeueAfter: 60 * time.Second}, nil
}

// teardown kills the restored container, cleans scratch, unpins, and acks
// via the TornDown condition so the manager can release the finalizer.
func (r *RestoreReconciler) teardown(ctx context.Context, pr *snapv1.PodRestore) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	if pr.Status.TargetNode != r.NodeName {
		return ctrl.Result{}, nil
	}
	if pr.Status.RestoredContainerID != "" {
		if err := r.Runc.Kill(ctx, pr.Status.RestoredContainerID); err != nil {
			logger.Error(err, "killing restored container; will retry")
			return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}
	}
	if r.Pinner != nil {
		if err := r.Pinner.Unpin(ctx, pinVolumeID(pr)); err != nil {
			logger.Info("unpin failed (session may not exist)", "err", err)
		}
	}
	if r.WorkRoot != "" {
		_ = os.RemoveAll(filepath.Join(r.WorkRoot, string(pr.UID)))
	}
	setCond(&pr.Status.Conditions, snapv1.ConditionTornDown, metav1.ConditionTrue, "TornDown", "")
	return ctrl.Result{}, r.Status().Update(ctx, pr)
}

// retryTransient records msg on the status and requeues shortly — for
// conditions expected to resolve on their own (artifact propagation, keeper
// pod scheduling).
func (r *RestoreReconciler) retryTransient(ctx context.Context, pr *snapv1.PodRestore, msg string) (ctrl.Result, error) {
	pr.Status.Message = msg
	if err := r.Status().Update(ctx, pr); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
}

func (r *RestoreReconciler) fail(ctx context.Context, pr *snapv1.PodRestore, msg string) (ctrl.Result, error) {
	pr.Status.Phase = snapv1.RestorePhaseFailed
	pr.Status.Message = msg
	setCond(&pr.Status.Conditions, snapv1.ConditionRestored, metav1.ConditionFalse, "Failed", msg)
	return ctrl.Result{}, r.Status().Update(ctx, pr)
}

func pinVolumeID(pr *snapv1.PodRestore) string { return "podrestore-" + string(pr.UID) }

func boolOrTrue(b *bool) bool { return b == nil || *b }

// SetupWithManager registers the controller, filtering to restores this node
// is (or may become) responsible for.
func (r *RestoreReconciler) SetupWithManager(mgr ctrl.Manager) error {
	mine := predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool { return r.relevant(e.Object) },
		UpdateFunc: func(e event.UpdateEvent) bool { return r.relevant(e.ObjectNew) },
		DeleteFunc: func(event.DeleteEvent) bool { return false },
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&snapv1.PodRestore{}).
		WithEventFilter(mine).
		Named("agent-restore").
		Complete(r)
}

func (r *RestoreReconciler) relevant(obj client.Object) bool {
	pr, ok := obj.(*snapv1.PodRestore)
	if !ok {
		return false
	}
	return pr.Status.TargetNode == r.NodeName
}
