package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// PodRestore phases.
const (
	RestorePhasePending    = "Pending"
	RestorePhasePreWarming = "PreWarming"
	RestorePhasePreparing  = "Preparing"
	RestorePhaseRestoring  = "Restoring"
	RestorePhaseRunning    = "Running"
	RestorePhaseFailed     = "Failed"
)

// PodRestore condition types.
const (
	ConditionArtifactAvailable = "ArtifactAvailable"
	ConditionPreWarmed         = "PreWarmed"
	ConditionPodPrepared       = "PodPrepared"
	ConditionRestored          = "Restored"
	ConditionTornDown          = "TornDown"
)

// TeardownFinalizer ensures the agent kills the restored runc container and
// unpins the artifact before the PodRestore disappears.
const TeardownFinalizer = "podsnapshot.io/restore-teardown"

// RestoreAnnotation links a placeholder pod back to its PodRestore (ns/name).
const RestoreAnnotation = "podsnapshot.io/restore"

// PodRestoreSpec defines the desired state of PodRestore.
type PodRestoreSpec struct {
	// ArtifactURI points at the checkpoint tar (fuse:// or file:// scheme).
	// Exactly one of ArtifactURI or SnapshotRef must be set.
	// +optional
	ArtifactURI string `json:"artifactURI,omitempty"`

	// SnapshotRef names a completed PodSnapshot in the same namespace; its
	// status.artifact.uri is used.
	// +optional
	SnapshotRef *corev1.LocalObjectReference `json:"snapshotRef,omitempty"`

	// PodTemplate for the target pod. The image MUST match the checkpointed
	// container's image and the template must request the same GPU count.
	// The controller rewrites the target container's command to a keeper
	// process; the restored workload joins this pod's namespaces.
	PodTemplate corev1.PodTemplateSpec `json:"podTemplate"`

	// Container in the template that receives the restored workload.
	// Defaults to the first container.
	// +optional
	Container string `json:"container,omitempty"`

	// NodeName pins the restore to a specific node. If empty the scheduler
	// picks a node and pre-warming happens after scheduling.
	// +optional
	NodeName string `json:"nodeName,omitempty"`

	// NodeSelector is merged into the placeholder pod's nodeSelector.
	// +optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`

	// Prewarm reads the artifact through the fuse-client cache on the target
	// node before restoring, so restore reads hit local NVMe.
	// +kubebuilder:default=true
	// +optional
	Prewarm *bool `json:"prewarm,omitempty"`

	// Pin keeps the artifact pinned in the node cache during restore.
	// +kubebuilder:default=true
	// +optional
	Pin *bool `json:"pin,omitempty"`

	// TimeoutSeconds is the deadline for the whole restore. Default 600.
	// +kubebuilder:default=600
	// +kubebuilder:validation:Minimum=1
	// +optional
	TimeoutSeconds int32 `json:"timeoutSeconds,omitempty"`
}

// PodRestoreStatus defines the observed state of PodRestore.
type PodRestoreStatus struct {
	// Phase: Pending | PreWarming | Preparing | Restoring | Running | Failed
	// +optional
	Phase string `json:"phase,omitempty"`

	// ArtifactURI actually used (resolved from SnapshotRef if given).
	// +optional
	ArtifactURI string `json:"artifactURI,omitempty"`

	// TargetNode the restore runs on.
	// +optional
	TargetNode string `json:"targetNode,omitempty"`

	// PodName / PodUID of the placeholder (target) pod.
	// +optional
	PodName string `json:"podName,omitempty"`
	// +optional
	PodUID string `json:"podUID,omitempty"`

	// RestoredContainerID is the runc id of the restored workload container.
	// +optional
	RestoredContainerID string `json:"restoredContainerID,omitempty"`
	// RestoredPID is the host PID of the restored init process.
	// +optional
	RestoredPID int32 `json:"restoredPID,omitempty"`

	// PrewarmBytes read through the cache during pre-warm.
	// +optional
	PrewarmBytes int64 `json:"prewarmBytes,omitempty"`

	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// +optional
	Message string `json:"message,omitempty"`

	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`
	// +optional
	RestoredAt *metav1.Time `json:"restoredAt,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=prestore
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Artifact",type=string,JSONPath=`.status.artifactURI`
// +kubebuilder:printcolumn:name="Node",type=string,JSONPath=`.status.targetNode`
// +kubebuilder:printcolumn:name="Pod",type=string,JSONPath=`.status.podName`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// PodRestore restores a pod from a checkpoint tar artifact.
type PodRestore struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   PodRestoreSpec   `json:"spec,omitempty"`
	Status PodRestoreStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// PodRestoreList contains a list of PodRestore.
type PodRestoreList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PodRestore `json:"items"`
}

func init() {
	SchemeBuilder.Register(&PodRestore{}, &PodRestoreList{})
}
