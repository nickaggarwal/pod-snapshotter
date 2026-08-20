package artifact

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeCheckpointTar builds a minimal CRI checkpoint archive.
func writeCheckpointTar(t *testing.T, path string, files map[string]string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	tw := tar.NewWriter(f)
	// A directory entry, as containerd emits.
	if err := tw.WriteHeader(&tar.Header{Name: "checkpoint/", Typeflag: tar.TypeDir, Mode: 0o755}); err != nil {
		t.Fatal(err)
	}
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{
			Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(body)),
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestExpandTarToDir(t *testing.T) {
	dir := t.TempDir()
	tarPath := filepath.Join(dir, "checkpoint.tar")
	content := map[string]string{
		"checkpoint/pages-1.img": "PAGES",
		"checkpoint/core-1.img":  "CORE",
		"spec.dump":              `{"ociVersion":"1.0.0"}`,
		"config.dump":            `{"name":"c_p_ns_uid_0"}`,
		"dump.log":               "log",
	}
	writeCheckpointTar(t, tarPath, content)

	dst := filepath.Join(dir, "artifact")
	m, err := ExpandTarToDir(tarPath, dst, &QuiesceInfo{Mode: "presence-file", Dir: "/snapshot"}, nil)
	if err != nil {
		t.Fatal(err)
	}

	if len(m.Files) != len(content) {
		t.Errorf("manifest lists %d files, want %d", len(m.Files), len(content))
	}
	if m.Format != FormatDir || m.Version != ManifestVersion {
		t.Errorf("unexpected manifest header: %+v", m)
	}
	if m.SourceTar != "checkpoint.tar" {
		t.Errorf("SourceTar = %q", m.SourceTar)
	}
	if m.Quiesce == nil || m.Quiesce.Dir != "/snapshot" {
		t.Errorf("quiesce info not carried into the manifest: %+v", m.Quiesce)
	}

	var total int64
	for _, f := range m.Files {
		body, ok := content[f.Path]
		if !ok {
			t.Fatalf("manifest lists unexpected file %q", f.Path)
		}
		total += int64(len(body))
		if f.Size != int64(len(body)) {
			t.Errorf("%s: size %d, want %d", f.Path, f.Size, len(body))
		}
		sum := sha256.Sum256([]byte(body))
		if f.SHA256 != hex.EncodeToString(sum[:]) {
			t.Errorf("%s: digest mismatch", f.Path)
		}
		got, err := os.ReadFile(filepath.Join(dst, filepath.FromSlash(f.Path)))
		if err != nil {
			t.Fatalf("%s: %v", f.Path, err)
		}
		if string(got) != body {
			t.Errorf("%s: content %q, want %q", f.Path, got, body)
		}
	}
	if m.TotalBytes != total {
		t.Errorf("TotalBytes = %d, want %d", m.TotalBytes, total)
	}

	// Sorted for stable output.
	for i := 1; i < len(m.Files); i++ {
		if m.Files[i-1].Path > m.Files[i].Path {
			t.Fatalf("manifest not sorted: %q before %q", m.Files[i-1].Path, m.Files[i].Path)
		}
	}

	// No .part litter: the MANIFEST commits the tree, so image files are
	// written straight to their final names.
	var parts []string
	_ = filepath.Walk(dst, func(p string, _ os.FileInfo, _ error) error {
		if strings.HasSuffix(p, ".part") {
			parts = append(parts, p)
		}
		return nil
	})
	if len(parts) > 0 {
		t.Errorf("left temp files behind: %v", parts)
	}

	// Round-trips through the reader.
	got, err := ReadManifestDir(dst)
	if err != nil {
		t.Fatal(err)
	}
	if got.TotalBytes != m.TotalBytes || len(got.Files) != len(m.Files) {
		t.Errorf("round-tripped manifest differs: %+v", got)
	}
}

func TestExpandTarToDirClearsStaleManifest(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "artifact")
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatal(err)
	}
	// A MANIFEST from an earlier, unrelated upload.
	if err := os.WriteFile(filepath.Join(dst, ManifestName), []byte(`{"version":1,"files":[{"path":"stale"}]}`), 0o644); err != nil {
		t.Fatal(err)
	}

	// An archive that fails partway: no checkpoint/ payload, a symlink entry.
	tarPath := filepath.Join(dir, "bad.tar")
	f, err := os.Create(tarPath)
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(f)
	if err := tw.WriteHeader(&tar.Header{Name: "spec.dump", Typeflag: tar.TypeReg, Mode: 0o644, Size: 2}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte("hi")); err != nil {
		t.Fatal(err)
	}
	if err := tw.WriteHeader(&tar.Header{Name: "evil", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"}); err != nil {
		t.Fatal(err)
	}
	tw.Close()
	f.Close()

	if _, err := ExpandTarToDir(tarPath, dst, nil, nil); err == nil {
		t.Fatal("expected the symlink entry to be rejected")
	}
	if _, err := os.Stat(filepath.Join(dst, ManifestName)); !os.IsNotExist(err) {
		t.Error("a failed expansion must leave the prefix uncommitted (no MANIFEST)")
	}
}

