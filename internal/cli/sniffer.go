// Package cli provides testable config parsing and entrypoint logic for
// imgsync subcommands that sit above the internal packages.
package cli

import (
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nineking424/imgsync/internal/health"
	"github.com/nineking424/imgsync/internal/metrics"
	"github.com/nineking424/imgsync/internal/sniffer"
	"github.com/nineking424/imgsync/internal/sourcedb"
)

// SnifferConfig carries all env-derived configuration for the sniffer loop.
type SnifferConfig struct {
	SourceID     string
	SourceDSN    string
	ImgsyncDSN   string
	Table        string
	PKColumn     string
	TSColumn     string
	ExtraColumns []string
	DstPattern   string
	SrcPattern   string
	SrcProtocol  string
	DstProtocol  string
	Shadow       bool
	BatchSize    int
	BiasDuration time.Duration
	IntervalSec  int
}

// ParseSnifferConfig reads environment variables and returns a validated
// SnifferConfig. Returns an error if any required variable is absent.
func ParseSnifferConfig() (SnifferConfig, error) {
	c := SnifferConfig{
		Shadow:       envBool("SNIFFER_SHADOW", true),
		BatchSize:    envInt("SNIFFER_BATCH_SIZE", 500),
		BiasDuration: time.Duration(envInt("SNIFFER_BIAS_SEC", 5)) * time.Second,
		IntervalSec:  envInt("SNIFFER_INTERVAL_SEC", 60),
		SourceID:     os.Getenv("SNIFFER_SOURCE_ID"),
		SourceDSN:    os.Getenv("SNIFFER_SOURCE_DSN"),
		ImgsyncDSN:   os.Getenv("SNIFFER_IMGSYNC_DSN"),
		Table:        os.Getenv("SNIFFER_TABLE"),
		PKColumn:     os.Getenv("SNIFFER_PK_COLUMN"),
		TSColumn:     os.Getenv("SNIFFER_TS_COLUMN"),
		DstPattern:   os.Getenv("SNIFFER_DST_PATTERN"),
		SrcPattern:   os.Getenv("SNIFFER_SRC_PATTERN"),
		SrcProtocol:  os.Getenv("SNIFFER_SRC_PROTOCOL"),
		DstProtocol:  os.Getenv("SNIFFER_DST_PROTOCOL"),
	}
	if extra := os.Getenv("SNIFFER_EXTRA_COLUMNS"); extra != "" {
		for _, col := range strings.Split(extra, ",") {
			if col = strings.TrimSpace(col); col != "" {
				c.ExtraColumns = append(c.ExtraColumns, col)
			}
		}
	}
	required := map[string]string{
		"SNIFFER_SOURCE_ID":    c.SourceID,
		"SNIFFER_SOURCE_DSN":   c.SourceDSN,
		"SNIFFER_IMGSYNC_DSN":  c.ImgsyncDSN,
		"SNIFFER_TABLE":        c.Table,
		"SNIFFER_PK_COLUMN":    c.PKColumn,
		"SNIFFER_TS_COLUMN":    c.TSColumn,
		"SNIFFER_DST_PATTERN":  c.DstPattern,
		"SNIFFER_SRC_PATTERN":  c.SrcPattern,
		"SNIFFER_SRC_PROTOCOL": c.SrcProtocol,
		"SNIFFER_DST_PROTOCOL": c.DstProtocol,
	}
	for k, v := range required {
		if v == "" {
			return c, fmt.Errorf("required env %s missing", k)
		}
	}
	if c.IntervalSec <= 0 {
		return c, fmt.Errorf("SNIFFER_INTERVAL_SEC must be > 0, got %d", c.IntervalSec)
	}
	if c.BatchSize <= 0 {
		return c, fmt.Errorf("SNIFFER_BATCH_SIZE must be > 0, got %d", c.BatchSize)
	}
	return c, nil
}

// RunSniffer opens DB pools, constructs a Sniffer, runs one poll immediately,
// then loops on a ticker until ctx is cancelled or a signal arrives.
// signal.NotifyContext is applied as defense-in-depth for non-cobra callers.
func RunSniffer(ctx context.Context, cfg SnifferConfig) error {
	srcPool, err := sourcedb.NewPool(ctx, sourcedb.Config{
		DSN:            cfg.SourceDSN,
		QueryTimeoutMs: 30000,
	})
	if err != nil {
		return fmt.Errorf("source pool: %w", err)
	}
	defer srcPool.Close()

	imgPool, err := pgxpool.New(ctx, cfg.ImgsyncDSN)
	if err != nil {
		return fmt.Errorf("imgsync pool: %w", err)
	}
	defer imgPool.Close()

	m := metrics.New()
	m.AttachQueueDepth(imgPool)
	m.AttachDBPool(imgPool)
	// lease lock age is the worker's responsibility; not exposed by the sniffer.

	healthAddr := os.Getenv("SNIFFER_HEALTH_ADDR")
	if healthAddr == "" {
		healthAddr = ":8080"
	}
	ln, err := net.Listen("tcp", healthAddr)
	if err != nil {
		return fmt.Errorf("sniffer health listen: %w", err)
	}
	status := health.NewStatus() // sniffer never updates lease/sweep TS — empty status is fine
	hs := health.NewServer(imgPool, status, health.WithMetrics(m.Handler()))
	go func() { _ = hs.Serve(ln) }()
	defer hs.Close()

	s := sniffer.New(sniffer.Config{
		SourceID: cfg.SourceID,
		Query: sniffer.Query{
			Table:        cfg.Table,
			PKColumn:     cfg.PKColumn,
			TSColumn:     cfg.TSColumn,
			ExtraColumns: cfg.ExtraColumns,
			BatchSize:    cfg.BatchSize,
			BiasDuration: cfg.BiasDuration,
			QueryTimeout: srcPool.QueryTimeout,
		},
		Dst:         sniffer.DstTemplate{Pattern: cfg.DstPattern, Shadow: cfg.Shadow},
		SrcPattern:  cfg.SrcPattern,
		SrcProtocol: cfg.SrcProtocol,
		DstProtocol: cfg.DstProtocol,
		ImgsyncPool: imgPool,
		SourcePool:  srcPool.Pool,
		OnEnqueue:   m.OnSnifferEnqueue,
		OnError:     m.OnSnifferError,
	})

	ctx, cancel := signal.NotifyContext(ctx, syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	tick := time.NewTicker(time.Duration(cfg.IntervalSec) * time.Second)
	defer tick.Stop()

	// Run immediately so failures surface fast rather than waiting one interval.
	if n, err := s.RunOnce(ctx); err != nil {
		log.Printf("sniffer run error: %v", err)
	} else {
		m.OnSnifferRun(cfg.SourceID)
		log.Printf("sniffer enqueued %d new jobs", n)
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
			n, err := s.RunOnce(ctx)
			if err != nil {
				log.Printf("sniffer run error: %v", err)
				continue
			}
			m.OnSnifferRun(cfg.SourceID)
			log.Printf("sniffer enqueued %d new jobs", n)
		}
	}
}

// envInt returns the integer value of key, or def if the variable is absent or
// cannot be parsed.
func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

// envBool returns the boolean value of key ("1" or "true", case-insensitive),
// or def if absent.
func envBool(key string, def bool) bool {
	if v := os.Getenv(key); v != "" {
		return v == "1" || strings.EqualFold(v, "true")
	}
	return def
}
