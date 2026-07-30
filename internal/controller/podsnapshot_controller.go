package controller

import (
	"context"
	"fmt"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	snapv1 "pod-snapshotter/api/v1alpha1"
	"pod-snapshotter/internal/artifact"
)

// KubeletCheckpointer abstracts the kubelet checkpoint call.
type KubeletCheckpointer interface {
	Checkpoint(ctx context.Context, nodeAddress, namespace, pod, container string, timeout time.Duration) (string, error)
}

// ArtifactDeleter deletes an artifact by URI (used for DeletionPolicy=Delete).
type ArtifactDeleter interface {
	Delete(ctx context.Context, uri artifact.URI) error
}

// PodSnapshotReconciler drives PodSnapshot through
// Pending -> Checkpointing -> Checkpointed. The node agent takes over from
// Checkpointed (upload to the fuse mount) through Completed.
type PodSnapshotReconciler struct {
	client.Client
	Kubelet KubeletCheckpointer
	// Artifacts is optional; when nil, DeletionPolicy=Delete only removes the
	// finalizer without deleting the artifact (logged).
	Artifacts ArtifactDeleter

	// RequirePrereqs gates checkpointing on the node prereq annotation set by
	// the agent. Disable in tests / CPU-only trials.
	RequirePrereqs bool

	// inflight tracks running checkpoint calls keyed by namespaced name, so a
	// reconcile re-entry does not launch a duplicate kubelet call.
	inflight sync.Map
}

// +kubebuilder:rbac:groups=podsnapshot.io,resources=podsnapshots,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=podsnapshot.io,resources=podsnapshots/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=podsnapshot.io,resources=podsnapshots/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=nodes/proxy,verbs=get;create
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

func (r *PodSnapshotReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var snap snapv1.PodSnapshot
	if err := r.Get(ctx, req.NamespacedName, &snap); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Deletion: honor DeletionPolicy=Delete via finalizer.
	if !snap.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, &snap)
	}

	// Ensure finalizer early when artifacts must be cleaned up.
	if snap.Spec.DeletionPolicy == snapv1.DeletionPolicyDelete &&
		controllerutil.AddFinalizer(&snap, snapv1.ArtifactCleanupFinalizer) {
		if err := r.Update(ctx, &snap); err != nil {
			return ctrl.Result{}, err
		}
	}

	switch snap.Status.Phase {
	case "":
		snap.Status.Phase = snapv1.SnapshotPhasePending
		snap.Status.StartedAt = ptrTime(metav1.Now())
		return ctrl.Result{}, r.Status().Update(ctx, &snap)
	case snapv1.SnapshotPhasePending:
		return r.reconcilePending(ctx, &snap)
	case snapv1.SnapshotPhaseCheckpointing:
		return r.reconcileCheckpointing(ctx, &snap)
	case snapv1.SnapshotPhaseCheckpointed, snapv1.SnapshotPhaseUploading:
		// Agent-owned phases; nothing for the manager to do.
		return ctrl.Result{}, nil
	case snapv1.SnapshotPhaseCompleted, snapv1.SnapshotPhaseFailed:
		return ctrl.Result{}, nil
	default:
		logger.Info("unknown phase", "phase", snap.Status.Phase)
		return ctrl.Result{}, nil
	}
}

