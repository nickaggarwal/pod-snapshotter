package controller

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	snapv1 "pod-snapshotter/api/v1alpha1"
	"pod-snapshotter/internal/artifact"
)

// ArtifactStat checks that an artifact exists and returns its size.
type ArtifactStat interface {
	Stat(ctx context.Context, uri artifact.URI) (int64, error)
}

// PodRestoreReconciler drives PodRestore through
// Pending -> [PreWarming] -> Preparing -> Restoring. The node agent owns
// PreWarming/Restoring -> Running and teardown of the restored runc container.
type PodRestoreReconciler struct {
	client.Client
	// Artifacts verifies artifact existence; optional (skipped when nil, the
	// agent will fail the restore instead if the tar is missing).
	Artifacts ArtifactStat
}

// +kubebuilder:rbac:groups=podsnapshot.io,resources=podrestores,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=podsnapshot.io,resources=podrestores/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=podsnapshot.io,resources=podrestores/finalizers,verbs=update
// +kubebuilder:rbac:groups=podsnapshot.io,resources=podsnapshots,verbs=get;list;watch
// +kubebuilder:rbac:groups=podsnapshot.io,resources=snapshotbuilds,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

func (r *PodRestoreReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var restore snapv1.PodRestore
	if err := r.Get(ctx, req.NamespacedName, &restore); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !restore.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, &restore)
	}

	if controllerutil.AddFinalizer(&restore, snapv1.TeardownFinalizer) {
		if err := r.Update(ctx, &restore); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Global timeout check for non-terminal phases.
	if restore.Status.StartedAt != nil &&
		restore.Status.Phase != snapv1.RestorePhaseRunning &&
		restore.Status.Phase != snapv1.RestorePhaseFailed {
		timeout := time.Duration(restore.Spec.TimeoutSeconds) * time.Second
		if timeout <= 0 {
			timeout = 600 * time.Second
		}
		if time.Since(restore.Status.StartedAt.Time) > timeout {
			return r.fail(ctx, &restore, fmt.Sprintf("restore timed out after %s in phase %s", timeout, restore.Status.Phase))
		}
	}

	switch restore.Status.Phase {
	case "":
		restore.Status.Phase = snapv1.RestorePhasePending
		restore.Status.StartedAt = ptrTime(metav1.Now())
		return ctrl.Result{}, r.Status().Update(ctx, &restore)
	case snapv1.RestorePhasePending:
		return r.reconcilePending(ctx, &restore)
	case snapv1.RestorePhasePreparing:
		return r.reconcilePreparing(ctx, &restore)
	case snapv1.RestorePhasePreWarming, snapv1.RestorePhaseRestoring:
		// Agent-owned; requeue to enforce the timeout above.
		return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
	case snapv1.RestorePhaseRunning, snapv1.RestorePhaseFailed:
		return ctrl.Result{}, nil
	default:
		return ctrl.Result{}, nil
	}
}

