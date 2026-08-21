package artifact

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
)

// DefaultPrefetchParallelism is the number of files fetched concurrently
// from a directory artifact. Directory artifacts exist so this can be more
// than one: the v1 tar forces a single sequential stream through
// fuse-client, leaving most of the NVMe/network bandwidth on the floor
// (docs/design-v2.md §3).
//
// Four, not eight, and not "as many as we can". Measured against a warm
// fuse-client on an A100 node, reading the 14B artifact's page images:
//
//	P=1   335-782 MiB/s
//	P=2   542 MiB/s
//	P=4   931-1322 MiB/s   <- peak
//	P=6   705 MiB/s
//	P=8   642 MiB/s        <- past the knee, and where the client OOMKilled
//
// Throughput peaks at 4 and then *falls*, so raising this is not a free
// knob to turn up under pressure — 8 was both slower than 4 and the setting
// that killed fuse-client (exit 137) mid-restore. The collapse and the OOM
// are the same event: the client runs close to its memory limit, and enough
// concurrent readers push it over, at which point everything queues behind a
// restart. See docs/design-v2.md §6c.
const DefaultPrefetchParallelism = 4

// PrefetchOpts configures a directory-artifact prefetch.
type PrefetchOpts struct {
	// SrcDir is the artifact prefix as visible on this node — normally the
	// path under the fuse-client mount.
	SrcDir string
	// Manifest is the committed file set. Required: it is what makes a
	// prefix safe to read (a prefix without one is treated as absent).
	Manifest *Manifest
	// StageDir, when set, copies each file into node-local storage and the
	// restore reads from there. When empty the files are only read through
	// (fuse-client promotes every miss to the node's NVMe tier) and the
	// restore reads them in place through the mount — no copy at all, which
	// is the point of directory artifacts.
	StageDir string
	// Parallelism defaults to DefaultPrefetchParallelism.
	Parallelism int
	// Verify recomputes each file's sha256 against the manifest. Off by
	// default: it is a second pass over multi-GB images on the critical path.
	Verify bool
}

// Prefetch fetches every file the manifest lists, concurrently, and returns
// the total bytes moved.
func Prefetch(ctx context.Context, opts PrefetchOpts) (int64, error) {
	if opts.Manifest == nil {
		return 0, fmt.Errorf("prefetch requires a manifest")
	}
	workers := opts.Parallelism
	if workers <= 0 {
		workers = DefaultPrefetchParallelism
	}
	if workers > len(opts.Manifest.Files) {
		workers = len(opts.Manifest.Files)
	}

	jobs := make(chan ManifestFile)
	var total atomic.Int64
	var once sync.Once
	var firstErr error
	fail := func(err error) {
		once.Do(func() { firstErr = err })
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			buf := make([]byte, 8<<20)
			for f := range jobs {
				n, err := fetchOne(opts, f, buf)
				total.Add(n)
				if err != nil {
					fail(err)
					cancel()
					return
				}
			}
		}()
	}

	for _, f := range opts.Manifest.Files {
		select {
		case jobs <- f:
		case <-ctx.Done():
		}
		if ctx.Err() != nil {
			break
		}
	}
	close(jobs)
	wg.Wait()

	if firstErr != nil {
		return total.Load(), firstErr
	}
	if err := ctx.Err(); err != nil {
		return total.Load(), err
	}
	return total.Load(), nil
}

// fetchOne warms (or stages) a single artifact file.
func fetchOne(opts PrefetchOpts, f ManifestFile, buf []byte) (int64, error) {
	src := filepath.Join(opts.SrcDir, filepath.FromSlash(f.Path))
	in, err := os.Open(src)
	if err != nil {
		return 0, fmt.Errorf("opening %s: %w", f.Path, err)
	}
	defer in.Close()

	var sinks []io.Writer
	var digest hash.Hash
	if opts.Verify {
		digest = sha256.New()
		sinks = append(sinks, digest)
	}
	var staged string
	if opts.StageDir != "" {
		staged = filepath.Join(opts.StageDir, filepath.FromSlash(f.Path))
		if err := os.MkdirAll(filepath.Dir(staged), 0o755); err != nil {
			return 0, err
		}
		mode := os.FileMode(f.Mode) & 0o777
		if mode == 0 {
			mode = 0o644
		}
		if err := os.Remove(staged); err != nil && !os.IsNotExist(err) {
			return 0, err
		}
		out, err := os.OpenFile(staged, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
		if err != nil {
			return 0, err
		}
		defer out.Close()
		sinks = append(sinks, out)
	}

	var sink io.Writer = io.Discard
	if len(sinks) == 1 {
		sink = sinks[0]
	} else if len(sinks) > 1 {
		sink = io.MultiWriter(sinks...)
	}

	n, err := io.CopyBuffer(sink, in, buf) // #nosec G110 -- size bounded by the manifest
	if err != nil {
		return n, fmt.Errorf("reading %s: %w", f.Path, err)
	}
	if n != f.Size {
		return n, fmt.Errorf("artifact file %s is %d bytes, manifest says %d", f.Path, n, f.Size)
	}
	if digest != nil && f.SHA256 != "" {
		if got := hex.EncodeToString(digest.Sum(nil)); got != f.SHA256 {
			return n, fmt.Errorf("artifact file %s digest %s does not match manifest %s", f.Path, got, f.SHA256)
		}
	}
	return n, nil
}