func (r *PodSnapshotReconciler) reconcilePending(ctx context.Context, snap *snapv1.PodSnapshot) (ctrl.Result, error) {
	// Resolve target pod.
	var pod corev1.Pod
	if err := r.Get(ctx, types.NamespacedName{Namespace: snap.Namespace, Name: snap.Spec.PodName}, &pod); err != nil {
		if apierrors.IsNotFound(err) {
			return r.fail(ctx, snap, fmt.Sprintf("pod %s not found", snap.Spec.PodName))
		}
		return ctrl.Result{}, err
	}
	if pod.Status.Phase != corev1.PodRunning {
		snap.Status.Message = fmt.Sprintf("pod %s is %s, waiting for Running", pod.Name, pod.Status.Phase)
		if err := r.Status().Update(ctx, snap); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	// Resolve container.
	container := snap.Spec.Container
	if container == "" {
		if len(pod.Spec.Containers) != 1 {
			return r.fail(ctx, snap, fmt.Sprintf("pod %s has %d containers; spec.container is required", pod.Name, len(pod.Spec.Containers)))
		}
		container = pod.Spec.Containers[0].Name
	} else {
		found := false
		for _, c := range pod.Spec.Containers {
			if c.Name == container {
				found = true
				break
			}
		}
		if !found {
			return r.fail(ctx, snap, fmt.Sprintf("container %q not found in pod %s", container, pod.Name))
		}
	}

	// Check node prerequisites (annotation maintained by the agent).
	if r.RequirePrereqs {
		var node corev1.Node
		if err := r.Get(ctx, types.NamespacedName{Name: pod.Spec.NodeName}, &node); err != nil {
			return ctrl.Result{}, err
		}
		if v := node.Annotations[snapv1.PrereqsAnnotation]; v != "ok" {
			setCondition(&snap.Status.Conditions, snapv1.ConditionNodeReady, metav1.ConditionFalse,
				"PrereqsNotMet", fmt.Sprintf("node %s prereqs: %s", pod.Spec.NodeName, orUnset(v)))
			snap.Status.Message = fmt.Sprintf("waiting for node %s prerequisites (annotation %s)", pod.Spec.NodeName, snapv1.PrereqsAnnotation)
			if err := r.Status().Update(ctx, snap); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
		}
	}
	setCondition(&snap.Status.Conditions, snapv1.ConditionNodeReady, metav1.ConditionTrue, "PrereqsMet", "")

	// Default artifact URI and validate it.
	uri := snap.Spec.ArtifactURI
	if uri == "" {
		uri = artifact.DefaultURI(snap.Namespace, snap.Name, container)
	}
	if _, err := artifact.Parse(uri); err != nil {
		return r.fail(ctx, snap, err.Error())
	}

	snap.Status.NodeName = pod.Spec.NodeName
	snap.Status.PodUID = string(pod.UID)
	snap.Status.Container = container
	if snap.Status.Artifact == nil {
		snap.Status.Artifact = &snapv1.ArtifactStatus{URI: uri}
	}
	snap.Status.Phase = snapv1.SnapshotPhaseCheckpointing
	snap.Status.Message = "calling kubelet checkpoint API"
	return ctrl.Result{}, r.Status().Update(ctx, snap)
}

func (r *PodSnapshotReconciler) reconcileCheckpointing(ctx context.Context, snap *snapv1.PodSnapshot) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	key := types.NamespacedName{Namespace: snap.Namespace, Name: snap.Name}

	// A previous reconcile may already have a checkpoint call running.
	if _, running := r.inflight.LoadOrStore(key, struct{}{}); running {
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	// Look up the node's address for the kubelet call.
	var node corev1.Node
	if err := r.Get(ctx, types.NamespacedName{Name: snap.Status.NodeName}, &node); err != nil {
		r.inflight.Delete(key)
		return ctrl.Result{}, err
	}
	nodeAddr := nodeAddress(&node)
	if nodeAddr == "" {
		r.inflight.Delete(key)
		return r.fail(ctx, snap, fmt.Sprintf("node %s has no InternalIP or Hostname address", node.Name))
	}

	timeout := time.Duration(snap.Spec.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 120 * time.Second
	}

	// The kubelet call is synchronous and can take minutes for large VRAM
	// dumps; run it in a goroutine and patch status on completion so we don't
	// block the workqueue.
	go func() {
		defer r.inflight.Delete(key)
		callCtx, cancel := context.WithTimeout(context.Background(), timeout+30*time.Second)
		defer cancel()

		tarPath, err := r.Kubelet.Checkpoint(callCtx, nodeAddr, snap.Namespace, snap.Spec.PodName, snap.Status.Container, timeout)

		// Re-fetch and patch under optimistic concurrency.
		var latest snapv1.PodSnapshot
		if getErr := r.Get(callCtx, key, &latest); getErr != nil {
			logger.Error(getErr, "fetching PodSnapshot after checkpoint call")
			return
		}
		if latest.Status.Phase != snapv1.SnapshotPhaseCheckpointing {
			return // superseded
		}
		if err != nil {
			latest.Status.Phase = snapv1.SnapshotPhaseFailed
			latest.Status.Message = err.Error()
			setCondition(&latest.Status.Conditions, snapv1.ConditionCheckpointCreated, metav1.ConditionFalse, "CheckpointFailed", err.Error())
		} else {
			latest.Status.KubeletCheckpointPath = tarPath
			latest.Status.Phase = snapv1.SnapshotPhaseCheckpointed
			latest.Status.Message = "checkpoint tar written by kubelet; waiting for node agent upload"
			setCondition(&latest.Status.Conditions, snapv1.ConditionCheckpointCreated, metav1.ConditionTrue, "CheckpointCreated", tarPath)
		}
		if updErr := r.Status().Update(callCtx, &latest); updErr != nil {
			logger.Error(updErr, "updating PodSnapshot status after checkpoint call")
		}
	}()

	return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
}

func (r *PodSnapshotReconciler) reconcileDelete(ctx context.Context, snap *snapv1.PodSnapshot) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	if !controllerutil.ContainsFinalizer(snap, snapv1.ArtifactCleanupFinalizer) {
		return ctrl.Result{}, nil
	}
	if snap.Spec.DeletionPolicy == snapv1.DeletionPolicyDelete &&
		snap.Status.Artifact != nil && snap.Status.Artifact.URI != "" {
		uri, err := artifact.Parse(snap.Status.Artifact.URI)
		if err == nil {
			if r.Artifacts != nil {
				if err := r.Artifacts.Delete(ctx, uri); err != nil {
					logger.Error(err, "deleting artifact; will retry", "uri", uri.String())
					return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
				}
			} else {
				logger.Info("no artifact deleter configured; skipping artifact deletion", "uri", uri.String())
			}
		}
	}
	controllerutil.RemoveFinalizer(snap, snapv1.ArtifactCleanupFinalizer)
	return ctrl.Result{}, r.Update(ctx, snap)
}

func (r *PodSnapshotReconciler) fail(ctx context.Context, snap *snapv1.PodSnapshot, msg string) (ctrl.Result, error) {
	snap.Status.Phase = snapv1.SnapshotPhaseFailed
	snap.Status.Message = msg
	setCondition(&snap.Status.Conditions, snapv1.ConditionReady, metav1.ConditionFalse, "Failed", msg)
	return ctrl.Result{}, r.Status().Update(ctx, snap)
}

func nodeAddress(node *corev1.Node) string {
	var hostname string
	for _, a := range node.Status.Addresses {
		switch a.Type {
		case corev1.NodeInternalIP:
			return a.Address
		case corev1.NodeHostName:
			hostname = a.Address
		}
	}
	return hostname
}

func orUnset(s string) string {
	if s == "" {
		return "<unset>"
	}
	return s
}

func ptrTime(t metav1.Time) *metav1.Time { return &t }

// SetupWithManager registers the controller.
func (r *PodSnapshotReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&snapv1.PodSnapshot{}).
		Named("podsnapshot").
		Complete(r)
}
