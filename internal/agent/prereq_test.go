package agent

import "testing"

func TestRuntimeSupportsCheckpoint(t *testing.T) {
	tests := []struct {
		in   string
		want bool
	}{
		{"containerd://1.7.30-2", false}, // AKS Ubuntu 22.04 — verified Unimplemented
		{"containerd://1.6.20", false},
		{"containerd://2.0.4", true}, // AKS Ubuntu 24.04
		{"containerd://2.1.0-beta.1", true},
		{"cri-o://1.24.6", false},
		{"cri-o://1.25.0", true},
		{"cri-o://1.29.1", true},
		{"docker://24.0.0", false},
		{"garbage", false},
		{"", false},
	}
	for _, tc := range tests {
		if got := runtimeSupportsCheckpoint(tc.in); got != tc.want {
			t.Errorf("runtimeSupportsCheckpoint(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}
