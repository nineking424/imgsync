package main

import (
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/nineking424/imgsync/internal/backoff"
	"github.com/nineking424/imgsync/internal/db"
	"github.com/nineking424/imgsync/internal/health"
	"github.com/nineking424/imgsync/internal/hostcap"
	"github.com/nineking424/imgsync/internal/metrics"
	srcftp "github.com/nineking424/imgsync/internal/sources/ftp"
	"github.com/nineking424/imgsync/internal/sources/localfs"
	"github.com/nineking424/imgsync/internal/sweeper"
	pftp "github.com/nineking424/imgsync/internal/transports/ftp"
	tlocalfs "github.com/nineking424/imgsync/internal/transports/localfs"
	"github.com/nineking424/imgsync/internal/worker"
	"github.com/spf13/cobra"
)

func newWorkerCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "worker",
		Short: "Drain the transfer_jobs queue",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			dsn := os.Getenv("IMGSYNC_DSN")
			if dsn == "" {
				return errors.New("IMGSYNC_DSN is required")
			}
			workers := envInt("IMGSYNC_WORKERS", 4)
			podName := os.Getenv("IMGSYNC_POD_NAME")
			if podName == "" {
				h, _ := os.Hostname()
				podName = h
			}

			pool, err := db.NewPool(ctx, db.PoolConfig{
				DSN:      dsn,
				MaxConns: int32(2 + workers),
			})
			if err != nil {
				return err
			}
			defer pool.Close()

			m := metrics.New()
			m.AttachQueueDepth(pool)
			m.AttachDBPool(pool)
			m.AttachLeaseLockAge(pool)

			ftpPool := pftp.NewPool(pftp.PoolConfig{
				MaxPerHost:   envInt("IMGSYNC_FTP_MAX_PER_HOST", 4),
				IdleTTL:      time.Duration(envInt("IMGSYNC_FTP_IDLE_TTL_SEC", 300)) * time.Second,
				NoopAfter:    time.Duration(envInt("IMGSYNC_FTP_NOOP_AFTER_SEC", 60)) * time.Second,
				AuthUser:     os.Getenv("IMGSYNC_FTP_USER"),
				AuthPassword: os.Getenv("IMGSYNC_FTP_PASSWORD"),
				OnPoolChange: m.OnFTPPoolChange,
			})
			defer ftpPool.Close()

			localSource := localfs.NewSource()
			localTransport := tlocalfs.NewTransport()
			ftpSrc := srcftp.NewSource(ftpPool)
			ftpRaw := pftp.NewTransport(ftpPool)
			ftpTr := hostcap.Wrap(pool, ftpRaw, hostcap.Config{Cap: envInt("IMGSYNC_FTP_HOST_CAP", 8)})

			idle := backoff.NewIdle(backoff.Config{
				BaseDelay: 50 * time.Millisecond,
				MaxDelay:  1 * time.Second,
			})

			r := &worker.Runner{
				Pool:        pool,
				Workers:     workers,
				PodName:     podName,
				IdleBackoff: idle,
				SourceFor: func(proto string) (worker.SourceLike, error) {
					switch proto {
					case "localfs":
						return localSource, nil
					case "ftp":
						return ftpSrc, nil
					}
					return nil, worker.ErrUnknownProtocol
				},
				TransportFor: func(proto string) (worker.TransportLike, error) {
					switch proto {
					case "localfs":
						return localTransport, nil
					case "ftp":
						return ftpTr, nil
					}
					return nil, worker.ErrUnknownProtocol
				},
			}

			status := health.NewStatus()
			healthAddr := os.Getenv("IMGSYNC_HEALTH_ADDR")
			if healthAddr == "" {
				healthAddr = ":8080"
			}
			ln, err := net.Listen("tcp", healthAddr)
			if err != nil {
				return err
			}
			hs := health.NewServer(pool, status, health.WithMetrics(m.Handler()))
			go func() { _ = hs.Serve(ln) }()
			defer hs.Close()

			go func() {
				_ = sweeper.Run(ctx, pool, sweeper.Config{
					Threshold: 5 * time.Minute,
					Interval:  30 * time.Second,
					OnCycle: func() {
						status.OnSweepCycle()
						m.OnSweepCycle()
					},
				})
			}()

			// Compose: status (existing) + metrics (new). Domain calls a single
			// callback; chaining keeps the runner ignorant of multiple consumers.
			var workersGauge int32
			r.OnLeaseAttempt = func(success bool) {
				status.OnLeaseAttempt(success)
				// worker has no err in this signature; lease errors are logged separately.
				m.OnLeaseAttempt(success, nil)
			}
			r.OnFinish = func(j *worker.Job) {
				m.OnJobFinished(j.SrcProtocol, j.DstProtocol, j.Status, j.Duration())
			}
			r.OnWorkerStart = func(pod string) {
				atomic.AddInt32(&workersGauge, 1)
				m.SetWorkersActive(pod, int(atomic.LoadInt32(&workersGauge)))
			}
			r.OnWorkerStop = func(pod string) {
				atomic.AddInt32(&workersGauge, -1)
				m.SetWorkersActive(pod, int(atomic.LoadInt32(&workersGauge)))
			}

			fmt.Fprintf(cmd.OutOrStdout(),
				"imgsync worker starting: pod=%s workers=%d\n", podName, workers)
			return r.Run(ctx)
		},
	}
	return cmd
}

func envInt(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		fmt.Fprintf(os.Stderr,
			"imgsync worker: warning: %s=%q is not a valid integer, using default %d\n",
			key, v, def)
		return def
	}
	return n
}
