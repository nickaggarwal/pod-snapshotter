package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	snapv1 "pod-snapshotter/api/v1alpha1"
	"pod-snapshotter/internal/artifact"
)

// runc is exec'd through nsenter into the host mount namespace, so when the
// NVMe bypass makes the agent and the host name the images directory
// differently, --image-path has to be the host spelling. Handing runc the
// agent's /host-prefixed path fails as a missing descriptors.json, which
// reads like a corrupt artifact rather than a path-translation bug.
func TestCRIUImagePath(t *testing.T) {
	const bypass = "/mnt/fuse-nvme0n1/fuse-cache/snapshots/builds/x"
	for _, tc := range []struct {
		name          string
		checkpointDir string
		hostImageDir  string
		want          string
	}{
		{
			name:          "no bypass keeps the bundle's own path",
			checkpointDir: "/mnt/fuse/snapshots/builds/x/checkpoint",
			want:          "/mnt/fuse/snapshots/builds/x/checkpoint",
		},
		{
			name:          "bypass rewrites to the host spelling",
			checkpointDir: "/host" + bypass + "/checkpoint",
			hostImageDir:  bypass,
			want:          bypass + "/checkpoint",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := criuImagePath(tc.checkpointDir, tc.hostImageDir); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// A file:// artifact is on this node's own disk by definition, so pre-warm has
// nothing to fetch. Reading it anyway is not merely wasted work: it pulls the
// whole artifact through the page cache, and a restore measured after a
// deliberate drop_caches would then be timed against a warm cache it was
// supposed to be cold for. The number that comes out looks like a fast
// restore rather than like a broken measurement.
func TestPrewarmSkipsAFileArtifactAlreadyOnTheNode(t *testing.T) {
	dir := t.TempDir()
	ckpt := filepath.Join(dir, "checkpoint")
	if err := os.MkdirAll(ckpt, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ckpt, "pages-1.img"), make([]byte, 4096), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ckpt, "inventory.img"),
		append([]byte{0x19, 0x43, 0x56, 0x54}, make([]byte, 64)...), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := artifact.PublishDir(artifact.PublishDirOptions{
		Dir:  dir,
		Meta: &artifact.CheckpointMeta{ID: "cid", Name: "vllm_p_default_uid_0"},
		Spec: json.RawMessage(`{"ociVersion":"1.0.2"}`),
	})
	if err != nil {
		t.Fatal(err)
	}

	pr := &snapv1.PodRestore{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "r"},
		Status: snapv1.PodRestoreStatus{
			ArtifactURI: "file://" + dir + "/",
			TargetNode:  "node-a",
			Phase:       snapv1.RestorePhasePreWarming,
		},
	}
	scheme := runtime.NewScheme()
	if err := snapv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(pr).WithStatusSubresource(pr).Build()

	// No Pinner: pinning is a fuse-client call and must not be reached for a
	// node-local artifact. A nil dereference here is the test working.
	r := &RestoreReconciler{Client: c, NodeName: "node-a"}
	if _, err := r.prewarm(context.Background(), pr); err != nil {
		t.Fatalf("pre-warm of a node-local artifact failed: %v", err)
	}

	var got snapv1.PodRestore
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(pr), &got); err != nil {
		t.Fatal(err)
	}
	// The bytes are reported as present, because they are -- the manifest is
	// the evidence, not a read.
	if got.Status.PrewarmBytes != m.TotalBytes {
		t.Fatalf("PrewarmBytes is %d, want the manifest's %d", got.Status.PrewarmBytes, m.TotalBytes)
	}
	var cond string
	for _, c := range got.Status.Conditions {
		if c.Type == snapv1.ConditionPreWarmed {
			cond = c.Message
		}
	}
	if !strings.Contains(cond, "node-local") {
		t.Fatalf("the PreWarmed condition does not say the artifact was already local: %q", cond)
	}
}

// The restore side needs the same confinement as the dump side, and for a
// sharper reason: a restore reads the artifact and then hands its spec.dump to
// runc, so a file:// URI pointing anywhere on the node is a way to have a
// privileged agent execute a container description it found lying around.
func TestPrewarmRefusesAFileArtifactOutsideTheLocalRoot(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()

	pr := &snapv1.PodRestore{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "r"},
		Status: snapv1.PodRestoreStatus{
			ArtifactURI: "file://" + outside + "/",
			TargetNode:  "node-a",
			Phase:       snapv1.RestorePhasePreWarming,
		},
	}
	scheme := runtime.NewScheme()
	if err := snapv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(pr).WithStatusSubresource(pr).Build()

	r := &RestoreReconciler{Client: c, NodeName: "node-a", LocalArtifactRoot: root}
	if _, err := r.prewarm(context.Background(), pr); err != nil {
		t.Fatalf("prewarm returned an error instead of failing the restore: %v", err)
	}
	var got snapv1.PodRestore
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(pr), &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase != snapv1.RestorePhaseFailed {
		t.Fatalf("phase is %q, want Failed", got.Status.Phase)
	}
	if !strings.Contains(got.Status.Message, "outside this node's local artifact root") {
		t.Fatalf("the message does not say why it was refused: %q", got.Status.Message)
	}
}

// Confinement must not reach fuse:// artifacts. They are relative to the mount
// by construction and Parse has already rejected traversal, so applying the
// local root to them would break every distributed restore on a node that
// happens to also have an NVMe tier configured.
func TestPrewarmDoesNotConfineAFuseArtifact(t *testing.T) {
	pr := &snapv1.PodRestore{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "r"},
		Status: snapv1.PodRestoreStatus{
			ArtifactURI: "fuse:///snapshots/builds/qwen/",
			TargetNode:  "node-a",
			Phase:       snapv1.RestorePhasePreWarming,
		},
	}
	scheme := runtime.NewScheme()
	if err := snapv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(pr).WithStatusSubresource(pr).Build()

	// FuseMount points at an empty dir, so the manifest read misses and the
	// restore requeues waiting for the artifact -- the not-yet-published
	// path, not the refused path.
	r := &RestoreReconciler{
		Client: c, NodeName: "node-a",
		FuseMount:         t.TempDir(),
		LocalArtifactRoot: t.TempDir(),
	}
	if _, err := r.prewarm(context.Background(), pr); err != nil {
		t.Fatalf("prewarm of a fuse artifact failed: %v", err)
	}
	var got snapv1.PodRestore
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(pr), &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase == snapv1.RestorePhaseFailed {
		t.Fatalf("a fuse artifact was refused by the node-local confinement: %q", got.Status.Message)
	}
}
