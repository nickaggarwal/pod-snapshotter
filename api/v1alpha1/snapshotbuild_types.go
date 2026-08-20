package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// SnapshotBuild phases.
const (
	BuildPhasePending      = "Pending"
	BuildPhaseBuilding     = "Building"
	BuildPhaseSnapshotting = "Snapshotting"
	BuildPhaseCompleted    = "Completed"
	BuildPhaseFailed       = "Failed"
)

// SnapshotBuild condition types.
const (
	ConditionBuildPodReady = "BuildPodReady"
	ConditionArtifactBuilt = "ArtifactBuilt"
)

// BuildCleanupFinalizer ensures the build pod and (optionally) the artifact
// are removed before the SnapshotBuild disappears.
const BuildCleanupFinalizer = "podsnapshot.io/build-cleanup"

// BuildAnnotation links a build pod back to its SnapshotBuild (ns/name).
const BuildAnnotation = "podsnapshot.io/build"

// CompatibilityAnnotation is set on Nodes by the agent: the environment tuple
// a restore must match. Format is "gpu=<model>;driver=<ver>;criu=<ver>".
const CompatibilityAnnotation = "podsnapshot.io/compat"

// CompatibilityKey is the environment tuple a checkpoint is bound to. CRIU
// and cuda-checkpoint restores require the target to match the source; today
// a mismatch surfaces as an opaque failure deep inside `runc restore`, so the
// build records the tuple and the restore controller checks it first
// (docs/design-v2.md §5).
type CompatibilityKey struct {
	// GPUModel as reported by nvidia-smi, e.g. "NVIDIA A100 80GB PCIe".
	// Empty on CPU-only builds.
	// +optional
	GPUModel string `json:"gpuModel,omitempty"`
	// DriverVersion as reported by nvidia-smi, e.g. "580.159.04".
	// +optional
	DriverVersion string `json:"driverVersion,omitempty"`
	// CRIUVersion, e.g. "4.2.1".
	// +optional
	CRIUVersion string `json:"criuVersion,omitempty"`
	// ImageDigest of the checkpointed container's image.
	// +optional
	ImageDigest string `json:"imageDigest,omitempty"`
}

// Matches reports whether a node's tuple can restore an artifact built
// against k. Fields the build did not record are not constrained.
func (k CompatibilityKey) Matches(node CompatibilityKey) (bool, string) {
	for _, f := range []struct{ what, want, got string }{
		{"GPU model", k.GPUModel, node.GPUModel},
		{"NVIDIA driver", k.DriverVersion, node.DriverVersion},
		{"CRIU version", k.CRIUVersion, node.CRIUVersion},
	} {
		if f.want != "" && f.got != "" && f.want != f.got {
			return false, f.what + " " + f.got + " does not match the build's " + f.want
		}
	}
	return true, ""
}

// SnapshotBuildSpec defines the desired state of SnapshotBuild.
type SnapshotBuildSpec struct {
	// PodTemplate is the build pod: the real workload plus its quiesce shim.
	// It is run once, snapshotted at the shim's safe point, then deleted.
	//
	// Quiescing is destructive — a replica that has released its KV cache and
	// parked in a poll loop is not serving — so it cannot run on a serving
	// pod. That is the whole reason builds exist.
	PodTemplate PodTemplate `json:"podTemplate"`

	// Container in the template to checkpoint. Defaults to the first.
	// +optional
	Container string `json:"container,omitempty"`

	// Revision identifies the artifact and names its directory under the
	// artifact root. Immutable.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="revision is immutable"
	Revision string `json:"revision"`

	// Compatibility is the environment tuple restores are matched against.
	// Any field left empty is filled in from the build node.
	// +optional
	Compatibility *CompatibilityKey `json:"compatibility,omitempty"`

	// ArtifactURI defaults to fuse:///snapshots/builds/<revision>/.
	// +optional
	ArtifactURI string `json:"artifactURI,omitempty"`

	// ArtifactFormat is "dir" (default) or "tar". Builds default to
	// directories: a build artifact is restored many times, so paying the
	// expansion once here is strictly better than untarring on every restore.
	// +kubebuilder:validation:Enum=tar;dir
	// +kubebuilder:default=dir
	// +optional
	ArtifactFormat string `json:"artifactFormat,omitempty"`

	// DeletionPolicy for the artifact: Retain (default) or Delete.
	// +kubebuilder:validation:Enum=Retain;Delete
	// +kubebuilder:default=Retain
	// +optional
	DeletionPolicy string `json:"deletionPolicy,omitempty"`

	// TimeoutSeconds bounds the whole build (pod start + quiesce + dump +
	// upload). Weight loading for a large model is minutes; default 1800.
	// +kubebuilder:default=1800
	// +kubebuilder:validation:Minimum=1
	// +optional
	TimeoutSeconds int32 `json:"timeoutSeconds,omitempty"`

	// KeepBuildPod leaves the build pod running after a successful build,
	// for debugging. It still counts against the GPU.
	// +optional
	KeepBuildPod bool `json:"keepBuildPod,omitempty"`
}

// SnapshotBuildStatus defines the observed state of SnapshotBuild.
type SnapshotBuildStatus struct {
	// Phase: Pending | Building | Snapshotting | Completed | Failed
	// +optional
	Phase string `json:"phase,omitempty"`

	// BuildPodName is the one-shot pod the checkpoint was taken from.
	// +optional
	BuildPodName string `json:"buildPodName,omitempty"`

	// NodeName the build ran on.
	// +optional
	NodeName string `json:"nodeName,omitempty"`

	// SnapshotName is the PodSnapshot the build drives.
	// +optional
	SnapshotName string `json:"snapshotName,omitempty"`

	// Artifact describes the build output.
	// +optional
	Artifact *ArtifactStatus `json:"artifact,omitempty"`

	// Compatibility as actually observed on the build node.
	// +optional
	Compatibility *CompatibilityKey `json:"compatibility,omitempty"`

	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	// +optional
	Message string `json:"message,omitempty"`
	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`
	// +optional
	CompletedAt *metav1.Time `json:"completedAt,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=snapbuild
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Revision",type=string,JSONPath=`.spec.revision`
// +kubebuilder:printcolumn:name="Artifact",type=string,JSONPath=`.status.artifact.uri`
// +kubebuilder:printcolumn:name="Size",type=integer,JSONPath=`.status.artifact.sizeBytes`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// SnapshotBuild produces a restorable checkpoint as a build output rather
// than as a picture of a serving pod: one artifact per
// (image, model, GPU SKU, driver, CRIU), built once at revision-publish time
// and restored by every 0->1 scale-up (docs/design-v2.md §5).
type SnapshotBuild struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   SnapshotBuildSpec   `json:"spec,omitempty"`
	Status SnapshotBuildStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// SnapshotBuildList contains a list of SnapshotBuild.
type SnapshotBuildList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SnapshotBuild `json:"items"`
}

func init() {
	SchemeBuilder.Register(&SnapshotBuild{}, &SnapshotBuildList{})
}
