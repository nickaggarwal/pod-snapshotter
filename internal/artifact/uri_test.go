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
	got := DefaultURI("default", "snap1", "vllm")
	want := "fuse:///snapshots/default/snap1/vllm.tar"
	if got != want {
		t.Errorf("DefaultURI = %q, want %q", got, want)
	}
	if _, err := Parse(got); err != nil {
		t.Errorf("DefaultURI output does not parse: %v", err)
	}
}
