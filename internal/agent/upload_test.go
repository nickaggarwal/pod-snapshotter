package agent

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestCopyAtomic(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.tar")
	dst := filepath.Join(dir, "fuse", "snapshots", "ns", "s1", "ctr.tar")

	content := []byte("checkpoint bytes here")
	if err := os.WriteFile(src, content, 0o644); err != nil {
		t.Fatal(err)
	}

	sum, n, err := copyAtomic(src, dst)
	if err != nil {
		t.Fatal(err)
	}
	if n != int64(len(content)) {
		t.Errorf("n = %d, want %d", n, len(content))
	}
	want := fmt.Sprintf("%x", sha256.Sum256(content))
	if sum != want {
		t.Errorf("sha = %s, want %s", sum, want)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(content) {
		t.Error("content mismatch")
	}
	// No .part remnants.
	if _, err := os.Stat(dst + ".part"); !os.IsNotExist(err) {
		t.Error(".part file left behind")
	}
}

func TestCopyAtomicMissingSource(t *testing.T) {
	dir := t.TempDir()
	if _, _, err := copyAtomic(filepath.Join(dir, "nope.tar"), filepath.Join(dir, "out.tar")); err == nil {
		t.Error("expected error for missing source")
	}
}
