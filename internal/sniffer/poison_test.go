package sniffer_test

import (
	"context"
	"testing"
	"time"

	"github.com/nineking424/imgsync/internal/sniffer"
	"github.com/stretchr/testify/require"
)

// poisonPattern renders a normal dst for every row EXCEPT the one whose
// file_path is the literal "POISON": that row dereferences a key absent from
// the row's Fields map, which under missingkey=error is a DETERMINISTIC render
// error (the same row will fail identically on every retry — it cannot self-heal).
// This is the un-renderable "poison row" of #29, isolated to a single row so a
// mixed batch can be asserted.
const poisonPattern = `{{if eq .file_path "POISON"}}{{.does_not_exist}}{{else}}/in/{{.file_path}}{{end}}`

// TestRunOnce_PoisonRowSkippedBatchContinues is the #29 regression: a single
// un-renderable row in the middle of a batch must NOT abort the whole batch or
// pin the watermark. The good rows on BOTH sides of the poison must be enqueued
// and the watermark must advance past the poison so the next poll makes progress.
//
// RED on current code: runOnceImpl returns early on the first render error
// (sniffer.go: `return inserted, fmt.Errorf("render dst ...")`), so the
// post-poison good row (id=3) is never enqueued and the watermark stays at zero.
func TestRunOnce_PoisonRowSkippedBatchContinues(t *testing.T) {
	ctx := context.Background()
	srcPool := setupSourceDB(t)
	imgPool := setupImgsyncDBWithTransferJobs(t)

	ts := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	// id=1 good, id=2 poison, id=3 good — ascending ts so order is 1,2,3.
	rows := []struct {
		id   int
		path string
	}{
		{1, "a.jpg"},
		{2, "POISON"},
		{3, "c.jpg"},
	}
	for _, r := range rows {
		_, err := srcPool.Exec(ctx, `INSERT INTO images VALUES ($1, $2, $3)`,
			r.id, ts.Add(time.Duration(r.id)*time.Second), r.path)
		require.NoError(t, err)
	}

	s := sniffer.New(sniffer.Config{
		SourceID:    "main.images",
		Query:       sniffer.Query{Table: "images", PKColumn: "id", TSColumn: "updated_at", ExtraColumns: []string{"file_path"}, BatchSize: 10},
		Dst:         sniffer.DstTemplate{Pattern: poisonPattern},
		SrcPattern:  "src://images/{{.id}}",
		SrcProtocol: "fs",
		DstProtocol: "fs",
		ImgsyncPool: imgPool,
		SourcePool:  srcPool,
	})

	// A deterministic poison row must not surface as a RunOnce error: it is
	// skipped, not retried. (Transient enqueue/DB errors still error out — that
	// path is covered by the existing watermark-preservation tests.)
	n, err := s.RunOnce(ctx)
	require.NoError(t, err, "a deterministic poison row must be skipped, not abort the batch")
	require.Equal(t, 2, n, "both good rows (id=1, id=3) must be enqueued; only the poison row is skipped")

	// The good row AFTER the poison must still be ingested — proof the loop
	// continued past the poison rather than returning early.
	var hasPostPoison int
	require.NoError(t, imgPool.QueryRow(ctx,
		`SELECT COUNT(*) FROM transfer_jobs WHERE trace_id = 'images-3'`).Scan(&hasPostPoison))
	require.Equal(t, 1, hasPostPoison, "good row id=3 (after the poison) must be enqueued")

	// The poison row itself must NOT be enqueued (it could not be rendered).
	var hasPoison int
	require.NoError(t, imgPool.QueryRow(ctx,
		`SELECT COUNT(*) FROM transfer_jobs WHERE trace_id = 'images-2'`).Scan(&hasPoison))
	require.Equal(t, 0, hasPoison, "the un-renderable poison row must not be enqueued")

	// Watermark must advance PAST the poison to the last row's pk, otherwise
	// every subsequent poll re-fetches and re-fails the same batch forever.
	st, err := sniffer.NewStateRepo(imgPool).Load(ctx, "main.images")
	require.NoError(t, err)
	require.Equal(t, "3", st.LastRunPK, "watermark must advance past the poison row to pk=3")
}

// TestRunOnce_PoisonRowDoesNotStallNextPoll is the "no permanent stall" half of
// #29: after a poison row is skipped and the watermark advances, a second poll
// must find no un-processed rows (the watermark moved past the poison) and must
// not re-fail. On current code the watermark never advances, so the second
// RunOnce re-fetches the same poison batch and errors again — a forever stall.
func TestRunOnce_PoisonRowDoesNotStallNextPoll(t *testing.T) {
	ctx := context.Background()
	srcPool := setupSourceDB(t)
	imgPool := setupImgsyncDBWithTransferJobs(t)

	ts := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	_, err := srcPool.Exec(ctx, `INSERT INTO images VALUES (1, $1, 'POISON')`, ts.Add(time.Second))
	require.NoError(t, err)
	_, err = srcPool.Exec(ctx, `INSERT INTO images VALUES (2, $1, 'ok.jpg')`, ts.Add(2*time.Second))
	require.NoError(t, err)

	s := sniffer.New(sniffer.Config{
		SourceID:    "main.images",
		Query:       sniffer.Query{Table: "images", PKColumn: "id", TSColumn: "updated_at", ExtraColumns: []string{"file_path"}, BatchSize: 10},
		Dst:         sniffer.DstTemplate{Pattern: poisonPattern},
		SrcPattern:  "src://images/{{.id}}",
		SrcProtocol: "fs",
		DstProtocol: "fs",
		ImgsyncPool: imgPool,
		SourcePool:  srcPool,
	})

	// First poll: poison at id=1 is skipped, id=2 enqueued, watermark -> pk=2.
	_, err = s.RunOnce(ctx)
	require.NoError(t, err, "first poll must not stall on the poison row")

	// Second poll: nothing new past the watermark, so no work and no error.
	n2, err := s.RunOnce(ctx)
	require.NoError(t, err, "second poll must not re-hit the poison row (watermark advanced)")
	require.Equal(t, 0, n2, "no new rows on the second poll")

	var total int
	require.NoError(t, imgPool.QueryRow(ctx, `SELECT COUNT(*) FROM transfer_jobs`).Scan(&total))
	require.Equal(t, 1, total, "exactly the one good row is enqueued; poison is never enqueued or duplicated")
}
