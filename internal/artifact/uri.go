// Package artifact handles checkpoint artifact URIs and storage.
//
// Supported schemes:
//
//	fuse:///<path> — a path under the fuse-client distributed cache mount.
//	                 On every node this resolves to <fuse-mount>/<path>
//	                 (default /mnt/fuse/<path>); fuse-client persists writes
//	                 to its cloud tier and serves reads through NVMe/peers.
//	file:///<path> — an absolute node-local path, for testing without fuse.
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

// URI is a parsed artifact location.
type URI struct {
	Scheme string
	// Path is the cleaned path component, always starting with "/".
	Path string
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
	p := path.Clean(u.Path)
	if p == "/" || p == "." || !strings.HasPrefix(p, "/") {
		return URI{}, fmt.Errorf("artifact URI %q has no usable path", raw)
	}
	return URI{Scheme: u.Scheme, Path: p}, nil
}

// String reassembles the URI.
func (u URI) String() string {
	return u.Scheme + "://" + u.Path
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

// DefaultURI builds the default artifact URI for a snapshot.
func DefaultURI(namespace, name, container string) string {
	return fmt.Sprintf("fuse:///snapshots/%s/%s/%s.tar", namespace, name, container)
}
