// Package app wires the keep-alive engine to go-common's cronx scheduler and
// exposes the Start/Stop lifecycle signalx expects.
//
// Scheduling has two modes (see config.ScheduleConfig): a fixed cron spec,
// or the default randomized-interval mode where each cycle reschedules the
// next one a random IntervalMin..IntervalMax later, so the cadence never
// aligns to wall-clock hours and carries no fixed period.
package app

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/yanrongliang/go-common/cronx"

	"github.com/servekit/oracle-keeper/internal/keeper"
	"github.com/servekit/oracle-keeper/pkg/config"
)

// App owns the cron scheduler and the run-lifetime context shared by all
// cycles. It implements signalx.Service (Start + Stop).
type App struct {
	cron     *cron.Cron
	kpr      *keeper.Keeper
	dbw      *keeper.DBWorkload
	schedule *config.ScheduleConfig
	maxRun   time.Duration

	// intervalMod selects the rescheduling behavior (randomized interval vs
	// fixed cron); see config.ScheduleConfig.
	intervalMod bool

	// entry is the current interval-mode cron entry; replaced after every
	// cycle. Guarded by cron's job serialization (skip overlap policy means
	// at most one runCycle/reschedule at a time; Stop drains before return).
	entry    cron.EntryID
	hasEntry bool

	runCtx    context.Context
	runCancel context.CancelFunc
}

// New validates the schedule, registers the keep-alive job (plus the DB
// workload job when configured), and prepares the engine. The scheduler does
// not run until Start.
func New(cfg *config.Config) (*App, error) {
	scheduler, err := cronx.New(&cronx.Config{
		Timezone:      cfg.Schedule.Timezone,
		OverlapPolicy: "skip",
	})
	if err != nil {
		return nil, fmt.Errorf("init cronx: %w", err)
	}
	kpr, err := keeper.New(cfg)
	if err != nil {
		return nil, err
	}
	a := &App{
		cron:     scheduler,
		kpr:      kpr,
		dbw:      keeper.NewDBWorkload(cfg),
		schedule: cfg.Schedule,
		maxRun:   cfg.Schedule.MaxRunDuration,
	}
	if cfg.DB != nil && cfg.DB.Driver != "" {
		if _, err := scheduler.AddFunc(cfg.DB.OpCron, a.runDBTick); err != nil {
			return nil, fmt.Errorf("register db workload job %q: %w", cfg.DB.OpCron, err)
		}
		slog.Info("db workload scheduled", "spec", cfg.DB.OpCron,
			"driver", cfg.DB.Driver, "table", cfg.DB.Table, "max_rows", cfg.DB.MaxRows)
	}
	if cfg.Schedule.Spec != "" {
		if _, err := scheduler.AddFunc(cfg.Schedule.Spec, a.runCycle); err != nil {
			return nil, fmt.Errorf("register keep-alive job %q: %w", cfg.Schedule.Spec, err)
		}
		return a, nil
	}
	a.intervalMod = true
	if err := a.scheduleNext(); err != nil {
		return nil, err
	}
	return a, nil
}

// Start launches the scheduler under a fresh run context.
func (a *App) Start() error {
	a.runCtx, a.runCancel = context.WithCancel(context.Background())
	a.cron.Start()
	if !a.intervalMod {
		for _, entry := range a.cron.Entries() {
			slog.Info("keep-alive scheduled", "spec", a.schedule.Spec, "next", entry.Next)
		}
	}
	return nil
}

// Stop cancels the run context (aborting any in-flight cycle), waits for
// the scheduler to drain, then closes the DB workload pool.
func (a *App) Stop() error {
	if a.runCancel != nil {
		a.runCancel()
	}
	<-a.cron.Stop().Done()
	a.dbw.Close()
	return nil
}

// runDBTick is the DB workload cron entry: one bounded set of small CRUD
// operations. The workload never fails the daemon — see DBWorkload.Tick.
func (a *App) runDBTick() {
	ctx, cancel := context.WithTimeout(a.runCtx, 30*time.Second)
	defer cancel()
	a.dbw.Tick(ctx)
}

// scheduleNext (re)arms the interval-mode entry with a fresh random gap,
// expressed to cronx as an "@every <duration>" spec so the scheduler stays
// go-common's cronx while the cadence stays random.
func (a *App) scheduleNext() error {
	if a.hasEntry {
		a.cron.Remove(a.entry)
		a.hasEntry = false
	}
	gap := a.kpr.NextCycleGap(a.schedule.IntervalMin, a.schedule.IntervalMax)
	id, err := a.cron.AddFunc("@every "+gap.String(), a.runCycle)
	if err != nil {
		return fmt.Errorf("schedule next interval %s: %w", gap, err)
	}
	a.entry, a.hasEntry = id, true
	slog.Info("next keep-alive cycle scheduled", "in", gap.Round(time.Second))
	return nil
}

// runCycle is the cron entry: one bounded keep-alive run. Errors are logged
// here — the cron wrapper already recovers panics. In interval mode the next
// cycle is armed after this one finishes, so the gap is measured cycle-end
// to cycle-start and cycles can never overlap.
func (a *App) runCycle() {
	if a.intervalMod {
		defer func() {
			if err := a.scheduleNext(); err != nil {
				slog.Error("rescheduling next cycle failed", "error", err)
			}
		}()
	}
	ctx, cancel := context.WithTimeout(a.runCtx, a.maxRun)
	defer cancel()
	if err := a.kpr.Run(ctx); err != nil {
		slog.Error("keep-alive cycle failed", "error", err)
	}
}
