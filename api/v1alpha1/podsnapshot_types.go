package v1alpha1

import (
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// PodSnapshot phases.
const (
	SnapshotPhasePending = "Pending"
	// Quiescing waits for the workload to reach its checkpoint-safe point
	// (docs/design-v2.md §4). Only entered for pods opting in via the
	// podsnapshot.io/quiesce annotation.
	SnapshotPhaseQuiescing     = "Quiescing"
	SnapshotPhaseCheckpointing = "Checkpointing"
	SnapshotPhaseCheckpointed  = "Checkpointed"
	SnapshotPhaseUploading     = "Uploading"
	SnapshotPhaseCompleted     = "Completed"
	SnapshotPhaseFailed        = "Failed"
)

// PodSnapshot condition types.
const (
	ConditionNodeReady         = "NodeReady"
	ConditionQuiesced          = "Quiesced"
	ConditionCheckpointCreated = "CheckpointCreated"
	ConditionArtifactUploaded  = "ArtifactUploaded"
	ConditionReady             = "Ready"
)

// Quiesce/resume contract (docs/design-v2.md §4). A workload opts in by
// annotating the pod being snapshotted; the shim inside the container writes
// a presence file when it has reached a checkpoint-safe point, and blocks
// until the agent writes the resume file back on restore.
const (
	// QuiesceAnnotation selects the protocol. Only "presence-file" is
	// implemented; absent means "checkpoint the live process as-is" (v1).
	QuiesceAnnotation = "podsnapshot.io/quiesce"
	// QuiesceDirAnnotation overrides the rendezvous directory. It must be a
	// writable volume mounted into the container (an emptyDir).
	QuiesceDirAnnotation = "podsnapshot.io/quiesce-dir"
	// QuiesceTimeoutAnnotation bounds the wait for the presence file, as a
	// Go duration ("600s", "10m").
	QuiesceTimeoutAnnotation = "podsnapshot.io/quiesce-timeout"

	// QuiesceModePresenceFile is the only supported protocol value.
	QuiesceModePresenceFile = "presence-file"

	// DefaultQuiesceDir is the rendezvous directory when unset.
	DefaultQuiesceDir = "/snapshot"
	// DefaultQuiesceTimeout bounds engine init + KV release.
	DefaultQuiesceTimeout = 10 * time.Minute

	// ReadyForCheckpointFile is written by the workload shim inside
	// <quiesce-dir> once it is safe to dump.
	ReadyForCheckpointFile = "ready-for-checkpoint"
	// RestoreCompleteFile is written by the node agent inside <quiesce-dir>
	// immediately before runc restore, so the shim's poll loop observes it
	// the first time it spins after CRIU resumes execution.
	RestoreCompleteFile = "restore-complete"
)

// Checkpointer values for PodSnapshotSpec.Checkpointer.
const (
	// CheckpointerKubelet POSTs to the kubelet checkpoint API (v1 default).
	CheckpointerKubelet = "kubelet"
	// CheckpointerAgent runs `runc checkpoint` on the node agent, with CRIU
	// writing directly into the artifact directory.
	CheckpointerAgent = "agent"
)

// CapabilitiesAnnotation is set on Nodes by the agent: a comma-separated list
// of optional behaviours this node's agent implements. It is separate from
// PrereqsAnnotation because the two answer different questions -- prereqs is
// "is this node healthy", capabilities is "how new is the code on it".
const CapabilitiesAnnotation = "podsnapshot.io/capabilities"

// AgentCheckpointCapability is advertised in CapabilitiesAnnotation by agents
// that implement CheckpointerAgent. The manager checks for it before honoring
// checkpointer: agent, so a partially upgraded cluster degrades to the kubelet
// path instead of stalling on a node whose agent would never pick the work up.
const AgentCheckpointCapability = "agent-checkpoint"

// NodeHasCapability reports whether the node advertises the named capability.
func NodeHasCapability(annotations map[string]string, want string) bool {
	for _, c := range strings.Split(annotations[CapabilitiesAnnotation], ",") {
		if strings.TrimSpace(c) == want {
			return true
		}
	}
	return false
}

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

	// ArtifactURI is where the checkpoint is stored. Supported schemes:
	//   fuse:///<path>  — a path under the fuse-client mount (/mnt/fuse/<path> on nodes)
	//   file:///<path>  — an absolute node-local path (testing only)
	// A trailing slash makes it a directory prefix (artifactFormat: dir).
	// Defaults to fuse:///snapshots/<namespace>/<name>/<container>.tar,
	// or .../<container>/ when artifactFormat is dir.
	// +optional
	ArtifactURI string `json:"artifactURI,omitempty"`

	// ArtifactFormat selects the on-storage layout:
	//
	//   tar — one CRI checkpoint archive (v1 default; restore untars it)
	//   dir — the archive expanded into per-file objects under a prefix,
	//         committed by a MANIFEST written last. Restore reads the CRIU
	//         images in place, so the untar disappears from the restore
	//         critical path (docs/design-v2.md §3).
	//
	// Ignored when artifactURI is set explicitly — the trailing slash decides.
	// +kubebuilder:validation:Enum=tar;dir
	// +kubebuilder:default=tar
	// +optional
	ArtifactFormat string `json:"artifactFormat,omitempty"`

	// DeletionPolicy controls what happens to the artifact when this
	// PodSnapshot is deleted: Retain (default) or Delete.
	// +kubebuilder:validation:Enum=Retain;Delete
	// +kubebuilder:default=Retain
	// +optional
	DeletionPolicy string `json:"deletionPolicy,omitempty"`

	// TimeoutSeconds for the checkpoint. Large VRAM dumps take minutes;
	// default 120.
	// +kubebuilder:default=120
	// +kubebuilder:validation:Minimum=1
	// +optional
	TimeoutSeconds int32 `json:"timeoutSeconds,omitempty"`

	// Checkpointer selects who runs the dump:
	//
	//   kubelet — POST to the kubelet checkpoint API, which drives CRIU and
	//             hands back a tar. The agent then expands that tar into the
	//             artifact directory.
	//   agent   — the node agent runs `runc checkpoint` itself, with CRIU
	//             writing its images straight into the artifact directory.
	//
	// The two produce the same artifact. What differs is how many times the
	// bytes are written to get there: the kubelet path writes them into a
	// tar and reads them back out, so a 56 GB checkpoint moves roughly three
	// times that much I/O, most of it on whichever disk holds
	// /var/lib/kubelet — the OS disk on a stock AKS GPU node, not the NVMe
	// tier the artifact is bound for. See docs/design-v2.md §3.
	//
	// agent requires artifactFormat: dir (there is no tar to hand back), and
	// falls back to kubelet if the node's agent is too old to advertise the
	// capability.
	// +kubebuilder:validation:Enum=kubelet;agent
	// +kubebuilder:default=kubelet
	// +optional
	Checkpointer string `json:"checkpointer,omitempty"`
}

