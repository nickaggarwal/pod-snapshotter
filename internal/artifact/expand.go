package artifact

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"os"
	"path"
	"path/filepath"
)

// maxTarEntries guards against pathological archives.
const maxTarEntries = 1 << 20

// ExpandTarToDir writes the CRI checkpoint archive at tarPath out as a
// directory artifact rooted at dstDir: one object per archive member, each
// published atomically (.part + rename), with a MANIFEST written strictly
// last as the commit marker.
//
// This is the checkpoint-side half of docs/design-v2.md §3. The untar still
// happens — the kubelet checkpoint API only returns a tar — but it happens
// here, once, off the restore critical path, instead of on every restore.
//
// Any MANIFEST left by an earlier attempt is removed first, so a prefix is
// never observable as committed while it is being rewritten.
func ExpandTarToDir(tarPath, dstDir string, quiesce *QuiesceInfo) (*Manifest, error) {
	if err := os.MkdirAll(dstDir, 0o755); err != nil {
		return nil, err
	}
	manifestPath := filepath.Join(dstDir, ManifestName)
	if err := os.Remove(manifestPath); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("clearing stale MANIFEST: %w", err)
	}

	f, err := os.Open(tarPath)
	if err != nil {
		return nil, fmt.Errorf("opening checkpoint tar: %w", err)
	}
	defer f.Close()

	var files []ManifestFile
	tr := tar.NewReader(f)
	entries := 0
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("reading checkpoint tar: %w", err)
		}
		if entries++; entries > maxTarEntries {
			return nil, fmt.Errorf("checkpoint tar has too many entries")
		}

		name := path.Clean(filepath.ToSlash(hdr.Name))
		if name == "." || name == "/" {
			continue
		}
		if err := validRelPath(name); err != nil {
			return nil, fmt.Errorf("checkpoint tar entry %q: %w", hdr.Name, err)
		}
		target := filepath.Join(dstDir, filepath.FromSlash(name))

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return nil, err
			}
		case tar.TypeReg:
			mode := os.FileMode(hdr.Mode) & 0o777
			sum, n, err := writeFile(target, tr, mode)
			if err != nil {
				return nil, fmt.Errorf("writing %s: %w", name, err)
			}
			files = append(files, ManifestFile{Path: name, Size: n, SHA256: sum, Mode: uint32(mode)})
		default:
			// CRI checkpoint archives are regular files and directories only.
			// Anything else (symlink, device, FIFO) means we are looking at
			// something that is not a checkpoint — refuse rather than guess.
			return nil, fmt.Errorf("checkpoint tar entry %q has unsupported type %q", hdr.Name, string(hdr.Typeflag))
		}
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("checkpoint tar %s contained no files", tarPath)
	}

	m := NewManifest(files, filepath.Base(tarPath), quiesce)
	if err := WriteManifestDir(dstDir, m); err != nil {
		return nil, err
	}
	return m, nil
}

// WriteManifestDir publishes the commit marker of the directory artifact at
// dir. It must be the last write: a prefix without a MANIFEST is treated as
// absent, which is what gives a directory tree the atomic-publish semantics
// a tar got for free from rename (docs/design-v2.md §9).
func WriteManifestDir(dir string, m *Manifest) error {
	manifestPath := filepath.Join(dir, ManifestName)
	tmp := manifestPath + ".part"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if err := m.Encode(out); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return RenamePublish(tmp, manifestPath)
}

// RenamePublish renames tmp over dst and makes sure tmp is gone afterwards.
//
// On a POSIX filesystem the Remove is a no-op. On the fuse-client mount it is
// not: rename there is implemented as a copy that leaves the source in place
// (measured on the cluster — every artifact tar on the mount had a full-size
// .tar.part beside it), so without this every artifact costs twice its size.
func RenamePublish(tmp, dst string) error {
	if err := os.Rename(tmp, dst); err != nil {
		return err
	}
	if err := os.Remove(tmp); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("published %s but could not remove %s: %w", dst, tmp, err)
	}
	return nil
}

// writeFile streams r into dst with a sha256 tee and fsyncs. Checkpoint image
// files are individually multi-GB, so nothing is buffered whole.
//
// Deliberately NOT a .part + rename publish. The MANIFEST is what commits a
// directory artifact — readers must ignore a prefix without one — so per-file
// atomicity buys nothing, and on the fuse mount a rename would leave a
// full-size copy of every image file behind.
func writeFile(dst string, r io.Reader, mode os.FileMode) (sum string, n int64, err error) {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return "", 0, err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return "", 0, err
	}
	defer func() {
		if err != nil {
			out.Close()
		}
	}()

	var h hash.Hash = sha256.New()
	buf := make([]byte, 4<<20)
	if n, err = io.CopyBuffer(io.MultiWriter(out, h), r, buf); err != nil { // #nosec G110
		return "", 0, err
	}
	if err = out.Sync(); err != nil {
		return "", 0, err
	}
	if err = out.Close(); err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// ReadManifestDir loads and validates the MANIFEST of a directory artifact
// materialized at dir. A missing MANIFEST means the artifact is absent (or
// still being written) — never partial.
func ReadManifestDir(dir string) (*Manifest, error) {
	f, err := os.Open(filepath.Join(dir, ManifestName))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return DecodeManifest(f)
}
