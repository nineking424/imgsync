package hostcap_test

import (
	"context"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nineking424/imgsync/internal/hostcap"
	"github.com/stretchr/testify/require"
)

// holdTransport blocks inside Send until released, so the test can inspect which
// advisory-lock slot the in-flight Send is currently holding.
type holdTransport struct {
	enter chan struct{}
	hold  chan struct{}
}

func (ht *holdTransport) Send(_ context.Context, _ string, body io.Reader, _ int64) (int64, string, error) {
	_, _ = io.Copy(io.Discard, body)
	ht.enter <- struct{}{}
	<-ht.hold
	return 0, "deadbeef", nil
}

// TestHostCap_SpreadsAcrossSlots_NotAllSlotZero is the red guard for issue #33.
//
// The current acquireSlot scans slot = 0..Cap-1 starting at 0 on every attempt,
// so a single in-flight Send ALWAYS lands on slot 0, and a stream of sequential
// distinct destinations all pile onto slot 0 (the "slot 0 maximally contended"
// complaint in #33). The suggested fix keys the lock by hash(...) % Cap and/or
// starts the scan at a randomized/hash-derived offset, which spreads distinct
// destinations across multiple slots.
//
// Property asserted: across many sequential Sends for DISTINCT destinations
// (each released before the next, so each sees an empty lock table), the set of
// slots actually used must contain MORE THAN ONE slot. On current code this set
// is exactly {0} and the test fails. After the fix it spans multiple slots.
func TestHostCap_SpreadsAcrossSlots_NotAllSlotZero(t *testing.T) {
	const (
		host    = "ftp.spread.local"
		capN    = 8
		samples = 24
	)
	pool := mustDB(t, 8)

	ht := &holdTransport{enter: make(chan struct{}, 1), hold: make(chan struct{}, 1)}
	hc := hostcap.Wrap(pool, ht, hostcap.Config{Cap: capN, Host: host})

	used := map[int]bool{}
	for i := 0; i < samples; i++ {
		dst := fmt.Sprintf("ftp://%s/path/object-%d.bin", host, i)
		done := make(chan struct{})
		go func() {
			_, _, _ = hc.Send(context.Background(), dst, strings.NewReader("y"), 1)
			close(done)
		}()

		select {
		case <-ht.enter:
		case <-time.After(3 * time.Second):
			t.Fatalf("Send %d never entered transport", i)
		}

		used[heldSlotIndex(t, pool, host, capN)] = true

		ht.hold <- struct{}{}
		<-done

		// Wait for the advisory lock to drop before the next sample so each Send
		// observes a fully empty lock table.
		require.Eventually(t, func() bool {
			var n int
			_ = pool.QueryRow(context.Background(),
				`SELECT COUNT(*) FROM pg_locks WHERE locktype='advisory' AND granted`).Scan(&n)
			return n == 0
		}, 2*time.Second, 10*time.Millisecond)
	}

	require.Greater(t, len(used), 1,
		"issue #33 regression: %d distinct destinations all landed on slots %v — "+
			"acquireSlot must not always start its scan at slot 0; spread via hash(...)%%%d",
		samples, mapKeys(used), capN)
}

// heldSlotIndex returns the single slot index whose advisory lock is currently
// granted (exactly one in-flight Send is expected). It matches pg_locks against
// the hash of each candidate slot key, tolerating either advisory-lock encoding
// the fix might use:
//   - single bigint key: pg_advisory_lock(hashtext('ftp_host_<host>_<slot>'))
//     -> objid = low 32 bits of that hash.
//   - two-int key:       pg_advisory_lock(hashtext('<host>'), <slot>)
//     -> classid = hash(host), objid = slot.
//
// The load-bearing assertion (spread across >1 slot) is independent of which
// encoding lands.
func heldSlotIndex(t *testing.T, pool *pgxpool.Pool, host string, capN int) int {
	t.Helper()
	var slot int
	err := pool.QueryRow(context.Background(), `
		WITH locks AS (
			SELECT classid, objid FROM pg_locks
			WHERE locktype='advisory' AND granted
		),
		slots AS (
			SELECT s AS slot,
			       (hashtext('ftp_host_'||$1||'_'||s)::bigint & 4294967295) AS single_obj,
			       (hashtext($1)::bigint & 4294967295)                      AS host_class,
			       s::bigint                                                AS twoint_obj
			FROM generate_series(0, $2 - 1) s
		)
		SELECT slot FROM locks
		JOIN slots ON locks.objid = slots.single_obj
		           OR (locks.classid = slots.host_class AND locks.objid = slots.twoint_obj)
		LIMIT 1`, host, capN).Scan(&slot)
	require.NoError(t, err, "could not map the in-flight advisory lock back to a slot index")
	return slot
}

func mapKeys(m map[int]bool) []int {
	out := make([]int, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
