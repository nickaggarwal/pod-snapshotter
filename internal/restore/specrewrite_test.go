package restore

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	rspec "github.com/opencontainers/runtime-spec/specs-go"
)

// sampleSpec builds a spec.dump resembling a containerd CRI checkpoint of a
// GPU pod container.
func sampleSpec(oldUID string) *rspec.Spec {
	return &rspec.Spec{
		Version: "1.1.0",
		Process: &rspec.Process{Args: []string{"python", "serve.py"}},
		Root:    &rspec.Root{Path: "rootfs"},
		Mounts: []rspec.Mount{
			{Destination: "/etc/hosts", Type: "bind", Source: "/var/lib/kubelet/pods/" + oldUID + "/etc-hosts", Options: []string{"rbind", "rprivate", "rw"}},
			{Destination: "/var/run/secrets/kubernetes.io/serviceaccount", Type: "bind", Source: "/var/lib/kubelet/pods/" + oldUID + "/volumes/kubernetes.io~projected/kube-api-access-xyz", Options: []string{"rbind", "rprivate", "ro"}},
			{Destination: "/dev/shm", Type: "bind", Source: "/run/containerd/io.containerd.grpc.v1.cri/sandboxes/abc/shm", Options: []string{"rbind", "rprivate", "rw"}},
			{Destination: "/dev/nvidia0", Type: "bind", Source: "/dev/nvidia0", Options: []string{"rbind", "rshared", "rw"}},
			{Destination: "/dev/nvidiactl", Type: "bind", Source: "/dev/nvidiactl", Options: []string{"rbind", "rw"}},
			{Destination: "/dev/nvidia-uvm", Type: "bind", Source: "/dev/nvidia-uvm", Options: []string{"rbind", "rprivate", "rw"}},
		},
		Linux: &rspec.Linux{
			CgroupsPath: "kubepods-burstable-pod" + oldUID + ".slice:cri-containerd:oldctr",
			Namespaces: []rspec.LinuxNamespace{
				{Type: rspec.PIDNamespace},
				{Type: rspec.NetworkNamespace, Path: "/proc/1111/ns/net"},
				{Type: rspec.IPCNamespace, Path: "/proc/1111/ns/ipc"},
				{Type: rspec.UTSNamespace, Path: "/proc/1111/ns/uts"},
				{Type: rspec.MountNamespace},
			},
		},
	}
}

func writeSpec(t *testing.T, dir string, spec *rspec.Spec) string {
	t.Helper()
	p := filepath.Join(dir, "spec.dump")
	data, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestRewriteSpec(t *testing.T) {
	dir := t.TempDir()
	oldUID := "11111111-1111-1111-1111-111111111111"
	newUID := "22222222-2222-2222-2222-222222222222"
	specPath := writeSpec(t, dir, sampleSpec(oldUID))
	outPath := filepath.Join(dir, "config.json")

	got, err := RewriteSpec(specPath, outPath, SandboxTarget{
		PausePID:    4242,
		PodUID:      newUID,
		OldPodUID:   oldUID,
		CgroupsPath: "kubepods-pod" + newUID + ".slice:snap:r1",
		RootfsPath:  "/proc/999/root",
	})
	if err != nil {
		t.Fatal(err)
	}

	nsPaths := map[rspec.LinuxNamespaceType]string{}
	for _, ns := range got.Linux.Namespaces {
		nsPaths[ns.Type] = ns.Path
	}
	if nsPaths[rspec.NetworkNamespace] != "/proc/4242/ns/net" {
		t.Errorf("netns = %q, want /proc/4242/ns/net", nsPaths[rspec.NetworkNamespace])
	}
	if nsPaths[rspec.IPCNamespace] != "/proc/4242/ns/ipc" {
		t.Errorf("ipcns = %q", nsPaths[rspec.IPCNamespace])
	}
	if nsPaths[rspec.UTSNamespace] != "/proc/4242/ns/uts" {
		t.Errorf("utsns = %q", nsPaths[rspec.UTSNamespace])
	}
	if nsPaths[rspec.PIDNamespace] != "" {
		t.Errorf("pidns path = %q, want fresh (empty)", nsPaths[rspec.PIDNamespace])
	}

	if got.Linux.CgroupsPath != "kubepods-pod"+newUID+".slice:snap:r1" {
		t.Errorf("cgroupsPath = %q", got.Linux.CgroupsPath)
	}
	if got.Root.Path != "/proc/999/root" {
		t.Errorf("root.path = %q", got.Root.Path)
	}

	for _, m := range got.Mounts {
		switch m.Destination {
		case "/etc/hosts":
			want := "/var/lib/kubelet/pods/" + newUID + "/etc-hosts"
			if m.Source != want {
				t.Errorf("/etc/hosts source = %q, want %q", m.Source, want)
			}
		case "/var/run/secrets/kubernetes.io/serviceaccount":
			if want := "/var/lib/kubelet/pods/" + newUID + "/volumes/kubernetes.io~projected/kube-api-access-xyz"; m.Source != want {
				t.Errorf("sa token source = %q, want %q", m.Source, want)
			}
		case "/dev/nvidia0":
			// rshared must have been replaced with rprivate.
			hasRprivate, hasRshared := false, false
			for _, o := range m.Options {
				if o == "rprivate" {
					hasRprivate = true
				}
				if o == "rshared" {
					hasRshared = true
				}
			}
			if !hasRprivate || hasRshared {
				t.Errorf("/dev/nvidia0 options = %v, want rprivate and no rshared", m.Options)
			}
		case "/dev/nvidiactl":
			found := false
			for _, o := range m.Options {
				if o == "rprivate" {
					found = true
				}
			}
			if !found {
				t.Errorf("/dev/nvidiactl options = %v, want rprivate added", m.Options)
			}
		}
	}

	// Output file must be valid JSON parseable as a spec.
	raw, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	var reparsed rspec.Spec
	if err := json.Unmarshal(raw, &reparsed); err != nil {
		t.Fatalf("output config.json invalid: %v", err)
	}
}

func TestValidateGPUDevices(t *testing.T) {
	spec := &rspec.Spec{Mounts: []rspec.Mount{
		{Destination: "/dev/nvidia0", Source: "/dev/definitely-not-present-nvidia0"},
	}}
	if err := ValidateGPUDevices(spec); err == nil {
		t.Error("expected missing device error")
	}

	// Non-GPU spec passes trivially.
	if err := ValidateGPUDevices(&rspec.Spec{Mounts: []rspec.Mount{{Destination: "/etc/hosts", Source: "/tmp"}}}); err != nil {
		t.Errorf("unexpected: %v", err)
	}
}
