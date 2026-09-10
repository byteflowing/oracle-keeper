package keeper

import (
	"context"
	"fmt"
	"runtime"
	"time"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/load"
	"github.com/shirou/gopsutil/v4/mem"

	"github.com/servekit/oracle-keeper/pkg/config"
)

// loadSampleInterval is how long the CPU sampler watches /proc/stat to derive
// a utilization percentage. Shortened in tests.
const loadSampleInterval = 3 * time.Second

// LoadSample is a point-in-time snapshot of host utilization used by the
// busy decision and the memory-exercise safety check.
type LoadSample struct {
	CPUPercent   float64 // host-wide CPU utilization, 0-100
	Load1        float64 // 1-minute load average
	MemTotal     uint64  // bytes
	MemAvailable uint64  // bytes
}

// sampleLoad reads host-wide CPU/load/memory figures. In a standard Docker
// container (no lxcfs) /proc exposes host-global values, which is exactly
// what the busy check needs — see README for the lxcfs caveat.
func (k *Keeper) sampleLoad(ctx context.Context) (LoadSample, error) {
	pcts, err := cpu.PercentWithContext(ctx, k.loadSampleInterval, false)
	if err != nil {
		return LoadSample{}, fmt.Errorf("sample cpu percent: %w", err)
	}
	lv, err := load.AvgWithContext(ctx)
	if err != nil {
		return LoadSample{}, fmt.Errorf("sample load average: %w", err)
	}
	mv, err := mem.VirtualMemoryWithContext(ctx)
	if err != nil {
		return LoadSample{}, fmt.Errorf("sample memory: %w", err)
	}
	s := LoadSample{
		Load1:        lv.Load1,
		MemTotal:     mv.Total,
		MemAvailable: mv.Available,
	}
	if len(pcts) > 0 {
		s.CPUPercent = pcts[0]
	}
	return s, nil
}

// decideBusy reports whether the host is currently too busy for a keep-alive
// cycle, plus a human-readable reason. A busy host is by definition non-idle,
// so skipping is itself safe with respect to Oracle's reclamation criteria.
func decideBusy(s LoadSample, cfg *config.BusyConfig, cores int) (busy bool, reason string) {
	if s.CPUPercent >= cfg.CPUPercent {
		return true, fmt.Sprintf("cpu %.1f%% >= %.1f%%", s.CPUPercent, cfg.CPUPercent)
	}
	if threshold := cfg.LoadFactor * float64(cores); s.Load1 >= threshold {
		return true, fmt.Sprintf("load1 %.2f >= %.2f (%.1f * %d cores)", s.Load1, threshold, cfg.LoadFactor, cores)
	}
	return false, ""
}

// burnCores resolves the worker count: a configured value, or all visible
// logical cores when 0.
func burnCores(configured int) int {
	if configured > 0 {
		return configured
	}
	return runtime.NumCPU()
}
