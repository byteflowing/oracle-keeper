// Package keeper implements the keep-alive engine for Oracle Cloud Always
// Free instances. One Run executes a single activity cycle against Oracle's
// three idle criteria (CPU, network, memory-on-A1); see the design doc at
// specs/2026-09-10-oracle-keeper-design.md.
package keeper

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/shirou/gopsutil/v4/disk"

	"github.com/servekit/oracle-keeper/pkg/config"
)

// budgetJitter spreads the per-cycle download budget ±20% so repeated cycles
// never transfer an identical volume.
const budgetJitter = 0.2

// staleDirAge is how old an orphaned run dir must be before the startup
// sweep removes it — two cycle periods by default assumptions, plus slack.
const staleDirAge = 26 * time.Hour

// Keeper executes keep-alive cycles. It is safe for sequential use from a
// single cron entry (cronx runs with the skip overlap policy).
type Keeper struct {
	cfg *config.Config
	rng *rand.Rand
	// loadSampleInterval is shortened by tests to keep the busy check fast.
	loadSampleInterval time.Duration
	// diskUsage is swappable so purge tests can fake high-water states.
	diskUsage func(ctx context.Context, path string) (*disk.UsageStat, error)

	sources []source
}

// New builds a Keeper. It returns an error only when the configured download
// source override cannot be parsed.
func New(cfg *config.Config) (*Keeper, error) {
	sources, err := resolveSources(cfg.Net)
	if err != nil {
		return nil, fmt.Errorf("resolve download sources: %w", err)
	}
	return &Keeper{
		cfg:                cfg,
		rng:                rand.New(rand.NewPCG(uint64(time.Now().UnixNano()), uint64(os.Getpid()))),
		loadSampleInterval: loadSampleInterval,
		diskUsage:          disk.UsageWithContext,
		sources:            sources,
	}, nil
}

// NextInterval returns a uniform random duration in [lo, hi]. It drives the
// randomized cycle cadence and per-phase duration jitter, so no two cycles —
// or their phases — repeat the same timing fingerprint.
func (k *Keeper) NextInterval(lo, hi time.Duration) time.Duration {
	if hi <= lo {
		return lo
	}
	return lo + time.Duration(k.rng.Int64N(int64(hi-lo)+1))
}

// Run executes one keep-alive cycle: jitter → busy check → temp dirs across
// both disks → CPU/memory/network/disk phases concurrently → cleanup →
// summary. It returns an error only for setup failures (sampling, planning);
// per-phase failures are logged and the cycle still completes.
func (k *Keeper) Run(ctx context.Context) error {
	start := time.Now()

	if !k.jitterSleep(ctx) {
		return ctx.Err()
	}

	sample, err := k.sampleLoad(ctx)
	if err != nil {
		return fmt.Errorf("sample host load: %w", err)
	}
	cores := burnCores(k.cfg.CPU.Cores)
	if busy, reason := decideBusy(sample, k.cfg.Busy, cores); busy {
		slog.Info("host busy, keep-alive cycle skipped", "reason", reason,
			"cpu_percent", sample.CPUPercent, "load1", sample.Load1)
		return nil
	}
	slog.Info("host idle, starting keep-alive cycle",
		"cpu_percent", sample.CPUPercent, "load1", sample.Load1, "cores", cores)

	roots := k.prepareRoots(ctx)
	if k.cfg.Disk.RetainFiles {
		// Retention mode: enforce the high-water mark before this cycle
		// writes anything new. sweepStale is skipped — old dirs are
		// intentional now and purgeRoots owns their lifecycle.
		k.purgeRoots(ctx, k.cfg.Disk.Roots, nil)
		roots = k.prepareRoots(ctx) // a purged root may have become usable
	}
	// Baseline gate: a root whose usage EXCLUDING our files already sits at
	// or above the threshold gets no new files this cycle — the volume is
	// sufficiently occupied by real data, so the disk camouflage adds nothing.
	// The network phase keeps running (streamed to /dev/null); only the
	// write pass and file-backed downloads stand down.
	var fileRoots []string
	for _, root := range roots {
		if base := k.baselinePct(ctx, root); base >= k.cfg.Disk.PurgePercent {
			slog.Info("root baseline usage already at/above threshold, file generation skipped on it",
				"root", root, "baseline_percent", base, "threshold_percent", k.cfg.Disk.PurgePercent)
			continue
		}
		fileRoots = append(fileRoots, root)
	}
	var runDirs []string
	for _, root := range fileRoots {
		dir, err := makeRunDir(root)
		if err != nil {
			slog.Warn("temp dir creation failed", "root", root, "error", err)
			continue
		}
		runDirs = append(runDirs, dir)
	}
	if !k.cfg.Disk.RetainFiles {
		if swept := sweepStale(roots, staleDirAge); swept > 0 {
			slog.Info("startup sweep removed stale run dirs", "count", swept)
		}
		defer cleanupRunDirs(runDirs)
	} else {
		defer slog.Info("run files retained until disk high-water mark",
			"dirs", runDirs, "purge_percent", k.cfg.Disk.PurgePercent)
	}
	if len(runDirs) == 0 && (k.cfg.Net.TotalMB > 0 || k.cfg.Disk.WriteMB > 0) {
		slog.Info("no file-writing root this cycle; downloads stream to /dev/null, disk write pass skipped")
	}

	res := k.runPhases(ctx, sample, runDirs, cores)

	var parts []string
	parts = append(parts,
		fmt.Sprintf("cpu_busy_seconds=%.0f", res.cpuSeconds),
		fmt.Sprintf("mem_touched_gb=%.1f", gb(res.memBytes)),
		fmt.Sprintf("net_mb=%.0f", mb(res.net.totalB)),
		fmt.Sprintf("net_hosts=%s", hostSummary(res.net)),
		fmt.Sprintf("disk_write_mb=%.0f", mb(res.diskBytes)),
	)
	if res.net.discardedB > 0 {
		parts = append(parts, fmt.Sprintf("net_discarded_mb=%.0f", mb(res.net.discardedB)))
	}
	if res.net.failures > 0 {
		parts = append(parts, fmt.Sprintf("net_failures=%d", res.net.failures))
	}
	if res.net.skipped > 0 {
		parts = append(parts, fmt.Sprintf("net_skipped=%d", res.net.skipped))
	}
	slog.Info("keep-alive cycle done", "duration", time.Since(start).Round(time.Second), "phases", strings.Join(parts, " "))
	return nil
}

