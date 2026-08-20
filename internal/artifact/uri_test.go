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
	for _, tc := range []struct{ format, want string }{
		{FormatTar, "fuse:///snapshots/default/snap1/vllm.tar"},
		{"", "fuse:///snapshots/default/snap1/vllm.tar"},
		{FormatDir, "fuse:///snapshots/default/snap1/vllm/"},
	} {
		got := DefaultURI("default", "snap1", "vllm", tc.format)
		if got != tc.want {
			t.Errorf("DefaultURI(format=%q) = %q, want %q", tc.format, got, tc.want)
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
