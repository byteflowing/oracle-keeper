package keeper

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/shirou/gopsutil/v4/disk"
)

// runDirPrefix names per-cycle temp dirs inside each root so the startup
// sweep can recognize its own leftovers.
const runDirPrefix = "run-"

// writeChunkB is the per-file size of the disk write pass.
const writeChunkB = 64 << 20

// prepareRoots returns the roots usable this cycle: the directory is created
// when missing and must currently hold at least cfg.Disk.MinFreeMB. Roots on
// different mounts (root disk vs /data) are what spreads I/O across volumes.
func (k *Keeper) prepareRoots(ctx context.Context) []string {
	var usable []string
	for _, root := range k.cfg.Disk.Roots {
		if err := os.MkdirAll(root, 0o755); err != nil {
			slog.Warn("disk root unusable", "root", root, "error", err)
			continue
		}
		usage, err := disk.UsageWithContext(ctx, root)
		if err != nil {
			slog.Warn("disk root: stat failed", "root", root, "error", err)
			continue
		}
		if usage.Free < k.cfg.Disk.MinFreeMB*(1<<20) {
			slog.Warn("disk root low on space, skipping this cycle", "root", root,
				"free_gb", float64(usage.Free)/(1<<30), "min_free_mb", k.cfg.Disk.MinFreeMB)
			continue
		}
		usable = append(usable, root)
	}
	return usable
}

// makeRunDir creates a unique run-<timestamp>-<rand> directory under root.
func makeRunDir(root string) (string, error) {
	dir, err := os.MkdirTemp(root, runDirPrefix)
	if err != nil {
		return "", fmt.Errorf("create run dir under %s: %w", root, err)
	}
	return dir, nil
}

// cleanupRunDirs removes run dirs created by this cycle.
func cleanupRunDirs(dirs []string) {
	for _, dir := range dirs {
		if err := os.RemoveAll(dir); err != nil {
			slog.Warn("cleanup run dir", "dir", dir, "error", err)
		}
	}
}

// sweepStale removes run-* directories older than maxAge across roots. It
// runs at startup to reclaim space from cycles killed by SIGKILL, where the
// deferred cleanup never fired. Returns the number of directories removed.
// Only used in delete mode — with RetainFiles on, purgeRoots owns cleanup.
func sweepStale(roots []string, maxAge time.Duration) int {
	cutoff := time.Now().Add(-maxAge)
	removed := 0
	for _, root := range roots {
		entries, err := os.ReadDir(root)
		if err != nil {
			continue // missing root is not worth a warn here
		}
		for _, e := range entries {
			if !e.IsDir() || !strings.HasPrefix(e.Name(), runDirPrefix) {
				continue
			}
			info, err := e.Info()
			if err != nil || info.ModTime().After(cutoff) {
				continue
			}
			dir := filepath.Join(root, e.Name())
			if err := os.RemoveAll(dir); err != nil {
				slog.Warn("sweep stale run dir", "dir", dir, "error", err)
				continue
			}
			removed++
			slog.Info("swept stale run dir", "dir", dir)
		}
	}
	return removed
}

// purgeRoots enforces the retention high-water mark: for every root whose
// used percentage is at or above cfg.Disk.PurgePercent, all retained run-*
// directories are removed at once. It runs at cycle start, before the fresh
// run dir exists, so `keep` is normally nil; the current cycle's dir is
// passed by tests. Roots whose usage stays above the watermark after the
// purge are occupied by foreign data — logged, not fought.
func (k *Keeper) purgeRoots(ctx context.Context, roots []string, keep map[string]bool) {
	for _, root := range roots {
		usage, err := k.diskUsage(ctx, root)
		if err != nil {
			slog.Warn("purge: stat root", "root", root, "error", err)
			continue
		}
		if usage.UsedPercent < k.cfg.Disk.PurgePercent {
			continue
		}
		entries, err := os.ReadDir(root)
		if err != nil {
			slog.Warn("purge: read root", "root", root, "error", err)
			continue
		}
		var freed int64
		count := 0
		for _, e := range entries {
			if !e.IsDir() || !strings.HasPrefix(e.Name(), runDirPrefix) {
				continue
			}
			dir := filepath.Join(root, e.Name())
			if keep[dir] {
				continue
			}
			freed += dirSize(dir)
			if err := os.RemoveAll(dir); err != nil {
				slog.Warn("purge: remove run dir", "dir", dir, "error", err)
				continue
			}
			count++
		}
		slog.Info("purged retained run dirs above high-water mark",
			"root", root, "used_percent", usage.UsedPercent,
			"purge_percent", k.cfg.Disk.PurgePercent,
			"dirs", count, "freed_mb", float64(freed)/(1<<20))
		if after, err := k.diskUsage(ctx, root); err == nil && after.UsedPercent >= k.cfg.Disk.PurgePercent {
			slog.Warn("root still above watermark after purge; space is used by foreign data",
				"root", root, "used_percent", after.UsedPercent)
		}
	}
}

// dirSize sums the regular file sizes under dir via a single walk.
func dirSize(dir string) int64 {
	var total int64
	// Best-effort accounting: a file vanishing mid-walk must not abort the
	// sum, so per-entry errors are swallowed on purpose.
	if err := filepath.WalkDir(dir, func(_ string, d os.DirEntry, err error) error {
		if err != nil {
			return nil //nolint:nilerr // see comment above
		}
		if info, infoErr := d.Info(); infoErr == nil && !d.IsDir() {
			total += info.Size()
		}
		return nil
	}); err != nil {
		slog.Warn("disk: size walk", "dir", dir, "error", err)
	}
	return total
}

// writePass writes totalB of random data across dirs in writeChunkB files,
// syncing each, then returns the byte count written. Even when every download
// fails this guarantees real write+flush I/O on every mounted disk.
func writePass(ctx context.Context, dirs []string, totalB int64) int64 {
	if totalB <= 0 || len(dirs) == 0 {
		return 0
	}
	buf := make([]byte, 1<<20)
	if _, err := io.ReadFull(rand.Reader, buf); err != nil {
		slog.Warn("disk write pass: fill random buffer", "error", err)
		return 0
	}

	var written int64
	for i := 0; written < totalB; i++ {
		if ctx.Err() != nil {
			break
		}
		size := min(writeChunkB, totalB-written)
		dir := dirs[i%len(dirs)]
		path := filepath.Join(dir, fmt.Sprintf("write-%03d.bin", i))
		n, err := writeFile(path, buf, size)
		written += n
		if err != nil {
			slog.Warn("disk write pass", "path", path, "error", err)
			if n == 0 {
				break // the disk is refusing writes; no point hammering it
			}
		}
	}
	return written
}

// writeFile streams buf repeatedly into a fresh file at path until size bytes
// are written, syncs, closes, and returns the byte count. Sync/Close errors
// surface alongside the first write error via the deferred join.
func writeFile(path string, buf []byte, size int64) (written int64, err error) {
	f, err := os.Create(path)
	if err != nil {
		return 0, fmt.Errorf("create %s: %w", path, err)
	}
	defer func() {
		err = errors.Join(err, f.Sync(), f.Close())
	}()
	for written < size {
		n, werr := f.Write(buf[:min(int64(len(buf)), size-written)])
		written += int64(n)
		if werr != nil {
			return written, fmt.Errorf("write %s: %w", path, werr)
		}
	}
	return written, nil
}