func (r *PodRestoreReconciler) reconcilePending(ctx context.Context, restore *snapv1.PodRestore) (ctrl.Result, error) {
	// Resolve the artifact URI from whichever source was given.
	uriStr := restore.Spec.ArtifactURI
	switch {
	case uriStr != "":
	case restore.Spec.BuildRef != nil:
		var build snapv1.SnapshotBuild
		if err := r.Get(ctx, types.NamespacedName{Namespace: restore.Namespace, Name: restore.Spec.BuildRef.Name}, &build); err != nil {
			if apierrors.IsNotFound(err) {
				return r.fail(ctx, restore, fmt.Sprintf("buildRef %q not found", restore.Spec.BuildRef.Name))
			}
			return ctrl.Result{}, err
		}
		if build.Status.Phase != snapv1.BuildPhaseCompleted || build.Status.Artifact == nil {
			restore.Status.Message = fmt.Sprintf("waiting for SnapshotBuild %s to complete (phase %s)", build.Name, build.Status.Phase)
			if err := r.Status().Update(ctx, restore); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}
		uriStr = build.Status.Artifact.URI
		// Carrying the tuple onto the restore is what lets placement be
		// constrained before the pod is scheduled, instead of discovering the
		// mismatch inside runc restore (docs/design-v2.md §5).
		restore.Status.Compatibility = build.Status.Compatibility.DeepCopy()
	case restore.Spec.SnapshotRef != nil:
		var snap snapv1.PodSnapshot
		if err := r.Get(ctx, types.NamespacedName{Namespace: restore.Namespace, Name: restore.Spec.SnapshotRef.Name}, &snap); err != nil {
			if apierrors.IsNotFound(err) {
				return r.fail(ctx, restore, fmt.Sprintf("snapshotRef %q not found", restore.Spec.SnapshotRef.Name))
			}
			return ctrl.Result{}, err
		}
		if snap.Status.Phase != snapv1.SnapshotPhaseCompleted || snap.Status.Artifact == nil {
			restore.Status.Message = fmt.Sprintf("waiting for PodSnapshot %s to complete (phase %s)", snap.Name, snap.Status.Phase)
			if err := r.Status().Update(ctx, restore); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}
		uriStr = snap.Status.Artifact.URI
	default:
		return r.fail(ctx, restore, "one of spec.artifactURI, spec.snapshotRef or spec.buildRef is required")
	}

	uri, err := artifact.Parse(uriStr)
	if err != nil {
		return r.fail(ctx, restore, err.Error())
	}

	// Validate the pod template early.
	if _, err := targetContainerIndex(&restore.Spec); err != nil {
		return r.fail(ctx, restore, err.Error())
	}

	// file:// artifacts are node-local — the manager cannot see them, only
	// the agent on the target node can (it re-checks before restoring).
	if r.Artifacts != nil && uri.Scheme != artifact.SchemeFile {
		if _, err := r.Artifacts.Stat(ctx, uri); err != nil {
			setCondition(&restore.Status.Conditions, snapv1.ConditionArtifactAvailable, metav1.ConditionFalse, "NotFound", err.Error())
			restore.Status.Message = fmt.Sprintf("artifact not available yet: %v", err)
			if uerr := r.Status().Update(ctx, restore); uerr != nil {
				return ctrl.Result{}, uerr
			}
			return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
		}
	}
	setCondition(&restore.Status.Conditions, snapv1.ConditionArtifactAvailable, metav1.ConditionTrue, "Found", uri.String())

	restore.Status.ArtifactURI = uri.String()

	// If the node is pinned and pre-warm is requested, warm before creating
	// the placeholder pod (the GPU stays free while bytes stream in). With
	// scheduler-chosen nodes we must create the pod first to learn the node.
	// A pinned node bypasses the scheduler, so the compat label cannot do the
	// filtering — check it here instead.
	if restore.Spec.NodeName != "" {
		if handled, res, err := r.checkNodeCompatibility(ctx, restore, restore.Spec.NodeName); handled {
			return res, err
		}
	}

	if restore.Spec.NodeName != "" && boolOrTrue(restore.Spec.Prewarm) {
		restore.Status.TargetNode = restore.Spec.NodeName
		restore.Status.Phase = snapv1.RestorePhasePreWarming
		restore.Status.Message = "pre-warming artifact on target node"
	} else {
		restore.Status.Phase = snapv1.RestorePhasePreparing
	}
	return ctrl.Result{}, r.Status().Update(ctx, restore)
}

