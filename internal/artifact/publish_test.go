package artifact

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// stage builds the on-disk shape a finished `runc checkpoint` leaves behind.
func stage(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	ckpt := filepath.Join(dir, "checkpoint")
	if err := os.MkdirAll(ckpt, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"pages-1.img":   "PAGES",
		"inventory.img": "INV",
		"core-1.img":    "CORE",
	} {
		if err := os.WriteFile(filepath.Join(ckpt, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func opts(dir string) PublishDirOptions {
	return PublishDirOptions{
		Dir:  dir,
		Meta: &CheckpointMeta{ID: "abc123", Name: "vllm_pod_default_uid_0", Runtime: "io.containerd.runc.v2"},
		Spec: json.RawMessage(`{"ociVersion":"1.0.2"}`),
	}
}

func TestPublishDirWritesMetadataAndManifest(t *testing.T) {
	dir := stage(t)

	m, err := PublishDir(opts(dir))
	if err != nil {
		t.Fatal(err)
	}

	// The two files the restore actually parses have to be there.
	for _, name := range []string{"config.dump", "spec.dump"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("%s missing: %v", name, err)
		}
	}
	// config.dump must round-trip as the same shape the kubelet writes, since
	// the restore cannot tell the two producers apart.
	raw, err := os.ReadFile(filepath.Join(dir, "config.dump"))
	if err != nil {
		t.Fatal(err)
	}
	var back CheckpointMeta
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("config.dump is not valid checkpoint metadata: %v", err)
	}
	if back.ID != "abc123" {
		t.Fatalf("config.dump lost the container id: %+v", back)
	}

	// Every file on disk except the MANIFEST is described.
	want := map[string]bool{
		"checkpoint/pages-1.img": true, "checkpoint/inventory.img": true,
		"checkpoint/core-1.img": true, "config.dump": true, "spec.dump": true,
	}
	got := map[string]bool{}
	for _, f := range m.Files {
		got[f.Path] = true
		if f.Path == ManifestName {
			t.Fatal("the MANIFEST described itself")
		}
	}
	for p := range want {
		if !got[p] {
			t.Fatalf("manifest is missing %s; has %v", p, got)
		}
	}

	// SourceTar is what distinguishes an agent-dumped artifact from a
	// kubelet-dumped one, and there was no tar here.
	if m.SourceTar != "" {
		t.Fatalf("SourceTar should be empty for a direct dump, got %q", m.SourceTar)
	}
	if _, err := ReadManifestDir(dir); err != nil {
		t.Fatalf("published artifact does not read back: %v", err)
	}
}

// A dump that wrote no memory images is a dump that did not happen. Failing
// at publish time beats failing at restore time, hours later.
func TestPublishDirRejectsAnArtifactWithNoPages(t *testing.T) {
	dir := t.TempDir()
	ckpt := filepath.Join(dir, "checkpoint")
	if err := os.MkdirAll(ckpt, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ckpt, "inventory.img"), []byte("INV"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := PublishDir(opts(dir))
	if err == nil || !strings.Contains(err.Error(), "pages-") {
		t.Fatalf("want a no-pages error, got %v", err)
	}
}

func TestPublishDirRequiresTheSpec(t *testing.T) {
	dir := stage(t)
	o := opts(dir)
	o.Spec = nil

	_, err := PublishDir(o)
	if err == nil || !strings.Contains(err.Error(), "spec.dump") {
		t.Fatalf("want a missing-spec error, got %v", err)
	}
}

func TestPublishDirRequiresTheImagesToExist(t *testing.T) {
	dir := t.TempDir() // no checkpoint/ subdir

	_, err := PublishDir(opts(dir))
	if err == nil || !strings.Contains(err.Error(), "no CRIU images") {
		t.Fatalf("want a missing-images error, got %v", err)
	}
}

// The MANIFEST is the commit marker, so a republish must clear the old one
// before it starts writing: a reader that arrives mid-rewrite has to see an
// uncommitted prefix, not a manifest describing files that no longer match.
func TestPublishDirClearsAStaleManifestBeforeWriting(t *testing.T) {
	dir := stage(t)
	if err := os.WriteFile(filepath.Join(dir, ManifestName), []byte("{stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Make the publish fail after the clear.
	o := opts(dir)
	o.Contribute = func(string) ([]ManifestFile, error) { return nil, os.ErrPermission }

	if _, err := PublishDir(o); err == nil {
		t.Fatal("expected the contributor error to surface")
	}
	if _, err := os.Stat(filepath.Join(dir, ManifestName)); !os.IsNotExist(err) {
		t.Fatal("the stale MANIFEST survived a failed publish; the prefix reads as committed")
	}
}

// Whatever the contributor writes (this is how /dev/shm gets in) must be in
// the manifest exactly once, not also picked up by the directory walk.
func TestPublishDirDoesNotDoubleCountContributedFiles(t *testing.T) {
	dir := stage(t)
	o := opts(dir)
	o.Contribute = func(d string) ([]ManifestFile, error) {
		if err := os.WriteFile(filepath.Join(d, "shm-diff.tar"), []byte("SHM"), 0o644); err != nil {
			return nil, err
		}
		mf, err := DescribeFile(d, "shm-diff.tar")
		return []ManifestFile{mf}, err
	}

	m, err := PublishDir(o)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, f := range m.Files {
		if f.Path == "shm-diff.tar" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("shm-diff.tar appears %d times in the manifest, want 1", n)
	}
}
