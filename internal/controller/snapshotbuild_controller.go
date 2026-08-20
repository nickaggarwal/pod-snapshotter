package controller

import (
	"context"
	"fmt"
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

// SnapshotBuildReconciler turns a pod template into a reusable checkpoint:
//
//	Pending      create the one-shot build pod
//	Building     wait for it to run and reach its quiesce point
//	Snapshotting drive a PodSnapshot against it
//	Completed    artifact published; build pod torn down
//
// The build pod is a bare Pod, not a Job: the checkpoint is taken of a
// specific running container, and a Job's restart semantics would only get in
// the way of a pod that is deliberately parked in a poll loop and never exits.
type SnapshotBuildReconciler struct {
	client.Client
	// Artifacts is optional; when nil, DeletionPolicy=Delete only removes the
	// finalizer without deleting the artifact (logged).
	Artifacts ArtifactDeleter
}

// +kubebuilder:rbac:groups=podsnapshot.io,resources=snapshotbuilds,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=podsnapshot.io,resources=snapshotbuilds/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=podsnapshot.io,resources=snapshotbuilds/finalizers,verbs=update
// +kubebuilder:rbac:groups=podsnapshot.io,resources=podsnapshots,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch

func (r *SnapshotBuildReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var build snapv1.SnapshotBuild
	if err := r.Get(ctx, req.NamespacedName, &build); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !build.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, &build)
	}
	if controllerutil.AddFinalizer(&build, snapv1.BuildCleanupFinalizer) {
		if err := r.Update(ctx, &build); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Deadline for everything but the terminal phases.
	if build.Status.StartedAt != nil &&
		build.Status.Phase != snapv1.BuildPhaseCompleted &&
		build.Status.Phase != snapv1.BuildPhaseFailed {
		timeout := time.Duration(build.Spec.TimeoutSeconds) * time.Second
		if timeout <= 0 {
			timeout = 1800 * time.Second
		}
		if time.Since(build.Status.StartedAt.Time) > timeout {
			return r.failBuild(ctx, &build, fmt.Sprintf("build timed out after %s in phase %s", timeout, build.Status.Phase))
		}
	}

	switch build.Status.Phase {
	case "":
		build.Status.Phase = snapv1.BuildPhasePending
		build.Status.StartedAt = ptrTime(metav1.Now())
		return ctrl.Result{}, r.Status().Update(ctx, &build)
	case snapv1.BuildPhasePending:
		return r.reconcileBuildPending(ctx, &build)
	case snapv1.BuildPhaseBuilding:
		return r.reconcileBuilding(ctx, &build)
	case snapv1.BuildPhaseSnapshotting:
		return r.reconcileSnapshotting(ctx, &build)
	default:
		return ctrl.Result{}, nil
	}
}

// reconcileBuildPending validates the template and creates the build pod.
func (r *SnapshotBuildReconciler) reconcileBuildPending(ctx context.Context, build *snapv1.SnapshotBuild) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	if _, err := buildContainerIndex(build); err != nil {
		return r.failBuild(ctx, build, err.Error())
	}
	uriStr := build.Spec.ArtifactURI
	if uriStr == "" {
		uriStr = artifact.DefaultBuildURI(build.Spec.Revision, buildFormat(build))
	}
	uri, err := artifact.Parse(uriStr)
	if err != nil {
		return r.failBuild(ctx, build, err.Error())
	}

	podName := buildPodName(build)
	var pod corev1.Pod
	err = r.Get(ctx, types.NamespacedName{Namespace: build.Namespace, Name: podName}, &pod)
	if apierrors.IsNotFound(err) {
		newPod, buildErr := BuildBuilderPod(build, podName)
		if buildErr != nil {
			return r.failBuild(ctx, build, buildErr.Error())
		}
		if err := controllerutil.SetControllerReference(build, newPod, r.Scheme()); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.Create(ctx, newPod); err != nil {
			return ctrl.Result{}, err
		}
		logger.Info("created build pod", "pod", podName, "revision", build.Spec.Revision)
	} else if err != nil {
		return ctrl.Result{}, err
	}

	build.Status.BuildPodName = podName
	build.Status.Artifact = &snapv1.ArtifactStatus{URI: uri.String(), Format: uri.Format()}
	build.Status.Phase = snapv1.BuildPhaseBuilding
	build.Status.Message = "waiting for the build pod to initialize the engine"
	setCondition(&build.Status.Conditions, snapv1.ConditionBuildPodReady, metav1.ConditionFalse, "Creating", podName)
	return ctrl.Result{}, r.Status().Update(ctx, build)
}

