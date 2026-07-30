package artifact

import (
	"context"
	"fmt"
	"os"
)

// FuseAPI is the subset of the fuse-client HTTP API the store needs.
type FuseAPI interface {
	Stat(ctx context.Context, fusePath string) (int64, error)
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
func (s *Store) Stat(ctx context.Context, uri URI) (int64, error) {
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

// Delete removes the artifact.
func (s *Store) Delete(ctx context.Context, uri URI) error {
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
