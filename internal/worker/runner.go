package worker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime/debug"
	"sync"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nineking424/imgsync/internal/backoff"
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
	IdleBackoff  *backoff.Idle
	SourceFor    func(protocol string) (SourceLike, error)
	TransportFor func(protocol string) (TransportLike, error)
	OnFinish     func(*Job) // optional, test hook
	// OnLeaseAttempt fires after every LeaseJob call. success=true means a
	// row was acquired and dispatched; success=false means empty queue or
	// transient DB error. Optional; nil-safe.
	OnLeaseAttempt func(success bool)
	// OnWorkerStart / OnWorkerStop are invoked when a worker goroutine enters /
	// leaves its loop. Both nil-safe. Used by metrics wiring.
	OnWorkerStart func(pod string)
	OnWorkerStop  func(pod string)
}

// Run blocks until ctx is cancelled.
func (r *Runner) Run(ctx context.Context) error {
	if r.Workers <= 0 {
		r.Workers = 4
	}
	if r.IdleBackoff == nil {
		r.IdleBackoff = backoff.NewIdle(backoff.Config{})
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
	r.emitStart()
	defer r.emitStop()
	lockedBy := fmt.Sprintf("%s-w%d", r.PodName, idx)
	defer func() {
		if rec := recover(); rec != nil {
			fmt.Fprintf(os.Stderr,
				"imgsync worker: panic in worker %d (%s): %v\n%s\n",
				idx, lockedBy, rec, debug.Stack())
		}
	}()
	for {
		if err := ctx.Err(); err != nil {
			return
		}
		job, err := LeaseJob(ctx, r.Pool, lockedBy)
		if err != nil {
			// Transient DB error: log and short sleep before retry.
			fmt.Fprintf(os.Stderr,
				"imgsync worker: lease error (worker %d, %s): %v\n",
				idx, lockedBy, err)
			if r.OnLeaseAttempt != nil {
				r.OnLeaseAttempt(false)
			}
			r.IdleBackoff.WaitOnce(ctx)
			continue
		}
		if job == nil {
			// TODO(F2): DB error and empty-queue currently share the same backoff schedule;
			// split if transient DB errors become a real incident source.
			if r.OnLeaseAttempt != nil {
				r.OnLeaseAttempt(false)
			}
			r.IdleBackoff.WaitOnce(ctx)
			continue
		}
		if r.OnLeaseAttempt != nil {
			r.OnLeaseAttempt(true)
		}
		r.IdleBackoff.WakeAll()

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

func (r *Runner) emitStart() {
	if r.OnWorkerStart != nil {
		r.OnWorkerStart(r.PodName)
	}
}
func (r *Runner) emitStop() {
	if r.OnWorkerStop != nil {
		r.OnWorkerStop(r.PodName)
	}
}