// phaseResults collects what each phase actually achieved, for the summary.
type phaseResults struct {
	cpuSeconds float64
	memBytes   uint64
	net        netStats
	diskBytes  int64
}

// runPhases executes the CPU burn, memory hold, downloads, and disk write
// pass concurrently under one context, then waits for all of them. The
// network phase doubles as disk I/O: download files rotate across runDirs.
// Phase durations/volumes are jittered per cycle (burn ±30%, disk write
// ±20%, network budget ±20%) so repeated cycles never produce identical
// activity signatures.
func (k *Keeper) runPhases(ctx context.Context, sample LoadSample, runDirs []string, cores int) phaseResults {
	var res phaseResults
	res.net.perHostB = make(map[string]int64)

	burn := k.NextInterval(k.cfg.CPU.BurnDuration*7/10, k.cfg.CPU.BurnDuration*14/10)
	writeTotal := int64(k.cfg.Disk.WriteMB) * (1 << 20)
	if writeTotal > 0 {
		writeTotal = randInt64(k.rng, writeTotal*8/10, writeTotal*12/10)
	}
	budget := jitterBudget(k.rng, int64(k.cfg.Net.TotalMB)*(1<<20))
	slog.Info("cycle plan",
		"cpu_burn", burn.Round(time.Second),
		"mem_hold", burn.Round(time.Second),
		"net_budget_mb", mb(budget),
		"disk_write_mb", mb(writeTotal))

	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		res.cpuSeconds = burnCPU(ctx, cores, k.cfg.CPU.DutyCycle, burn)
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		res.memBytes = exerciseMemory(ctx, sample.MemTotal, sample.MemAvailable, k.cfg.Mem, burn)
	}()

	if k.cfg.Net.TotalMB > 0 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tasks := planDownloads(k.rng, k.sources, budget, int64(k.cfg.Net.MaxPerHostMB)*(1<<20))
			slog.Info("network phase planned", "tasks", len(tasks), "hosts", len(k.sources),
				"file_backed", len(runDirs) > 0)
			// Empty runDirs = discard mode: fetch and drop without touching disk.
			res.net = k.runDownloads(ctx, tasks, runDirs)
		}()
	}

	if len(runDirs) > 0 && writeTotal > 0 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res.diskBytes = writePass(ctx, runDirs, writeTotal)
		}()
	}

	wg.Wait()
	return res
}

// jitterSleep waits a random 0..JitterMinutes so cycles don't always fire on
// the exact cron minute. It reports false when the context was cancelled
// while waiting.
func (k *Keeper) jitterSleep(ctx context.Context) bool {
	if k.cfg.Schedule.JitterMinutes <= 0 {
		return true
	}
	d := time.Duration(k.rng.IntN(k.cfg.Schedule.JitterMinutes+1)) * time.Minute
	slog.Info("jitter delay before cycle", "minutes", d.Minutes())
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// jitterBudget randomizes the download budget ±20% around base.
func jitterBudget(rng *rand.Rand, base int64) int64 {
	if base <= 0 {
		return 0
	}
	factor := 1 - budgetJitter + rng.Float64()*2*budgetJitter
	return int64(float64(base) * factor)
}

// hostSummary renders per-host download volumes for the cycle summary.
func hostSummary(stats netStats) string {
	if len(stats.perHostB) == 0 {
		return "-"
	}
	parts := make([]string, 0, len(stats.perHostB))
	for host, b := range stats.perHostB {
		parts = append(parts, fmt.Sprintf("%s=%.0fMB", host, mb(b)))
	}
	return strings.Join(parts, ",")
}