// checkNodeCompatibility fails the restore when the target node's environment
// tuple contradicts the one the artifact was built against. handled is false
// when there is nothing to check (no build reference, or the node has not
// reported a tuple), so the caller carries on.
//
// The label selector on the placeholder pod already keeps the scheduler off
// incompatible nodes; this turns the remaining cases — a pinned nodeName, an
// unlabeled node — into an explicit message instead of a CRIU failure.
func (r *PodRestoreReconciler) checkNodeCompatibility(ctx context.Context, restore *snapv1.PodRestore, nodeName string) (handled bool, res ctrl.Result, err error) {
	want := restore.Status.Compatibility
	if want == nil || nodeName == "" {
		return false, ctrl.Result{}, nil
	}
	var node corev1.Node
	if err := r.Get(ctx, types.NamespacedName{Name: nodeName}, &node); err != nil {
		return true, ctrl.Result{}, err
	}
	raw, ok := node.Annotations[snapv1.CompatibilityAnnotation]
	if !ok {
		return false, ctrl.Result{}, nil
	}
	if match, why := want.Matches(snapv1.ParseCompatibility(raw)); !match {
		res, err := r.fail(ctx, restore, fmt.Sprintf("node %s cannot restore this artifact: %s", nodeName, why))
		return true, res, err
	}
	return false, ctrl.Result{}, nil
}

func (r *PodRestoreReconciler) reconcilePreparing(ctx context.Context, restore *snapv1.PodRestore) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Create (or find) the placeholder pod.
	podName := restore.Status.PodName
	if podName == "" {
		podName = restore.Name + "-restored"
	}

	var pod corev1.Pod
	err := r.Get(ctx, types.NamespacedName{Namespace: restore.Namespace, Name: podName}, &pod)
	if apierrors.IsNotFound(err) {
		newPod, buildErr := BuildPlaceholderPod(restore, podName)
		if buildErr != nil {
			return r.fail(ctx, restore, buildErr.Error())
		}
		if err := controllerutil.SetControllerReference(restore, newPod, r.Scheme()); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.Create(ctx, newPod); err != nil {
			return ctrl.Result{}, err
		}
		logger.Info("created placeholder pod", "pod", podName)
		restore.Status.PodName = podName
		restore.Status.Message = "waiting for placeholder pod to run"
		setCondition(&restore.Status.Conditions, snapv1.ConditionPodPrepared, metav1.ConditionFalse, "Creating", "")
		if err := r.Status().Update(ctx, restore); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	} else if err != nil {
		return ctrl.Result{}, err
	}

	// A pod with our name may be a leftover from a previous PodRestore of
	// the same name (still terminating). Never adopt it: wait for it to go
	// away, then create our own.
	if !metav1.IsControlledBy(&pod, restore) || pod.DeletionTimestamp != nil {
		restore.Status.Message = fmt.Sprintf("waiting for stale placeholder pod %s to terminate", pod.Name)
		if err := r.Status().Update(ctx, restore); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	switch pod.Status.Phase {
	case corev1.PodRunning:
	case corev1.PodFailed:
		return r.fail(ctx, restore, fmt.Sprintf("placeholder pod %s failed: %s", pod.Name, pod.Status.Message))
	default:
		restore.Status.PodName = pod.Name
		restore.Status.Message = fmt.Sprintf("placeholder pod %s is %s", pod.Name, pod.Status.Phase)
		if err := r.Status().Update(ctx, restore); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	if handled, res, err := r.checkNodeCompatibility(ctx, restore, pod.Spec.NodeName); handled {
		return res, err
	}

	restore.Status.PodName = pod.Name
	restore.Status.PodUID = string(pod.UID)
	restore.Status.TargetNode = pod.Spec.NodeName
	setCondition(&restore.Status.Conditions, snapv1.ConditionPodPrepared, metav1.ConditionTrue, "Running", pod.Name)

	// Pre-warm now if the scheduler picked the node and it wasn't warmed yet.
	preWarmed := meta.IsStatusConditionTrue(restore.Status.Conditions, snapv1.ConditionPreWarmed)
	if boolOrTrue(restore.Spec.Prewarm) && !preWarmed {
		restore.Status.Phase = snapv1.RestorePhasePreWarming
		restore.Status.Message = "pre-warming artifact on target node"
	} else {
		restore.Status.Phase = snapv1.RestorePhaseRestoring
		restore.Status.Message = "node agent restoring workload into placeholder pod"
	}
	return ctrl.Result{}, r.Status().Update(ctx, restore)
}

func (r *PodRestoreReconciler) reconcileDelete(ctx context.Context, restore *snapv1.PodRestore) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(restore, snapv1.TeardownFinalizer) {
		return ctrl.Result{}, nil
	}

	// If a restore ever ran on a node, wait for the agent's teardown ack.
	needsAgentTeardown := restore.Status.RestoredContainerID != ""
	tornDown := meta.IsStatusConditionTrue(restore.Status.Conditions, snapv1.ConditionTornDown)
	if needsAgentTeardown && !tornDown {
		// Agent watches deletionTimestamp and performs runc kill/delete + unpin.
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	// Delete the placeholder pod (ownerRef would cover it, but be explicit).
	if restore.Status.PodName != "" {
		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: restore.Namespace, Name: restore.Status.PodName}}
		if err := r.Delete(ctx, pod); err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
	}

	controllerutil.RemoveFinalizer(restore, snapv1.TeardownFinalizer)
	return ctrl.Result{}, r.Update(ctx, restore)
}

