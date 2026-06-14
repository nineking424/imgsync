package hostcap

import (
	"testing"
	"time"
)

// TestNextBackoff_GrowsExponentiallyToCap is the property guard for the #33
// backoff change: the flat AcquireBackoff is replaced by an exponential schedule
// that doubles each step and saturates at maxBackoff.
func TestNextBackoff_GrowsExponentiallyToCap(t *testing.T) {
	base := 100 * time.Millisecond
	want := []time.Duration{
		200 * time.Millisecond,
		400 * time.Millisecond,
		800 * time.Millisecond,
		1600 * time.Millisecond,
		maxBackoff, // 1600ms*2 = 3200ms clamped to 2s
		maxBackoff, // saturated
	}
	cur := base
	for i, w := range want {
		cur = nextBackoff(cur, base)
		if cur != w {
			t.Fatalf("step %d: got %v, want %v", i, cur, w)
		}
	}
}

// TestNextBackoff_GuardsZeroBase ensures a zero/negative configured backoff does
// not degenerate the schedule into all-zero waits.
func TestNextBackoff_GuardsZeroBase(t *testing.T) {
	got := nextBackoff(0, 0)
	if got <= 0 {
		t.Fatalf("nextBackoff with zero base must produce a positive wait, got %v", got)
	}
}

// TestJittered_IsHashSeededAndBounded asserts jitter is derived from the seed
// (deterministic, NOT wall-clock) and stays within ±25% of the base duration.
func TestJittered_IsHashSeededAndBounded(t *testing.T) {
	d := time.Second
	// Deterministic: same seed -> same result across calls.
	if a, b := jittered(d, 42), jittered(d, 42); a != b {
		t.Fatal("jittered must be deterministic for a fixed seed (no wall-clock input)")
	}
	// Bounded within [-25%, +25%].
	lo := d - d/4
	hi := d + d/4
	for seed := uint32(0); seed < 2000; seed++ {
		got := jittered(d, seed)
		if got < lo || got > hi {
			t.Fatalf("seed %d: jittered=%v out of ±25%% bound [%v,%v]", seed, got, lo, hi)
		}
	}
	// Different seeds must produce spread (at least two distinct values).
	a := jittered(d, 0)
	b := jittered(d, 999)
	if a == b {
		t.Fatal("distinct seeds should yield distinct jitter; got identical values")
	}
}

// TestSlotHash_SpreadsDistinctDsts is the unit-level mirror of the integration
// spread test: distinct dsts must not all hash to slot 0.
func TestSlotHash_SpreadsDistinctDsts(t *testing.T) {
	const (
		host = "ftp.spread.local"
		cap  = 8
	)
	used := map[uint32]bool{}
	for i := 0; i < 24; i++ {
		dst := "ftp://" + host + "/path/object-" + time.Duration(i).String() + ".bin"
		s := slotHash(host, dst, cap)
		if s >= cap {
			t.Fatalf("slotHash returned %d, out of range [0,%d)", s, cap)
		}
		used[s] = true
	}
	if len(used) <= 1 {
		t.Fatalf("issue #33: 24 distinct dsts mapped to %d slot(s) %v — must spread across more than one", len(used), used)
	}
}
