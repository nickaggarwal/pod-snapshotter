package agent

import (
	"context"
	"fmt"
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
)

// quiescePollInterval is how often the rendezvous directory is checked. The
// wait is bounded by the PodSnapshot's quiesce deadline, enforced by the
// manager.
const quiescePollInterval = 2 * time.Second

// QuiesceReconciler owns the Quiescing phase for PodSnapshots on this node:
// it waits for the workload shim to write <quiesce-dir>/ready-for-checkpoint
// and only then hands the snapshot back to the manager for the actual
// checkpoint call (docs/design-v2.md §4).
//
// The rendezvous directory is an in-container path, so it is read through
// the container init's mount namespace (/proc/<pid>/root/...) rather than
// through the host filesystem: an emptyDir is a mount inside the container
// and is not visible under the container's bundle rootfs. This requires the
// agent's hostPID: true.
type QuiesceReconciler struct {
	client.Client
	NodeName string
	Resolver SandboxResolver
}

// +kubebuilder:rbac:groups=podsnapshot.io,resources=podsnapshots,verbs=get;list;watch
// +kubebuilder:rbac:groups=podsnapshot.io,resources=podsnapshots/status,verbs=get;update;patch

func (r *QuiesceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var snap snapv1.PodSnapshot
	if err := r.Get(ctx, req.NamespacedName, &snap); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if snap.Status.NodeName != r.NodeName || snap.Status.Phase != snapv1.SnapshotPhaseQuiescing {
		return ctrl.Result{}, nil
	}
	q := snap.Status.Quiesce
	if q == nil || q.Dir == "" {
		return ctrl.Result{}, nil
	}
	// The manager fails the snapshot once the deadline passes; stop polling.
	if q.Deadline != nil && time.Now().After(q.Deadline.Time) {
		return ctrl.Result{}, nil
	}

	sandbox, err := r.Resolver.Resolve(ctx, snap.Status.PodUID, snap.Status.Container)
	if err != nil {
		logger.V(1).Info("resolving snapshot target container; retrying", "err", err)
		return ctrl.Result{RequeueAfter: quiescePollInterval}, nil
	}
	if sandbox.KeeperPID <= 0 {
		return ctrl.Result{RequeueAfter: quiescePollInterval}, nil
	}

	ready := containerPath(sandbox.KeeperPID, q.Dir, snapv1.ReadyForCheckpointFile)
	if _, err := os.Stat(ready); err != nil {
		return ctrl.Result{RequeueAfter: quiescePollInterval}, nil
	}

	logger.Info("workload reported quiesce-ready", "snapshot", req.NamespacedName, "file", ready)
	now := metav1.Now()
	snap.Status.Quiesce.ReadyAt = &now
	snap.Status.Phase = snapv1.SnapshotPhaseCheckpointing
	snap.Status.Message = "workload quiesced; calling kubelet checkpoint API"
	setCond(&snap.Status.Conditions, snapv1.ConditionQuiesced, metav1.ConditionTrue, "Quiesced",
		fmt.Sprintf("%s/%s present", q.Dir, snapv1.ReadyForCheckpointFile))
	return ctrl.Result{}, r.Status().Update(ctx, &snap)
}

// containerPath resolves an in-container path through a process's mount
// namespace as seen from the host's /proc.
func containerPath(pid int, elems ...string) string {
	return filepath.Join(append([]string{fmt.Sprintf("/proc/%d/root", pid)}, elems...)...)
}

// SetupWithManager registers the controller, narrowed to snapshots quiescing
// on this node.
func (r *QuiesceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	mine := predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool { return r.owns(e.Object) },
		UpdateFunc: func(e event.UpdateEvent) bool { return r.owns(e.ObjectNew) },
		DeleteFunc: func(event.DeleteEvent) bool { return false },
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&snapv1.PodSnapshot{}).
		WithEventFilter(mine).
		Named("agent-quiesce").
		Complete(r)
}

func (r *QuiesceReconciler) owns(obj client.Object) bool {
	snap, ok := obj.(*snapv1.PodSnapshot)
	if !ok {
		return false
	}
	return snap.Status.NodeName == r.NodeName && snap.Status.Phase == snapv1.SnapshotPhaseQuiescing
}
