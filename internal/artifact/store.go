package artifact

import (
	"context"
	"fmt"
	"io"
	"os"
)

// FuseAPI is the subset of the fuse-client HTTP API the store needs.
type FuseAPI interface {
	Stat(ctx context.Context, fusePath string) (int64, error)
	Get(ctx context.Context, fusePath string) (io.ReadCloser, error)
	Delete(ctx context.Context, fusePath string) error
}

// Store resolves artifact URIs against a fuse-client endpoint (fuse://) or
// the local filesystem (file://). Used by the manager (Stat before restore,
// Delete on DeletionPolicy=Delete) — the node agent uses direct host-path
// I/O through the mount instead.
type Store struct {
	Fuse FuseAPI
}

// Stat returns the artifact size or an error if it does not exist.
//
// For a directory artifact this reads the MANIFEST — the object written last
// — so a prefix that is still being uploaded reads as absent rather than as
// a partial artifact (docs/design-v2.md §9), and the size returned is the
// tree total rather than the marker's own size.
func (s *Store) Stat(ctx context.Context, uri URI) (int64, error) {
	if uri.Dir {
		m, err := s.manifest(ctx, uri)
		if err != nil {
			return 0, err
		}
		return m.TotalBytes, nil
	}
	return s.statObject(ctx, uri)
}

func (s *Store) statObject(ctx context.Context, uri URI) (int64, error) {
	switch uri.Scheme {
	case SchemeFile:
		fi, err := os.Stat(uri.Path)
		if err != nil {
			return 0, err
		}
		return fi.Size(), nil
	case SchemeFuse:
		if s.Fuse == nil {
			return 0, fmt.Errorf("no fuse-client endpoint configured for %s", uri.String())
		}
		return s.Fuse.Stat(ctx, uri.FusePath())
	}
	return 0, fmt.Errorf("unsupported scheme %q", uri.Scheme)
}

// manifest fetches and decodes a directory artifact's commit marker.
func (s *Store) manifest(ctx context.Context, uri URI) (*Manifest, error) {
	commit := uri.CommitPath()
	var r io.ReadCloser
	switch commit.Scheme {
	case SchemeFile:
		f, err := os.Open(commit.Path)
		if err != nil {
			return nil, err
		}
		r = f
	case SchemeFuse:
		if s.Fuse == nil {
			return nil, fmt.Errorf("no fuse-client endpoint configured for %s", uri.String())
		}
		var err error
		if r, err = s.Fuse.Get(ctx, commit.FusePath()); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("unsupported scheme %q", uri.Scheme)
	}
	defer r.Close()
	return DecodeManifest(r)
}

// Delete removes the artifact. A directory artifact is un-committed first
// (MANIFEST removed) so a concurrent restore never sees a half-deleted tree
// as usable, then the listed files go.
func (s *Store) Delete(ctx context.Context, uri URI) error {
	if !uri.Dir {
		return s.deleteObject(ctx, uri)
	}

	m, err := s.manifest(ctx, uri)
	if err != nil {
		// No manifest: either already gone, or never committed. Nothing
		// safe or useful left to enumerate.
		return nil
	}
	if err := s.deleteObject(ctx, uri.CommitPath()); err != nil {
		return err
	}
	for _, f := range m.Files {
		fileURI, err := uri.Join(f.Path)
		if err != nil {
			continue
		}
		if err := s.deleteObject(ctx, fileURI); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) deleteObject(ctx context.Context, uri URI) error {
	switch uri.Scheme {
	case SchemeFile:
		err := os.Remove(uri.Path)
		if os.IsNotExist(err) {
			return nil
		}
		return err
	case SchemeFuse:
		if s.Fuse == nil {
			return fmt.Errorf("no fuse-client endpoint configured for %s", uri.String())
		}
		return s.Fuse.Delete(ctx, uri.FusePath())
	}
	return fmt.Errorf("unsupported scheme %q", uri.Scheme)
}
