package agent

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	snapv1 "pod-snapshotter/api/v1alpha1"
)

func snapFor(node, checkpointer, uri string) *snapv1.PodSnapshot {
	return &snapv1.PodSnapshot{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "s"},
		Spec:       snapv1.PodSnapshotSpec{PodName: "vllm", Checkpointer: checkpointer},
		Status: snapv1.PodSnapshotStatus{
			NodeName: node,
			Phase:    snapv1.SnapshotPhaseCheckpointing,
			Artifact: &snapv1.ArtifactStatus{URI: uri},
		},
	}
}

// This predicate is the only thing standing between one snapshot and two
// agents dumping the same container, so each clause matters on its own.
func TestCheckpointReconcilerOwnership(t *testing.T) {
	const dirURI = "fuse:///snapshots/default/s/vllm/"
	r := &CheckpointReconciler{NodeName: "node-a"}

	cases := []struct {
		name string
		snap func() *snapv1.PodSnapshot
		want bool
	}{
		{
			name: "ours",
			snap: func() *snapv1.PodSnapshot { return snapFor("node-a", snapv1.CheckpointerAgent, dirURI) },
			want: true,
		},
		{
			name: "another node's snapshot is not ours to dump",
			snap: func() *snapv1.PodSnapshot { return snapFor("node-b", snapv1.CheckpointerAgent, dirURI) },
		},
		{
			name: "the kubelet path stays with the manager",
			snap: func() *snapv1.PodSnapshot { return snapFor("node-a", snapv1.CheckpointerKubelet, dirURI) },
		},
		{
			// The field defaults to kubelet at the API server, but an object
			// constructed in code (or an older CRD) can arrive empty, and an
			// empty value must not be read as opting in.
			name: "an unset checkpointer is not the agent",
			snap: func() *snapv1.PodSnapshot { return snapFor("node-a", "", dirURI) },
		},
		{
			name: "a phase we do not own",
			snap: func() *snapv1.PodSnapshot {
				s := snapFor("node-a", snapv1.CheckpointerAgent, dirURI)
				s.Status.Phase = snapv1.SnapshotPhaseUploading
				return s
			},
		},
		{
			// Without a URI there is nowhere to point --image-path, and the
			// manager has not finished admitting the snapshot yet.
			name: "no artifact yet",
			snap: func() *snapv1.PodSnapshot {
				s := snapFor("node-a", snapv1.CheckpointerAgent, dirURI)
				s.Status.Artifact = nil
				return s
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := r.owns(tc.snap()); got != tc.want {
				t.Fatalf("owns = %v, want %v", got, tc.want)
			}
		})
	}
}

// config.dump is the one file both producers write and the restore parses
// without knowing which produced it. The CRI name shape is load-bearing:
// restore.OldPodUID splits it to find the pod UID the images were dumped
// under, and a wrong shape yields a restore that fails deep inside CRIU.
func TestCheckpointMetaMatchesTheCRINameShape(t *testing.T) {
	snap := &snapv1.PodSnapshot{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "s"},
		Spec:       snapv1.PodSnapshotSpec{PodName: "vllm"},
		Status:     snapv1.PodSnapshotStatus{Container: "server", PodUID: "9f2c-uid"},
	}
	m := checkpointMeta(snap, "cid123", metav1.Now())

	if m.ID != "cid123" {
		t.Fatalf("container id lost: %q", m.ID)
	}
	if want := "server_vllm_default_9f2c-uid_0"; m.Name != want {
		t.Fatalf("name = %q, want %q", m.Name, want)
	}
	if m.CheckpointedAt.IsZero() {
		t.Fatal("checkpointedTime is zero; the artifact cannot say when it was taken")
	}
}

// Only dump-side knobs belong in the dump environment. The restore-side
// annotations share a prefix family, and passing them here would make it look
// like they had been honored.
func TestCRIUDumpTuning(t *testing.T) {
	snap := &snapv1.PodSnapshot{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
		"podsnapshot.io/criu-dump-env-criu-image-io-mode": "direct",
		"podsnapshot.io/criu-aio-depth":                   "128", // restore-side; not ours
		"podsnapshot.io/criu-dump-env-empty":              "",
		"unrelated":                                       "x",
	}}}

	env := criuDumpTuning(snap)
	if got, want := env["CRIU_IMAGE_IO_MODE"], "direct"; got != want {
		t.Fatalf("CRIU_IMAGE_IO_MODE = %q, want %q", got, want)
	}
	if len(env) != 1 {
		t.Fatalf("dump env picked up more than the dump-side knob: %v", env)
	}

	if criuDumpTuning(&snapv1.PodSnapshot{}) != nil {
		t.Fatal("no annotations should mean no env, not an empty map runc has to be handed")
	}
}
