package keeper

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestSweepStale(t *testing.T) {
	root := t.TempDir()

	stale := filepath.Join(root, "run-stale")
	fresh := filepath.Join(root, runDirPrefix+"fresh")
	other := filepath.Join(root, "unrelated-dir")
	for _, dir := range []string{stale, fresh, other} {
		require.NoError(t, os.MkdirAll(dir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "leftover.bin"), []byte("x"), 0o644))
	}

	old := time.Now().Add(-48 * time.Hour)
	require.NoError(t, os.Chtimes(stale, old, old))

	removed := sweepStale([]string{root}, 26*time.Hour)

	require.Equal(t, 1, removed)
	require.NoDirExists(t, stale)
	require.DirExists(t, fresh)
	require.DirExists(t, other)
}

func TestSweepStaleMissingRoot(t *testing.T) {
	require.Equal(t, 0, sweepStale([]string{filepath.Join(t.TempDir(), "does-not-exist")}, time.Nanosecond))
}

func TestRunDirLifecycle(t *testing.T) {
	root := t.TempDir()

	dir, err := makeRunDir(root)
	require.NoError(t, err)
	require.DirExists(t, dir)
	require.Contains(t, filepath.Base(dir), runDirPrefix)
	require.Equal(t, root, filepath.Dir(dir))

	require.NoError(t, os.WriteFile(filepath.Join(dir, "dl-test.bin"), []byte("payload"), 0o644))
	cleanupRunDirs([]string{dir})
	require.NoDirExists(t, dir)
}

func TestWritePass(t *testing.T) {
	dirs := []string{t.TempDir(), t.TempDir()}

	// 130 MiB = 3 chunks (64+64+2 MiB), rotating across both dirs.
	written := writePass(context.Background(), dirs, 130*(1<<20))

	require.Equal(t, int64(130<<20), written)
	// Both dirs received at least one file; the files stay in place until
	// the caller's run-dir cleanup removes them.
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		require.NoError(t, err)
		require.NotEmpty(t, entries)
	}
}

func TestWritePassZeroDisabled(t *testing.T) {
	require.Zero(t, writePass(context.Background(), []string{t.TempDir()}, 0))
}

func TestWritePassCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	dirs := []string{t.TempDir()}
	require.Zero(t, writePass(ctx, dirs, 10*(1<<20)))
}
