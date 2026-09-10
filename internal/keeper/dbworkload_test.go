package keeper

import (
	"context"
	"math/rand/v2"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/servekit/oracle-keeper/pkg/config"
)

func TestNextOp(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	high, low := int64(90), int64(70)

	t.Run("over high watermark is delete-only", func(t *testing.T) {
		for range 100 {
			require.Equal(t, dbOpDelete, nextOp(rng, high, low, high))
		}
	})

	t.Run("cooldown band has no inserts", func(t *testing.T) {
		for range 200 {
			require.NotEqual(t, dbOpInsert, nextOp(rng, high, low, (high+low)/2))
		}
	})

	t.Run("below low watermark uses the business mix", func(t *testing.T) {
		seen := map[dbOpKind]bool{}
		for range 1000 {
			seen[nextOp(rng, high, low, 0)] = true
		}
		for _, want := range []dbOpKind{dbOpInsert, dbOpSelect, dbOpUpdate, dbOpReport, dbOpDelete} {
			require.True(t, seen[want], "kind %d should appear in the mix", want)
		}
	})
}

// TestDBWorkloadSQLite drives the full workload against a real sqlite file:
// schema creation, the op mix, and the row-count cap.
func TestDBWorkloadSQLite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "business.db")
	cfg := &config.Config{
		Schedule: &config.ScheduleConfig{IntervalMin: time.Minute, IntervalMax: time.Minute, MaxRunDuration: time.Minute},
		Busy:     &config.BusyConfig{CPUPercent: 100, LoadFactor: 1e6},
		CPU:      &config.CPUConfig{},
		Mem:      &config.MemConfig{},
		Net:      &config.NetConfig{RequestTimeout: time.Second},
		Disk:     &config.DiskConfig{Roots: []string{t.TempDir()}},
		DB: &config.DBConfig{
			Driver:     "sqlite",
			SQLitePath: path,
			Table:      "keeper_orders",
			OpCron:     "@every 20s",
			MaxRows:    200,
		},
	}
	w := NewDBWorkload(cfg)
	w.tickJitter = 0 // no jitter in tests
	t.Cleanup(w.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Grow phase: enough ticks to pass the high watermark.
	for range 300 {
		w.Tick(ctx)
	}
	require.True(t, w.migrated.Load(), "workload should have opened and migrated")
	require.Positive(t, w.rows.Load())

	// Shrink phase: keep ticking; deletes must bring the count under the
	// high watermark and never above the cap.
	for range 400 {
		w.Tick(ctx)
	}
	high := int64(cfg.DB.MaxRows) * dbWatermarkHigh / 100
	require.LessOrEqual(t, w.rows.Load(), high, "row count must respect the cap")

	// The real table agrees with the in-memory counter.
	var count int64
	require.NoError(t, w.gorm.WithContext(ctx).Table("keeper_orders").Count(&count).Error)
	require.Equal(t, count, w.rows.Load())

	// Lifecycle sanity: statuses come from the known set.
	var orders []dbOrder
	require.NoError(t, w.gorm.WithContext(ctx).Table("keeper_orders").Limit(20).Find(&orders).Error)
	valid := map[string]bool{}
	for _, s := range dbStatuses {
		valid[s] = true
	}
	for _, o := range orders {
		require.True(t, valid[o.Status], "unexpected status %q", o.Status)
		require.NotEmpty(t, o.SKU)
		require.Positive(t, o.Qty)
	}
}

// TestDBWorkloadDisabled pins the no-op path: no config, no connection.
func TestDBWorkloadDisabled(t *testing.T) {
	w := NewDBWorkload(&config.Config{DB: &config.DBConfig{}})
	w.Tick(context.Background()) // must not panic or connect
	require.Nil(t, w.gorm)

	// Unreachable MySQL: tick logs (warn-gated) and returns without failing.
	w2 := NewDBWorkload(&config.Config{DB: &config.DBConfig{
		Driver: "mysql", Host: "127.0.0.1", Port: 1, User: "u", Password: "p", DBName: "d", MaxRows: 10,
	}})
	w2.tickJitter = 0
	t.Cleanup(w2.Close)
	w2.Tick(context.Background())
	require.False(t, w2.migrated.Load())
	require.True(t, w2.downLogged.Load(), "first failure should arm the warn gate")
}
