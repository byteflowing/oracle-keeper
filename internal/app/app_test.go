package app

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/servekit/oracle-keeper/pkg/config"
)

// testConfig builds a config whose phases are all effectively disabled — the
// app tests exercise scheduling, not the engine. spec "" selects the
// randomized-interval mode.
func testConfig(t *testing.T, spec string) *config.Config {
	t.Helper()
	return &config.Config{
		Schedule: &config.ScheduleConfig{
			Spec: spec, Timezone: "UTC", JitterMinutes: 0, MaxRunDuration: time.Minute,
			IntervalMin: 48 * time.Minute, IntervalMax: 72 * time.Minute,
		},
		Busy: &config.BusyConfig{CPUPercent: 100, LoadFactor: 1e6},
		CPU:  &config.CPUConfig{BurnDuration: time.Millisecond, Cores: 1, DutyCycle: 1},
		Mem:  &config.MemConfig{AllocPercent: 0, MinFreePercent: 25},
		Net:  &config.NetConfig{TotalMB: 0, MaxPerHostMB: 1, RequestTimeout: time.Second},
		Disk: &config.DiskConfig{Roots: []string{t.TempDir()}, WriteMB: 0, MinFreeMB: 0},
	}
}

func TestNewRejectsInvalidSpec(t *testing.T) {
	_, err := New(testConfig(t, "not-a-cron-spec"))
	require.ErrorContains(t, err, "register keep-alive job")
}

func TestIntervalModeSchedulesWithinBounds(t *testing.T) {
	a, err := New(testConfig(t, ""))
	require.NoError(t, err)
	require.True(t, a.intervalMod)

	require.NoError(t, a.Start())
	entries := a.cron.Entries()
	require.NotEmpty(t, entries)

	// Next fire is a gap draw: uniform 48-72m here, or a short follow-up
	// (8-20m) from the lottery — never aligned to wall-clock hours.
	delay := time.Until(entries[0].Next)
	require.GreaterOrEqual(t, delay, 7*time.Minute)
	require.LessOrEqual(t, delay, 73*time.Minute)

	done := make(chan error, 1)
	go func() { done <- a.Stop() }()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("Stop did not drain")
	}
}

func TestIntervalModeReschedulesAfterCycle(t *testing.T) {
	a, err := New(testConfig(t, ""))
	require.NoError(t, err)
	require.NoError(t, a.Start())

	oldID := a.cron.Entries()[0].ID
	a.runCycle() // phase-disabled config: returns quickly, then re-arms

	entries := a.cron.Entries()
	require.Len(t, entries, 1)
	require.NotEqual(t, oldID, entries[0].ID, "runCycle must replace the interval entry")
	delay := time.Until(entries[0].Next)
	require.GreaterOrEqual(t, delay, 7*time.Minute) // short follow-ups allowed
	require.LessOrEqual(t, delay, 73*time.Minute)

	require.NoError(t, a.Stop())
}

func TestStartStopLifecycle(t *testing.T) {
	a, err := New(testConfig(t, "@every 1h"))
	require.NoError(t, err)

	require.NoError(t, a.Start())
	require.NotEmpty(t, a.cron.Entries())

	done := make(chan error, 1)
	go func() { done <- a.Stop() }()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("Stop did not drain")
	}
}

func TestStopWithoutStart(t *testing.T) {
	a, err := New(testConfig(t, "@every 1h"))
	require.NoError(t, err)
	require.NoError(t, a.Stop())
}
