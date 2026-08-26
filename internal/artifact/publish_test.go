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
	// pages-1.img is raw page data and carries no header; the metadata images
	// have to start with the CRIU magic, because publishing verifies it.
	if err := os.WriteFile(filepath.Join(ckpt, "pages-1.img"), []byte("PAGES"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"inventory.img", "core-1.img"} {
		body := append([]byte{0x19, 0x43, 0x56, 0x54}, []byte(name)...)
		if err := os.WriteFile(filepath.Join(ckpt, name), body, 0o644); err != nil {
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

// magic writes n bytes starting with the CRIU image header, the way a real
// image file looks to the four-byte check.
func writeImage(t *testing.T, dir, rel string, n int) {
	t.Helper()
	b := make([]byte, n)
	copy(b, []byte{0x19, 0x43, 0x56, 0x54})
	if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, rel)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, rel), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// The store has handed back correctly-sized runs of zeros for files whose
// bytes never reached origin -- a second node with its own cache read the same
// zeros, so it is a lost write, not a stale cache. Size checks cannot see it:
// the manifest records the right length and Prefetch verifies the right
// length. CRIU finds out 200 seconds into a restore, when files.img will not
// parse. Publishing must refuse instead.
func TestPublishRefusesAnImageThatIsAllZeros(t *testing.T) {
	dst := t.TempDir()
	writeImage(t, dst, "checkpoint/inventory.img", 99)
	writeImage(t, dst, "checkpoint/pages-1.img", 4096)
	// Right length, no content: exactly what was found on the node.
	if err := os.WriteFile(filepath.Join(dst, "checkpoint/files.img"), make([]byte, 45454), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := PublishDir(PublishDirOptions{
		Dir:  dst,
		Meta: &CheckpointMeta{ID: "abc", Name: "c_p_ns_uid_0"},
		Spec: []byte("{}"),
	})
	if err == nil {
		t.Fatal("committed a MANIFEST over an artifact that cannot restore")
	}
	if !strings.Contains(err.Error(), "files.img") {
		t.Fatalf("the error does not name the file that is wrong: %v", err)
	}
	// And nothing may be committed: the MANIFEST is what makes an artifact
	// look usable to a restore.
	if _, err := ReadManifestDir(dst); err == nil {
		t.Fatal("a MANIFEST was written despite the failure")
	}
}

// The check must not reject what CRIU legitimately produces. pages-*.img are
// raw page data with no header, and the tmpfs images are gzip.
func TestPublishAcceptsPagesAndGzipImages(t *testing.T) {
	dst := t.TempDir()
	writeImage(t, dst, "checkpoint/inventory.img", 99)
	// Raw page data: no CRIU header, and must not be read back anyway.
	if err := os.WriteFile(filepath.Join(dst, "checkpoint/pages-1.img"), []byte{0, 1, 2, 3, 4, 5, 6, 7}, 0o644); err != nil {
		t.Fatal(err)
	}
	// gzip magic, not CRIU magic.
	if err := os.WriteFile(filepath.Join(dst, "checkpoint/tmpfs-dev-321.tar.gz.img"),
		[]byte{0x1f, 0x8b, 0x08, 0x00, 0x09}, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := PublishDir(PublishDirOptions{
		Dir:  dst,
		Meta: &CheckpointMeta{ID: "abc", Name: "c_p_ns_uid_0"},
		Spec: []byte("{}"),
	}); err != nil {
		t.Fatalf("rejected an artifact CRIU legitimately produced: %v", err)
	}
}
