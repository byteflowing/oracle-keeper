package keeper

import (
	"context"
	"math/rand/v2"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/servekit/oracle-keeper/pkg/config"
)

func TestNextInterval(t *testing.T) {
	cfg := &config.Config{
		Schedule: &config.ScheduleConfig{Spec: "", IntervalMin: 48 * time.Minute, IntervalMax: 72 * time.Minute},
		Busy:     &config.BusyConfig{CPUPercent: 100, LoadFactor: 1e6},
		CPU:      &config.CPUConfig{},
		Mem:      &config.MemConfig{},
		Net:      &config.NetConfig{RequestTimeout: time.Second},
		Disk:     &config.DiskConfig{Roots: []string{t.TempDir()}},
	}
	kpr, err := New(cfg)
	require.NoError(t, err)

	for range 100 {
		got := kpr.NextInterval(48*time.Minute, 72*time.Minute)
		require.GreaterOrEqual(t, got, 48*time.Minute)
		require.LessOrEqual(t, got, 72*time.Minute)
	}
	// Degenerate range collapses to the single value.
	require.Equal(t, 5*time.Minute, kpr.NextInterval(5*time.Minute, 5*time.Minute))
}

func TestExerciseMemory(t *testing.T) {
	cfg := &config.MemConfig{AllocPercent: 30, MinFreePercent: 25}
	ctx := context.Background()

	t.Run("touches requested share", func(t *testing.T) {
		total, available := uint64(100<<20), uint64(80<<20)
		got := exerciseMemory(ctx, total, available, cfg, 50*time.Millisecond)
		require.Equal(t, uint64(30<<20), got)
	})

	t.Run("scales down to headroom", func(t *testing.T) {
		// want 30M but available-floor only leaves 15M.
		total, available := uint64(100<<20), uint64(40<<20)
		got := exerciseMemory(ctx, total, available, cfg, 50*time.Millisecond)
		require.Equal(t, uint64(15<<20), got)
	})

	t.Run("skips below safety floor", func(t *testing.T) {
		total, available := uint64(100<<20), uint64(20<<20)
		require.Zero(t, exerciseMemory(ctx, total, available, cfg, 50*time.Millisecond))
	})

	t.Run("disabled at zero percent", func(t *testing.T) {
		off := &config.MemConfig{AllocPercent: 0, MinFreePercent: 25}
		require.Zero(t, exerciseMemory(ctx, 1<<30, 1<<29, off, 50*time.Millisecond))
	})
}

func TestBurnCPU(t *testing.T) {
	t.Run("accumulates bounded busy time", func(t *testing.T) {
		// 2 workers × 600ms × 0.5 duty ≈ 0.6s; the ramp envelope (150ms in
		// and out here) only trims the edges, and the deadline check bounds
		// any overshoot.
		busy := burnCPU(context.Background(), 2, 0.5, 600*time.Millisecond)
		require.Greater(t, busy, 0.15)
		require.LessOrEqual(t, busy, 1.0)
	})

	t.Run("cancellation stops immediately", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		require.Zero(t, burnCPU(ctx, 2, 0.5, time.Minute))
	})

	t.Run("zero duration disabled", func(t *testing.T) {
		require.Zero(t, burnCPU(context.Background(), 2, 0.5, 0))
	})
}

// newTestKeeper builds a Keeper with the fast load sampler used by the
// full-cycle tests below.
func newTestKeeper(t *testing.T, cfg *config.Config) *Keeper {
	t.Helper()
	kpr, err := New(cfg)
	require.NoError(t, err)
	kpr.loadSampleInterval = 50 * time.Millisecond
	return kpr
}

// idleBusyConfig disables the busy check for machines running the test suite.
func idleBusyConfig() *config.BusyConfig {
	return &config.BusyConfig{CPUPercent: 101, LoadFactor: 1e6}
}

