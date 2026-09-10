package keeper

import (
	"context"
	"crypto/sha256"
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

// burnCPU spins `cores` workers hashing cpuChunk with the given duty cycle
// until d elapses or ctx is cancelled, and returns the total busy-seconds
// accumulated across all workers.
//
// The duty ramps in and out over cpuRampTime instead of switching on and off
// at full intensity — an abrupt constant burst that stops dead looks like a
// load generator; a short ramp resembles an ordinary batch job spinning up
// and winding down.
func burnCPU(ctx context.Context, cores int, duty float64, d time.Duration) float64 {
	if cores <= 0 || d <= 0 {
		return 0
	}
	if duty <= 0 {
		duty = 0.05
	}
	if duty > 1 {
		duty = 1
	}
	ramp := min(30*time.Second, d/4)

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
				elapsed := time.Since(start)
				remaining := time.Until(deadline)
				envelope := 1.0
				if ramp > 0 {
					envelope = min(float64(elapsed)/float64(ramp), float64(remaining)/float64(ramp), 1)
				}
				dutyNow := duty * envelope
				busy := time.Duration(float64(cpuCycle) * dutyNow)
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
