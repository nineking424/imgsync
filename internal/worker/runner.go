package worker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime/debug"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nineking424/imgsync/internal/backoff"
	"github.com/nineking424/imgsync/internal/transfer"
)

// drainTimeout bounds the best-effort lease-reset performed when a worker loop
// exits due to ctx cancellation (SIGTERM). It uses a fresh context because the
// loop ctx is already cancelled at drain time.
const drainTimeout = 5 * time.Second

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
	// OnFinish fires after each job reaches a terminal outcome. result is the
	// metric result label for that outcome (succeeded / skipped / dead / fail).
	// Optional; nil-safe.
	OnFinish func(job *Job, result string)
	// OnRetry fires when a job is rescheduled for retry (DB status=pending,
	// attempts<max), not on a terminal outcome. stage is the error-category that
	// triggered the retry (e.g. "transport", "open"). Optional; nil-safe.
	OnRetry func(job *Job, stage string)
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
	// Graceful drain (#21): when the loop exits because ctx was cancelled
	// (SIGTERM), best-effort requeue any row this worker still holds leased so it
	// reschedules immediately instead of waiting ~5 min for the sweeper.
	defer r.drainLease(lockedBy)
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

		// processOne recovers panics per-iteration (#23): a panicking
		// Source/Transport must not unwind the loop goroutine. On panic it
		// records a retry/terminal outcome so attempts advances (poison caps to
		// dead) and the loop continues draining.
		r.processOne(ctx, idx, lockedBy, job)
	}
}

// processOne dispatches a single leased job and guarantees the worker loop
// survives a panic raised anywhere inside ProcessJob. A recovered panic is
// turned into a retry/terminal outcome via writeRetryOrDead so attempts
// advances under the existing lease guard; a poison job therefore eventually
// caps to dead instead of being re-leased and re-panicking forever.
func (r *Runner) processOne(ctx context.Context, idx int, lockedBy string, job *Job) {
	defer func() {
		if rec := recover(); rec != nil {
			fmt.Fprintf(os.Stderr,
				"imgsync worker: recovered panic in worker %d (%s) job %d: %v\n%s\n",
				idx, lockedBy, job.ID, rec, debug.Stack())
			// Use a fresh context: ctx may already be cancelled if the panic
			// coincided with shutdown, but the outcome write must still land.
			wctx, cancel := context.WithTimeout(context.Background(), drainTimeout)
			defer cancel()
			result, _ := writeRetryOrDead(wctx,
				Deps{Pool: r.Pool, LockedBy: lockedBy}, job,
				fmt.Errorf("panic in job processing: %v", rec),
				map[string]any{"error": fmt.Sprintf("%v", rec), "stage": "panic"})
			r.fire(job, result)
		}
	}()

	src, err := r.SourceFor(job.SrcProtocol)
	if err != nil {
		_ = writeTerminal(ctx, Deps{Pool: r.Pool, LockedBy: lockedBy}, job,
			"dead", "dead",
			map[string]any{"error": err.Error(), "stage": "source-factory"}, true)
		r.fire(job, "dead")
		return
	}
	tr, err := r.TransportFor(job.DstProtocol)
	if err != nil {
		_ = writeTerminal(ctx, Deps{Pool: r.Pool, LockedBy: lockedBy}, job,
			"dead", "dead",
			map[string]any{"error": err.Error(), "stage": "transport-factory"}, true)
		r.fire(job, "dead")
		return
	}

	result, _ := ProcessJob(ctx, Deps{
		Pool: r.Pool, LockedBy: lockedBy, Source: src, Transport: tr,
		OnRetry: func(stage string) {
			if r.OnRetry != nil {
				r.OnRetry(job, stage)
			}
		},
	}, job)
	r.fire(job, result)
}

// drainLease best-effort requeues any row still leased by this worker on a clean
// shutdown (#21). It runs after the loop ctx is already cancelled, so it derives
// a fresh short-lived context. Failures are logged and ignored — the sweeper is
// the backstop.
func (r *Runner) drainLease(lockedBy string) {
	ctx, cancel := context.WithTimeout(context.Background(), drainTimeout)
	defer cancel()
	if _, err := r.Pool.Exec(ctx, `
UPDATE transfer_jobs
SET status='pending', locked_at=NULL, locked_by=NULL, updated_at=NOW()
WHERE status='leased' AND locked_by=$1`, lockedBy); err != nil {
		fmt.Fprintf(os.Stderr,
			"imgsync worker: drain reset failed (%s): %v\n", lockedBy, err)
	}
}

func (r *Runner) fire(job *Job, result string) {
	if r.OnFinish != nil {
		r.OnFinish(job, result)
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
