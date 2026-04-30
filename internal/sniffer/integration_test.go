//go:build integration

package sniffer_test

import (
	"context"
	"testing"
	"time"

	"github.com/nineking424/imgsync/internal/sniffer"
	"github.com/stretchr/testify/require"
)

// S0: polling overlap correctness
//
//	Run #1 sniffs window. Then we rewind the watermark by 25 minutes (via direct
//	UPDATE on sniffer_state) so run #2's window overlaps the row run #1 already
//	enqueued. The same source row must NOT produce a duplicate transfer_jobs row,
//	because the enqueue path uses ON CONFLICT (trace_id, dst) DO NOTHING.
func TestS0_PollingOverlapNoDuplicate(t *testing.T) {
	ctx := context.Background()
	srcPool := setupSourceDB(t)
	imgPool := setupImgsyncDBWithTransferJobs(t)

	t0 := time.Now().UTC().Add(-30 * time.Minute)
	_, err := srcPool.Exec(ctx, `INSERT INTO images VALUES (1, $1, 'p.jpg')`, t0)
	require.NoError(t, err)

	makeS := func(bias time.Duration) *sniffer.Sniffer {
		return sniffer.New(sniffer.Config{
			SourceID:    "src",
			Query:       sniffer.Query{Table: "images", PKColumn: "id", TSColumn: "updated_at", ExtraColumns: []string{"file_path"}, BatchSize: 100, BiasDuration: bias},
			Dst:         sniffer.DstTemplate{Pattern: "/in/{{.file_path}}", Shadow: true},
			SrcPattern:  "src://images/{{.id}}",
			SrcProtocol: "fs",
			DstProtocol: "fs",
			ImgsyncPool: imgPool, SourcePool: srcPool,
		})
	}

	_, err = makeS(0).RunOnce(ctx)
	require.NoError(t, err)
	// Force overlap by rewinding the persisted watermark 25 minutes (so run #2's
	// window includes the row run #1 already saw). RowsAffected guards against
	// a regression where run #1 silently failed to write sniffer_state — without
	// this check, a no-op UPDATE would let the assertion pass for the wrong reason.
	tag, err := imgPool.Exec(ctx, `UPDATE sniffer_state SET last_run_ts = last_run_ts - INTERVAL '25 minutes', last_run_pk = NULL`)
	require.NoError(t, err)
	require.Equal(t, int64(1), tag.RowsAffected(), "run #1 must have written sniffer_state")

	_, err = makeS(0).RunOnce(ctx)
	require.NoError(t, err)

	var n int
	require.NoError(t, imgPool.QueryRow(ctx, `SELECT COUNT(*) FROM transfer_jobs`).Scan(&n))
	require.Equal(t, 1, n, "overlapping sniff must not duplicate")
}

// S1: crash recovery — simulate "kill -9 mid-batch then restart" by running two
// fresh Sniffer instances back-to-back. The persistent sniffer_state watermark
// is the only carry-over; in-memory state is dropped between runs. With 100
// rows and BatchSize=50, run #1 covers the first 50, run #2 covers the rest.
// Dst pattern uses {{.id}} so each row gets a unique dst — combined with the
// UNIQUE(trace_id, dst) constraint, this is what makes COUNT(*)==COUNT(DISTINCT)
// the right way to assert "no loss AND no dup".
func TestS1_CrashRecoveryNoLossNoDup(t *testing.T) {
	ctx := context.Background()
	srcPool := setupSourceDB(t)
	imgPool := setupImgsyncDBWithTransferJobs(t)

	t0 := time.Now().UTC().Add(-time.Hour)
	for i := 1; i <= 100; i++ {
		_, err := srcPool.Exec(ctx, `INSERT INTO images VALUES ($1, $2, $3)`,
			i, t0.Add(time.Duration(i)*time.Second), "f.jpg")
		require.NoError(t, err)
	}

	makeS := func() *sniffer.Sniffer {
		return sniffer.New(sniffer.Config{
			SourceID:    "src",
			Query:       sniffer.Query{Table: "images", PKColumn: "id", TSColumn: "updated_at", ExtraColumns: []string{"file_path"}, BatchSize: 50},
			Dst:         sniffer.DstTemplate{Pattern: "/in/{{.id}}", Shadow: true},
			SrcPattern:  "src://images/{{.id}}",
			SrcProtocol: "fs",
			DstProtocol: "fs",
			ImgsyncPool: imgPool, SourcePool: srcPool,
		})
	}

	_, err := makeS().RunOnce(ctx)
	require.NoError(t, err)
	_, err = makeS().RunOnce(ctx)
	require.NoError(t, err)

	var distinct, total int
	require.NoError(t, imgPool.QueryRow(ctx, `SELECT COUNT(DISTINCT trace_id) FROM transfer_jobs`).Scan(&distinct))
	require.NoError(t, imgPool.QueryRow(ctx, `SELECT COUNT(*) FROM transfer_jobs`).Scan(&total))
	require.Equal(t, 100, distinct, "must enqueue all 100 rows")
	require.Equal(t, 100, total, "must not duplicate any row across the two runs")
}

