package restore

import (
	"archive/tar"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	rspec "github.com/opencontainers/runtime-spec/specs-go"
)

func writeSpecDump(t *testing.T, path string, mounts []rspec.Mount) {
	t.Helper()
	raw, err := json.Marshal(rspec.Spec{Version: "1.1.0", Mounts: mounts})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCaptureAndApplyShm(t *testing.T) {
	dir := t.TempDir()
	oldShm := filepath.Join(dir, "old-pod-shm")
	newShm := filepath.Join(dir, "new-pod-shm")
	for _, d := range []string{oldShm, newShm} {
		if err := os.MkdirAll(d, 0o1777); err != nil {
			t.Fatal(err)
		}
	}
	// What a checkpointed vLLM leaves behind: the live semaphore plus the
	// hard link CRIU made for the unlinked one glibc keeps mapped.
	content := map[string]string{
		"sem.mp-abcdef":  "semaphore-state",
		"link_remap.337": "semaphore-state",
		"psm_deadbeef":   "ring buffer",
	}
	for name, body := range content {
		if err := os.WriteFile(filepath.Join(oldShm, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	specDump := filepath.Join(dir, "spec.dump")
	writeSpecDump(t, specDump, []rspec.Mount{
		{Destination: "/proc", Source: "proc", Type: "proc"},
		{Destination: ShmPath, Source: oldShm, Type: "bind"},
	})

	out := filepath.Join(dir, ShmDiffName)
	captured, err := CaptureShm(specDump, "", out)
	if err != nil {
		t.Fatal(err)
	}
	if !captured {
		t.Fatal("expected /dev/shm to be captured")
	}

	// The restore side sees the NEW pod's tmpfs in the rewritten spec.
	spec := &rspec.Spec{Mounts: []rspec.Mount{{Destination: ShmPath, Source: newShm}}}
	if err := ApplyShm(dir, spec); err != nil {
		t.Fatal(err)
	}
	for name, body := range content {
		got, err := os.ReadFile(filepath.Join(newShm, name))
		if err != nil {
			t.Fatalf("%s not restored: %v", name, err)
		}
		if string(got) != body {
			t.Errorf("%s = %q, want %q", name, got, body)
		}
	}
}

func TestCaptureShmNoMountOrEmpty(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, ShmDiffName)

	// No /dev/shm mount at all.
	noShm := filepath.Join(dir, "no-shm.dump")
	writeSpecDump(t, noShm, []rspec.Mount{{Destination: "/proc", Source: "proc"}})
	captured, err := CaptureShm(noShm, "", out)
	if err != nil || captured {
		t.Errorf("got (%v, %v), want (false, nil) when the container has no %s", captured, err, ShmPath)
	}

	// Mounted but empty: nothing to carry.
	empty := filepath.Join(dir, "empty")
	if err := os.MkdirAll(empty, 0o1777); err != nil {
		t.Fatal(err)
	}
	emptyDump := filepath.Join(dir, "empty.dump")
	writeSpecDump(t, emptyDump, []rspec.Mount{{Destination: ShmPath, Source: empty}})
	captured, err = CaptureShm(emptyDump, "", out)
	if err != nil || captured {
		t.Errorf("got (%v, %v), want (false, nil) for an empty %s", captured, err, ShmPath)
	}
}

func TestApplyShmToleratesV1Artifacts(t *testing.T) {
	dir := t.TempDir()
	newShm := filepath.Join(dir, "shm")
	if err := os.MkdirAll(newShm, 0o1777); err != nil {
		t.Fatal(err)
	}
	// A v1 artifact has no shm-diff.tar; that is not an error.
	spec := &rspec.Spec{Mounts: []rspec.Mount{{Destination: ShmPath, Source: newShm}}}
	if err := ApplyShm(dir, spec); err != nil {
		t.Errorf("ApplyShm on an artifact without %s should be a no-op, got %v", ShmDiffName, err)
	}
	// So is a restore target with no /dev/shm mount.
	if err := ApplyShm(dir, &rspec.Spec{}); err != nil {
		t.Errorf("ApplyShm with no %s mount should be a no-op, got %v", ShmPath, err)
	}
}

func TestCaptureShmSkipsNonRegularEntries(t *testing.T) {
	dir := t.TempDir()
	shm := filepath.Join(dir, "shm")
	if err := os.MkdirAll(filepath.Join(shm, "subdir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(shm, "sem.x"), []byte("s"), 0o600); err != nil {
		t.Fatal(err)
	}
	specDump := filepath.Join(dir, "spec.dump")
	writeSpecDump(t, specDump, []rspec.Mount{{Destination: ShmPath, Source: shm}})

	out := filepath.Join(dir, ShmDiffName)
	if _, err := CaptureShm(specDump, "", out); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(out)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	tr := tar.NewReader(f)
	var names []string
	for {
		h, err := tr.Next()
		if err != nil {
			break
		}
		names = append(names, h.Name)
	}
	if len(names) != 1 || names[0] != "sem.x" {
		t.Errorf("captured %v, want only the regular file", names)
	}
}

func TestSpecMountSource(t *testing.T) {
	dir := t.TempDir()
	specDump := filepath.Join(dir, "spec.dump")
	writeSpecDump(t, specDump, []rspec.Mount{{Destination: ShmPath, Source: "/host/shm"}})

	got, err := SpecMountSource(specDump, ShmPath)
	if err != nil || got != "/host/shm" {
		t.Errorf("SpecMountSource = (%q, %v)", got, err)
	}
	if got, err := SpecMountSource(filepath.Join(dir, "absent.dump"), ShmPath); err != nil || got != "" {
		t.Errorf("a missing spec.dump should be (\"\", nil), got (%q, %v)", got, err)
	}
}
