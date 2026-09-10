package keeper

import (
	"context"
	"log/slog"
	"time"

	"github.com/servekit/oracle-keeper/pkg/config"
)

// memPageSize matches the common 4 KiB page: writing one byte per page forces
// physical residency, which is what Oracle's hypervisor-side measurement sees.
const memPageSize = 4096

// exerciseMemory allocates and touches a share of host RAM, holds it for the
// given duration, then releases it (caller lets the slice go out of scope).
// It counters the A1 memory-utilization idle criterion.
//
// total/available come from a fresh LoadSample. The allocation scales down so
// that MinFreePercent of total RAM stays available; it skips entirely when
// even that floor is not reachable. Returns the number of bytes touched.
func exerciseMemory(ctx context.Context, total, available uint64, cfg *config.MemConfig, hold time.Duration) uint64 {
	if cfg.AllocPercent <= 0 || total == 0 || hold <= 0 {
		return 0
	}
	want := total * uint64(cfg.AllocPercent) / 100
	floor := total * uint64(cfg.MinFreePercent) / 100
	if available < floor {
		slog.Warn("memory exercise skipped: available below safety floor",
			"available_gb", gb(available), "floor_gb", gb(floor))
		return 0
	}
	if want > available-floor {
		want = available - floor
		slog.Info("memory exercise scaled down to available headroom",
			"target_gb", gb(total*uint64(cfg.AllocPercent)/100), "actual_gb", gb(want))
	}
	if want < 1<<20 { // below 1 MiB the exercise is noise
		return 0
	}

	buf := make([]byte, want)
	for i := 0; i < len(buf); i += memPageSize {
		buf[i] = 1
		if i%(memPageSize*16384) == 0 && ctx.Err() != nil { // check every 64 MiB
			return 0
		}
	}
	slog.Info("memory resident", "gb", gb(uint64(len(buf))))

	timer := time.NewTimer(hold)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
	return uint64(len(buf))
}

// gb formats bytes as whole GiB for logs.
func gb(b uint64) float64 {
	return float64(b) / (1 << 30)
}
