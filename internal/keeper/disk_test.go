package keeper

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/shirou/gopsutil/v4/disk"
	"github.com/stretchr/testify/require"

	"github.com/servekit/oracle-keeper/pkg/config"
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

// fakeUsage returns a diskUsage func reporting a fixed percentage.
func fakeUsage(pct float64) func(context.Context, string) (*disk.UsageStat, error) {
	return func(_ context.Context, _ string) (*disk.UsageStat, error) {
		return &disk.UsageStat{UsedPercent: pct}, nil
	}
}

// newPurgeKeeper builds a Keeper with the given reported disk usage.
func newPurgeKeeper(t *testing.T, pct float64) *Keeper {
	t.Helper()
	cfg := &config.Config{
		Schedule: &config.ScheduleConfig{IntervalMin: time.Minute, IntervalMax: time.Minute, MaxRunDuration: time.Minute},
		Busy:     &config.BusyConfig{CPUPercent: 100, LoadFactor: 1e6},
		CPU:      &config.CPUConfig{},
		Mem:      &config.MemConfig{},
		Net:      &config.NetConfig{RequestTimeout: time.Second},
		Disk:     &config.DiskConfig{RetainFiles: true, PurgePercent: 40},
	}
	kpr, err := New(cfg)
	require.NoError(t, err)
	kpr.diskUsage = fakeUsage(pct)
	return kpr
}

func TestPurgeRoots(t *testing.T) {
	newRoot := func(t *testing.T) (string, string, string) {
		t.Helper()
		root := t.TempDir()
		old1 := filepath.Join(root, "run-old1")
		old2 := filepath.Join(root, "run-old2")
		foreign := filepath.Join(root, "unrelated")
		for _, dir := range []string{old1, old2, foreign} {
			require.NoError(t, os.MkdirAll(dir, 0o755))
			require.NoError(t, os.WriteFile(filepath.Join(dir, "data.bin"), []byte("payload"), 0o644))
		}
		return root, old1, old2
	}

	t.Run("below watermark keeps everything", func(t *testing.T) {
		root, old1, old2 := newRoot(t)
		newPurgeKeeper(t, 30).purgeRoots(context.Background(), []string{root}, nil)
		require.DirExists(t, old1)
		require.DirExists(t, old2)
	})

	t.Run("above watermark purges old run dirs only", func(t *testing.T) {
		root, old1, old2 := newRoot(t)
		newPurgeKeeper(t, 50).purgeRoots(context.Background(), []string{root}, nil)
		require.NoDirExists(t, old1)
		require.NoDirExists(t, old2)
		require.DirExists(t, filepath.Join(root, "unrelated"), "foreign data must not be touched")
	})

	t.Run("keep set survives the purge", func(t *testing.T) {
		root, old1, _ := newRoot(t)
		newPurgeKeeper(t, 50).purgeRoots(context.Background(), []string{root}, map[string]bool{old1: true})
		require.DirExists(t, old1)
		require.NoDirExists(t, filepath.Join(root, "run-old2"))
	})

	t.Run("at exact watermark purges", func(t *testing.T) {
		root, old1, _ := newRoot(t)
		newPurgeKeeper(t, 40).purgeRoots(context.Background(), []string{root}, nil)
		require.NoDirExists(t, old1)
	})
}

func TestDirSize(t *testing.T) {
	dir := t.TempDir()
	require.Zero(t, dirSize(dir))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.bin"), make([]byte, 1000), 0o644))
	sub := filepath.Join(dir, "sub")
	require.NoError(t, os.MkdirAll(sub, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(sub, "b.bin"), make([]byte, 2500), 0o644))
	require.Equal(t, int64(3500), dirSize(dir))
	// Missing dir sizes to zero rather than erroring.
	require.Zero(t, dirSize(filepath.Join(dir, "does-not-exist")))
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
