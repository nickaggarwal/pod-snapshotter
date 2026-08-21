package agent

import "testing"

// runc is exec'd through nsenter into the host mount namespace, so when the
// NVMe bypass makes the agent and the host name the images directory
// differently, --image-path has to be the host spelling. Handing runc the
// agent's /host-prefixed path fails as a missing descriptors.json, which
// reads like a corrupt artifact rather than a path-translation bug.
func TestCRIUImagePath(t *testing.T) {
	const bypass = "/mnt/fuse-nvme0n1/fuse-cache/snapshots/builds/x"
	for _, tc := range []struct {
		name          string
		checkpointDir string
		hostImageDir  string
		want          string
	}{
		{
			name:          "no bypass keeps the bundle's own path",
			checkpointDir: "/mnt/fuse/snapshots/builds/x/checkpoint",
			want:          "/mnt/fuse/snapshots/builds/x/checkpoint",
		},
		{
			name:          "bypass rewrites to the host spelling",
			checkpointDir: "/host" + bypass + "/checkpoint",
			hostImageDir:  bypass,
			want:          bypass + "/checkpoint",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := criuImagePath(tc.checkpointDir, tc.hostImageDir); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}
