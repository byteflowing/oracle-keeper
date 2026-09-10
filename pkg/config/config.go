// Package config defines the oracle-keeper configuration shape and loads it
// via go-common's configx.
//
// serviceName and envPrefix are the two anchors external tooling (Docker env,
// systemd unit) must agree on with this binary.
package config

import (
	"fmt"
	"time"

	"github.com/servekit/go-common/configx"
	"github.com/servekit/go-common/logging"
)

const (
	serviceName = "oracle-keeper"
	envPrefix   = "ORACLE_KEEPER"
)

// Config holds all configuration for oracle-keeper.
//
// Sub-config fields are pointers per golang-development skill §14: keeps
// style consistent with the outer *Config return, and avoids large-struct
// copies. configx (viper) always allocates nil pointer fields during
// unmarshal, so cfg.X == nil is never true — express "off" via zero values
// of inner fields, not pointer nil-checks.
type Config struct {
	Schedule *ScheduleConfig
	Busy     *BusyConfig
	CPU      *CPUConfig
	Mem      *MemConfig
	Net      *NetConfig
	Disk     *DiskConfig
	Log      *logging.Config
}

// ScheduleConfig controls when keep-alive cycles fire and how long one may run.
//
// Two scheduling modes:
//
//   - Randomized interval (default, Spec empty): after each cycle completes,
//     the next one is scheduled IntervalMin..IntervalMax later. The cadence
//     never aligns to wall-clock hours and is not an exact multiple of any
//     period, which avoids a "runs every hour at :00" fingerprint.
//   - Fixed cron (Spec non-empty): classic cron expression. Predictable —
//     use it only when you explicitly want a deterministic schedule.
type ScheduleConfig struct {
	// Spec is a 5-field cron expression. Empty (default) selects the
	// randomized-interval mode described above.
	Spec string
	// IntervalMin/IntervalMax bound the uniform random gap between cycles
	// (interval mode only). Defaults average to ~1h.
	IntervalMin time.Duration `default:"48m"`
	IntervalMax time.Duration `default:"72m"`
	// Timezone for cron expression evaluation (fixed-cron mode only).
	Timezone string `default:"Asia/Shanghai"`
	// JitterMinutes is the upper bound of the random delay applied after a
	// cycle fires, adding entropy on top of the randomized interval.
	JitterMinutes int `default:"2"`
	// MaxRunDuration caps one full cycle; combined with cronx's skip overlap
	// policy it guarantees cycles never stack.
	MaxRunDuration time.Duration `default:"45m"`
}

// BusyConfig defines when the host is considered too busy for a keep-alive
// cycle — busy cycles are skipped so real workload is never disturbed.
type BusyConfig struct {
	// CPUPercent: skip the cycle when sampled host CPU utilization >= this.
	CPUPercent float64 `default:"40"`
	// LoadFactor: skip when load1 >= LoadFactor * logical cores.
	LoadFactor float64 `default:"0.8"`
}

// CPUConfig controls the CPU burn phase, which counters Oracle's
// "95% of time below 10% CPU" idle criterion.
type CPUConfig struct {
	// BurnDuration is how long the burn phase runs each cycle.
	BurnDuration time.Duration `default:"5m"`
	// Cores is the worker count; 0 means all visible logical cores.
	Cores int `default:"0"`
	// DutyCycle is the busy fraction (0..1) of each spin cycle — keeps a
	// safety valve for the scheduler instead of pegging every core at 100%.
	DutyCycle float64 `default:"0.7"`
}

// MemConfig controls the memory exercise, which counters Oracle's A1 (ARM)
// memory-utilization idle criterion.
type MemConfig struct {
	// AllocPercent of total host RAM to allocate and touch per cycle; 0 disables.
	AllocPercent int `default:"30"`
	// MinFreePercent of total RAM that must stay free — the exercise scales
	// down (or skips) when available memory gets close to this floor.
	MinFreePercent int `default:"25"`
}

// NetConfig controls download volume and per-host politeness.
type NetConfig struct {
	// TotalMB is the per-cycle download budget (±20% random jitter); 0 disables.
	TotalMB int `default:"700"`
	// MaxPerHostMB caps how much a single source may serve per cycle.
	MaxPerHostMB int `default:"250"`
	// RequestTimeout bounds each individual download.
	RequestTimeout time.Duration `default:"5m"`
	// Sources overrides the built-in download source list (comma-separated
	// "name=url" pairs, see internal/keeper/net.go). Empty = built-ins.
	Sources []string
}

