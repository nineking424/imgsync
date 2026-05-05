package worker

import (
	"testing"
	"time"
)

func TestJob_Duration_NilLockedAt(t *testing.T) {
	j := &Job{}
	if got := j.Duration(); got != 0 {
		t.Fatalf("Duration with nil LockedAt = %v, want 0", got)
	}
}

func TestJob_Duration_TimeSinceLockedAt(t *testing.T) {
	now := time.Now().Add(-3 * time.Second)
	j := &Job{LockedAt: &now}
	got := j.Duration()
	if got < 2900*time.Millisecond || got > 3500*time.Millisecond {
		t.Fatalf("Duration = %v, want ~3s", got)
	}
}