// TestRunSmoke drives one full cycle with tiny parameters: a short burn, no
// memory/net phases, one temp root. It verifies the orchestrator wires the
// phases together and that the run dir is cleaned up afterwards.
func TestRunSmoke(t *testing.T) {
	if testing.Short() {
		t.Skip("full-cycle smoke test")
	}

	root := t.TempDir()
	cfg := &config.Config{
		Schedule: &config.ScheduleConfig{
			Spec: "0 * * * *", Timezone: "UTC",
			JitterMinutes: 0, MaxRunDuration: time.Minute,
		},
		Busy: idleBusyConfig(),
		CPU:  &config.CPUConfig{BurnDuration: 200 * time.Millisecond, Cores: 1, DutyCycle: 0.5},
		Mem:  &config.MemConfig{AllocPercent: 0, MinFreePercent: 25},
		Net:  &config.NetConfig{TotalMB: 0, MaxPerHostMB: 1, RequestTimeout: time.Second},
		Disk: &config.DiskConfig{Roots: []string{root}, WriteMB: 1, MinFreeMB: 0},
	}
	kpr := newTestKeeper(t, cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	require.NoError(t, kpr.Run(ctx))

	entries, err := os.ReadDir(root)
	require.NoError(t, err)
	require.Empty(t, entries, "cycle must clean up its run dir")
}

// TestRunSkipsWhenBusy pins the yield-to-workload behavior: a busy host must
// return without creating anything on disk.
func TestRunSkipsWhenBusy(t *testing.T) {
	root := t.TempDir()
	cfg := &config.Config{
		Schedule: &config.ScheduleConfig{Spec: "0 * * * *", Timezone: "UTC", MaxRunDuration: time.Minute},
		// CPUPercent 0 makes "sample >= threshold" true for any sample
		// (including a 0.0% reading) — deterministically busy.
		Busy: &config.BusyConfig{CPUPercent: 0, LoadFactor: 1e6},
		CPU:  &config.CPUConfig{BurnDuration: time.Hour, Cores: 1, DutyCycle: 1},
		Mem:  &config.MemConfig{AllocPercent: 0},
		Net:  &config.NetConfig{TotalMB: 0, RequestTimeout: time.Second},
		Disk: &config.DiskConfig{Roots: []string{root}, WriteMB: 1, MinFreeMB: 0},
	}
	kpr := newTestKeeper(t, cfg)

	start := time.Now()
	require.NoError(t, kpr.Run(context.Background()))
	require.Less(t, time.Since(start), 10*time.Second, "busy cycle must not run phases")

	entries, err := os.ReadDir(root)
	require.NoError(t, err)
	require.Empty(t, entries, "busy cycle must not create temp dirs")
}

// TestRunCancelledDuringJitter verifies shutdown responsiveness while the
// cycle is still in its jitter sleep. The rng is pinned to seeds whose first
// draw is a non-zero jitter (skipped defensively otherwise).
func TestRunCancelledDuringJitter(t *testing.T) {
	probe := rand.New(rand.NewPCG(42, 43))
	if time.Duration(probe.IntN(31))*time.Minute == 0 {
		t.Skip("seeded jitter landed on 0 minutes")
	}

	root := t.TempDir()
	cfg := &config.Config{
		Schedule: &config.ScheduleConfig{Spec: "0 * * * *", Timezone: "UTC", JitterMinutes: 30, MaxRunDuration: time.Minute},
		Busy:     idleBusyConfig(),
		CPU:      &config.CPUConfig{BurnDuration: time.Millisecond, Cores: 1, DutyCycle: 1},
		Mem:      &config.MemConfig{AllocPercent: 0},
		Net:      &config.NetConfig{TotalMB: 0, RequestTimeout: time.Second},
		Disk:     &config.DiskConfig{Roots: []string{root}, WriteMB: 0, MinFreeMB: 0},
	}
	kpr := newTestKeeper(t, cfg)
	kpr.rng = rand.New(rand.NewPCG(42, 43))

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	require.ErrorIs(t, kpr.Run(ctx), context.DeadlineExceeded)

	entries, err := os.ReadDir(root)
	require.NoError(t, err)
	require.Empty(t, entries, "cancelled cycle must not leave temp dirs")
}