func (r *PodRestoreReconciler) fail(ctx context.Context, restore *snapv1.PodRestore, msg string) (ctrl.Result, error) {
	restore.Status.Phase = snapv1.RestorePhaseFailed
	restore.Status.Message = msg
	setCondition(&restore.Status.Conditions, snapv1.ConditionRestored, metav1.ConditionFalse, "Failed", msg)
	return ctrl.Result{}, r.Status().Update(ctx, restore)
}

func boolOrTrue(b *bool) bool { return b == nil || *b }

// SetupWithManager registers the controller, also watching owned pods so pod
// phase changes trigger reconciles.
func (r *PodRestoreReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&snapv1.PodRestore{}).
		Owns(&corev1.Pod{}).
		Watches(&snapv1.PodSnapshot{}, handler.EnqueueRequestsFromMapFunc(r.restoresForSnapshot)).
		Watches(&snapv1.SnapshotBuild{}, handler.EnqueueRequestsFromMapFunc(r.restoresForBuild)).
		Named("podrestore").
		Complete(r)
}

// restoresForBuild requeues PodRestores whose buildRef matches an updated
// SnapshotBuild.
func (r *PodRestoreReconciler) restoresForBuild(ctx context.Context, obj client.Object) []reconcile.Request {
	build, ok := obj.(*snapv1.SnapshotBuild)
	if !ok {
		return nil
	}
	var list snapv1.PodRestoreList
	if err := r.List(ctx, &list, client.InNamespace(build.Namespace)); err != nil {
		return nil
	}
	var reqs []reconcile.Request
	for i := range list.Items {
		pr := &list.Items[i]
		if pr.Spec.BuildRef != nil && pr.Spec.BuildRef.Name == build.Name {
			reqs = append(reqs, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: pr.Namespace, Name: pr.Name}})
		}
	}
	return reqs
}

// restoresForSnapshot requeues PodRestores whose snapshotRef matches an
// updated PodSnapshot (so a restore waiting on a snapshot wakes promptly).
func (r *PodRestoreReconciler) restoresForSnapshot(ctx context.Context, obj client.Object) []reconcile.Request {
	snap, ok := obj.(*snapv1.PodSnapshot)
	if !ok {
		return nil
	}
	var list snapv1.PodRestoreList
	if err := r.List(ctx, &list, client.InNamespace(snap.Namespace)); err != nil {
		return nil
	}
	var reqs []reconcile.Request
	for i := range list.Items {
		pr := &list.Items[i]
		if pr.Spec.SnapshotRef != nil && pr.Spec.SnapshotRef.Name == snap.Name {
			reqs = append(reqs, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: pr.Namespace, Name: pr.Name}})
		}
	}
	return reqs
}
