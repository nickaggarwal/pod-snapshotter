package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// PodSnapshot phases.
const (
	SnapshotPhasePending       = "Pending"
	SnapshotPhaseCheckpointing = "Checkpointing"
	SnapshotPhaseCheckpointed  = "Checkpointed"
	SnapshotPhaseUploading     = "Uploading"
	SnapshotPhaseCompleted     = "Completed"
	SnapshotPhaseFailed        = "Failed"
)

// PodSnapshot condition types.
const (
	ConditionNodeReady         = "NodeReady"
	ConditionCheckpointCreated = "CheckpointCreated"
	ConditionArtifactUploaded  = "ArtifactUploaded"
	ConditionReady             = "Ready"
)

// Deletion policies for the snapshot artifact.
const (
	DeletionPolicyRetain = "Retain"
	DeletionPolicyDelete = "Delete"
)

// ArtifactCleanupFinalizer is added to PodSnapshots with DeletionPolicy=Delete.
const ArtifactCleanupFinalizer = "podsnapshot.io/artifact-cleanup"

// PrereqsAnnotation is set on Nodes by the agent: "ok" or a CSV of failed checks.
const PrereqsAnnotation = "podsnapshot.io/prereqs"

// PodSnapshotSpec defines the desired state of PodSnapshot.
type PodSnapshotSpec struct {
	// PodName is the name of the running pod (same namespace) to checkpoint.
	// +kubebuilder:validation:MinLength=1
	PodName string `json:"podName"`

	// Container to checkpoint. Defaults to the pod's only container; required
	// when the pod has more than one.
	// +optional
	Container string `json:"container,omitempty"`

	// ArtifactURI is where the checkpoint tar is stored. Supported schemes:
	//   fuse:///<path>  — a path under the fuse-client mount (/mnt/fuse/<path> on nodes)
	//   file:///<path>  — an absolute node-local path (testing only)
	// Defaults to fuse:///snapshots/<namespace>/<name>/<container>.tar
	// +optional
	ArtifactURI string `json:"artifactURI,omitempty"`

	// DeletionPolicy controls what happens to the artifact when this
	// PodSnapshot is deleted: Retain (default) or Delete.
	// +kubebuilder:validation:Enum=Retain;Delete
	// +kubebuilder:default=Retain
	// +optional
	DeletionPolicy string `json:"deletionPolicy,omitempty"`

	// TimeoutSeconds for the kubelet checkpoint call. Large VRAM dumps take
	// minutes; default 120.
	// +kubebuilder:default=120
	// +kubebuilder:validation:Minimum=1
	// +optional
	TimeoutSeconds int32 `json:"timeoutSeconds,omitempty"`
}

// ArtifactStatus describes the produced checkpoint artifact.
type ArtifactStatus struct {
	URI       string      `json:"uri,omitempty"`
	SizeBytes int64       `json:"sizeBytes,omitempty"`
	SHA256    string      `json:"sha256,omitempty"`
	CreatedAt metav1.Time `json:"createdAt,omitempty"`
}

// PodSnapshotStatus defines the observed state of PodSnapshot.
type PodSnapshotStatus struct {
	// Phase: Pending | Checkpointing | Checkpointed | Uploading | Completed | Failed
	// +optional
	Phase string `json:"phase,omitempty"`

	// NodeName is the node the checkpointed pod runs on; the agent there
	// performs the artifact upload.
	// +optional
	NodeName string `json:"nodeName,omitempty"`

	// PodUID of the checkpointed pod at snapshot time.
	// +optional
	PodUID string `json:"podUID,omitempty"`

	// Container that was checkpointed (spec.container after defaulting).
	// +optional
	Container string `json:"container,omitempty"`

	// KubeletCheckpointPath is the node-local tar written by the kubelet,
	// e.g. /var/lib/kubelet/checkpoints/checkpoint-<pod>_<ns>-<ctr>-<ts>.tar
	// +optional
	KubeletCheckpointPath string `json:"kubeletCheckpointPath,omitempty"`

	// Artifact describes the uploaded tar.
	// +optional
	Artifact *ArtifactStatus `json:"artifact,omitempty"`

	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// Message holds a human-readable explanation of the current phase.
	// +optional
	Message string `json:"message,omitempty"`

	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`
	// +optional
	CompletedAt *metav1.Time `json:"completedAt,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=psnap
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Pod",type=string,JSONPath=`.spec.podName`
// +kubebuilder:printcolumn:name="Artifact",type=string,JSONPath=`.status.artifact.uri`
// +kubebuilder:printcolumn:name="Size",type=integer,JSONPath=`.status.artifact.sizeBytes`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// PodSnapshot checkpoints a running (GPU) pod container into a tar artifact.
type PodSnapshot struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   PodSnapshotSpec   `json:"spec,omitempty"`
	Status PodSnapshotStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// PodSnapshotList contains a list of PodSnapshot.
type PodSnapshotList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PodSnapshot `json:"items"`
}

func init() {
	SchemeBuilder.Register(&PodSnapshot{}, &PodSnapshotList{})
}
