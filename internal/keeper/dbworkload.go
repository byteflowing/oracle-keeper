package keeper

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"os"
	"sync/atomic"
	"time"

	"gorm.io/gorm"

	"github.com/yanrongliang/go-common/dbx"

	"github.com/servekit/oracle-keeper/pkg/config"
)

// dbOrder is the workload row: the shape a small order-processing service
// would own. The table is keeper-created (AutoMigrate) and keeper-owned.
type dbOrder struct {
	ID        int64  `gorm:"primaryKey;autoIncrement"`
	SKU       string `gorm:"size:32;index"`
	Qty       int
	Amount    float64 `gorm:"type:decimal(10,2)"`
	Status    string  `gorm:"size:16;index"`
	Note      string  `gorm:"size:128"`
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Operation mix weights for the normal (under-watermark) phase.
const (
	opWeightInsert = 0.35
	opWeightSelect = 0.25
	opWeightUpdate = 0.20
	opWeightReport = 0.10
	// opWeightDelete (the remainder, 0.10) reaps a few oldest rows even in
	// the normal phase — steady churn instead of grow-only-then-purge.
)

// dbWatermarkHigh/low bound the row-count oscillation around MaxRows: above
// 90% inserts stop and deletes run in batches until 70%.
const (
	dbWatermarkHigh = 90
	dbWatermarkLow  = 70
)

// dbStatuses models an order lifecycle; updates transition along it.
var dbStatuses = []string{"created", "paid", "shipped", "done", "refunded"}

// dbNotes are filler remarks a real service might attach.
var dbNotes = []string{
	"", "gift wrap", "priority shipping", "monthly deal", "left at door",
	"customer called", "invoice needed", "fragile", "",
}

// dbTickJitter spreads ticks off the exact cron second.
const dbTickJitter = 8 * time.Second

// dbReconcileEvery re-syncs the in-memory row counter with COUNT(*) every N
// ticks, correcting any drift from external deletes.
const dbReconcileEvery = 50

// DBWorkload runs the simulated small-business CRUD workload. It is driven
// by its own cron entry (cfg.DB.OpCron) and never fails the daemon: the pool
// opens lazily on the first reachable tick and retries on later ones.
type DBWorkload struct {
	cfg  *config.DBConfig
	rng  *rand.Rand
	gorm *gorm.DB

	migrated   atomic.Bool
	downLogged atomic.Bool
	rows       atomic.Int64
	ticks      atomic.Int64

	// tickJitter is shortened by tests to keep ticks fast.
	tickJitter time.Duration
}

// NewDBWorkload builds the workload; nothing connects until the first Tick.
// A nil DB config yields a permanently-disabled workload (Tick is a no-op).
func NewDBWorkload(cfg *config.Config) *DBWorkload {
	w := &DBWorkload{
		rng:        rand.New(rand.NewPCG(uint64(time.Now().UnixNano())^0xdb, uint64(os.Getpid()))),
		tickJitter: dbTickJitter,
	}
	if cfg.DB != nil {
		w.cfg = cfg.DB
	}
	return w
}

// Close releases the connection pool if one was opened.
func (w *DBWorkload) Close() {
	if w.gorm != nil {
		if sqlDB, err := w.gorm.DB(); err == nil && sqlDB != nil {
			if err := sqlDB.Close(); err != nil {
				slog.Warn("db workload: close pool", "error", err)
			}
		}
	}
}

// Tick is the cron entry: jitter, ensure the pool and schema exist, then
// run 1-3 random operations. Errors are logged once per state change and
// retried on the next tick — a down database must not disturb keep-alive.
// With no DB configured it is a no-op.
func (w *DBWorkload) Tick(ctx context.Context) {
	if w.cfg == nil || w.cfg.Driver == "" {
		return
	}
	if w.tickJitter > 0 {
		timer := time.NewTimer(time.Duration(w.rng.Int64N(int64(w.tickJitter) + 1)))
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
	}
	if err := w.ensureReady(ctx); err != nil {
		if !w.downLogged.Swap(true) {
			slog.Warn("db workload unavailable, will keep retrying each tick", "error", err)
		}
		return
	}
	if w.downLogged.Swap(false) {
		slog.Info("db workload recovered")
	}

	n := 1 + w.rng.IntN(3)
	for range n {
		if ctx.Err() != nil {
			return
		}
		if err := w.oneOp(ctx); err != nil {
			slog.Warn("db workload op failed", "error", err)
			return
		}
	}

	if w.ticks.Add(1)%dbReconcileEvery == 0 {
		w.reconcile(ctx)
	}
}

// ensureReady opens the pool (lazily, on first success) and migrates the
// table exactly once, then initializes the row counter from COUNT(*).
func (w *DBWorkload) ensureReady(ctx context.Context) error {
	if w.migrated.Load() {
		return nil
	}
	if w.gorm == nil {
		db, err := dbx.New(buildDBXConfig(w.cfg))
		if err != nil {
			return fmt.Errorf("open database: %w", err)
		}
		w.gorm = db
	}
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	sqlDB, err := w.gorm.DB()
	if err != nil {
		return fmt.Errorf("get sql db: %w", err)
	}
	if err := sqlDB.PingContext(pingCtx); err != nil {
		return fmt.Errorf("ping database: %w", err)
	}
	if err := w.gorm.WithContext(ctx).Table(w.cfg.Table).AutoMigrate(&dbOrder{}); err != nil {
		return fmt.Errorf("auto migrate %s: %w", w.cfg.Table, err)
	}
	var count int64
	if err := w.gorm.WithContext(ctx).Table(w.cfg.Table).Count(&count).Error; err != nil {
		return fmt.Errorf("count %s: %w", w.cfg.Table, err)
	}
	w.rows.Store(count)
	w.migrated.Store(true)
	slog.Info("db workload ready", "driver", w.cfg.Driver, "table", w.cfg.Table, "rows", count)
	return nil
}

// buildDBXConfig maps the flat env config onto dbx's dialect configs.
func buildDBXConfig(cfg *config.DBConfig) *dbx.Config {
	out := &dbx.Config{
		MaxOpenConns:    4,
		MaxIdleConns:    2,
		ConnMaxLifetime: 30 * time.Minute,
	}
	switch cfg.Driver {
	case "mysql":
		port := cfg.Port
		if port == 0 {
			port = 3306
		}
		out.Driver = dbx.DriverMySQL
		out.MySQL = &dbx.MySQLConfig{Host: cfg.Host, Port: port, User: cfg.User, Password: cfg.Password, DBName: cfg.DBName}
	case "postgres":
		port := cfg.Port
		if port == 0 {
			port = 5432
		}
		out.Driver = dbx.DriverPostgres
		out.Postgres = &dbx.PostgresConfig{Host: cfg.Host, Port: port, User: cfg.User, Password: cfg.Password, DBName: cfg.DBName}
	case "sqlite":
		out.Driver = dbx.DriverSQLite
		out.SQLite = &dbx.SQLiteConfig{Path: cfg.SQLitePath}
	}
	return out
}

// dbOpKind enumerates the simulated operations.
type dbOpKind int

const (
	dbOpInsert dbOpKind = iota
	dbOpSelect
	dbOpUpdate
	dbOpReport
	dbOpDelete
)

// nextOp draws the next operation. Above the high watermark the mix becomes
// delete-only; in the cooldown band (low..high) inserts stop and deletes
// dominate, letting the count drain back to the low watermark; below it the
// normal business mix resumes (including occasional small deletes for churn).
func nextOp(rng *rand.Rand, highMark, lowMark, rows int64) dbOpKind {
	if rows >= highMark {
		return dbOpDelete
	}
	r := rng.Float64()
	if rows >= lowMark { // cooldown band: no inserts, mostly deletes
		if r < 0.8 {
			return dbOpDelete
		}
		return dbOpUpdate
	}
	switch {
	case r < opWeightInsert:
		return dbOpInsert
	case r < opWeightInsert+opWeightSelect:
		return dbOpSelect
	case r < opWeightInsert+opWeightSelect+opWeightUpdate:
		return dbOpUpdate
	case r < opWeightInsert+opWeightSelect+opWeightUpdate+opWeightReport:
		return dbOpReport
	default:
		return dbOpDelete
	}
}

// oneOp runs a single random operation.
func (w *DBWorkload) oneOp(ctx context.Context) error {
	high := int64(w.cfg.MaxRows) * dbWatermarkHigh / 100
	low := int64(w.cfg.MaxRows) * dbWatermarkLow / 100
	switch nextOp(w.rng, high, low, w.rows.Load()) {
	case dbOpInsert:
		return w.opInsert(ctx)
	case dbOpSelect:
		return w.opSelect(ctx)
	case dbOpUpdate:
		return w.opUpdate(ctx)
	case dbOpReport:
		return w.opReport(ctx)
	default:
		return w.opDelete(ctx)
	}
}

// opInsert adds 1-5 fresh orders.
func (w *DBWorkload) opInsert(ctx context.Context) error {
	n := 1 + w.rng.IntN(5)
	orders := make([]dbOrder, 0, n)
	for range n {
		orders = append(orders, dbOrder{
			SKU:    fmt.Sprintf("SKU-%06d", w.rng.IntN(1000000)),
			Qty:    1 + w.rng.IntN(20),
			Amount: float64(990+w.rng.IntN(99000)) / 100,
			Status: dbStatuses[w.rng.IntN(len(dbStatuses))],
			Note:   dbNotes[w.rng.IntN(len(dbNotes))],
		})
	}
	if err := w.gorm.WithContext(ctx).Table(w.cfg.Table).Create(&orders).Error; err != nil {
		return fmt.Errorf("insert %d orders: %w", n, err)
	}
	w.rows.Add(int64(n))
	slog.Debug("db workload insert", "rows", n, "total", w.rows.Load())
	return nil
}

// opSelect reads a small slice in one of three shapes a business app uses.
func (w *DBWorkload) opSelect(ctx context.Context) error {
	var orders []dbOrder
	q := w.gorm.WithContext(ctx).Table(w.cfg.Table)
	switch w.rng.IntN(3) {
	case 0:
		q = q.Order("id DESC").Limit(5 + w.rng.IntN(46))
	case 1:
		q = q.Where("status = ?", dbStatuses[w.rng.IntN(len(dbStatuses))]).
			Order("id DESC").Limit(5 + w.rng.IntN(21))
	default:
		q = q.Where("sku = ?", fmt.Sprintf("SKU-%06d", w.rng.IntN(1000000))).
			Order("id DESC").Limit(10)
	}
	if err := q.Find(&orders).Error; err != nil {
		return fmt.Errorf("select orders: %w", err)
	}
	slog.Debug("db workload select", "matched", len(orders))
	return nil
}

// opUpdate transitions a few recent orders to their next lifecycle status —
// select-then-update, the way an actual application writes.
func (w *DBWorkload) opUpdate(ctx context.Context) error {
	var ids []int64
	if err := w.gorm.WithContext(ctx).Table(w.cfg.Table).
		Order("id DESC").Limit(1+w.rng.IntN(5)).Pluck("id", &ids).Error; err != nil {
		return fmt.Errorf("select ids for update: %w", err)
	}
	if len(ids) == 0 {
		return nil
	}
	next := dbStatuses[1+w.rng.IntN(len(dbStatuses)-1)] // never back to "created"
	res := w.gorm.WithContext(ctx).Table(w.cfg.Table).Where("id IN ?", ids).
		Updates(map[string]any{"status": next, "updated_at": time.Now()})
	if res.Error != nil {
		return fmt.Errorf("update %d orders: %w", len(ids), res.Error)
	}
	slog.Debug("db workload update", "rows", res.RowsAffected, "status", next)
	return nil
}

// opReport runs a group-by aggregate, like a dashboard query.
func (w *DBWorkload) opReport(ctx context.Context) error {
	var rows []struct {
		Status string
		Cnt    int64
		Total  float64
	}
	err := w.gorm.WithContext(ctx).Table(w.cfg.Table).
		Select("status, COUNT(*) AS cnt, COALESCE(SUM(amount), 0) AS total").
		Group("status").Scan(&rows).Error
	if err != nil {
		return fmt.Errorf("report by status: %w", err)
	}
	slog.Debug("db workload report", "groups", len(rows))
	return nil
}

// opDelete reaps oldest rows: 1-5 during normal churn, 20-200 while
// shrinking from the high watermark back to the low one.
func (w *DBWorkload) opDelete(ctx context.Context) error {
	n := 1 + w.rng.IntN(5)
	if w.rows.Load() >= int64(w.cfg.MaxRows)*dbWatermarkHigh/100 {
		n = 20 + w.rng.IntN(181)
	}
	var ids []int64
	if err := w.gorm.WithContext(ctx).Table(w.cfg.Table).
		Order("id ASC").Limit(n).Pluck("id", &ids).Error; err != nil {
		return fmt.Errorf("select ids for delete: %w", err)
	}
	if len(ids) == 0 {
		return nil
	}
	res := w.gorm.WithContext(ctx).Table(w.cfg.Table).Where("id IN ?", ids).Delete(&dbOrder{})
	if res.Error != nil {
		return fmt.Errorf("delete %d orders: %w", len(ids), res.Error)
	}
	w.rows.Add(-res.RowsAffected)
	slog.Debug("db workload delete", "rows", res.RowsAffected, "total", w.rows.Load())
	return nil
}

// reconcile re-syncs the in-memory counter with the real COUNT(*).
func (w *DBWorkload) reconcile(ctx context.Context) {
	var count int64
	if err := w.gorm.WithContext(ctx).Table(w.cfg.Table).Count(&count).Error; err != nil {
		slog.Warn("db workload reconcile count", "error", err)
		return
	}
	before := w.rows.Swap(count)
	drift := before - count
	if drift < 0 {
		drift = -drift
	}
	if drift > count/20 { // >5% drift is worth a line
		slog.Info("db workload counter reconciled", "before", before, "actual", count)
	}
}
