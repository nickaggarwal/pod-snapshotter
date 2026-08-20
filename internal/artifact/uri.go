// Package artifact handles checkpoint artifact URIs and storage.
//
// Supported schemes:
//
//	fuse:///<path> — a path under the fuse-client distributed cache mount.
//	                 On every node this resolves to <fuse-mount>/<path>
//	                 (default /mnt/fuse/<path>); fuse-client persists writes
//	                 to its cloud tier and serves reads through NVMe/peers.
//	file:///<path> — an absolute node-local path, for testing without fuse.
//
// A URI naming a single object (…/vllm.tar) is a v1 tar artifact. A URI with
// a trailing slash (…/vllm/) is a v2 image-directory prefix: the CRIU image
// files as individual objects, committed by a MANIFEST written last. See
// docs/design-v2.md §3.
package artifact

import (
	"fmt"
	"net/url"
	"path"
	"strings"
)

const (
	SchemeFuse = "fuse"
	SchemeFile = "file"
)

// Artifact formats. Format is recorded on the snapshot status; absent means
// FormatTar, so v1 artifacts keep restoring unchanged.
const (
	FormatTar = "tar"
	FormatDir = "dir"
)

// URI is a parsed artifact location.
type URI struct {
	Scheme string
	// Path is the cleaned path component, always starting with "/" and never
	// carrying a trailing slash (see Dir).
	Path string
	// Dir reports whether the URI names an image-directory prefix rather than
	// a single object. Set by a trailing slash in the raw URI.
	Dir bool
}

// Parse validates and parses an artifact URI.
func Parse(raw string) (URI, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return URI{}, fmt.Errorf("invalid artifact URI %q: %w", raw, err)
	}
	switch u.Scheme {
	case SchemeFuse, SchemeFile:
	default:
		return URI{}, fmt.Errorf("unsupported artifact URI scheme %q (want fuse:// or file://)", u.Scheme)
	}
	if u.Host != "" {
		return URI{}, fmt.Errorf("artifact URI %q must not have a host component (use three slashes: %s:///path)", raw, u.Scheme)
	}
	// Reject traversal in the raw path before Clean can normalize it away.
	for _, seg := range strings.Split(u.Path, "/") {
		if seg == ".." {
			return URI{}, fmt.Errorf("artifact URI %q must not contain '..'", raw)
		}
	}
	isDir := strings.HasSuffix(u.Path, "/")
	p := path.Clean(u.Path)
	if p == "/" || p == "." || !strings.HasPrefix(p, "/") {
		return URI{}, fmt.Errorf("artifact URI %q has no usable path", raw)
	}
	return URI{Scheme: u.Scheme, Path: p, Dir: isDir}, nil
}

// String reassembles the URI, preserving the trailing slash that marks a
// directory prefix.
func (u URI) String() string {
	s := u.Scheme + "://" + u.Path
	if u.Dir {
		s += "/"
	}
	return s
}

// Format returns FormatDir for directory prefixes, FormatTar otherwise.
func (u URI) Format() string {
	if u.Dir {
		return FormatDir
	}
	return FormatTar
}

// HostPath resolves the URI to an on-node filesystem path. fuseMount is the
// node's fuse-client mount point (e.g. /mnt/fuse); it is ignored for file://.
func (u URI) HostPath(fuseMount string) string {
	if u.Scheme == SchemeFile {
		return u.Path
	}
	return path.Join(fuseMount, u.Path)
}

// FusePath returns the path relative to the fuse mount root (no leading
// slash), as used by the fuse-client HTTP API /api/files/{path}.
func (u URI) FusePath() string {
	return strings.TrimPrefix(u.Path, "/")
}

// Join returns the URI of a file inside a directory prefix. rel must be a
// clean relative path; traversal out of the prefix is rejected.
func (u URI) Join(rel string) (URI, error) {
	if !u.Dir {
		return URI{}, fmt.Errorf("artifact URI %s is not a directory prefix", u.String())
	}
	for _, seg := range strings.Split(rel, "/") {
		if seg == ".." {
			return URI{}, fmt.Errorf("relative path %q must not traverse out of the prefix", rel)
		}
	}
	clean := path.Clean("/" + rel)
	if clean == "/" {
		return URI{}, fmt.Errorf("empty relative path")
	}
	return URI{Scheme: u.Scheme, Path: u.Path + clean}, nil
}

// CommitPath returns the URI of the object whose presence commits a directory
// artifact — the MANIFEST, written last. For tar artifacts it is the tar
// itself, so callers can Stat one path either way.
func (u URI) CommitPath() URI {
	if !u.Dir {
		return u
	}
	return URI{Scheme: u.Scheme, Path: u.Path + "/" + ManifestName}
}

// DefaultURI builds the default artifact URI for a snapshot. format is
// FormatTar (a single .tar object) or FormatDir (a directory prefix).
func DefaultURI(namespace, name, container, format string) string {
	return withFormat(fmt.Sprintf("fuse:///snapshots/%s/%s/%s", namespace, name, container), format)
}

// DefaultBuildURI builds the default artifact URI for a SnapshotBuild.
// Builds are keyed by revision, not by pod: one artifact, many restores.
func DefaultBuildURI(revision, format string) string {
	return withFormat("fuse:///snapshots/builds/"+revision, format)
}

func withFormat(base, format string) string {
	if format == FormatDir {
		return base + "/"
	}
	return base + ".tar"
}
