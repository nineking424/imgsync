//go:build e2e
// +build e2e

package e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	clusterName = "imgsync-e2e"
	namespace   = "imgsync-e2e"
	releaseName = "imgsync"
	chartPath   = "../deploy/helm/imgsync"
	hostFsRoot  = "/tmp/imgsync-e2e-localfs"
)

// kindEnv holds the live cluster + DB handle.
type kindEnv struct {
	pool        *pgxpool.Pool
	dsnLocal    string // pgx-friendly DSN reachable from the test host (port-forwarded)
	pgPFCmd     *exec.Cmd
	pgPFCancl   context.CancelFunc
	tearingDown atomic.Bool // set by teardown() so the PF watchdog stays quiet on intentional exit
}

func bootstrapKindEnv(t *testing.T, ctx context.Context) *kindEnv {
	t.Helper()

	// Run scripts/e2e-up.sh from the repo root
	root := repoRoot(t)
	if err := runCmd(ctx, root, "./scripts/e2e-up.sh"); err != nil {
		t.Fatalf("e2e-up.sh failed: %v", err)
	}

	env := &kindEnv{}
	env.startPostgresPortForward(t)

	dsn := "postgres://imgsync:imgsync@127.0.0.1:5433/imgsync?sslmode=disable"
	env.dsnLocal = dsn

	// Connect with retry
	deadline := time.Now().Add(60 * time.Second)
	for {
		pool, err := pgxpool.New(ctx, dsn)
		if err == nil {
			if pingErr := pool.Ping(ctx); pingErr == nil {
				env.pool = pool
				break
			}
			pool.Close()
		}
		if time.Now().After(deadline) {
			t.Fatalf("postgres port-forward never came up: %v", err)
		}
		time.Sleep(1 * time.Second)
	}

	// Seed source files on the shared volume (1000 × 10MB)
	env.seedFixtures(t, ctx, 1000, 10*1024*1024)

	return env
}

func (e *kindEnv) teardown() {
	e.tearingDown.Store(true)
	if e.pool != nil {
		e.pool.Close()
	}
	if e.pgPFCancl != nil {
		e.pgPFCancl()
	}
	// Note: leaving the kind cluster running can speed up dev iteration.
	// The CI job runs `make e2e-down` after the test.
}

func (e *kindEnv) startPostgresPortForward(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	e.pgPFCancl = cancel
	cmd := exec.CommandContext(ctx, "kubectl", "-n", namespace,
		"port-forward", "svc/postgres", "5433:5432")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("kubectl port-forward failed: %v", err)
	}
	e.pgPFCmd = cmd
	time.Sleep(2 * time.Second) // give it time to establish

	// Watchdog: if port-forward dies before teardown (pod restart, eviction, blip),
	// surface it instead of letting subsequent pgxpool dials silently fail one-by-one.
	go func() {
		err := cmd.Wait()
		if e.tearingDown.Load() {
			return // intentional cancel
		}
		t.Errorf("postgres port-forward exited unexpectedly: %v", err)
	}()
}

// seedFixtures writes N source files onto the shared host volume that the worker
// nodes see at /srv/imgsync.  We write directly to /tmp/imgsync-e2e-localfs on
// the test host (kind extraMount maps host:/tmp/imgsync-e2e-localfs → node:/srv/imgsync).
func (e *kindEnv) seedFixtures(t *testing.T, ctx context.Context, count int, sizeBytes int) {
	t.Helper()
	srcDir := hostFsRoot + "/src"
	dstDir := hostFsRoot + "/dst"
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatalf("mkdir src: %v", err)
	}
	if err := os.MkdirAll(dstDir, 0o755); err != nil {
		t.Fatalf("mkdir dst: %v", err)
	}
	// 10MB sparse file repeated; fast to create
	chunk := make([]byte, 1024*1024)
	for i := 0; i < len(chunk); i++ {
		chunk[i] = byte(i % 256)
	}
	for i := 1; i <= count; i++ {
		path := fmt.Sprintf("%s/file-%05d.bin", srcDir, i)
		if _, err := os.Stat(path); err == nil {
			continue // already seeded
		}
		f, err := os.Create(path)
		if err != nil {
			t.Fatalf("create %s: %v", path, err)
		}
		for written := 0; written < sizeBytes; written += len(chunk) {
			n := len(chunk)
			if written+n > sizeBytes {
				n = sizeBytes - written
			}
			if _, err := f.Write(chunk[:n]); err != nil {
				t.Fatalf("write %s: %v", path, err)
			}
		}
		_ = f.Close()
	}
}

