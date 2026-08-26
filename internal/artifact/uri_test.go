package artifact

import "testing"

func TestParse(t *testing.T) {
	tests := []struct {
		in       string
		wantErr  bool
		scheme   string
		path     string
		hostPath string
		fusePath string
	}{
		{in: "fuse:///snapshots/default/s1/vllm.tar", scheme: "fuse", path: "/snapshots/default/s1/vllm.tar",
			hostPath: "/mnt/fuse/snapshots/default/s1/vllm.tar", fusePath: "snapshots/default/s1/vllm.tar"},
		{in: "file:///tmp/x.tar", scheme: "file", path: "/tmp/x.tar", hostPath: "/tmp/x.tar", fusePath: "tmp/x.tar"},
		{in: "s3://bucket/key.tar", wantErr: true},
		{in: "fuse://host/path.tar", wantErr: true},   // host component not allowed
		{in: "fuse:///", wantErr: true},               // empty path
		{in: "fuse:///a/../../etc/pw", wantErr: true}, // escapes
		{in: "://bad", wantErr: true},
	}
	for _, tc := range tests {
		u, err := Parse(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("Parse(%q): expected error, got %+v", tc.in, u)
			}
			continue
		}
		if err != nil {
			t.Errorf("Parse(%q): %v", tc.in, err)
			continue
		}
		if u.Scheme != tc.scheme || u.Path != tc.path {
			t.Errorf("Parse(%q) = %+v, want scheme=%s path=%s", tc.in, u, tc.scheme, tc.path)
		}
		if got := u.HostPath("/mnt/fuse"); got != tc.hostPath {
			t.Errorf("HostPath(%q) = %q, want %q", tc.in, got, tc.hostPath)
		}
		if got := u.FusePath(); got != tc.fusePath {
			t.Errorf("FusePath(%q) = %q, want %q", tc.in, got, tc.fusePath)
		}
	}
}

func TestDefaultURI(t *testing.T) {
	for _, tc := range []struct{ root, format, want string }{
		{"", FormatTar, "fuse:///snapshots/default/snap1/vllm.tar"},
		{"", "", "fuse:///snapshots/default/snap1/vllm.tar"},
		{"", FormatDir, "fuse:///snapshots/default/snap1/vllm/"},
		// An empty root means DefaultRoot, and naming DefaultRoot explicitly
		// must give the same answer -- otherwise the manager's flag default
		// and its zero value are two different storage locations.
		{DefaultRoot, FormatDir, "fuse:///snapshots/default/snap1/vllm/"},
		// The node-local tier. This is the whole point of the parameter: the
		// scheme carries "this artifact is on one node's disk" through to
		// every consumer, and the trailing slash still decides the format.
		{"file:///mnt/fuse-nvme0n1/ps-artifacts", FormatDir, "file:///mnt/fuse-nvme0n1/ps-artifacts/default/snap1/vllm/"},
		{"file:///mnt/fuse-nvme0n1/ps-artifacts", FormatTar, "file:///mnt/fuse-nvme0n1/ps-artifacts/default/snap1/vllm.tar"},
		// A trailing slash on the root must not double up in the result.
		{"file:///mnt/nvme/ps/", FormatDir, "file:///mnt/nvme/ps/default/snap1/vllm/"},
	} {
		got := DefaultURI(tc.root, "default", "snap1", "vllm", tc.format)
		if got != tc.want {
			t.Errorf("DefaultURI(root=%q, format=%q) = %q, want %q", tc.root, tc.format, got, tc.want)
		}
		u, err := Parse(got)
		if err != nil {
			t.Fatalf("DefaultURI output %q does not parse: %v", got, err)
		}
		wantFormat := tc.format
		if wantFormat == "" {
			wantFormat = FormatTar
		}
		if u.Format() != wantFormat {
			t.Errorf("Parse(%q).Format() = %q, want %q", got, u.Format(), wantFormat)
		}
		if u.String() != got {
			t.Errorf("round-trip of %q gave %q", got, u.String())
		}
	}
}

