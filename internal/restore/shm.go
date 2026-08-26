package restore

import (
	"archive/tar"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"

	rspec "github.com/opencontainers/runtime-spec/specs-go"
)

// ShmDiffName is the artifact member holding the contents of the
// checkpointed container's /dev/shm.
const ShmDiffName = "shm-diff.tar"

// ShmPath is the mount destination captured and restored.
const ShmPath = "/dev/shm"

// maxShmCapture bounds what is copied into the artifact. /dev/shm holds
// semaphores and ring buffers, not model weights; a workload using more than
// this is doing something the capture was not designed for.
const maxShmCapture = 1 << 30 // 1 GiB

// CaptureShm archives the checkpointed container's /dev/shm into outPath.
//
// CRIU cannot ghost a *mapped* deleted file, so for the unlinked
// /dev/shm/sem.* that glibc's sem_open leaves mapped behind every POSIX
// semaphore it fails the dump outright unless link-remap is on:
//
//	Error (criu/files-reg.c:1122): Can't create link remap for
//	/dev/shm/sem.XXXXXX. Use link-remap option.
//
// With link-remap, CRIU instead hard-links the inode to /dev/shm/link_remap.N
// at dump time and relinks it at restore. That works when dump and restore
// share a filesystem — but a restored pod gets a brand-new tmpfs, the link is
// not there, and the restore fails:
//
//	Error (criu/files-reg.c:2258): Can't link dev/shm/link_remap.337 ->
//	dev/shm/sem.l0JKK0: No such file or directory
//
// /dev/shm is pod-scoped and external to the CRIU image, so nothing else
// carries it. Capturing it here is the same idea as rootfs-diff.tar: whatever
// the container wrote and still needs has to travel with the artifact.
//
// hostRoot is where the host filesystem is visible to the agent (may be ""),
// and specDumpPath is the checkpoint's spec.dump. Returns false when the
// container had no /dev/shm mount, or it held nothing worth carrying.
func CaptureShm(specDumpPath, hostRoot, outPath string) (bool, error) {
	src, err := SpecMountSource(specDumpPath, ShmPath)
	if err != nil || src == "" {
		return false, err
	}
	// Prefer the path as the agent sees it directly (the kubelet pods dir and
	// the containerd state dir are both mounted in); fall back to hostRoot.
	dir := src
	if _, err := os.Stat(dir); err != nil && hostRoot != "" {
		dir = filepath.Join(hostRoot, src)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false, fmt.Errorf("reading %s (%s): %w", ShmPath, dir, err)
	}
	if len(entries) == 0 {
		return false, nil
	}

	out, err := os.OpenFile(outPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return false, err
	}
	defer out.Close()

	tw := tar.NewWriter(out)
	var total int64
	for _, e := range entries {
		if !e.Type().IsRegular() {
			// Sockets and directories in /dev/shm are not what link-remap
			// needs, and CRIU handles the rest itself.
			continue
		}
		fi, err := e.Info()
		if err != nil {
			return false, err
		}
		if total += fi.Size(); total > maxShmCapture {
			return false, fmt.Errorf("%s holds more than %d bytes; refusing to capture it into the artifact", ShmPath, int64(maxShmCapture))
		}
		if err := writeTarFile(tw, dir, e.Name(), fi); err != nil {
			return false, err
		}
	}
	if err := tw.Close(); err != nil {
		return false, err
	}
	if err := out.Sync(); err != nil {
		return false, err
	}
	return true, nil
}

func writeTarFile(tw *tar.Writer, dir, name string, fi os.FileInfo) error {
	f, err := os.Open(filepath.Join(dir, name))
	if err != nil {
		// A semaphore can vanish between ReadDir and Open; it is then not
		// something the restore needs either.
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer f.Close()
	if err := tw.WriteHeader(&tar.Header{
		Name:    name,
		Mode:    int64(fi.Mode().Perm()),
		Size:    fi.Size(),
		ModTime: fi.ModTime(),
	}); err != nil {
		return err
	}
	_, err = io.CopyN(tw, f, fi.Size())
	if err == io.EOF {
		return fmt.Errorf("%s shrank while being captured", name)
	}
	return err
}

// ApplyShm unpacks a captured /dev/shm into the restore target's own tmpfs,
// which the rewritten spec points at. A missing shm-diff.tar is normal: v1
// artifacts do not carry one, and a container that never touched /dev/shm has
// nothing to restore.
func ApplyShm(bundleDir string, spec *rspec.Spec) error {
	dst := specMountSource(spec, ShmPath)
	if dst == "" {
		return nil
	}
	f, err := os.Open(filepath.Join(bundleDir, ShmDiffName))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer f.Close()
	if err := extractTar(f, dst); err != nil {
		return fmt.Errorf("restoring %s into %s: %w", ShmPath, dst, err)
	}
	return nil
}

// SpecMountSource reads spec.dump and returns the host source of the mount at
// dest, or "" when the container has no such mount.
func SpecMountSource(specDumpPath, dest string) (string, error) {
	raw, err := os.ReadFile(specDumpPath)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	var spec rspec.Spec
	if err := json.Unmarshal(raw, &spec); err != nil {
		return "", fmt.Errorf("parsing %s: %w", specDumpPath, err)
	}
	return specMountSource(&spec, dest), nil
}

func specMountSource(spec *rspec.Spec, dest string) string {
	if spec == nil {
		return ""
	}
	for _, m := range spec.Mounts {
		if m.Destination == dest {
			return m.Source
		}
	}
	return ""
}
