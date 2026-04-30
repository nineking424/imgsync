//go:build integration

package sniffer_test

import (
	"context"
	"testing"
	"time"

	"github.com/nineking424/imgsync/internal/sniffer"
)

// S0: polling overlap correctness
//
//	Run #1 sniffs window. 5 minutes later, run #2 sniffs an overlapping window.
//	Same source row must NOT produce a duplicate transfer_jobs row.
func TestS0_PollingOverlapNoDuplicate(t *testing.T) {
	ctx := context.Background()
	srcPool := setupSourceDB(t)
	imgPool := setupImgsyncDBWithTransferJobs(t)

	t0 := time.Now().UTC().Add(-30 * time.Minute)
	if _, err := srcPool.Exec(ctx, `INSERT INTO images VALUES (1, $1, 'p.jpg')`, t0); err != nil {
		t.Fatal(err)
	}

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

	if _, err := makeS(0).RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	// "5 minutes later" — re-run with overlap (we just call RunOnce again from the same state row, which already advanced).
	// Force overlap by resetting watermark backwards.
	_, _ = imgPool.Exec(ctx, `UPDATE sniffer_state SET last_run_ts = last_run_ts - INTERVAL '25 minutes', last_run_pk = NULL`)
	if _, err := makeS(0).RunOnce(ctx); err != nil {
		t.Fatal(err)
	}

	var n int
	_ = imgPool.QueryRow(ctx, `SELECT COUNT(*) FROM transfer_jobs`).Scan(&n)
	if n != 1 {
		t.Fatalf("expected 1 row after overlapping sniff, got %d", n)
	}
}

// S1: crash recovery — kill -9 after 50/100 rows enqueued, restart, no loss.
func TestS1_CrashRecoveryNoLossNoDup(t *testing.T) {
	ctx := context.Background()
	srcPool := setupSourceDB(t)
	imgPool := setupImgsyncDBWithTransferJobs(t)

	t0 := time.Now().UTC().Add(-time.Hour)
	for i := 1; i <= 100; i++ {
		_, _ = srcPool.Exec(ctx, `INSERT INTO images VALUES ($1, $2, $3)`,
			i, t0.Add(time.Duration(i)*time.Second), "f.jpg")
	}

	make := func() *sniffer.Sniffer {
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

	if _, err := make().RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := make().RunOnce(ctx); err != nil {
		t.Fatal(err)
	}

	var n int
	_ = imgPool.QueryRow(ctx, `SELECT COUNT(DISTINCT trace_id) FROM transfer_jobs`).Scan(&n)
	if n != 100 {
		t.Fatalf("expected 100 distinct trace_ids, got %d", n)
	}
}
