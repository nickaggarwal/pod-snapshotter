package artifact

import (
	"encoding/json"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"
	"time"
)

// ManifestName is the object whose presence commits a directory artifact.
// A tar publishes atomically by rename; a directory tree does not, so the
// MANIFEST is written strictly last and a prefix without one must be treated
// as absent rather than as a partial artifact (docs/design-v2.md §9).
const ManifestName = "MANIFEST"

// ManifestVersion is bumped when the on-disk shape changes incompatibly.
const ManifestVersion = 1

// ManifestFile describes one object inside a directory artifact.
type ManifestFile struct {
	// Path is relative to the artifact prefix, slash-separated, no leading
	// slash (e.g. "checkpoint/pages-1.img").
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
	// Mode is the file's permission bits, preserved from the CRI archive.
	Mode uint32 `json:"mode,omitempty"`
}

// QuiesceInfo records the rendezvous contract the checkpointed process is
// parked on. Carrying it in the artifact makes a quiesced checkpoint
// self-describing: a restore knows to write the resume file without the
// PodRestore having to repeat the workload's annotations.
type QuiesceInfo struct {
	Mode string `json:"mode"`
	Dir  string `json:"dir"`
}

// Manifest is the commit record of a directory artifact.
type Manifest struct {
	Version int    `json:"version"`
	Format  string `json:"format"`
	// SourceTar names the CRI archive the tree was expanded from, when the
	// kubelet checkpoint API produced it (docs/design-v2.md §3 keeps that
	// path as the fallback until the agent drives runc checkpoint directly).
	SourceTar string `json:"sourceTar,omitempty"`
	// Quiesce is set when the checkpoint was taken at a shim-declared safe
	// point rather than off a live process (docs/design-v2.md §4).
	Quiesce    *QuiesceInfo   `json:"quiesce,omitempty"`
	Files      []ManifestFile `json:"files"`
	TotalBytes int64          `json:"totalBytes"`
	CreatedAt  time.Time      `json:"createdAt"`
}

// NewManifest builds a manifest over files, sorted by path for stable output.
func NewManifest(files []ManifestFile, sourceTar string, quiesce *QuiesceInfo) *Manifest {
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	var total int64
	for _, f := range files {
		total += f.Size
	}
	return &Manifest{
		Version:    ManifestVersion,
		Format:     FormatDir,
		SourceTar:  sourceTar,
		Quiesce:    quiesce,
		Files:      files,
		TotalBytes: total,
		CreatedAt:  time.Now().UTC(),
	}
}

// Encode writes the manifest as indented JSON.
func (m *Manifest) Encode(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(m)
}

// DecodeManifest reads and validates a manifest.
func DecodeManifest(r io.Reader) (*Manifest, error) {
	var m Manifest
	if err := json.NewDecoder(r).Decode(&m); err != nil {
		return nil, fmt.Errorf("decoding artifact MANIFEST: %w", err)
	}
	if m.Version > ManifestVersion {
		return nil, fmt.Errorf("artifact MANIFEST version %d is newer than supported version %d", m.Version, ManifestVersion)
	}
	if len(m.Files) == 0 {
		return nil, fmt.Errorf("artifact MANIFEST lists no files")
	}
	for _, f := range m.Files {
		if err := validRelPath(f.Path); err != nil {
			return nil, fmt.Errorf("artifact MANIFEST entry %q: %w", f.Path, err)
		}
	}
	return &m, nil
}

// validRelPath rejects absolute paths and traversal — a MANIFEST comes off
// shared storage and drives writes into a node-local staging directory.
func validRelPath(p string) error {
	if p == "" {
		return fmt.Errorf("empty path")
	}
	if strings.HasPrefix(p, "/") {
		return fmt.Errorf("must be relative")
	}
	if p != path.Clean(p) {
		return fmt.Errorf("must be a clean path")
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return fmt.Errorf("must not traverse out of the artifact prefix")
		}
	}
	return nil
}
