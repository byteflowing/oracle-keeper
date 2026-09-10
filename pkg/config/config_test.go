package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// loadForTest points configx at an empty config file: configx requires a
// config file to exist, and an empty one means all values come from default
// tags and env overrides.
func loadForTest(t *testing.T) (*Config, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("# empty test config\n"), 0o644); err != nil {
		t.Fatalf("write test config: %v", err)
	}
	t.Setenv("ORACLE_KEEPER_CONFIG", path)
	return Load()
}

func TestLoadDefaults(t *testing.T) {
	cfg, err := loadForTest(t)
	require.NoError(t, err)

	require.Empty(t, cfg.Schedule.Spec) // empty = randomized interval mode
	require.Equal(t, 48*time.Minute, cfg.Schedule.IntervalMin)
	require.Equal(t, 72*time.Minute, cfg.Schedule.IntervalMax)
	require.Equal(t, 2, cfg.Schedule.JitterMinutes)
	require.Equal(t, 45*time.Minute, cfg.Schedule.MaxRunDuration)

	require.InDelta(t, 40, cfg.Busy.CPUPercent, 0)
	require.InDelta(t, 0.8, cfg.Busy.LoadFactor, 0)

	require.Equal(t, 5*time.Minute, cfg.CPU.BurnDuration)
	require.Zero(t, cfg.CPU.Cores)
	require.InDelta(t, 0.7, cfg.CPU.DutyCycle, 0)

	require.Equal(t, 30, cfg.Mem.AllocPercent)
	require.Equal(t, 25, cfg.Mem.MinFreePercent)

	require.Equal(t, 700, cfg.Net.TotalMB)
	require.Equal(t, 250, cfg.Net.MaxPerHostMB)
	require.Equal(t, 5*time.Minute, cfg.Net.RequestTimeout)

	require.Equal(t, []string{"/var/tmp/oracle-keeper", "/data/oracle-keeper"}, cfg.Disk.Roots)
	require.Equal(t, 256, cfg.Disk.WriteMB)
	require.Equal(t, uint64(2048), cfg.Disk.MinFreeMB)
}

func TestLoadEnvOverrides(t *testing.T) {
	t.Setenv("ORACLE_KEEPER_NET_TOTAL_MB", "42")
	t.Setenv("ORACLE_KEEPER_CPU_BURN_DURATION", "90s")
	t.Setenv("ORACLE_KEEPER_SCHEDULE_JITTER_MINUTES", "0")
	t.Setenv("ORACLE_KEEPER_DISK_ROOTS", "/mnt/root-tmp,/mnt/data-tmp")

	cfg, err := loadForTest(t)
	require.NoError(t, err)
	require.Equal(t, 42, cfg.Net.TotalMB)
	require.Equal(t, 90*time.Second, cfg.CPU.BurnDuration)
	require.Equal(t, 0, cfg.Schedule.JitterMinutes)
	require.Equal(t, []string{"/mnt/root-tmp", "/mnt/data-tmp"}, cfg.Disk.Roots)
}

func TestLoadRejectsInvalid(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
	}{
		{name: "duty cycle above one", env: map[string]string{"ORACLE_KEEPER_CPU_DUTY_CYCLE": "1.5"}},
		{name: "alloc percent too high", env: map[string]string{"ORACLE_KEEPER_MEM_ALLOC_PERCENT": "95"}},
		{name: "per-host cap missing with budget", env: map[string]string{"ORACLE_KEEPER_NET_MAX_PER_HOST_MB": "0"}},
		{name: "negative write pass", env: map[string]string{"ORACLE_KEEPER_DISK_WRITE_MB": "-1"}},
		{name: "interval max below min", env: map[string]string{
			"ORACLE_KEEPER_SCHEDULE_INTERVAL_MIN": "60m",
			"ORACLE_KEEPER_SCHEDULE_INTERVAL_MAX": "30m",
		}},
		{name: "interval min zero", env: map[string]string{"ORACLE_KEEPER_SCHEDULE_INTERVAL_MIN": "0s"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for k, v := range tt.env {
				t.Setenv(k, v)
			}
			_, err := loadForTest(t)
			require.Error(t, err)
		})
	}
}
