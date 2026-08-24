package controller

import (
	"testing"

	snapv1 "pod-snapshotter/api/v1alpha1"
	"pod-snapshotter/internal/artifact"
)

// The manager's --artifact-root decides where every snapshot that does not
// name its own URI lands, so the two reconcilers have to agree on it. They
// build their defaults from different functions against different key spaces
// (namespace/name/container vs builds/revision), and it would be entirely
// possible to thread the flag into one and leave the other on the compiled-in
// fuse:/// -- producing a cluster whose builds go to NVMe and whose ad-hoc
// snapshots quietly go somewhere else.
func TestBothReconcilersHangDefaultsOffTheConfiguredRoot(t *testing.T) {
	const nvme = "file:///mnt/fuse-nvme0n1/ps-artifacts"

	snapR := &PodSnapshotReconciler{ArtifactRoot: nvme}
	buildR := &SnapshotBuildReconciler{ArtifactRoot: nvme}

	got := artifact.DefaultURI(snapR.ArtifactRoot, "default", "s", "vllm", artifact.FormatDir)
	if want := nvme + "/default/s/vllm/"; got != want {
		t.Errorf("snapshot default URI = %q, want %q", got, want)
	}
	got = artifact.DefaultBuildURI(buildR.ArtifactRoot, "qwen-r1", artifact.FormatDir)
	if want := nvme + "/builds/qwen-r1/"; got != want {
		t.Errorf("build default URI = %q, want %q", got, want)
	}

	// An unset root is the upgrade case: an existing deployment that has not
	// added the flag must keep addressing the artifacts it already wrote.
	zero := &PodSnapshotReconciler{}
	got = artifact.DefaultURI(zero.ArtifactRoot, "default", "s", "vllm", artifact.FormatTar)
	if want := "fuse:///snapshots/default/s/vllm.tar"; got != want {
		t.Errorf("unset root changed the legacy default URI: got %q, want %q", got, want)
	}
}

// Routing the dump to the agent and pointing the artifact at the node's own
// NVMe are independent switches, and the agent path has to survive both
// settings of the other one -- the file:// scheme is not a reason to fall
// back to the kubelet, only a non-directory artifact is.
func TestAgentRoutingIsIndependentOfTheArtifactScheme(t *testing.T) {
	for _, uri := range []string{
		"fuse:///snapshots/default/s/vllm/",
		"file:///mnt/fuse-nvme0n1/ps-artifacts/default/s/vllm/",
	} {
		u, err := artifact.Parse(uri)
		if err != nil {
			t.Fatalf("Parse(%q): %v", uri, err)
		}
		if !u.Dir {
			t.Fatalf("%q should be a directory prefix", uri)
		}
		if u.Format() != artifact.FormatDir {
			t.Fatalf("%q format = %q, want dir -- the agent path requires it", uri, u.Format())
		}
	}
	// And the constant the routing compares against is still what the CRD
	// defaults to, so an empty spec routes to the agent rather than nowhere.
	if snapv1.CheckpointerAgent != "agent" {
		t.Fatalf("CheckpointerAgent = %q; the CRD default says agent", snapv1.CheckpointerAgent)
	}
}
