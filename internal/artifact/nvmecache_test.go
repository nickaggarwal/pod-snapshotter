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
	if got.Host != dir || got.Local != dir {
		t.Fatalf("got %+v, want both paths %q", got, dir)
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
	if got.Host != "" {
		t.Fatalf("expected decline, got %+v", got)
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
	if got.Host != "" {
		t.Fatalf("expected no path alongside the error, got %+v", got)
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
			if err != nil || got.Host != "" {
				t.Fatalf("got (%+v, %v), want a zero CacheDir and no error", got, err)
			}
		})
	}
}

// The path handed back must be the host's, not this process's, even though
// the checks are done through HostRoot. runc resolves it in the host mount
// namespace, so returning the /host-prefixed path fails at restore time as a
// missing file — which looks like an incomplete artifact rather than the
// path-translation bug it is.
func TestNVMeCacheResolveReturnsHostPathNotLocalPath(t *testing.T) {
	hostRoot := t.TempDir()
	hostView := "/mnt/fuse-nvme0n1/fuse-cache"
	dir := filepath.Join(hostRoot, hostView, "snapshots/builds/x")
	writeCacheFile(t, filepath.Join(dir, "checkpoint/pages-1.img"), 100)
	writeCacheFile(t, filepath.Join(dir, "checkpoint/core-1.img"), 20)

	got, err := NVMeCache{Root: hostView, HostRoot: hostRoot}.Resolve("snapshots/builds/x", testManifest())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	wantHost := filepath.Join(hostView, "snapshots/builds/x")
	if got.Host != wantHost {
		t.Fatalf("Host = %q, want the host path %q", got.Host, wantHost)
	}
	// And Local must be the one this process can actually open.
	wantLocal := filepath.Join(hostRoot, wantHost)
	if got.Local != wantLocal {
		t.Fatalf("Local = %q, want %q", got.Local, wantLocal)
	}
	if _, err := os.Stat(got.Local); err != nil {
		t.Fatalf("Local is not openable: %v", err)
	}
}

// With HostRoot set, a tier that exists only at the unprefixed path is not
// visible to this process and must not be claimed.
func TestNVMeCacheResolveChecksThroughHostRoot(t *testing.T) {
	root := t.TempDir()
	writeCacheFile(t, filepath.Join(root, "snapshots/builds/x/checkpoint/pages-1.img"), 100)
	writeCacheFile(t, filepath.Join(root, "snapshots/builds/x/checkpoint/core-1.img"), 20)

	got, err := NVMeCache{Root: root, HostRoot: t.TempDir()}.Resolve("snapshots/builds/x", testManifest())
	if err != nil || got.Host != "" {
		t.Fatalf("got (%+v, %v), want a decline", got, err)
	}
}