// reconcileBuilding waits for the build pod to run, then creates the
// PodSnapshot that does the actual checkpoint. The wait for the shim's
// quiesce point happens inside the PodSnapshot (phase Quiescing), so there
// is no second protocol here.
func (r *SnapshotBuildReconciler) reconcileBuilding(ctx context.Context, build *snapv1.SnapshotBuild) (ctrl.Result, error) {
	var pod corev1.Pod
	if err := r.Get(ctx, types.NamespacedName{Namespace: build.Namespace, Name: build.Status.BuildPodName}, &pod); err != nil {
		if apierrors.IsNotFound(err) {
			return r.failBuild(ctx, build, fmt.Sprintf("build pod %s disappeared", build.Status.BuildPodName))
		}
		return ctrl.Result{}, err
	}
	switch pod.Status.Phase {
	case corev1.PodRunning:
	case corev1.PodFailed, corev1.PodSucceeded:
		// A build pod that exits never reached its quiesce point: the shim is
		// supposed to park in a poll loop, not terminate.
		return r.failBuild(ctx, build, fmt.Sprintf("build pod %s is %s before the checkpoint; the shim must block, not exit", pod.Name, pod.Status.Phase))
	default:
		build.Status.Message = fmt.Sprintf("build pod %s is %s", pod.Name, pod.Status.Phase)
		if err := r.Status().Update(ctx, build); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	build.Status.NodeName = pod.Spec.NodeName
	build.Status.Compatibility = r.observeCompatibility(ctx, build, &pod)
	setCondition(&build.Status.Conditions, snapv1.ConditionBuildPodReady, metav1.ConditionTrue, "Running", pod.Name)

	idx, err := buildContainerIndex(build)
	if err != nil {
		return r.failBuild(ctx, build, err.Error())
	}
	container := build.Spec.PodTemplate.Spec.Containers[idx].Name

	snapName := buildSnapshotName(build)
	var snap snapv1.PodSnapshot
	err = r.Get(ctx, types.NamespacedName{Namespace: build.Namespace, Name: snapName}, &snap)
	if apierrors.IsNotFound(err) {
		newSnap := &snapv1.PodSnapshot{
			ObjectMeta: metav1.ObjectMeta{Namespace: build.Namespace, Name: snapName},
			Spec: snapv1.PodSnapshotSpec{
				PodName:        pod.Name,
				Container:      container,
				ArtifactURI:    build.Status.Artifact.URI,
				DeletionPolicy: snapv1.DeletionPolicyRetain,
				// The dump itself, once the workload has already quiesced.
				TimeoutSeconds: build.Spec.TimeoutSeconds,
			},
		}
		if err := controllerutil.SetControllerReference(build, newSnap, r.Scheme()); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.Create(ctx, newSnap); err != nil {
			return ctrl.Result{}, err
		}
	} else if err != nil {
		return ctrl.Result{}, err
	}

	build.Status.SnapshotName = snapName
	build.Status.Phase = snapv1.BuildPhaseSnapshotting
	build.Status.Message = "checkpointing the build pod at its quiesce point"
	return ctrl.Result{}, r.Status().Update(ctx, build)
}

// reconcileSnapshotting mirrors the PodSnapshot's outcome onto the build and
// tears the build pod down once the artifact exists.
func (r *SnapshotBuildReconciler) reconcileSnapshotting(ctx context.Context, build *snapv1.SnapshotBuild) (ctrl.Result, error) {
	var snap snapv1.PodSnapshot
	if err := r.Get(ctx, types.NamespacedName{Namespace: build.Namespace, Name: build.Status.SnapshotName}, &snap); err != nil {
		if apierrors.IsNotFound(err) {
			return r.failBuild(ctx, build, fmt.Sprintf("PodSnapshot %s disappeared", build.Status.SnapshotName))
		}
		return ctrl.Result{}, err
	}

	switch snap.Status.Phase {
	case snapv1.SnapshotPhaseFailed:
		setCondition(&build.Status.Conditions, snapv1.ConditionArtifactBuilt, metav1.ConditionFalse, "SnapshotFailed", snap.Status.Message)
		return r.failBuild(ctx, build, fmt.Sprintf("PodSnapshot %s failed: %s", snap.Name, snap.Status.Message))
	case snapv1.SnapshotPhaseCompleted:
	default:
		build.Status.Message = fmt.Sprintf("PodSnapshot %s is %s: %s", snap.Name, snap.Status.Phase, snap.Status.Message)
		if err := r.Status().Update(ctx, build); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	// The build pod has served its purpose; free the GPU.
	if !build.Spec.KeepBuildPod && build.Status.BuildPodName != "" {
		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: build.Namespace, Name: build.Status.BuildPodName}}
		if err := r.Delete(ctx, pod); err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
	}

	now := metav1.Now()
	build.Status.Artifact = snap.Status.Artifact.DeepCopy()
	build.Status.Phase = snapv1.BuildPhaseCompleted
	build.Status.Message = ""
	build.Status.CompletedAt = &now
	setCondition(&build.Status.Conditions, snapv1.ConditionArtifactBuilt, metav1.ConditionTrue, "Built", build.Status.Artifact.URI)
	return ctrl.Result{}, r.Status().Update(ctx, build)
}

