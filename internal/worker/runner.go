package worker

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nineking424/imgsync/internal/transfer"
)

// SourceLike and TransportLike are aliases for the streaming interfaces. Used
// by the runner factories.
type SourceLike = transfer.Source
type TransportLike = transfer.Transport

// ErrUnknownProtocol is returned by Source/Transport factories when src_protocol
// or dst_protocol does not match a registered impl.
var ErrUnknownProtocol = errors.New("unknown protocol")

// Runner drains the queue with N goroutines.
type Runner struct {
	Pool         *pgxpool.Pool
	Workers      int
	PodName      string
	IdleSleep    time.Duration
	SourceFor    func(protocol string) (SourceLike, error)
	TransportFor func(protocol string) (TransportLike, error)
	OnFinish     func(*Job) // optional, test hook
}

// Run blocks until ctx is cancelled.
func (r *Runner) Run(ctx context.Context) error {
	if r.Workers <= 0 {
		r.Workers = 4
	}
	if r.IdleSleep <= 0 {
		r.IdleSleep = 1 * time.Second
	}
	if r.PodName == "" {
		r.PodName = "imgsync-worker"
	}

	var wg sync.WaitGroup
	for i := 0; i < r.Workers; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			r.loop(ctx, idx)
		}(i)
	}
	wg.Wait()
	return nil
}

func (r *Runner) loop(ctx context.Context, idx int) {
	lockedBy := fmt.Sprintf("%s-w%d", r.PodName, idx)
	for {
		if err := ctx.Err(); err != nil {
			return
		}
		job, err := LeaseJob(ctx, r.Pool, lockedBy)
		if err != nil {
			// Transient DB error: short sleep and retry.
			select {
			case <-ctx.Done():
				return
			case <-time.After(r.IdleSleep):
				continue
			}
		}
		if job == nil {
			select {
			case <-ctx.Done():
				return
			case <-time.After(r.IdleSleep):
				continue
			}
		}

		src, err := r.SourceFor(job.SrcProtocol)
		if err != nil {
			_ = writeTerminal(ctx, Deps{Pool: r.Pool, LockedBy: lockedBy}, job,
				"dead", "dead",
				map[string]any{"error": err.Error(), "stage": "source-factory"}, true)
			r.fire(job)
			continue
		}
		tr, err := r.TransportFor(job.DstProtocol)
		if err != nil {
			_ = writeTerminal(ctx, Deps{Pool: r.Pool, LockedBy: lockedBy}, job,
				"dead", "dead",
				map[string]any{"error": err.Error(), "stage": "transport-factory"}, true)
			r.fire(job)
			continue
		}

		_ = ProcessJob(ctx, Deps{
			Pool: r.Pool, LockedBy: lockedBy, Source: src, Transport: tr,
		}, job)
		r.fire(job)
	}
}

func (r *Runner) fire(job *Job) {
	if r.OnFinish != nil {
		r.OnFinish(job)
	}
}
