package worker

import (
	"sync/atomic"
	"testing"
)

func TestRunner_StartStopCallbacksAreInvokedAroundLoop(t *testing.T) {
	var startedCount, stoppedCount int32
	r := &Runner{
		PodName: "test-pod",
		OnWorkerStart: func(pod string) {
			if pod != "test-pod" {
				t.Errorf("start pod = %q, want test-pod", pod)
			}
			atomic.AddInt32(&startedCount, 1)
		},
		OnWorkerStop: func(pod string) {
			atomic.AddInt32(&stoppedCount, 1)
		},
	}
	// Drive emitStart/emitStop directly without spinning a goroutine.
	r.emitStart()
	r.emitStop()

	if got := atomic.LoadInt32(&startedCount); got != 1 {
		t.Fatalf("started = %d, want 1", got)
	}
	if got := atomic.LoadInt32(&stoppedCount); got != 1 {
		t.Fatalf("stopped = %d, want 1", got)
	}
}
