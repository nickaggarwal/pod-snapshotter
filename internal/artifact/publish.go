package artifact

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"time"
)

// CheckpointMeta is the small metadata the CRI checkpoint archive carries
// alongside CRIU's own images. The kubelet writes it into the tar; when the
// agent drives the dump itself, it writes it here instead.
//
// Field names and JSON tags match what the kubelet produces, because the
// restore path parses config.dump either way and must not care which side
// produced the artifact.
type CheckpointMeta struct {
	ID              string    `json:"id"`
	Name            string    `json:"name"`
	RootfsImage     string    `json:"rootfsImage"`
	RootfsImageRef  string    `json:"rootfsImageRef"`
	RootfsImageName string    `json:"rootfsImageName"`
	Runtime         string    `json:"runtime"`
	CreatedTime     time.Time `json:"createdTime"`
	CheckpointedAt  time.Time `json:"checkpointedTime"`
	RestoredTime    time.Time `json:"restoredTime"`
	Restored        bool      `json:"restored"`
}

// PublishDirOptions describes a directory artifact assembled in place, from
// CRIU images the agent's own `runc checkpoint` has already written into
// <dstDir>/checkpoint.
type PublishDirOptions struct {
	// Dir is the artifact root. CRIU images are expected at Dir/checkpoint.
	Dir string
	// Meta becomes config.dump.
	Meta *CheckpointMeta
	// Spec is the container's OCI runtime spec, written as spec.dump. The
	// restore rewrites it into the new sandbox's config.json, so an artifact
	// without it cannot be restored.
	Spec json.RawMessage
	// Quiesce records the rendezvous contract, as for ExpandTarToDir.
	Quiesce *QuiesceInfo
	// Contribute runs after the metadata is written and before the MANIFEST,
	// exactly as in ExpandTarToDir — this is where /dev/shm is captured.
	Contribute func(dstDir string) ([]ManifestFile, error)
	// Digest computes a sha256 for every image file.
	//
	// Off by default, and that default is the point. Hashing means reading
	// all 56 GB back immediately after writing it — the same read this whole
	// path exists to remove. The kubelet path gets its digests for free
	// because it is streaming the bytes through the expander anyway; here
	// there is no stream to tee off, so a digest costs a full extra pass.
	//
	// Nothing downstream requires it. Prefetch verifies a digest when the
	// manifest carries one and checks size otherwise, and the NVMe cache
	// check is size-only. Turn it on when the artifact will outlive the
	// node it was written on and you want the stronger check.
	Digest bool
}

// PublishDir commits a directory artifact whose CRIU images are already in
// place, writing the metadata files the CRI archive would have carried and
// then the MANIFEST as the commit marker.
//
// This is the other half of docs/design-v2.md §3, and the point of it is what
// it does NOT do. ExpandTarToDir exists because the kubelet checkpoint API
// hands back a tar: CRIU writes the bytes, the kubelet reads them back and
// writes them again into an archive, and the agent reads that archive back
// and writes them a third time to get the directory layout a restore can use.
// Publishing 56 GB that way moved 112 GB of reads and 168 GB of writes,
// measured — and on the node this was measured on, most of it landed on the
// OS disk rather than the NVMe tier, because that is where
// /var/lib/kubelet lives.
//
// Here CRIU writes each image once, directly where it will be read from, and
// this function adds four small files on top. Nothing is copied.
//
// A MANIFEST left by an earlier attempt is cleared first, so a partially
// written prefix is never observable as committed.
func PublishDir(opts PublishDirOptions) (*Manifest, error) {
	if opts.Dir == "" {
		return nil, fmt.Errorf("artifact dir is required")
	}
	if opts.Meta == nil {
		return nil, fmt.Errorf("checkpoint metadata is required")
	}
	if len(opts.Spec) == 0 {
		return nil, fmt.Errorf("the container's OCI spec is required; without spec.dump the artifact cannot be restored")
	}

	manifestPath := filepath.Join(opts.Dir, ManifestName)
	if err := os.Remove(manifestPath); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("clearing stale MANIFEST: %w", err)
	}

	ckptDir := filepath.Join(opts.Dir, "checkpoint")
	if fi, err := os.Stat(ckptDir); err != nil || !fi.IsDir() {
		return nil, fmt.Errorf("no CRIU images at %s: the dump must write there before publishing", ckptDir)
	}

	metaJSON, err := json.Marshal(opts.Meta)
	if err != nil {
		return nil, fmt.Errorf("encoding config.dump: %w", err)
	}
	// "status" and the *.log files the kubelet's archive also carries are
	// diagnostics; the restore reads none of them, so they are not
	// reconstructed here. config.dump and spec.dump are the two it does read.
	for name, body := range map[string][]byte{
		"config.dump": metaJSON,
		"spec.dump":   opts.Spec,
	} {
		if err := os.WriteFile(filepath.Join(opts.Dir, name), body, 0o644); err != nil {
			return nil, fmt.Errorf("writing %s: %w", name, err)
		}
	}

	var files []ManifestFile
	if opts.Contribute != nil {
		extra, err := opts.Contribute(opts.Dir)
		if err != nil {
			return nil, err
		}
		files = append(files, extra...)
	}

	// Walk what is on disk rather than tracking writes: CRIU decides how many
	// image files it produces, and the manifest has to describe the tree that
	// actually exists. Files the contributor already described are skipped so
	// they are not listed twice.
	//
	// The walk stats; it does not read. See PublishDirOptions.Digest.
	described := make(map[string]bool, len(files))
	for _, f := range files {
		described[f.Path] = true
	}
	walkErr := filepath.WalkDir(opts.Dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(opts.Dir, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel == ManifestName || described[rel] {
			return nil
		}
		var mf ManifestFile
		if opts.Digest {
			if mf, err = DescribeFile(opts.Dir, rel); err != nil {
				return fmt.Errorf("describing %s: %w", rel, err)
			}
		} else {
			fi, err := d.Info()
			if err != nil {
				return fmt.Errorf("describing %s: %w", rel, err)
			}
			mf = ManifestFile{Path: rel, Size: fi.Size(), Mode: uint32(fi.Mode().Perm())}
		}
		files = append(files, mf)
		return nil
	})
	if walkErr != nil {
		return nil, walkErr
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("artifact at %s is empty", opts.Dir)
	}
	// Sanity: an artifact with no page images is a dump that did not happen.
	// Catching it here beats discovering it at restore time, hours later.
	var sawPages bool
	for _, f := range files {
		if base := path.Base(f.Path); len(base) > 6 && base[:6] == "pages-" {
			sawPages = true
			break
		}
	}
	if !sawPages {
		return nil, fmt.Errorf("artifact at %s has no pages-*.img: the dump produced no memory images", opts.Dir)
	}

	// SourceTar stays empty: there was no tar. That is the field a reader can
	// use to tell an agent-dumped artifact from a kubelet-dumped one.
	m := NewManifest(files, "", opts.Quiesce)
	if err := WriteManifestDir(opts.Dir, m); err != nil {
		return nil, err
	}
	return m, nil
}
