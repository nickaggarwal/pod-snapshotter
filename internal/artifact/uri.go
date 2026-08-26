// Package artifact handles checkpoint artifact URIs and storage.
//
// Supported schemes:
//
//	fuse:///<path> — a path under the fuse-client distributed cache mount.
//	                 On every node this resolves to <fuse-mount>/<path>
//	                 (default /mnt/fuse/<path>); fuse-client persists writes
//	                 to its cloud tier and serves reads through NVMe/peers.
//	file:///<path> — an absolute node-local path: the node's own disk, most
//	                 usefully its NVMe. There is no network filesystem in the
//	                 path at all, which is the fast tier for dump and restore
//	                 both, and also the narrow one -- the artifact exists on
//	                 exactly one node, so the restore has to land there.
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

// DefaultRoot is the artifact root used when nothing configures one: the
// fuse-client distributed mount, which is the only tier every node can read.
const DefaultRoot = "fuse:///snapshots"

// ParseRoot validates an artifact root -- a scheme plus a directory prefix
// that DefaultURI and DefaultBuildURI hang snapshot paths off, e.g.
// "fuse:///snapshots" or "file:///mnt/fuse-nvme0n1/ps-artifacts". It returns
// the root with any trailing slash removed, so callers can concatenate.
//
// This exists so a misconfigured root is a startup error in one place rather
// than a per-snapshot parse failure: the manager resolves it once at boot,
// and every URI built from it is then known to parse.
func ParseRoot(root string) (string, error) {
	if root == "" {
		root = DefaultRoot
	}
	root = strings.TrimRight(root, "/")
	// Probe with a path component: a bare root has nothing after the scheme
	// for Parse to accept, and it is the joined form that has to be legal.
	if _, err := Parse(root + "/probe"); err != nil {
		return "", fmt.Errorf("invalid artifact root %q: %w", root, err)
	}
	return root, nil
}

// DefaultURI builds the default artifact URI for a snapshot under root (see
// ParseRoot; empty means DefaultRoot). format is FormatTar (a single .tar
// object) or FormatDir (a directory prefix).
func DefaultURI(root, namespace, name, container, format string) string {
	return withFormat(fmt.Sprintf("%s/%s/%s/%s", rootOrDefault(root), namespace, name, container), format)
}

// DefaultBuildURI builds the default artifact URI for a SnapshotBuild under
// root. Builds are keyed by revision, not by pod: one artifact, many restores.
func DefaultBuildURI(root, revision, format string) string {
	return withFormat(rootOrDefault(root)+"/builds/"+revision, format)
}

// rootOrDefault is the lenient counterpart to ParseRoot, for callers holding a
// root that was already validated (or that never configured one). It does not
// re-validate: a root that got here unparseable produces an unparseable URI,
// which the caller's Parse then reports against the actual snapshot.
func rootOrDefault(root string) string {
	if root == "" {
		return DefaultRoot
	}
	return strings.TrimRight(root, "/")
}

func withFormat(base, format string) string {
	if format == FormatDir {
		return base + "/"
	}
	return base + ".tar"
}

// CheckLocalRoot rejects a file:// URI that points outside root.
//
// A file:// artifact is a raw host path, and on the agent it is a raw host
// path written by a privileged process — so the URI on a CRD is, without
// this, a request to have the agent create directories and image files
// anywhere on the node. Confining them to one configured directory (the
// agent's --local-artifact-root, which is also the only host path the
// DaemonSet mounts writable) turns that into a bounded blast radius: the
// worst a bad URI can do is make a mess inside the artifact tier.
//
// An empty root disables the check, which is what unit tests and single-node
// dev clusters run with — there, file:// under a t.TempDir() is the point.
// fuse:// URIs are never affected: they are relative to the mount by
// construction and Parse has already rejected traversal.
func CheckLocalRoot(u URI, root string) error {
	if u.Scheme != SchemeFile || root == "" {
		return nil
	}
	root = strings.TrimRight(path.Clean(root), "/")
	if u.Path != root && !strings.HasPrefix(u.Path, root+"/") {
		return fmt.Errorf("artifact %s is outside this node's local artifact root %s", u.String(), root)
	}
	return nil
}
