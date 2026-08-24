package agent

import (
	"context"
	"encoding/json"
	"github.com/go-logr/logr"
	"os"
	"path/filepath"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	snapv1 "pod-snapshotter/api/v1alpha1"
	"pod-snapshotter/internal/artifact"
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

// A dump is one-way: runc checkpoint takes the container with it. So a
// reconcile that arrives after the artifact is already committed -- the final
// status write lost a conflict, or the agent restarted between publishing and
// updating -- must finish the bookkeeping rather than try to dump a container
// that no longer exists.
//
// This was a real failure: the dump succeeded in 47s, the artifact was
// complete on disk, and the snapshot then spun in Checkpointing forever
// reporting "no ready sandbox found" because every retry re-entered the dump.
func TestCheckpointCompletesFromACommittedArtifactWithoutDumping(t *testing.T) {
	dir := t.TempDir()
	ckpt := filepath.Join(dir, "checkpoint")
	if err := os.MkdirAll(ckpt, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"pages-1.img", "inventory.img"} {
		if err := os.WriteFile(filepath.Join(ckpt, n), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	m, err := artifact.PublishDir(artifact.PublishDirOptions{
		Dir:  dir,
		Meta: &artifact.CheckpointMeta{ID: "cid", Name: "vllm_p_default_uid_0"},
		Spec: json.RawMessage(`{"ociVersion":"1.0.2"}`),
	})
	if err != nil {
		t.Fatal(err)
	}

	snap := snapFor("node-a", snapv1.CheckpointerAgent, "file://"+dir+"/")
	scheme := runtime.NewScheme()
	if err := snapv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(snap).WithStatusSubresource(snap).Build()

	// No Resolver and no Runc: if the reconciler reaches either, the nil
	// dereference is the test failing loudly, which is the point.
	r := &CheckpointReconciler{Client: c, NodeName: "node-a"}
	if _, err := r.Reconcile(context.Background(),
		ctrl.Request{NamespacedName: client.ObjectKeyFromObject(snap)}); err != nil {
		t.Fatalf("reconcile of a committed artifact failed: %v", err)
	}

	var got snapv1.PodSnapshot
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(snap), &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase != snapv1.SnapshotPhaseCompleted {
		t.Fatalf("phase is %q, want Completed", got.Status.Phase)
	}
	if got.Status.Artifact.SizeBytes != m.TotalBytes {
		t.Fatalf("size is %d, want %d", got.Status.Artifact.SizeBytes, m.TotalBytes)
	}
}

// A dump of a 56 GB engine takes minutes, and the pod it came from starts
// terminating the moment runc takes the container. The kubelet is then free to
// reap the emptyDirs the shm and rootfs diffs are read from, so those captures
// can fail purely on timing -- and losing the whole artifact over a scratch
// tmpfs that unmounted a second early is not a trade worth making.
func TestPublishSurvivesADiffSourceThatVanished(t *testing.T) {
	dst := t.TempDir()
	ckpt := filepath.Join(dst, "checkpoint")
	if err := os.MkdirAll(ckpt, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"inventory.img", "pages-1.img"} {
		if err := os.WriteFile(filepath.Join(ckpt, f), []byte(f), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	r := &CheckpointReconciler{
		// A path that does not exist is exactly the state the kubelet leaves
		// behind after it reaps the pod's writable layer.
		HostRoot: filepath.Join(dst, "no-such-host-root"),
	}
	contribute := r.contribute(logr.Discard(), filepath.Join(dst, "no-such-upperdir"))

	m, err := artifact.PublishDir(artifact.PublishDirOptions{
		Dir:        dst,
		Meta:       &artifact.CheckpointMeta{ID: "abc", Name: "c_p_ns_uid_0"},
		Spec:       []byte("{}"),
		Contribute: contribute,
	})
	if err != nil {
		t.Fatalf("publish gave up because a diff source was missing: %v", err)
	}
	if len(m.Files) == 0 {
		t.Fatal("published an empty manifest")
	}
	// The commit marker has to be there: a restore reads it to decide the
	// artifact is whole.
	if _, err := artifact.ReadManifestDir(dst); err != nil {
		t.Fatalf("no committed manifest after publish: %v", err)
	}
}

// The artifact store lies about deletion. An rmdir of a non-empty directory
// returns 0 there without unlinking anything, which is enough to make
// os.RemoveAll report success while leaving the whole tree in place -- it
// tries a plain Remove first and returns early when that "succeeds".
//
// A dump that starts on top of a previous attempt's images either dies on the
// first file CRIU will not overwrite (descriptors.json: operation not
// permitted, seventeen minutes after the container was already taken) or,
// worse, appends to a stale pages-*.img and commits a MANIFEST over it. So
// the clear has to be checked, not attempted.
func TestClearArtifactDirRemovesAPreviousAttempt(t *testing.T) {
	dir := t.TempDir()
	ckpt := filepath.Join(dir, "checkpoint")
	if err := os.MkdirAll(ckpt, 0o755); err != nil {
		t.Fatal(err)
	}
	// What was actually found in the artifact directory after the failure:
	// images and descriptors.json underneath, and the top-level pair that the
	// old cleanup never touched at all.
	for _, f := range []string{
		"checkpoint/descriptors.json",
		"checkpoint/inventory.img",
		"checkpoint/pages-1.img",
		"dump.log",
		"spec.dump",
		"config.dump",
	} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("stale"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	if err := clearArtifactDir(dir); err != nil {
		t.Fatalf("clearing a directory holding a previous attempt: %v", err)
	}

	left, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 0 {
		var names []string
		for _, e := range left {
			names = append(names, e.Name())
		}
		t.Fatalf("previous attempt survived the clear: %v", names)
	}
}

// Nothing to clear is not an error: the first dump of a new revision starts
// on a path that does not exist yet.
func TestClearArtifactDirOnAPathThatIsNotThere(t *testing.T) {
	if err := clearArtifactDir(filepath.Join(t.TempDir(), "never-created")); err != nil {
		t.Fatalf("a missing directory should clear trivially: %v", err)
	}
}

// The case the check exists for. If the files are still readable back after
// being removed, the caller has to hear about it before the dump rather than
// after -- a clear that silently did nothing is exactly what produced the
// descriptors.json failure.
func TestClearArtifactDirReportsAStoreThatDidNotDelete(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "pages-1.img"), []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Deny the unlink the way the mount does: the entry stays readable.
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	err := clearArtifactDir(dir)
	if err == nil {
		t.Fatal("reported a successful clear over files that are still there")
	}
	if !strings.Contains(err.Error(), "pages-1.img") {
		t.Fatalf("the error does not name what survived: %v", err)
	}
}
