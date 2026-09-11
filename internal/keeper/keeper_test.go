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
	shape := cpuShape{duty: 0.5, ramp: 50 * time.Millisecond, wobbleAmp: 0.3, wobblePeriod: 300 * time.Millisecond}

	t.Run("accumulates bounded busy time", func(t *testing.T) {
		// 2 workers × 600ms; duty wobbles within [0.2, 0.8] and the ramp
		// trims the edges — busy seconds stay well inside these bounds.
		busy := burnCPU(context.Background(), 2, 600*time.Millisecond, shape)
		require.Greater(t, busy, 0.1)
		require.LessOrEqual(t, busy, 1.3)
	})

	t.Run("cancellation stops immediately", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		require.Zero(t, burnCPU(ctx, 2, time.Minute, shape))
	})

	t.Run("zero duration disabled", func(t *testing.T) {
		require.Zero(t, burnCPU(context.Background(), 2, 0, shape))
	})
}

func TestDutyAt(t *testing.T) {
	s := cpuShape{duty: 0.8, ramp: 10 * time.Second, wobbleAmp: 0.3, wobblePeriod: 60 * time.Second, wobblePhase: 0}

	t.Run("mid-burn sits within wobble band", func(t *testing.T) {
		d := s.dutyAt(30*time.Second, 30*time.Second)
		require.GreaterOrEqual(t, d, 0.8*0.7)
		require.LessOrEqual(t, d, 0.8*1.3)
	})

	t.Run("edges ramp toward zero", func(t *testing.T) {
		start := s.dutyAt(1*time.Second, 59*time.Second)
		mid := s.dutyAt(30*time.Second, 30*time.Second)
		require.Less(t, start, mid)
	})

	t.Run("always clamped to (0,1]", func(t *testing.T) {
		wide := cpuShape{duty: 0.95, wobbleAmp: 0.9, wobblePeriod: time.Second, wobblePhase: 0}
		for e := range 10 {
			d := wide.dutyAt(time.Duration(e)*100*time.Millisecond, time.Minute)
			require.Greater(t, d, 0.0)
			require.LessOrEqual(t, d, 1.0)
		}
	})
}

// TestNextCycleGap pins the gap lottery: draws stay inside the configured
// range except for the short follow-up branch, which fires often enough to
// be visible in a series.
func TestNextCycleGap(t *testing.T) {
	cfg := &config.Config{
		Schedule: &config.ScheduleConfig{IntervalMin: 30 * time.Minute, IntervalMax: 95 * time.Minute},
		Busy:     &config.BusyConfig{CPUPercent: 100, LoadFactor: 1e6},
		CPU:      &config.CPUConfig{},
		Mem:      &config.MemConfig{},
		Net:      &config.NetConfig{RequestTimeout: time.Second},
		Disk:     &config.DiskConfig{Roots: []string{t.TempDir()}},
	}
	kpr, err := New(cfg)
	require.NoError(t, err)

	shorts := 0
	for range 1000 {
		gap := kpr.NextCycleGap(30*time.Minute, 95*time.Minute)
		require.GreaterOrEqual(t, gap, 8*time.Minute)
		if gap < 30*time.Minute {
			shorts++
			require.LessOrEqual(t, gap, 20*time.Minute)
		} else {
			require.LessOrEqual(t, gap, 95*time.Minute)
		}
	}
	require.Greater(t, shorts, 30, "short follow-up gaps should occur (~12%)")
	require.Less(t, shorts, 250)
}

// newTestKeeper builds a Keeper with the fast load sampler and a low-usage
// fake disk (the baseline gate must not block the test machine's real
// 60-80%-full volumes). Tests needing other values override after.
func newTestKeeper(t *testing.T, cfg *config.Config) *Keeper {
	t.Helper()
	kpr, err := New(cfg)
	require.NoError(t, err)
	kpr.loadSampleInterval = 50 * time.Millisecond
	kpr.diskUsage = fakeUsage(20)
	return kpr
}

// idleBusyConfig disables the busy check for machines running the test suite.
func idleBusyConfig() *config.BusyConfig {
	return &config.BusyConfig{CPUPercent: 101, LoadFactor: 1e6}
}

// TestRunSmoke drives one full cycle with tiny parameters: a short burn, no
// memory/net phases, one temp root. It verifies the orchestrator wires the
// phases together and that the run dir is cleaned up afterwards (delete mode).
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
		Disk: &config.DiskConfig{Roots: []string{root}, WriteMB: 1, MinFreeMB: 0, RetainFiles: false, PurgePercent: 40},
	}
	kpr := newTestKeeper(t, cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	require.NoError(t, kpr.Run(ctx))

	entries, err := os.ReadDir(root)
	require.NoError(t, err)
	require.Empty(t, entries, "delete mode must clean up the run dir")
}

// TestRunRetainsFiles pins retention mode: the cycle's files stay in the run
// dir afterwards (purged only by the high-water mark, exercised separately).
func TestRunRetainsFiles(t *testing.T) {
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
		Disk: &config.DiskConfig{Roots: []string{root}, WriteMB: 1, MinFreeMB: 0, RetainFiles: true, PurgePercent: 40},
	}
	kpr := newTestKeeper(t, cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	require.NoError(t, kpr.Run(ctx))

	entries, err := os.ReadDir(root)
	require.NoError(t, err)
	require.NotEmpty(t, entries, "retention mode must keep the run dir")
}

// TestRunSkipsFileGenerationWhenBaselineHigh pins the baseline gate: when the
// root's usage WITHOUT our files already meets the threshold, the cycle must
// not create any run dir or file at all.
func TestRunSkipsFileGenerationWhenBaselineHigh(t *testing.T) {
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
		Disk: &config.DiskConfig{Roots: []string{root}, WriteMB: 1, MinFreeMB: 0, RetainFiles: true, PurgePercent: 40},
	}
	kpr := newTestKeeper(t, cfg)
	kpr.diskUsage = fakeUsage(50) // baseline 50% >= 40% -> no file generation

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	require.NoError(t, kpr.Run(ctx))

	entries, err := os.ReadDir(root)
	require.NoError(t, err)
	require.Empty(t, entries, "baseline-blocked cycle must not create anything")
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
