// Package backoff implements the per-pod shared idle backoff used by worker
// goroutines when the queue is empty. Schedule is 50ms→200ms→500ms→1s with
// ±25% jitter applied per goroutine. WakeAll resets the schedule and unblocks
// every sleeping goroutine immediately so that latency to first lease after a
// new job arrives is bounded by ctx-switch cost, not the current delay step.
//
// This is the F2 design fix: a fixed time.After(1s) per worker creates both a
// thundering-herd wakeup pattern (all N workers query the DB at once) and a
// tail-latency floor (a job enqueued just after the wakeup waits up to 1s for
// the next tick). Shared backoff with WakeAll fixes both.
package backoff

import (
	"context"
	"math/rand"
	"sync"
	"time"
)

// Config controls the schedule.
type Config struct {
	BaseDelay time.Duration // first wait; default 50ms
	MaxDelay  time.Duration // cap; default 1s
}

// Idle is shared backoff state for one pod's worker goroutines. Goroutine-safe.
type Idle struct {
	cfg Config

	mu      sync.Mutex
	nominal time.Duration   // current scheduled delay (no jitter)
	wakers  []chan struct{} // one per parked goroutine
	rng     *rand.Rand
}

// NewIdle constructs an Idle backoff.
func NewIdle(cfg Config) *Idle {
	if cfg.BaseDelay <= 0 {
		cfg.BaseDelay = 50 * time.Millisecond
	}
	if cfg.MaxDelay <= 0 {
		cfg.MaxDelay = 1 * time.Second
	}
	return &Idle{
		cfg:     cfg,
		nominal: cfg.BaseDelay,
		rng:     rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

// WaitOnce blocks for the current jittered delay, then advances the nominal
// schedule one step toward MaxDelay. Returns early if ctx is cancelled or
// WakeAll is called.
func (i *Idle) WaitOnce(ctx context.Context) {
	i.mu.Lock()
	delay := i.jitter(i.nominal)
	i.advance()
	wake := make(chan struct{}, 1)
	i.wakers = append(i.wakers, wake)
	i.mu.Unlock()

	t := time.NewTimer(delay)
	defer t.Stop()
	select {
	case <-t.C:
	case <-wake:
	case <-ctx.Done():
	}

	// Best-effort waker eviction so a long-lived Idle doesn't accumulate
	// closed channels in the slice.
	i.mu.Lock()
	for k, w := range i.wakers {
		if w == wake {
			i.wakers = append(i.wakers[:k], i.wakers[k+1:]...)
			break
		}
	}
	i.mu.Unlock()
}

// WakeAll resets the nominal schedule to BaseDelay and unblocks every sleeping
// goroutine. Call this when a goroutine successfully leases a job — the queue
// is non-empty so the schedule should restart from the bottom.
func (i *Idle) WakeAll() {
	i.mu.Lock()
	i.nominal = i.cfg.BaseDelay
	wakers := i.wakers
	i.wakers = nil
	i.mu.Unlock()
	for _, w := range wakers {
		select {
		case w <- struct{}{}:
		default:
		}
	}
}

// CurrentNominalDelay returns the current pre-jitter delay. Test helper.
func (i *Idle) CurrentNominalDelay() time.Duration {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.nominal
}

// NumParked returns the count of goroutines currently parked in WaitOnce. Test helper.
func (i *Idle) NumParked() int {
	i.mu.Lock()
	defer i.mu.Unlock()
	return len(i.wakers)
}

// advance must be called with mu held.
func (i *Idle) advance() {
	switch i.nominal {
	case 50 * time.Millisecond:
		i.nominal = 200 * time.Millisecond
	case 200 * time.Millisecond:
		i.nominal = 500 * time.Millisecond
	case 500 * time.Millisecond:
		i.nominal = 1 * time.Second
	default:
		// Generic doubling for non-canonical configs (used in tests).
		next := i.nominal * 2
		if next > i.cfg.MaxDelay {
			next = i.cfg.MaxDelay
		}
		i.nominal = next
	}
	if i.nominal > i.cfg.MaxDelay {
		i.nominal = i.cfg.MaxDelay
	}
}

// jitter applies ±25% uniform jitter. mu must be held (uses i.rng).
func (i *Idle) jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	span := float64(d) * 0.5 // total ±25% range = 50% span
	offset := time.Duration((i.rng.Float64() - 0.5) * span)
	return d + offset
}
