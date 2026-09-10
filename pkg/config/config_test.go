package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// loadForTest points configx at a minimal config file: configx requires a
// config file to exist, and env overrides only apply to keys the file (or a
// default tag) registers — so the file mirrors the shipped config.example.yaml
// structure for the env-driven fields (empty values).
func loadForTest(t *testing.T) (*Config, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	content := `# test config
schedule:
  spec: ""
net:
  sources: []
db:
  driver: ""
  port: 0
  user: ""
  password: ""
  db_name: ""
  sqlite_path: ""
  table: "keeper_orders"
  op_cron: "@every 20s"
  max_rows: 1000000
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
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

	require.Equal(t, []string{"/data/oracle-keeper"}, cfg.Disk.Roots)
	require.Equal(t, 256, cfg.Disk.WriteMB)
	require.Equal(t, uint64(2048), cfg.Disk.MinFreeMB)
	require.True(t, cfg.Disk.RetainFiles)
	require.InDelta(t, 20, cfg.Disk.PurgePercent, 0)

	require.Empty(t, cfg.DB.Driver) // empty = DB workload off
	require.Equal(t, "host.docker.internal", cfg.DB.Host)
	require.Equal(t, "keeper_orders", cfg.DB.Table)
	require.Equal(t, "@every 20s", cfg.DB.OpCron)
	require.Equal(t, 1000000, cfg.DB.MaxRows)
}

func TestLoadEnvOverrides(t *testing.T) {
	t.Setenv("ORACLE_KEEPER_NET_TOTAL_MB", "42")
	t.Setenv("ORACLE_KEEPER_CPU_BURN_DURATION", "90s")
	t.Setenv("ORACLE_KEEPER_SCHEDULE_JITTER_MINUTES", "0")
	t.Setenv("ORACLE_KEEPER_DISK_ROOTS", "/mnt/root-tmp,/mnt/data-tmp")
	t.Setenv("ORACLE_KEEPER_SCHEDULE_SPEC", "0 4-8 * * *")
	t.Setenv("ORACLE_KEEPER_DB_DRIVER", "mysql")
	t.Setenv("ORACLE_KEEPER_DB_USER", "keeper")
	t.Setenv("ORACLE_KEEPER_DB_PASSWORD", "secret")
	t.Setenv("ORACLE_KEEPER_DB_DB_NAME", "keeperdb")
	t.Setenv("ORACLE_KEEPER_DB_PORT", "3307")

	cfg, err := loadForTest(t)
	require.NoError(t, err)
	require.Equal(t, 42, cfg.Net.TotalMB)
	require.Equal(t, 90*time.Second, cfg.CPU.BurnDuration)
	require.Equal(t, 0, cfg.Schedule.JitterMinutes)
	require.Equal(t, []string{"/mnt/root-tmp", "/mnt/data-tmp"}, cfg.Disk.Roots)
	require.Equal(t, "0 4-8 * * *", cfg.Schedule.Spec)
	require.Equal(t, "mysql", cfg.DB.Driver)
	require.Equal(t, "keeper", cfg.DB.User)
	require.Equal(t, "secret", cfg.DB.Password)
	require.Equal(t, "keeperdb", cfg.DB.DBName)
	require.Equal(t, 3307, cfg.DB.Port)
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
		{name: "purge percent out of range", env: map[string]string{"ORACLE_KEEPER_DISK_PURGE_PERCENT": "120"}},
		{name: "db driver unknown", env: map[string]string{"ORACLE_KEEPER_DB_DRIVER": "oracle"}},
		{name: "db mysql missing user", env: map[string]string{
			"ORACLE_KEEPER_DB_DRIVER":  "mysql",
			"ORACLE_KEEPER_DB_DB_NAME": "keeper",
		}},
		{name: "db sqlite missing path", env: map[string]string{"ORACLE_KEEPER_DB_DRIVER": "sqlite"}},
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
