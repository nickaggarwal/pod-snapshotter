// Package restore implements the node-local restore pipeline:
//
//	unpack checkpoint tar -> build OCI bundle -> rewrite spec.dump ->
//	runc restore (joining the placeholder pod's sandbox namespaces)
//
// The kubelet checkpoint tar (CRI archive) contains:
//
//	checkpoint/       CRIU image files (incl. CUDA plugin dumps)
//	config.dump       container runtime metadata (image, id, ...)
//	spec.dump         the original OCI runtime spec of the container
//	rootfs-diff.tar   filesystem writes on top of the image
//	dump.log          CRIU dump log
package restore

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
)

// Bundle is an unpacked checkpoint plus a prepared OCI bundle directory.
type Bundle struct {
	// Dir is the working directory: <root>/<restore-uid>/
	Dir string
	// CheckpointDir contains the CRIU images (<Dir>/checkpoint).
	CheckpointDir string
	// RootfsDir is the container rootfs the restored process runs over.
	RootfsDir string
	// SpecPath is <Dir>/bundle/config.json (rewritten spec.dump).
	SpecPath string
	// ConfigDump is the parsed config.dump.
	ConfigDump map[string]any
	// NvidiaHookMounts are container mountpoints created by the NVIDIA
	// container toolkit's device-node-modification hook (tmpfs files at
	// /run/nvidia-ctk-hook<uuid>, bind-mounted over
	// /proc/driver/nvidia/params). They are mounted OUTSIDE the OCI spec, so
	// runc restore has no external mapping for them; the spec rewriter must
	// add bind mounts so CRIU can map them. Extracted from dump.log.
	NvidiaHookMounts []string
}

var nvidiaHookRe = regexp.MustCompile(`/run/nvidia-ctk-hook[0-9a-fA-F-]+`)

// dumpMountRe matches CRIU dump.log mount records:
//
//	type ext4 source /dev/root mnt_id 2521 s_dev 0x800001 /usr/lib64/... @ ./usr/lib64/... flags ...
var dumpMountRe = regexp.MustCompile(`type (\S+) source (\S+) mnt_id \d+ s_dev \S+ (\S+) @ \./(\S+) flags`)

// ExtraMount is a mount present in the checkpointed container but absent
// from its OCI spec (injected by runtime hooks like nvidia-ctk).
type ExtraMount struct {
	// ContainerPath is the in-container mountpoint (leading /).
	ContainerPath string
	// HostPath is the source path on the host root filesystem ("" for
	// tmpfs and other non-host-backed mounts).
	HostPath string
	// FSType as recorded by CRIU (ext4, tmpfs, ...).
	FSType string
}

// ScanExtraMounts parses dump.log for every mount CRIU recorded inside the
// container. The caller diffs these against the OCI spec to find hook-
// injected mounts that need explicit spec entries for restore.
func ScanExtraMounts(dumpLogPath string) []ExtraMount {
	raw, err := os.ReadFile(dumpLogPath)
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	var out []ExtraMount
	for _, m := range dumpMountRe.FindAllStringSubmatch(string(raw), -1) {
		fsType, source, hostPath, containerPath := m[1], m[2], m[3], "/"+m[4]
		if containerPath == "/." || seen[containerPath] {
			continue
		}
		seen[containerPath] = true
		em := ExtraMount{ContainerPath: containerPath, FSType: fsType}
		// "source /dev/root ... <path>" records a bind from the host root
		// fs; other block sources are similar. tmpfs has no host backing.
		if fsType != "tmpfs" && source != "tmpfs" {
			em.HostPath = hostPath
		}
		out = append(out, em)
	}
	return out
}

// scanNvidiaHookMounts extracts toolkit hook mountpoints from dump.log.
func scanNvidiaHookMounts(dumpLogPath string) []string {
	raw, err := os.ReadFile(dumpLogPath)
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, m := range nvidiaHookRe.FindAllString(string(raw), -1) {
		if !seen[m] {
			seen[m] = true
			out = append(out, m)
		}
	}
	if len(out) > 0 && strings.Contains(string(raw), "/proc/driver/nvidia/params") {
		out = append(out, "/proc/driver/nvidia/params")
	}
	return out
}

// maxTarEntries and maxTarSize guard against pathological archives.
const (
	maxTarEntries = 1 << 20
	// 1 TiB — checkpoint tars are legitimately huge (VRAM + heap).
	maxTarSize = 1 << 40
)

