package artifact

import (
	"fmt"
	"os"
	"path/filepath"
)

// NVMeCache describes fuse-client's node-local cache tier as a directory the
// restore can read *directly*, bypassing the FUSE mount.
//
// Why bypass a filesystem we already have mounted: fuse-client promotes every
// artifact byte onto the node's NVMe on first read, so after pre-warm the
// bytes exist twice — once on the device, and once behind a userspace round
// trip that re-serves them. Measured on an A100 node against the same 53 GiB
// checkpoint, same page files, same warm cache:
//
//	through the FUSE mount   500-690 MB/s
//	raw NVMe, 1 stream       2.6 GB/s
//	raw NVMe, 4 streams      4.5 GB/s
//
// That is a ~7x tax for re-reading bytes that are already local, and it is
// what put a 53 GiB restore in minutes rather than seconds. CRIU is perfectly
// happy to read the cache files itself: the tier is a faithful 1:1 mirror of
// the artifact prefix (same relative paths, same names, same sizes).
//
// The mirror is an implementation detail of fuse-client, not a contract, so
// every use is guarded by Resolve below and silently declines to the mount
// whenever the layout does not match what the manifest says it should be.
type NVMeCache struct {
	// Root is the cache tier's path *on the host*, e.g.
	// /mnt/fuse-nvme0n1/fuse-cache. Empty disables the bypass.
	//
	// Host path, not container path, because the consumer is runc: the agent
	// execs it through nsenter into the host mount namespace, so it resolves
	// this string there and never sees the agent's own filesystem. Handing it
	// a container path fails as a missing file at restore time, which reads
	// like an incomplete artifact rather than the path-translation bug it is.
	Root string
	// HostRoot is where the host's / is mounted inside the agent (e.g.
	// /host). Resolve's own checks go through it, and it is what makes
	// CacheDir.Local differ from CacheDir.Host. Empty means the two
	// namespaces agree.
	HostRoot string
}

// CacheDir is one artifact prefix on the cache tier, named from both mount
// namespaces. The two differ, and which one is correct depends on who does
// the opening: the agent reads the checkpoint's metadata itself (Local),
// while runc is exec'd through nsenter into the host namespace and opens the
// page images there (Host). Passing one where the other belongs fails as a
// missing file at restore time, which reads like an incomplete artifact
// rather than the path-translation bug it is — so they are carried
// separately rather than as one string with a convention attached.
type CacheDir struct {
	// Host is the path in the host mount namespace. Hand this to runc.
	Host string
	// Local is the same directory as this process can reach it. Read
	// through this.
	Local string
}

// Resolve returns the cache-tier directory holding this artifact prefix, or
// the zero CacheDir when the bypass must not be used.
//
// fusePath is the artifact prefix relative to the fuse mount root (URI.FusePath).
//
// It verifies every file the manifest lists is present at exactly the right
// size before returning a path. That check is the whole safety argument: a
// partially-promoted prefix, an evicted file, or a cache layout that stops
// mirroring 1:1 all produce a zero value and the caller reads through the
// mount as before. Size is what CRIU would trip over — it seeks to absolute
// offsets in the page images — and it is cheap to check even for 632 files,
// unlike a digest pass over 53 GiB on the critical path.
//
// Note the asymmetry with Prefetch's Verify: there, hashing is optional
// because a bad read through the mount is fuse-client's bug to report. Here we
// are stepping around fuse-client, so the burden of proving the bytes are
// really there is ours.
func (c NVMeCache) Resolve(fusePath string, m *Manifest) (CacheDir, error) {
	if c.Root == "" || m == nil {
		return CacheDir{}, nil
	}
	dir := CacheDir{Host: filepath.Join(c.Root, filepath.FromSlash(fusePath))}
	dir.Local = dir.Host
	if c.HostRoot != "" {
		dir.Local = filepath.Join(c.HostRoot, dir.Host)
	}
	fi, err := os.Stat(dir.Local)
	if err != nil || !fi.IsDir() {
		return CacheDir{}, nil
	}
	for _, f := range m.Files {
		st, err := os.Stat(filepath.Join(dir.Local, filepath.FromSlash(f.Path)))
		if err != nil {
			return CacheDir{}, nil
		}
		if st.Size() != f.Size {
			return CacheDir{}, fmt.Errorf("cache file %s is %d bytes, manifest says %d", f.Path, st.Size(), f.Size)
		}
	}
	return dir, nil
}
