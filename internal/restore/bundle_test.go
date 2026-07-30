package restore

import (
	"archive/tar"
	"os"
	"path/filepath"
	"testing"
)

// makeCheckpointTar writes a minimal CRI-shaped checkpoint archive.
func makeCheckpointTar(t *testing.T, path string, entries map[string]string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	tw := tar.NewWriter(f)
	defer tw.Close()
	for name, content := range entries {
		if content == "<dir>" {
			if err := tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeDir, Mode: 0o755}); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(content))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
}

func TestUnpack(t *testing.T) {
	dir := t.TempDir()
	tarPath := filepath.Join(dir, "ckpt.tar")
	rootfs := filepath.Join(dir, "rootfs")
	if err := os.MkdirAll(rootfs, 0o755); err != nil {
		t.Fatal(err)
	}

	makeCheckpointTar(t, tarPath, map[string]string{
		"checkpoint/":          "<dir>",
		"checkpoint/inventory": "img",
		"config.dump":          `{"id":"ctr1","sandbox":{"uid":"old-uid"}}`,
		"spec.dump":            `{"ociVersion":"1.1.0"}`,
	})

	b, err := Unpack(tarPath, filepath.Join(dir, "work"), rootfs)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(b.CheckpointDir, "inventory")); err != nil {
		t.Errorf("checkpoint dir not extracted: %v", err)
	}
	if b.ConfigDump["id"] != "ctr1" {
		t.Errorf("config.dump not parsed: %+v", b.ConfigDump)
	}
}

func TestUnpackRejectsNonCheckpoint(t *testing.T) {
	dir := t.TempDir()
	tarPath := filepath.Join(dir, "bad.tar")
	makeCheckpointTar(t, tarPath, map[string]string{"random.txt": "hello"})
	if _, err := Unpack(tarPath, filepath.Join(dir, "work"), ""); err == nil {
		t.Error("expected error for archive without checkpoint/ dir")
	}
}

func TestUnpackRejectsPathEscape(t *testing.T) {
	dir := t.TempDir()
	tarPath := filepath.Join(dir, "evil.tar")
	makeCheckpointTar(t, tarPath, map[string]string{"../escape.txt": "pwn"})
	if _, err := Unpack(tarPath, filepath.Join(dir, "work"), ""); err == nil {
		t.Error("expected error for path traversal entry")
	}
	if _, err := os.Stat(filepath.Join(dir, "escape.txt")); err == nil {
		t.Error("path traversal file was written")
	}
}

func TestUnpackAppliesRootfsDiff(t *testing.T) {
	dir := t.TempDir()
	tarPath := filepath.Join(dir, "ckpt.tar")
	rootfs := filepath.Join(dir, "rootfs")
	if err := os.MkdirAll(rootfs, 0o755); err != nil {
		t.Fatal(err)
	}

	// Build an inner rootfs-diff tar.
	diffPath := filepath.Join(dir, "inner-diff.tar")
	makeCheckpointTar(t, diffPath, map[string]string{"app/state.json": `{"n":42}`})
	diffBytes, err := os.ReadFile(diffPath)
	if err != nil {
		t.Fatal(err)
	}

	f, err := os.Create(tarPath)
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(f)
	for name, content := range map[string]string{
		"checkpoint/":          "<dir>",
		"checkpoint/inventory": "img",
		"spec.dump":            `{"ociVersion":"1.1.0"}`,
	} {
		if content == "<dir>" {
			tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeDir, Mode: 0o755})
			continue
		}
		tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(content))})
		tw.Write([]byte(content))
	}
	tw.WriteHeader(&tar.Header{Name: "rootfs-diff.tar", Mode: 0o644, Size: int64(len(diffBytes))})
	tw.Write(diffBytes)
	tw.Close()
	f.Close()

	if _, err := Unpack(tarPath, filepath.Join(dir, "work"), rootfs); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(rootfs, "app", "state.json")); err != nil {
		t.Errorf("rootfs-diff not applied: %v", err)
	}
}