func (e *kindEnv) truncateJobs(t *testing.T, ctx context.Context) {
	t.Helper()
	if _, err := e.pool.Exec(ctx, "TRUNCATE transfer_jobs, transfer_events RESTART IDENTITY CASCADE"); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	// Phase B re-enqueues the same dst paths as Phase A. If we leave Phase A's
	// output files in place, Transport.Send's tempfile+rename overwrites them —
	// which on most filesystems is dramatically faster than allocating new
	// blocks, and would inflate tputB / skew the 8/2 ratio. Wipe dst so each
	// phase pays the same allocation cost.
	dstDir := hostFsRoot + "/dst"
	if err := os.RemoveAll(dstDir); err != nil {
		t.Fatalf("remove dst dir: %v", err)
	}
	if err := os.MkdirAll(dstDir, 0o755); err != nil {
		t.Fatalf("recreate dst dir: %v", err)
	}
}

func (e *kindEnv) enqueueLocalFSJobs(t *testing.T, ctx context.Context, prefix string, n int) {
	t.Helper()
	// Direct INSERT via the test's pool. We're skipping the `imgsync enqueue` CLI
	// here because the round-trip latency would dominate; the SQL is the same.
	tx, err := e.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)

	// Bug fix #1: use raw filesystem paths (no file:// URI scheme).
	// LocalFS Source/Transport use os.Open/os.Create on the raw path directly.
	batch := `
INSERT INTO transfer_jobs (trace_id, src, dst, src_protocol, dst_protocol, status, attempts, max_attempts, payload, next_run_at)
SELECT
  $1 || lpad(i::text, 5, '0'),
  '/srv/imgsync/src/file-' || lpad(i::text, 5, '0') || '.bin',
  '/srv/imgsync/dst/file-' || lpad(i::text, 5, '0') || '.bin',
  'localfs', 'localfs', 'pending', 0, 5, '{}'::jsonb, NOW()
FROM generate_series(1, $2) AS i
ON CONFLICT (trace_id, dst) DO NOTHING
`
	if _, err := tx.Exec(ctx, batch, prefix, n); err != nil {
		t.Fatalf("bulk insert: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

func (e *kindEnv) waitAllSucceeded(t *testing.T, ctx context.Context, expected int, budget time.Duration) time.Duration {
	t.Helper()
	start := time.Now()
	deadline := start.Add(budget)
	tick := time.NewTicker(1 * time.Second)
	defer tick.Stop()

	for {
		select {
		case <-ctx.Done():
			t.Fatal("ctx cancelled while waiting for jobs")
		case <-tick.C:
			n := e.countByStatus(t, ctx, "succeeded")
			if n >= expected {
				return time.Since(start)
			}
			if time.Now().After(deadline) {
				dead := e.countByStatus(t, ctx, "dead")
				// Bug fix #2: status enum has no 'processing'; use 'leased'.
				leased := e.countByStatus(t, ctx, "leased")
				t.Fatalf("waitAllSucceeded: only %d/%d succeeded after %v (dead=%d, leased=%d)",
					n, expected, budget, dead, leased)
			}
		}
	}
}

func (e *kindEnv) countByStatus(t *testing.T, ctx context.Context, status string) int {
	t.Helper()
	var n int
	row := e.pool.QueryRow(ctx, "SELECT count(*) FROM transfer_jobs WHERE status=$1", status)
	if err := row.Scan(&n); err != nil {
		t.Fatalf("countByStatus: %v", err)
	}
	return n
}

func (e *kindEnv) helmUpgrade(t *testing.T, ctx context.Context, sets map[string]string) {
	t.Helper()
	args := []string{
		"upgrade", "--install", releaseName, chartPath,
		"--namespace", namespace,
		"--set", "image.repository=imgsync",
		"--set", "image.tag=e2e",
		"--set", "image.pullPolicy=IfNotPresent",
		"--wait", "--timeout", "5m",
	}
	// Sort keys so --set order is deterministic across runs — easier to diff failure logs.
	keys := make([]string, 0, len(sets))
	for k := range sets {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		args = append(args, "--set", k+"="+sets[k])
	}
	if err := runCmd(ctx, repoRoot(t), "helm", args...); err != nil {
		t.Fatalf("helm upgrade: %v", err)
	}
}

func (e *kindEnv) waitReplicasReady(t *testing.T, ctx context.Context, want int, budget time.Duration) {
	t.Helper()
	deadline := time.Now().Add(budget)
	for {
		out, err := exec.CommandContext(ctx, "kubectl", "-n", namespace,
			"get", "deployment", releaseName,
			"-o", "jsonpath={.status.readyReplicas}").Output()
		if err == nil {
			s := strings.TrimSpace(string(out))
			if s == "" {
				s = "0"
			}
			ready, _ := strconv.Atoi(s)
			if ready >= want {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %s replicas ready after %v (wanted %d)", string(out), budget, want)
		}
		time.Sleep(2 * time.Second)
	}
}

func runCmd(ctx context.Context, dir string, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func repoRoot(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Fatalf("git rev-parse: %v", err)
	}
	return strings.TrimSpace(string(out))
}