// observeCompatibility merges the tuple the user pinned with what the build
// node actually reports, so restores are matched against reality.
func (r *SnapshotBuildReconciler) observeCompatibility(ctx context.Context, build *snapv1.SnapshotBuild, pod *corev1.Pod) *snapv1.CompatibilityKey {
	key := snapv1.CompatibilityKey{}
	if build.Spec.Compatibility != nil {
		key = *build.Spec.Compatibility
	}
	var node corev1.Node
	if err := r.Get(ctx, types.NamespacedName{Name: pod.Spec.NodeName}, &node); err == nil {
		observed := snapv1.ParseCompatibility(node.Annotations[snapv1.CompatibilityAnnotation])
		if key.GPUModel == "" {
			key.GPUModel = observed.GPUModel
		}
		if key.DriverVersion == "" {
			key.DriverVersion = observed.DriverVersion
		}
		if key.CRIUVersion == "" {
			key.CRIUVersion = observed.CRIUVersion
		}
	}
	if key.ImageDigest == "" {
		idx, err := buildContainerIndex(build)
		if err == nil {
			name := build.Spec.PodTemplate.Spec.Containers[idx].Name
			for _, cs := range pod.Status.ContainerStatuses {
				if cs.Name == name {
					key.ImageDigest = cs.ImageID
					break
				}
			}
		}
	}
	return &key
}

func (r *SnapshotBuildReconciler) reconcileDelete(ctx context.Context, build *snapv1.SnapshotBuild) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	if !controllerutil.ContainsFinalizer(build, snapv1.BuildCleanupFinalizer) {
		return ctrl.Result{}, nil
	}
	if build.Spec.DeletionPolicy == snapv1.DeletionPolicyDelete &&
		build.Status.Artifact != nil && build.Status.Artifact.URI != "" {
		uri, err := artifact.Parse(build.Status.Artifact.URI)
		if err == nil {
			if r.Artifacts != nil {
				if err := r.Artifacts.Delete(ctx, uri); err != nil {
					logger.Error(err, "deleting build artifact; will retry", "uri", uri.String())
					return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
				}
			} else {
				logger.Info("no artifact deleter configured; skipping artifact deletion", "uri", uri.String())
			}
		}
	}
	// The build pod and PodSnapshot are owned; garbage collection removes them.
	controllerutil.RemoveFinalizer(build, snapv1.BuildCleanupFinalizer)
	return ctrl.Result{}, r.Update(ctx, build)
}

func (r *SnapshotBuildReconciler) failBuild(ctx context.Context, build *snapv1.SnapshotBuild, msg string) (ctrl.Result, error) {
	build.Status.Phase = snapv1.BuildPhaseFailed
	build.Status.Message = msg
	setCondition(&build.Status.Conditions, snapv1.ConditionArtifactBuilt, metav1.ConditionFalse, "Failed", msg)
	return ctrl.Result{}, r.Status().Update(ctx, build)
}

func buildFormat(build *snapv1.SnapshotBuild) string {
	if build.Spec.ArtifactFormat == "" {
		return artifact.FormatDir
	}
	return build.Spec.ArtifactFormat
}

func buildPodName(build *snapv1.SnapshotBuild) string  { return build.Name + "-build" }
func buildSnapshotName(b *snapv1.SnapshotBuild) string { return b.Name + "-snap" }

// SetupWithManager registers the controller, watching the pods and snapshots
// it owns so their transitions wake it promptly.
func (r *SnapshotBuildReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&snapv1.SnapshotBuild{}).
		Owns(&corev1.Pod{}).
		Owns(&snapv1.PodSnapshot{}).
		Named("snapshotbuild").
		Complete(r)
}