// DiskConfig controls temp directories across the mounted disks. Oracle
// boxes typically have two volumes (root and /data) — roots default to one
// directory on each so both spindles see writes and frees.
type DiskConfig struct {
	// Roots are parent directories for per-cycle temp dirs.
	Roots []string `default:"/var/tmp/oracle-keeper,/data/oracle-keeper"`
	// WriteMB is an extra random-data write pass split across roots,
	// guaranteeing disk I/O on every root even if downloads fail; 0 disables.
	WriteMB int `default:"256"`
	// MinFreeMB: roots with less free space are skipped for that cycle.
	MinFreeMB uint64 `default:"2048"`
	// RetainFiles keeps the downloaded/written files on disk instead of
	// deleting them at cycle end — the instance then holds persistent data,
	// which some prefer as extra camouflage. Oracle's published idle
	// criteria do not include storage, so this is optional hardening, not a
	// criterion requirement.
	RetainFiles bool `default:"true"`
	// PurgePercent is the per-root used-percentage high watermark: when a
	// root is fuller than this at cycle start, all retained run dirs are
	// purged. 40 leaves the majority of the volume for real workloads while
	// still amortizing purges; raise it if business data normally sits
	// above this level (the purge only ever deletes our own run dirs).
	PurgePercent float64 `default:"40"`
}

// Load reads config from the standard configx locations (env > file >
// defaults). See the configx package doc for the resolution order.
func Load() (*Config, error) {
	var cfg Config
	if err := configx.Load(&cfg,
		configx.WithServiceName(serviceName),
		configx.WithEnvPrefix(envPrefix),
		// Expand ${VAR} placeholders in config values from the process env.
		configx.WithExpandEnv(),
	); err != nil {
		return nil, err
	}
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("validate config: %w", err)
	}
	return &cfg, nil
}

// validate rejects values that would make a cycle misbehave. Bounds are
// deliberately loose: this is a knob-tuning-friendly daemon, not a public API.
func (c *Config) validate() error {
	if c.Schedule.Spec == "" {
		if c.Schedule.IntervalMin <= 0 {
			return fmt.Errorf("schedule.interval_min must be > 0 when schedule.spec is empty, got %s", c.Schedule.IntervalMin)
		}
		if c.Schedule.IntervalMax < c.Schedule.IntervalMin {
			return fmt.Errorf("schedule.interval_max (%s) must be >= schedule.interval_min (%s)",
				c.Schedule.IntervalMax, c.Schedule.IntervalMin)
		}
	}
	if c.Schedule.JitterMinutes < 0 {
		return fmt.Errorf("schedule.jitter_minutes must be >= 0, got %d", c.Schedule.JitterMinutes)
	}
	if c.Schedule.MaxRunDuration <= 0 {
		return fmt.Errorf("schedule.max_run_duration must be > 0, got %s", c.Schedule.MaxRunDuration)
	}
	if c.Busy.CPUPercent <= 0 || c.Busy.CPUPercent > 100 {
		return fmt.Errorf("busy.cpu_percent must be in (0,100], got %v", c.Busy.CPUPercent)
	}
	if c.Busy.LoadFactor <= 0 {
		return fmt.Errorf("busy.load_factor must be > 0, got %v", c.Busy.LoadFactor)
	}
	if c.CPU.DutyCycle <= 0 || c.CPU.DutyCycle > 1 {
		return fmt.Errorf("cpu.duty_cycle must be in (0,1], got %v", c.CPU.DutyCycle)
	}
	if c.CPU.Cores < 0 {
		return fmt.Errorf("cpu.cores must be >= 0, got %d", c.CPU.Cores)
	}
	if c.Mem.AllocPercent < 0 || c.Mem.AllocPercent > 80 {
		return fmt.Errorf("mem.alloc_percent must be in [0,80], got %d", c.Mem.AllocPercent)
	}
	if c.Mem.MinFreePercent < 0 || c.Mem.MinFreePercent > 90 {
		return fmt.Errorf("mem.min_free_percent must be in [0,90], got %d", c.Mem.MinFreePercent)
	}
	if c.Net.TotalMB < 0 {
		return fmt.Errorf("net.total_mb must be >= 0, got %d", c.Net.TotalMB)
	}
	if c.Net.TotalMB > 0 && c.Net.MaxPerHostMB <= 0 {
		return fmt.Errorf("net.max_per_host_mb must be > 0 when net.total_mb > 0, got %d", c.Net.MaxPerHostMB)
	}
	if c.Net.RequestTimeout <= 0 {
		return fmt.Errorf("net.request_timeout must be > 0, got %s", c.Net.RequestTimeout)
	}
	if len(c.Disk.Roots) == 0 {
		return fmt.Errorf("disk.roots must not be empty")
	}
	if c.Disk.WriteMB < 0 {
		return fmt.Errorf("disk.write_mb must be >= 0, got %d", c.Disk.WriteMB)
	}
	if c.Disk.PurgePercent <= 0 || c.Disk.PurgePercent > 100 {
		return fmt.Errorf("disk.purge_percent must be in (0,100], got %v", c.Disk.PurgePercent)
	}
	return nil
}
