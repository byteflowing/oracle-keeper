package keeper

import (
	"context"
	"crypto/sha256"
	"math"
	"sync"
	"sync/atomic"
	"time"
)

// cpuCycle is the spin/sleep period per burn worker. 200ms keeps the duty
// cycle responsive to context cancellation without scheduler churn.
const cpuCycle = 200 * time.Millisecond

// cpuChunk is the buffer each worker hashes; large enough that the SHA-256
// rounds dominate (real ALU work, not loop overhead).
var cpuChunk = make([]byte, 64*1024)

// cpuShape captures the per-cycle burn texture. Every field is randomized
// per cycle by runPhases so consecutive spikes differ in height, slope, and
// top texture — identical rectangles are the most obvious synthetic pattern
// on a utilization graph.
type cpuShape struct {
	duty float64 // base busy fraction for this cycle
	ramp time.Duration
	// Wobble modulates the plateau with a slow sine so the top is a rolling
	// hill rather than a flat mesa (visible at 1-min monitoring granularity).
	wobbleAmp    float64       // 0..~0.4
	wobblePeriod time.Duration // tens of seconds
	wobblePhase  float64       // radians
}

// dutyAt returns the effective busy fraction at a given point in the burn,
// combining the symmetric ramp envelope (rise and fall) and the wobble.
// Shared across workers (computed from the clock, not per-goroutine state)
// so host-level utilization actually oscillates instead of averaging out.
func (s cpuShape) dutyAt(elapsed, remaining time.Duration) float64 {
	envelope := 1.0
	if s.ramp > 0 {
		up := elapsed.Seconds() / s.ramp.Seconds()
		down := remaining.Seconds() / s.ramp.Seconds()
		envelope = math.Min(math.Min(up, down), 1)
	}
	wobble := 1.0
	if s.wobblePeriod > 0 && s.wobbleAmp > 0 {
		wobble = 1 + s.wobbleAmp*math.Sin(2*math.Pi*elapsed.Seconds()/s.wobblePeriod.Seconds()+s.wobblePhase)
	}
	d := s.duty * envelope * wobble
	if d < 0.05 {
		return 0.05
	}
	if d > 1 {
		return 1
	}
	return d
}

// burnCPU spins `cores` workers hashing cpuChunk following the shape's duty
// curve until d elapses or ctx is cancelled, and returns the total
// busy-seconds accumulated across all workers.
func burnCPU(ctx context.Context, cores int, d time.Duration, s cpuShape) float64 {
	if cores <= 0 || d <= 0 {
		return 0
	}
	if s.duty <= 0 {
		s.duty = 0.05
	}
	if s.duty > 1 {
		s.duty = 1
	}

	var busyNanos atomic.Int64
	start := time.Now()
	deadline := start.Add(d)

	var wg sync.WaitGroup
	for range cores {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h := sha256.New()
			for time.Now().Before(deadline) {
				if ctx.Err() != nil {
					return
				}
				busy := time.Duration(float64(cpuCycle) * s.dutyAt(time.Since(start), time.Until(deadline)))
				spinStart := time.Now()
				for time.Since(spinStart) < busy && time.Now().Before(deadline) {
					if ctx.Err() != nil {
						return
					}
					// Fixed-size state: streaming the same chunk repeatedly is
					// pure CPU, no allocation, no memory growth.
					h.Write(cpuChunk)
				}
				busyNanos.Add(int64(time.Since(spinStart)))
				sleep := cpuCycle - busy
				if sleep <= 0 {
					continue
				}
				timer := time.NewTimer(sleep)
				select {
				case <-ctx.Done():
					timer.Stop()
					return
				case <-timer.C:
				}
			}
		}()
	}
	wg.Wait()
	return time.Duration(busyNanos.Load()).Seconds()
}
