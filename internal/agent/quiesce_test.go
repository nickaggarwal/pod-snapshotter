package agent

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	snapv1 "pod-snapshotter/api/v1alpha1"
	"pod-snapshotter/internal/artifact"
)

func podTemplateWithAnnotations(a map[string]string) corev1.PodTemplateSpec {
	return corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Annotations: a}}
}

func TestResolveResumeDir(t *testing.T) {
	presence := snapv1.QuiesceModePresenceFile
	quiescedManifest := &artifact.Manifest{Quiesce: &artifact.QuiesceInfo{Mode: presence, Dir: "/from-manifest"}}

	cases := []struct {
		name     string
		pr       *snapv1.PodRestore
		manifest *artifact.Manifest
		want     string
	}{
		{
			name: "no quiesce anywhere is the v1 path",
			pr:   &snapv1.PodRestore{},
			want: "",
		},
		{
			name: "artifact manifest alone is enough",
			pr:   &snapv1.PodRestore{},
			// A quiesced checkpoint is self-describing: the PodRestore needs
			// no annotations for the agent to know where to drop the marker.
			manifest: quiescedManifest,
			want:     "/from-manifest",
		},
		{
			name: "manifest without a dir falls back to the default",
			pr:   &snapv1.PodRestore{},
			manifest: &artifact.Manifest{
				Quiesce: &artifact.QuiesceInfo{Mode: presence},
			},
			want: snapv1.DefaultQuiesceDir,
		},
		{
			name: "pod template annotation, no manifest (tar artifact)",
			pr: &snapv1.PodRestore{
				Spec: snapv1.PodRestoreSpec{PodTemplate: podTemplateWithAnnotations(map[string]string{
					snapv1.QuiesceAnnotation:    presence,
					snapv1.QuiesceDirAnnotation: "/from-template",
				})},
			},
			want: "/from-template",
		},
		{
			name: "PodRestore annotation overrides the manifest",
			pr: &snapv1.PodRestore{
				ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
					snapv1.QuiesceAnnotation:    presence,
					snapv1.QuiesceDirAnnotation: "/override",
				}},
			},
			manifest: quiescedManifest,
			want:     "/override",
		},
		{
			name: "annotation without a dir uses the default",
			pr: &snapv1.PodRestore{
				ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
					snapv1.QuiesceAnnotation: presence,
				}},
			},
			want: snapv1.DefaultQuiesceDir,
		},
		{
			name: "an unrecognized mode is not honored",
			pr: &snapv1.PodRestore{
				ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
					snapv1.QuiesceAnnotation: "some-future-protocol",
				}},
			},
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveResumeDir(tc.pr, tc.manifest); got != tc.want {
				t.Errorf("resolveResumeDir = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestWriteResumeMarkerNeedsAPID(t *testing.T) {
	if err := writeResumeMarker(0, "/snapshot"); err == nil {
		t.Error("expected an error without a keeper PID to resolve through")
	}
}

func TestContainerPath(t *testing.T) {
	got := containerPath(1234, "/snapshot", snapv1.ReadyForCheckpointFile)
	want := filepath.Join("/proc/1234/root", "snapshot", snapv1.ReadyForCheckpointFile)
	if got != want {
		t.Errorf("containerPath = %q, want %q", got, want)
	}
}

// TestWriteResumeMarkerContent exercises the marker write against this
// process's own /proc/<pid>/root, which on Linux is the real filesystem root.
func TestWriteResumeMarkerContent(t *testing.T) {
	root := filepath.Join("/proc", strconv.Itoa(os.Getpid()), "root")
	if _, err := os.Stat(root); err != nil {
		t.Skipf("no /proc/<pid>/root on this platform: %v", err)
	}
	dir := t.TempDir()
	if err := writeResumeMarker(os.Getpid(), dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, snapv1.RestoreCompleteFile)); err != nil {
		t.Errorf("resume marker not created: %v", err)
	}
}