func TestExpandTarToDirRejectsTraversal(t *testing.T) {
	dir := t.TempDir()
	tarPath := filepath.Join(dir, "evil.tar")
	f, err := os.Create(tarPath)
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(f)
	if err := tw.WriteHeader(&tar.Header{Name: "../escape", Typeflag: tar.TypeReg, Mode: 0o644, Size: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	tw.Close()
	f.Close()

	if _, err := ExpandTarToDir(tarPath, filepath.Join(dir, "out"), nil, nil); err == nil {
		t.Fatal("expected traversal to be rejected")
	}
}

func TestPrefetchWarmAndStage(t *testing.T) {
	dir := t.TempDir()
	tarPath := filepath.Join(dir, "checkpoint.tar")
	content := map[string]string{
		"checkpoint/pages-1.img": strings.Repeat("a", 4096),
		"checkpoint/pages-2.img": strings.Repeat("b", 8192),
		"spec.dump":              "{}",
	}
	writeCheckpointTar(t, tarPath, content)
	src := filepath.Join(dir, "artifact")
	m, err := ExpandTarToDir(tarPath, src, nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	// Warm-only: reads everything, writes nothing.
	n, err := Prefetch(context.Background(), PrefetchOpts{SrcDir: src, Manifest: m, Verify: true})
	if err != nil {
		t.Fatal(err)
	}
	if n != m.TotalBytes {
		t.Errorf("warmed %d bytes, want %d", n, m.TotalBytes)
	}

	// Staging: a byte-identical copy on node-local storage.
	stage := filepath.Join(dir, "stage")
	n, err = Prefetch(context.Background(), PrefetchOpts{SrcDir: src, Manifest: m, StageDir: stage, Parallelism: 2, Verify: true})
	if err != nil {
		t.Fatal(err)
	}
	if n != m.TotalBytes {
		t.Errorf("staged %d bytes, want %d", n, m.TotalBytes)
	}
	for path, body := range content {
		got, err := os.ReadFile(filepath.Join(stage, filepath.FromSlash(path)))
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if string(got) != body {
			t.Errorf("%s: staged content differs", path)
		}
	}
}

func TestPrefetchDetectsTruncatedFile(t *testing.T) {
	dir := t.TempDir()
	tarPath := filepath.Join(dir, "checkpoint.tar")
	writeCheckpointTar(t, tarPath, map[string]string{"checkpoint/pages-1.img": "0123456789"})
	src := filepath.Join(dir, "artifact")
	m, err := ExpandTarToDir(tarPath, src, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "checkpoint", "pages-1.img"), []byte("012"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Prefetch(context.Background(), PrefetchOpts{SrcDir: src, Manifest: m}); err == nil {
		t.Fatal("expected a size mismatch against the manifest")
	}
}

func TestDecodeManifestRejectsTraversal(t *testing.T) {
	_, err := DecodeManifest(strings.NewReader(`{"version":1,"files":[{"path":"../etc/passwd"}]}`))
	if err == nil {
		t.Fatal("expected a traversing manifest entry to be rejected")
	}
}

func TestExpandTarToDirContributedFiles(t *testing.T) {
	dir := t.TempDir()
	tarPath := filepath.Join(dir, "checkpoint.tar")
	writeCheckpointTar(t, tarPath, map[string]string{
		"checkpoint/pages-1.img": "PAGES",
		"spec.dump":              `{"ociVersion":"1.0.0"}`,
	})
	dst := filepath.Join(dir, "artifact")

	// A contributor sees the expanded tree (so it can read spec.dump) and
	// adds state the CRI archive does not carry.
	contribute := func(dstDir string) ([]ManifestFile, error) {
		if _, err := os.Stat(filepath.Join(dstDir, "spec.dump")); err != nil {
			t.Errorf("contributor ran before the archive was expanded: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dstDir, "shm-diff.tar"), []byte("SHM"), 0o644); err != nil {
			return nil, err
		}
		e, err := DescribeFile(dstDir, "shm-diff.tar")
		return []ManifestFile{e}, err
	}

	m, err := ExpandTarToDir(tarPath, dst, nil, contribute)
	if err != nil {
		t.Fatal(err)
	}
	var found *ManifestFile
	for i := range m.Files {
		if m.Files[i].Path == "shm-diff.tar" {
			found = &m.Files[i]
		}
	}
	if found == nil {
		t.Fatalf("contributed file missing from the manifest: %+v", m.Files)
	}
	if found.Size != 3 || found.SHA256 == "" {
		t.Errorf("contributed entry not described: %+v", *found)
	}
	if m.TotalBytes != int64(len("PAGES")+len(`{"ociVersion":"1.0.0"}`)+3) {
		t.Errorf("TotalBytes = %d, does not include the contributed file", m.TotalBytes)
	}

	// A contributor failure leaves the artifact uncommitted.
	dst2 := filepath.Join(dir, "artifact2")
	if _, err := ExpandTarToDir(tarPath, dst2, nil, func(string) ([]ManifestFile, error) {
		return nil, errContributor
	}); err == nil {
		t.Fatal("expected the contributor error to fail the expansion")
	}
	if _, err := os.Stat(filepath.Join(dst2, ManifestName)); !os.IsNotExist(err) {
		t.Error("a failed contributor must leave the prefix uncommitted")
	}
}

var errContributor = errors.New("contributor blew up")