// Unpack extracts the checkpoint tar into workDir and prepares the bundle
// layout. rootfsDir is where the image rootfs was materialized (by the CRI
// resolver, from the placeholder container's image mounts); rootfs-diff.tar
// is applied on top of it.
func Unpack(tarPath, workDir, rootfsDir string) (*Bundle, error) {
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		return nil, err
	}
	f, err := os.Open(tarPath)
	if err != nil {
		return nil, fmt.Errorf("opening checkpoint tar: %w", err)
	}
	defer f.Close()

	if err := extractTar(f, workDir); err != nil {
		return nil, fmt.Errorf("extracting checkpoint tar: %w", err)
	}

	b := &Bundle{
		Dir:           workDir,
		CheckpointDir: filepath.Join(workDir, "checkpoint"),
		RootfsDir:     rootfsDir,
		SpecPath:      filepath.Join(workDir, "bundle", "config.json"),
	}
	if fi, err := os.Stat(b.CheckpointDir); err != nil || !fi.IsDir() {
		return nil, fmt.Errorf("checkpoint tar %s has no checkpoint/ directory (not a CRI checkpoint archive?)", tarPath)
	}

	// Parse config.dump (json).
	if raw, err := os.ReadFile(filepath.Join(workDir, "config.dump")); err == nil {
		_ = json.Unmarshal(raw, &b.ConfigDump)
	}

	b.NvidiaHookMounts = scanNvidiaHookMounts(filepath.Join(workDir, "dump.log"))

	// Apply rootfs writes on top of the image rootfs. containerd 2.x writes
	// this member gzip-compressed despite the .tar name; sniff the magic.
	diff := filepath.Join(workDir, "rootfs-diff.tar")
	if _, err := os.Stat(diff); err == nil && rootfsDir != "" {
		df, err := os.Open(diff)
		if err != nil {
			return nil, err
		}
		defer df.Close()
		var dr io.Reader = df
		br := bufio.NewReader(df)
		if magic, err := br.Peek(2); err == nil && magic[0] == 0x1f && magic[1] == 0x8b {
			gz, err := gzip.NewReader(br)
			if err != nil {
				return nil, fmt.Errorf("opening gzipped rootfs-diff.tar: %w", err)
			}
			defer gz.Close()
			dr = gz
		} else {
			dr = br
		}
		// tolerateReadOnly: the live rootfs contains read-only bind mounts
		// injected by the runtime (NVIDIA CDI files like
		// /etc/vulkan/icd.d/nvidia_icd.json, resolv.conf, etc.) that also
		// show up in the diff; they are re-injected identically in the new
		// pod, so EROFS on those paths is expected and safe to skip.
		if err := extractTarOpts(dr, rootfsDir, true); err != nil {
			return nil, fmt.Errorf("applying rootfs-diff.tar: %w", err)
		}
	}

	if err := os.MkdirAll(filepath.Dir(b.SpecPath), 0o755); err != nil {
		return nil, err
	}
	return b, nil
}

// extractTar safely extracts a tar stream into dir, rejecting path escapes.
func extractTar(r io.Reader, dir string) error {
	return extractTarOpts(r, dir, false)
}

func extractTarOpts(r io.Reader, dir string, tolerateReadOnly bool) error {
	tr := tar.NewReader(r)
	entries := 0
	var total int64
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if entries++; entries > maxTarEntries {
			return fmt.Errorf("archive has too many entries")
		}

		name := filepath.Clean(hdr.Name)
		if name == "." {
			continue
		}
		if strings.HasPrefix(name, "..") || filepath.IsAbs(name) {
			return fmt.Errorf("archive entry %q escapes extraction dir", hdr.Name)
		}
		target := filepath.Join(dir, name)

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, os.FileMode(hdr.Mode)&0o777|0o700); err != nil {
				return err
			}
		case tar.TypeReg:
			if total += hdr.Size; total > maxTarSize {
				return fmt.Errorf("archive exceeds size limit")
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(hdr.Mode)&0o777)
			if err != nil {
				if tolerateReadOnly && isReadOnlyErr(err) {
					if _, err := io.Copy(io.Discard, tr); err != nil { // #nosec G110
						return err
					}
					continue
				}
				return err
			}
			if _, err := io.Copy(out, tr); err != nil { // #nosec G110 -- size capped above
				out.Close()
				return err
			}
			if err := out.Close(); err != nil {
				return err
			}
		case tar.TypeSymlink:
			// Reject absolute or escaping link targets.
			if filepath.IsAbs(hdr.Linkname) {
				return fmt.Errorf("archive symlink %q has absolute target %q", hdr.Name, hdr.Linkname)
			}
			joined := filepath.Clean(filepath.Join(filepath.Dir(name), hdr.Linkname))
			if strings.HasPrefix(joined, "..") {
				return fmt.Errorf("archive symlink %q escapes extraction dir", hdr.Name)
			}
			os.Remove(target)
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			if err := os.Symlink(hdr.Linkname, target); err != nil {
				return err
			}
		case tar.TypeLink:
			linkSrc := filepath.Join(dir, filepath.Clean(hdr.Linkname))
			if !strings.HasPrefix(linkSrc, filepath.Clean(dir)+string(os.PathSeparator)) {
				return fmt.Errorf("archive hardlink %q escapes extraction dir", hdr.Name)
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			os.Remove(target)
			if err := os.Link(linkSrc, target); err != nil {
				return err
			}
		default:
			// FIFOs/devices inside checkpoint archives are unexpected; skip.
		}
	}
}

// OldPodUID extracts the checkpointed pod's UID from config.dump. containerd
// names CRI containers "<container>_<pod>_<namespace>_<podUID>_<attempt>";
// some runtimes also provide a sandbox object — try both.
func OldPodUID(configDump map[string]any) string {
	if configDump == nil {
		return ""
	}
	if s, ok := configDump["sandbox"].(map[string]any); ok {
		if uid, ok := s["uid"].(string); ok && uid != "" {
			return uid
		}
	}
	if name, ok := configDump["name"].(string); ok {
		parts := strings.Split(name, "_")
		if len(parts) >= 5 {
			return parts[len(parts)-2]
		}
	}
	return ""
}

// isReadOnlyErr reports EROFS/EPERM/EACCES-style failures from writing over
// runtime-injected read-only mounts.
func isReadOnlyErr(err error) bool {
	return errors.Is(err, syscall.EROFS) || errors.Is(err, os.ErrPermission)
}
