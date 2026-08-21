package artifact

import (
	"os"
	"path/filepath"
	"testing"
)

func writeCacheFile(t *testing.T, p string, n int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, make([]byte, n), 0o644); err != nil {
		t.Fatal(err)
	}
}

func testManifest() *Manifest {
	return &Manifest{Files: []ManifestFile{
		{Path: "checkpoint/pages-1.img", Size: 100},
		{Path: "checkpoint/core-1.img", Size: 20},
	}}
}

func TestNVMeCacheResolveComplete(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "snapshots/builds/x")
	writeCacheFile(t, filepath.Join(dir, "checkpoint/pages-1.img"), 100)
	writeCacheFile(t, filepath.Join(dir, "checkpoint/core-1.img"), 20)

	got, err := NVMeCache{Root: root}.Resolve("snapshots/builds/x", testManifest())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != dir {
		t.Fatalf("got %q, want %q", got, dir)
	}
}

// A tier missing one file must decline silently: the mount still has the real
// bytes, so this is a fallback, not a failure.
func TestNVMeCacheResolveDeclinesWhenIncomplete(t *testing.T) {
	root := t.TempDir()
	writeCacheFile(t, filepath.Join(root, "snapshots/builds/x/checkpoint/pages-1.img"), 100)

	got, err := NVMeCache{Root: root}.Resolve("snapshots/builds/x", testManifest())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "" {
		t.Fatalf("expected decline, got %q", got)
	}
}

// A size disagreement is different: the tier claims to have the file but does
// not mirror it. That is reportable, because it means the layout assumption
// has broken rather than the file simply not being promoted yet.
func TestNVMeCacheResolveReportsSizeMismatch(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "snapshots/builds/x")
	writeCacheFile(t, filepath.Join(dir, "checkpoint/pages-1.img"), 99)
	writeCacheFile(t, filepath.Join(dir, "checkpoint/core-1.img"), 20)

	got, err := NVMeCache{Root: root}.Resolve("snapshots/builds/x", testManifest())
	if err == nil {
		t.Fatal("expected an error for a size mismatch")
	}
	if got != "" {
		t.Fatalf("expected no path alongside the error, got %q", got)
	}
}

func TestNVMeCacheDisabled(t *testing.T) {
	for _, tc := range []struct {
		name string
		root string
		m    *Manifest
	}{
		{"empty root", "", testManifest()},
		{"nil manifest", t.TempDir(), nil},
		{"absent dir", t.TempDir(), testManifest()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NVMeCache{Root: tc.root}.Resolve("snapshots/builds/x", tc.m)
			if err != nil || got != "" {
				t.Fatalf("got (%q, %v), want (\"\", nil)", got, err)
			}
		})
	}
}
