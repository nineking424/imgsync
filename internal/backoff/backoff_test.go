package backoff_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/nineking424/imgsync/internal/backoff"
	"github.com/stretchr/testify/require"
)

func TestIdle_FirstWait_NearBaseDelay(t *testing.T) {
	b := backoff.NewIdle(backoff.Config{
		BaseDelay: 50 * time.Millisecond,
		MaxDelay:  1 * time.Second,
	})
	start := time.Now()
	b.WaitOnce(context.Background())
	d := time.Since(start)
	// 50ms ±25% = [37.5ms, 62.5ms], with scheduler slack widen to [30ms, 150ms]
	require.GreaterOrEqual(t, d, 30*time.Millisecond)
	require.LessOrEqual(t, d, 150*time.Millisecond,
		"first wait took %v; should be near base delay even on slow CI", d)
}

func TestIdle_DelayClimbsToCap(t *testing.T) {
	b := backoff.NewIdle(backoff.Config{
		BaseDelay: 10 * time.Millisecond,
		MaxDelay:  40 * time.Millisecond,
	})
	for i := 0; i < 5; i++ {
		b.WaitOnce(context.Background())
	}
	require.Equal(t, 40*time.Millisecond, b.CurrentNominalDelay(),
		"after several waits without wakes, delay must reach MaxDelay cap")
}

func TestIdle_WakeAll_ResetsAndUnblocksSiblings(t *testing.T) {
	b := backoff.NewIdle(backoff.Config{
		BaseDelay: 200 * time.Millisecond,
		MaxDelay:  1 * time.Second,
	})

	var wg sync.WaitGroup
	woke := make([]time.Duration, 4)
	start := time.Now()
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			b.WaitOnce(context.Background())
			woke[idx] = time.Since(start)
		}(i)
	}

	// Let goroutines arm their timers, then wake.
	time.Sleep(20 * time.Millisecond)
	b.WakeAll()

	wg.Wait()

	for i, d := range woke {
		require.Less(t, d, 100*time.Millisecond,
			"goroutine %d woke at %v — WakeAll must unblock sleeping siblings", i, d)
	}
	require.Equal(t, 200*time.Millisecond, b.CurrentNominalDelay(),
		"WakeAll must reset nominal delay back to BaseDelay")
}

func TestIdle_ContextCancel_UnblocksWait(t *testing.T) {
	b := backoff.NewIdle(backoff.Config{
		BaseDelay: 1 * time.Second,
		MaxDelay:  10 * time.Second,
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		b.WaitOnce(ctx)
		close(done)
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("WaitOnce did not return on ctx cancel")
	}
}