// ArtifactStatus describes the produced checkpoint artifact.
type ArtifactStatus struct {
	URI string `json:"uri,omitempty"`
	// Format is "tar" or "dir"; absent means "tar" so v1 artifacts and the
	// PodRestores pointing at them keep working unchanged.
	// +optional
	Format    string `json:"format,omitempty"`
	SizeBytes int64  `json:"sizeBytes,omitempty"`
	// SHA256 of the tar. Empty for directory artifacts — their per-file
	// digests live in the MANIFEST.
	// +optional
	SHA256 string `json:"sha256,omitempty"`
	// FileCount is the number of objects in a directory artifact.
	// +optional
	FileCount int32       `json:"fileCount,omitempty"`
	CreatedAt metav1.Time `json:"createdAt,omitempty"`
}

// QuiesceStatus records the resolved quiesce contract for a snapshot.
type QuiesceStatus struct {
	// Mode is the protocol in use ("presence-file").
	Mode string `json:"mode,omitempty"`
	// Dir is the in-container rendezvous directory.
	Dir string `json:"dir,omitempty"`
	// Deadline is when the wait for ready-for-checkpoint gives up.
	// +optional
	Deadline *metav1.Time `json:"deadline,omitempty"`
	// ReadyAt is when the presence file appeared.
	// +optional
	ReadyAt *metav1.Time `json:"readyAt,omitempty"`
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

	// Artifact describes the uploaded checkpoint.
	// +optional
	Artifact *ArtifactStatus `json:"artifact,omitempty"`

	// Quiesce is set when the target pod opted into the quiesce protocol.
	// +optional
	Quiesce *QuiesceStatus `json:"quiesce,omitempty"`

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
// +kubebuilder:printcolumn:name="Format",type=string,JSONPath=`.status.artifact.format`
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