func TestParseDirPrefix(t *testing.T) {
	u, err := Parse("fuse:///snapshots/ns/name/ctr/")
	if err != nil {
		t.Fatal(err)
	}
	if !u.Dir {
		t.Fatalf("trailing slash should mark a directory prefix: %+v", u)
	}
	if u.Path != "/snapshots/ns/name/ctr" {
		t.Errorf("Path = %q, want the cleaned path without the trailing slash", u.Path)
	}
	if got := u.HostPath("/mnt/fuse"); got != "/mnt/fuse/snapshots/ns/name/ctr" {
		t.Errorf("HostPath = %q", got)
	}
	if got := u.CommitPath().String(); got != "fuse:///snapshots/ns/name/ctr/MANIFEST" {
		t.Errorf("CommitPath = %q, want the MANIFEST", got)
	}

	f, err := u.Join("checkpoint/pages-1.img")
	if err != nil {
		t.Fatal(err)
	}
	if f.String() != "fuse:///snapshots/ns/name/ctr/checkpoint/pages-1.img" {
		t.Errorf("Join = %q", f.String())
	}
	if _, err := u.Join("../../escape"); err == nil {
		t.Error("Join should reject traversal out of the prefix")
	}

	tarURI, err := Parse("fuse:///snapshots/ns/name/ctr.tar")
	if err != nil {
		t.Fatal(err)
	}
	if tarURI.Dir {
		t.Error("a plain object URI must not be a directory prefix")
	}
	if _, err := tarURI.Join("x"); err == nil {
		t.Error("Join on a non-directory URI should fail")
	}
	if tarURI.CommitPath() != tarURI {
		t.Error("CommitPath of a tar artifact is the tar itself")
	}
}

func TestDefaultBuildURI(t *testing.T) {
	for _, tc := range []struct{ root, format, want string }{
		{"", FormatDir, "fuse:///snapshots/builds/qwen-r1/"},
		{"", FormatTar, "fuse:///snapshots/builds/qwen-r1.tar"},
		{"file:///mnt/fuse-nvme0n1/ps-artifacts", FormatDir, "file:///mnt/fuse-nvme0n1/ps-artifacts/builds/qwen-r1/"},
		{"file:///mnt/nvme/ps/", FormatDir, "file:///mnt/nvme/ps/builds/qwen-r1/"},
	} {
		got := DefaultBuildURI(tc.root, "qwen-r1", tc.format)
		if got != tc.want {
			t.Errorf("DefaultBuildURI(root=%q, format=%q) = %q, want %q", tc.root, tc.format, got, tc.want)
		}
		if _, err := Parse(got); err != nil {
			t.Errorf("DefaultBuildURI output %q does not parse: %v", got, err)
		}
	}
}

// ParseRoot is where a misconfigured root is supposed to stop, so that a bad
// value is one startup error rather than an identical parse failure on every
// snapshot the cluster subsequently takes.
func TestParseRoot(t *testing.T) {
	for _, tc := range []struct {
		in, want string
		wantErr  bool
	}{
		{"", DefaultRoot, false},
		{"fuse:///snapshots", "fuse:///snapshots", false},
		{"fuse:///snapshots/", "fuse:///snapshots", false},
		{"file:///mnt/fuse-nvme0n1/ps-artifacts", "file:///mnt/fuse-nvme0n1/ps-artifacts", false},
		// A root with no scheme is the likely typo -- someone pastes the host
		// path the chart mounts and drops the file:// in front of it.
		{"/mnt/fuse-nvme0n1/ps-artifacts", "", true},
		{"s3://bucket/snapshots", "", true},
		{"fuse://host/snapshots", "", true},
		{"fuse:///snapshots/../etc", "", true},
	} {
		got, err := ParseRoot(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("ParseRoot(%q) = %q, want an error", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseRoot(%q) failed: %v", tc.in, err)
		} else if got != tc.want {
			t.Errorf("ParseRoot(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// A file:// URI is a raw host path executed by a privileged agent, so the
// confinement check is the difference between "an artifact tier" and "write
// anywhere on the node as root".
func TestCheckLocalRoot(t *testing.T) {
	const root = "/mnt/fuse-nvme0n1/ps-artifacts"
	for _, tc := range []struct {
		uri, root string
		wantErr   bool
	}{
		{"file:///mnt/fuse-nvme0n1/ps-artifacts/builds/qwen/", root, false},
		{"file:///mnt/fuse-nvme0n1/ps-artifacts", root, false},
		{"file:///mnt/fuse-nvme0n1/ps-artifacts/", root, false},
		// Outside the tier entirely.
		{"file:///var/lib/kubelet/pods", root, true},
		// A prefix of the root's name is not inside the root: this is the
		// sibling-directory case that a plain HasPrefix would wave through.
		{"file:///mnt/fuse-nvme0n1/ps-artifacts-evil/x/", root, true},
		// fuse:// is relative to the mount by construction; the check must
		// not touch it, whatever the local root happens to be.
		{"fuse:///snapshots/builds/qwen/", root, false},
		// An unset root disables confinement (dev clusters, unit tests).
		{"file:///tmp/whatever/", "", false},
	} {
		u, err := Parse(tc.uri)
		if err != nil {
			t.Fatalf("Parse(%q): %v", tc.uri, err)
		}
		err = CheckLocalRoot(u, tc.root)
		if tc.wantErr && err == nil {
			t.Errorf("CheckLocalRoot(%q, %q) = nil, want an error", tc.uri, tc.root)
		}
		if !tc.wantErr && err != nil {
			t.Errorf("CheckLocalRoot(%q, %q) = %v, want nil", tc.uri, tc.root, err)
		}
	}
}
