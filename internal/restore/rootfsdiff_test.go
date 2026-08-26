package restore

import (
	"archive/tar"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestOverlayUpperDir(t *testing.T) {
	const overlayLine = `1877 1653 0:196 / / rw,relatime master:1 - overlay overlay rw,lowerdir=/var/lib/containerd/x/1:/var/lib/containerd/x/2,upperdir=/var/lib/containerd/snapshots/99/fs,workdir=/var/lib/containerd/snapshots/99/work`

	cases := []struct {
		name      string
		mountinfo string
		want      string
	}{
		{
			name:      "overlay root",
			mountinfo: "1650 1649 0:56 / /proc rw - proc proc rw\n" + overlayLine + "\n",
			want:      "/var/lib/containerd/snapshots/99/fs",
		},
		{
			// A mount whose ROOT field is /something is a bind of a subtree,
			// not the container's own root; taking its upperdir would tar the
			// wrong layer.
			name:      "an overlay bind that is not the root is skipped",
			mountinfo: `1877 1653 0:196 /sub /data rw - overlay overlay rw,upperdir=/wrong` + "\n",
			want:      "",
		},
		{
			// Not every snapshotter is overlayfs. No upperdir is a fact about
			// the node, not a failure.
			name:      "non-overlay root",
			mountinfo: "1877 1653 0:196 / / rw - ext4 /dev/sda1 rw\n",
			want:      "",
		},
		{
			name:      "overlay without an upperdir option",
			mountinfo: "1877 1653 0:196 / / ro - overlay overlay ro,lowerdir=/a:/b\n",
			want:      "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseOverlayUpper([]byte(tc.mountinfo))
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func members(t *testing.T, path string) map[string]string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	out := map[string]string{}
	tr := tar.NewReader(f)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		out[hdr.Name] = string(body)
	}
	return out
}

func TestCaptureRootfsDiff(t *testing.T) {
	upper := t.TempDir()
	if err := os.MkdirAll(filepath.Join(upper, "var/log"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(upper, "var/log/app.log"), []byte("started"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/var/log/app.log", filepath.Join(upper, "current")); err != nil {
		t.Fatal(err)
	}

	out := filepath.Join(t.TempDir(), RootfsDiffName)
	captured, err := CaptureRootfsDiff(upper, "", out)
	if err != nil {
		t.Fatal(err)
	}
	if !captured {
		t.Fatal("a layer with writes in it reported nothing to capture")
	}

	got := members(t, out)
	if got["var/log/app.log"] != "started" {
		t.Fatalf("the container's own write is missing or wrong: %v", got)
	}
	// Directories have to travel too: the extractor creates parents from
	// headers, and a diff of files alone loses their modes.
	if _, ok := got["var/log/"]; !ok {
		t.Fatalf("directory entries missing: %v", got)
	}
	if _, ok := got["current"]; !ok {
		t.Fatalf("symlink missing: %v", got)
	}
	// Paths are relative to the layer root, since they are applied onto the
	// new container's rootfs. An absolute path here would escape it.
	for name := range got {
		if filepath.IsAbs(name) {
			t.Fatalf("absolute path in the diff: %q", name)
		}
	}
}

// An empty writable layer is the common case for a container that only ever
// wrote to its mounts. Writing a valid-but-empty tar would put a member in
// the manifest that means nothing.
func TestCaptureRootfsDiffSkipsAnEmptyLayer(t *testing.T) {
	out := filepath.Join(t.TempDir(), RootfsDiffName)

	captured, err := CaptureRootfsDiff(t.TempDir(), "", out)
	if err != nil {
		t.Fatal(err)
	}
	if captured {
		t.Fatal("an empty layer should not produce a member")
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Fatal("an empty tar was left behind")
	}
}

// No overlay means no diff, and that is not an error: the restore then reuses
// the keeper's rootfs unchanged, which is what it does today.
func TestCaptureRootfsDiffWithNoUpperDir(t *testing.T) {
	captured, err := CaptureRootfsDiff("", "", filepath.Join(t.TempDir(), RootfsDiffName))
	if err != nil || captured {
		t.Fatalf("captured=%v err=%v; want false, nil", captured, err)
	}
}