// S2: 10 rows with identical updated_at, batch_size=3 forces 4 batches.
// All 10 enqueued exactly once; sniffer_state.last_run_pk == "10".
func TestS2_TieBreakBatchCorrectness(t *testing.T) {
	ctx := context.Background()
	srcPool := setupSourceDB(t)
	imgPool := setupImgsyncDBWithTransferJobs(t)

	ts := time.Now().UTC().Add(-time.Hour)
	for i := 1; i <= 10; i++ {
		_, err := srcPool.Exec(ctx, `INSERT INTO images VALUES ($1, $2, 'p')`, i, ts)
		require.NoError(t, err)
	}

	s := sniffer.New(sniffer.Config{
		SourceID:    "src",
		Query:       sniffer.Query{Table: "images", PKColumn: "id", TSColumn: "updated_at", ExtraColumns: []string{"file_path"}, BatchSize: 3},
		Dst:         sniffer.DstTemplate{Pattern: "/in/{{.id}}"},
		SrcPattern:  "src://{{.id}}",
		SrcProtocol: "fs",
		DstProtocol: "fs",
		ImgsyncPool: imgPool, SourcePool: srcPool,
	})

	for i := 0; i < 5; i++ {
		_, err := s.RunOnce(ctx)
		require.NoError(t, err)
	}

	var n int
	require.NoError(t, imgPool.QueryRow(ctx, `SELECT COUNT(*) FROM transfer_jobs`).Scan(&n))
	require.Equal(t, 10, n, "expected 10 rows")

	st, err := sniffer.NewStateRepo(imgPool).Load(ctx, "src")
	require.NoError(t, err)
	require.Equal(t, "10", st.LastRunPK, "last_run_pk must be 10")
}

// S3: source DB query takes longer than the per-query timeout.
// Sniffer's RunOnce returns an error (timeout) and watermark stays unchanged.
func TestS3_QueryTimeoutLeavesWatermarkUnchanged(t *testing.T) {
	srcPool := setupSourceDB(t)
	imgPool := setupImgsyncDBWithTransferJobs(t)

	ts := time.Now().UTC().Add(-time.Hour)
	_, err := srcPool.Exec(context.Background(), `INSERT INTO images VALUES (1, $1, 'p')`, ts)
	require.NoError(t, err)

	_, err = srcPool.Exec(context.Background(), `
		CREATE OR REPLACE VIEW images_slow AS
		SELECT i.id, i.updated_at, i.file_path
		FROM images i CROSS JOIN (SELECT pg_sleep(2)) AS _delay`)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	s := sniffer.New(sniffer.Config{
		SourceID:    "src",
		Query:       sniffer.Query{Table: "images_slow", PKColumn: "id", TSColumn: "updated_at", ExtraColumns: []string{"file_path"}, BatchSize: 10},
		Dst:         sniffer.DstTemplate{Pattern: "/in/{{.id}}"},
		SrcPattern:  "src://{{.id}}",
		SrcProtocol: "fs",
		DstProtocol: "fs",
		ImgsyncPool: imgPool, SourcePool: srcPool,
	})

	_, err = s.RunOnce(ctx)
	require.Error(t, err, "expected timeout error")

	st, err := sniffer.NewStateRepo(imgPool).Load(context.Background(), "src")
	require.NoError(t, err)
	require.True(t, st.LastRunTS.IsZero(), "watermark must not advance despite timeout: %v", st.LastRunTS)
}
