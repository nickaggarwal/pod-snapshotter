package restore

import (
	"archive/tar"
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// RootfsDiffName is the artifact member holding the container's filesystem
// writes: everything it changed on top of its image.
const RootfsDiffName = "rootfs-diff.tar"

// maxRootfsCapture bounds the diff. A container's writable layer holds logs,
// compile caches, and scratch files -- megabytes. A workload that has written
// more than this into its rootfs is storing state somewhere a checkpoint was
// never going to carry correctly, and silently tarring tens of gigabytes into
// the artifact would hide that rather than surface it.
const maxRootfsCapture = 4 << 30 // 4 GiB

// OverlayUpperDir returns the overlay upperdir backing pid's root mount --
// the container's writable layer, on the host.
//
// This must be read while the container is still alive. After
// `runc checkpoint` the process is gone and so is /proc/<pid>.
//
// Returns "" with no error when the root mount is not an overlay (a
// devicemapper or native-snapshotter node), which is not a failure: there is
// then no upperdir to diff and the restore reuses the keeper's rootfs as-is.
func OverlayUpperDir(pid int) (string, error) {
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/mountinfo", pid))
	if err != nil {
		return "", err
	}
	return parseOverlayUpper(raw)
}

func parseOverlayUpper(mountinfo []byte) (string, error) {
	sc := bufio.NewScanner(bytes.NewReader(mountinfo))
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		// mountinfo: ID PARENT MAJ:MIN ROOT MOUNTPOINT OPTS... - FSTYPE SOURCE SUPEROPTS
		fields := strings.Fields(sc.Text())
		sep := -1
		for i, f := range fields {
			if f == "-" {
				sep = i
				break
			}
		}
		if sep < 0 || sep+3 > len(fields) || len(fields) < 5 {
			continue
		}
		if fields[4] != "/" || fields[sep+1] != "overlay" {
			continue
		}
		for _, opt := range strings.Split(fields[sep+3], ",") {
			if upper, ok := strings.CutPrefix(opt, "upperdir="); ok {
				return upper, nil
			}
		}
	}
	return "", sc.Err()
}

// CaptureRootfsDiff archives the container's writable layer into outPath, so
// the artifact carries what the container wrote on top of its image.
//
// The kubelet checkpoint API produces this member itself -- containerd builds
// it while assembling the tar. When the agent drives the dump there is no
// such assembly step, so the diff has to be taken here or the restored
// container comes up with a pristine image and none of its own writes.
//
// hostRoot is where the host filesystem is visible to the agent (may be "");
// upperDir is a host path from OverlayUpperDir. Returns false when there is
// nothing to carry.
func CaptureRootfsDiff(upperDir, hostRoot, outPath string) (bool, error) {
	if upperDir == "" {
		return false, nil
	}
	dir := upperDir
	if _, err := os.Stat(dir); err != nil && hostRoot != "" {
		dir = filepath.Join(hostRoot, upperDir)
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return false, fmt.Errorf("reading the container's writable layer (%s): %w", dir, err)
	}

	out, err := os.OpenFile(outPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return false, err
	}
	defer out.Close()

	tw := tar.NewWriter(out)
	var total, entries int64
	walkErr := filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			// A path that vanished mid-walk was scratch; a path we cannot
			// stat is not worth failing the whole checkpoint over.
			if os.IsNotExist(err) || os.IsPermission(err) {
				return nil
			}
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil || rel == "." {
			return err
		}
		rel = filepath.ToSlash(rel)

		// Overlay whiteouts are character devices 0:0 marking a deletion.
		// They are meaningful, but the extractor applies a plain tar onto a
		// live rootfs and has no way to act on them; skipping is what the
		// kubelet path's own diff effectively does for our purposes.
		if fi.Mode()&os.ModeCharDevice != 0 {
			return nil
		}

		var link string
		if fi.Mode()&os.ModeSymlink != 0 {
			if link, err = os.Readlink(p); err != nil {
				return nil
			}
		} else if !fi.Mode().IsRegular() && !fi.IsDir() {
			return nil // sockets, fifos, block devices: not restorable state
		}

		hdr, err := tar.FileInfoHeader(fi, link)
		if err != nil {
			return err
		}
		hdr.Name = rel
		if fi.IsDir() {
			hdr.Name += "/"
		}
		if fi.Mode().IsRegular() {
			if total += fi.Size(); total > maxRootfsCapture {
				return fmt.Errorf("the container's writable layer holds more than %d bytes; refusing to capture it into the artifact", int64(maxRootfsCapture))
			}
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		entries++
		if !fi.Mode().IsRegular() {
			return nil
		}
		f, err := os.Open(p) // #nosec G304 -- walking a path the runtime owns
		if err != nil {
			if os.IsNotExist(err) || os.IsPermission(err) {
				return nil
			}
			return err
		}
		defer f.Close()
		// Write exactly the size in the header. A file that shrank between
		// the stat and the read would otherwise leave the archive short by
		// the difference and desync every entry after it, so a short copy is
		// padded rather than propagated.
		n, err := io.CopyN(tw, f, fi.Size())
		if err != nil && err != io.EOF {
			return err
		}
		if n < fi.Size() {
			_, err = io.CopyN(tw, zeroReader{}, fi.Size()-n)
			return err
		}
		return nil
	})
	if walkErr != nil {
		return false, walkErr
	}
	if err := tw.Close(); err != nil {
		return false, err
	}
	if entries == 0 {
		return false, os.Remove(outPath)
	}
	return true, out.Sync()
}

// zeroReader pads a truncated file back to its declared tar size.
type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 0
	}
	return len(p), nil
}
