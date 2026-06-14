package worker

import (
	"testing"
	"time"
)

// These tests pin the issue-#37 requirement that the retry backoff carries ±25%
// jitter (currently an exact 1<<n, which synchronizes retry waves across the
// fleet). They are RED until writeRetryOrDead computes its delay through a
// retryBackoff(attempts) helper that applies jitter.
//
// retryBackoff(attempts) must return a delay centered on the nominal
// (1<<attempts) seconds, jittered by ±25%.

func nominalFor(attempts int) time.Duration {
	return time.Duration(1<<attempts) * time.Second
}

func TestRetryBackoff_WithinJitterBand(t *testing.T) {
	for _, attempts := range []int{1, 2, 3, 5} {
		nominal := nominalFor(attempts)
		lo := time.Duration(float64(nominal) * 0.75)
		hi := time.Duration(float64(nominal) * 1.25)
		for i := 0; i < 200; i++ {
			d := retryBackoff(attempts)
			if d < lo || d > hi {
				t.Fatalf("retryBackoff(%d)=%v out of ±25%% band [%v,%v]", attempts, d, lo, hi)
			}
		}
	}
}

func TestRetryBackoff_ProducesVariance(t *testing.T) {
	const attempts = 4
	seen := map[time.Duration]struct{}{}
	for i := 0; i < 200; i++ {
		seen[retryBackoff(attempts)] = struct{}{}
	}
	// An exact 1<<n (no jitter) would yield exactly one distinct value.
	if len(seen) < 5 {
		t.Fatalf("retryBackoff(%d) produced %d distinct values across 200 calls; "+
			"expected jitter to vary the delay", attempts, len(seen))
	}
}

func TestRetryBackoff_CenteredOnNominal(t *testing.T) {
	const attempts = 4
	nominal := nominalFor(attempts)
	var sum time.Duration
	const n = 2000
	for i := 0; i < n; i++ {
		sum += retryBackoff(attempts)
	}
	mean := sum / n
	// Mean of uniform ±25% jitter should sit near the nominal; allow ±5%.
	lo := time.Duration(float64(nominal) * 0.95)
	hi := time.Duration(float64(nominal) * 1.05)
	if mean < lo || mean > hi {
		t.Fatalf("retryBackoff(%d) mean=%v not near nominal %v ([%v,%v])", attempts, mean, nominal, lo, hi)
	}
}
