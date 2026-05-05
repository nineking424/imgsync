package sniffer_test

import (
	"context"
	"testing"
	"time"

	"github.com/nineking424/imgsync/internal/sniffer"
	"github.com/stretchr/testify/require"
)

func TestRunOnce_EnqueuesAllAndAdvancesWatermark(t *testing.T) {
	ctx := context.Background()
	srcPool := setupSourceDB(t)
	imgPool := setupImgsyncDBWithTransferJobs(t)

	ts := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	for i := 1; i <= 3; i++ {
		_, err := srcPool.Exec(ctx, `INSERT INTO images VALUES ($1, $2, $3)`,
			i, ts.Add(time.Duration(i)*time.Second), "row.jpg")
		require.NoError(t, err)
	}

	s := sniffer.New(sniffer.Config{
		SourceID:    "main.images",
		Query:       sniffer.Query{Table: "images", PKColumn: "id", TSColumn: "updated_at", ExtraColumns: []string{"file_path"}, BatchSize: 10},
		Dst:         sniffer.DstTemplate{Pattern: "/in/{{.file_path}}", Shadow: true},
		SrcPattern:  "src://images/{{.id}}",
		SrcProtocol: "fs",
		DstProtocol: "fs",
		ImgsyncPool: imgPool,
		SourcePool:  srcPool,
	})

	n, err := s.RunOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("enqueued=%d", n)
	}

	var jobs int
	_ = imgPool.QueryRow(ctx, `SELECT COUNT(*) FROM transfer_jobs`).Scan(&jobs)
	if jobs != 3 {
		t.Fatalf("transfer_jobs=%d", jobs)
	}

	// Watermark advanced to last row.
	st, _ := sniffer.NewStateRepo(imgPool).Load(ctx, "main.images")
	if st.LastRunPK != "3" {
		t.Fatalf("last_run_pk=%q", st.LastRunPK)
	}
}

func TestRunOnce_InvokesEnqueueCallbackWithRowCount(t *testing.T) {
	ctx := context.Background()
	srcPool := setupSourceDB(t)
	imgPool := setupImgsyncDBWithTransferJobs(t)

	ts := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	for i := 1; i <= 3; i++ {
		_, err := srcPool.Exec(ctx, `INSERT INTO images VALUES ($1, $2, $3)`,
			i, ts.Add(time.Duration(i)*time.Second), "row.jpg")
		require.NoError(t, err)
	}

	var calls []struct {
		src string
		n   int
	}
	s := sniffer.New(sniffer.Config{
		SourceID:    "main.images",
		Query:       sniffer.Query{Table: "images", PKColumn: "id", TSColumn: "updated_at", ExtraColumns: []string{"file_path"}, BatchSize: 10},
		Dst:         sniffer.DstTemplate{Pattern: "/in/{{.file_path}}", Shadow: true},
		SrcPattern:  "src://images/{{.id}}",
		SrcProtocol: "fs",
		DstProtocol: "fs",
		ImgsyncPool: imgPool,
		SourcePool:  srcPool,
		OnEnqueue: func(src string, n int) {
			calls = append(calls, struct {
				src string
				n   int
			}{src, n})
		},
		OnError: func(string) {
			t.Fatalf("OnError must not fire on success")
		},
	})

	n, err := s.RunOnce(ctx)
	require.NoError(t, err)
	require.Equal(t, 3, n)
	require.Len(t, calls, 1)
	require.Equal(t, "main.images", calls[0].src)
	require.Equal(t, 3, calls[0].n)
}

func TestRunOnce_InvokesEnqueueCallbackWithZeroOnEmptyBatch(t *testing.T) {
	ctx := context.Background()
	srcPool := setupSourceDB(t)
	imgPool := setupImgsyncDBWithTransferJobs(t)

	var got struct {
		src   string
		n     int
		fired bool
	}
	s := sniffer.New(sniffer.Config{
		SourceID:    "main.images",
		Query:       sniffer.Query{Table: "images", PKColumn: "id", TSColumn: "updated_at", BatchSize: 10},
		Dst:         sniffer.DstTemplate{Pattern: "/in/{{.id}}"},
		SrcPattern:  "src://images/{{.id}}",
		SrcProtocol: "fs",
		DstProtocol: "fs",
		ImgsyncPool: imgPool,
		SourcePool:  srcPool,
		OnEnqueue: func(src string, n int) {
			got.src, got.n, got.fired = src, n, true
		},
	})

	n, err := s.RunOnce(ctx)
	require.NoError(t, err)
	require.Equal(t, 0, n)
	require.True(t, got.fired, "OnEnqueue must fire even when no rows are enqueued")
	require.Equal(t, "main.images", got.src)
	require.Equal(t, 0, got.n)
}

func TestRunOnce_InvokesErrorCallbackOnFailure(t *testing.T) {
	ctx := context.Background()
	srcPool := setupSourceDB(t)
	imgPool := setupImgsyncDBWithTransferJobs(t)

	var errCalls []string
	s := sniffer.New(sniffer.Config{
		SourceID: "main.images",
		// Invalid table name forces Query.Fetch to fail.
		Query:       sniffer.Query{Table: "no_such_table", PKColumn: "id", TSColumn: "updated_at", BatchSize: 10},
		Dst:         sniffer.DstTemplate{Pattern: "/in/{{.id}}"},
		SrcPattern:  "src://images/{{.id}}",
		SrcProtocol: "fs",
		DstProtocol: "fs",
		ImgsyncPool: imgPool,
		SourcePool:  srcPool,
		OnEnqueue: func(string, int) {
			t.Fatalf("OnEnqueue must not fire on error")
		},
		OnError: func(src string) {
			errCalls = append(errCalls, src)
		},
	})

	_, err := s.RunOnce(ctx)
	require.Error(t, err)
	require.Equal(t, []string{"main.images"}, errCalls)
}

func TestRunOnce_NoRowsLeavesWatermarkUnchanged(t *testing.T) {
	ctx := context.Background()
	srcPool := setupSourceDB(t)
	imgPool := setupImgsyncDBWithTransferJobs(t)

	s := sniffer.New(sniffer.Config{
		SourceID:    "main.images",
		Query:       sniffer.Query{Table: "images", PKColumn: "id", TSColumn: "updated_at", BatchSize: 10},
		Dst:         sniffer.DstTemplate{Pattern: "/in/{{.id}}"},
		SrcPattern:  "src://images/{{.id}}",
		SrcProtocol: "fs",
		DstProtocol: "fs",
		ImgsyncPool: imgPool,
		SourcePool:  srcPool,
	})

	n, err := s.RunOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("expected 0, got %d", n)
	}
	st, _ := sniffer.NewStateRepo(imgPool).Load(ctx, "main.images")
	if !st.LastRunTS.IsZero() {
		t.Fatalf("watermark should remain zero, got %v", st.LastRunTS)
	}
}
