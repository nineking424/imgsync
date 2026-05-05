# imgsync Monitoring Phase 1 — Metrics + Grafana Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** imgsync 의 worker · sniffer 두 프로세스가 Prometheus 가 scrape 할 수 있는 `/metrics` 엔드포인트를 노출하고, Grafana 대시보드 한 장으로 큐 / 워커 / 처리량 / 실패 4 분면을 즉시 볼 수 있게 한다.

**Architecture:** 신규 `internal/metrics` 패키지가 자체 prometheus registry 를 보유한다. 도메인 패키지 (worker, sweeper, sniffer, ftp pool) 는 metrics 를 import 하지 않고 기존 `OnXxx` 콜백 패턴을 그대로 재사용한다 — `Metrics` struct 의 메서드 값이 콜백에 바인딩되며, 의존성은 cmd 와이어링 한 곳에서만 양방향이 된다. 큐 깊이 / DB pool / lease lock age 는 `prometheus.Collector` 구현체로 scrape 시 SQL 한 번. 카운터/히스토그램은 in-process push.

**Tech Stack:** Go 1.25, `github.com/prometheus/client_golang` (신규 의존성, 유일), pgx/v5 (기존), Helm (ServiceMonitor + sniffer service/probes), Grafana 11 (JSON 대시보드).

---

## Spec reference

원본: `docs/superpowers/specs/2026-05-05-monitoring-stack-integration-design.md`. 이 plan 은 Phase 1 만 다룬다. Phase 1.5 (`transfer_jobs.status` 인덱스) 는 별도 plan: `docs/superpowers/plans/2026-05-05-monitoring-phase-1-5-status-index.md` — 같은 sprint 안에 머지된다.

## File structure

### 신규 파일

| 파일 | 역할 |
|---|---|
| `internal/metrics/metrics.go` | `Metrics` struct + registry + Handler + push 콜백 |
| `internal/metrics/buckets.go` | `defaultDurationBuckets` (file transfer 도메인용) |
| `internal/metrics/queue_depth.go` | scrape-time `queueDepthCollector` (jobs_in_status) |
| `internal/metrics/db_pool.go` | scrape-time `dbPoolCollector` (pgxpool.Stat) |
| `internal/metrics/lease_lock_age.go` | scrape-time `leaseLockAge` `GaugeFunc` |
| `internal/metrics/metrics_test.go` | unit — push 콜백, format anchor |
| `internal/metrics/integration_test.go` | `-tags integration` — testcontainers postgres + scrape collectors |
| `deploy/helm/imgsync/templates/servicemonitor.yaml` | Prometheus Operator ServiceMonitor (조건부) |
| `deploy/helm/imgsync/templates/sniffer-service.yaml` | sniffer pod 의 :8080 노출 |
| `deploy/helm/imgsync/dashboards/imgsync-overview.json` | Grafana 대시보드 |
| `docs/operating/dashboards.md` | Grafana import 가이드 |

### 변경 파일

| 파일 | 변경 |
|---|---|
| `go.mod` / `go.sum` | `prometheus/client_golang` 추가 |
| `internal/health/server.go` | `Option` 패턴 + `WithMetrics(http.Handler)` 옵션 |
| `internal/health/server_test.go` | `WithMetrics` 옵션 on/off 어설션 |
| `internal/worker/job.go` | `Job.Duration()` 메서드 추가 (`LockedAt` 기반) |
| `internal/worker/runner.go` | `OnWorkerStart` / `OnWorkerStop` 콜백 필드 + 호출 |
| `internal/sniffer/sniffer.go` | `Config.OnEnqueue` / `Config.OnError` 콜백 + `RunOnce` 호출 |
| `internal/transports/ftp/pool.go` | `PoolConfig.OnPoolChange` 콜백 + `Acquire` / `release` 시점 호출 |
| `cmd/imgsync/worker.go` | `metrics.New()` 와이어링 + Runner 콜백 + `health.WithMetrics` |
| `internal/cli/sniffer.go` | `metrics.New()` + HTTP listener (`health.NewServer`) 신설 |
| `deploy/helm/imgsync/values.yaml` | `monitoring.*`, `logging.format` 추가 |
| `deploy/helm/imgsync/templates/service.yaml` | `component: worker` selector + port `http-metrics` |
| `deploy/helm/imgsync/templates/deployment.yaml` | `component: worker` label |
| `deploy/helm/imgsync/templates/sniffer-deployment.yaml` | port `http`, livez/readyz/startup probes, `SNIFFER_HEALTH_ADDR` env |
| `deploy/helm/imgsync/tests/template_test.sh` | ServiceMonitor / sniffer service / probes / selector 어설션 |
| `docs/operating/monitoring.md` | "향후 계획" → 정식 카탈로그 |
| `docs/operating/runbook.md` | metric 기반 알람 표 추가 |

---

## Task 1: `prometheus/client_golang` 의존성 추가 + `internal/metrics` 골격

**Files:**
- Modify: `go.mod`
- Modify: `go.sum`
- Create: `internal/metrics/metrics.go`
- Test: `internal/metrics/metrics_test.go`

- [ ] **Step 1: 빈 패키지 + skeleton 테스트 작성 (failing)**

`internal/metrics/metrics_test.go` 신규:

```go
package metrics

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNew_ExposesMetricsHandlerWithIsolatedRegistry(t *testing.T) {
	m := New()

	srv := httptest.NewServer(m.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	ct := resp.Header.Get("Content-Type")
	if !strings.Contains(ct, "text/plain") && !strings.Contains(ct, "openmetrics") {
		t.Fatalf("content-type = %q, want prometheus text or openmetrics", ct)
	}
}
```

`internal/metrics/metrics.go` 신규 (스켈레톤만):

```go
// Package metrics owns the Prometheus collectors imgsync exposes via /metrics.
// One Metrics instance per process. Domain packages (worker/sweeper/sniffer/
// ftp pool) call methods on Metrics via the existing OnXxx callback pattern,
// so they do not import this package.
package metrics

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics holds every imgsync_* collector. Each instance has its own
// prometheus.Registry so tests can run in parallel.
type Metrics struct {
	reg *prometheus.Registry
}

// New constructs an empty Metrics with a fresh registry.
func New() *Metrics {
	return &Metrics{reg: prometheus.NewRegistry()}
}

// Handler returns the HTTP handler that serves the metrics in this registry.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.reg, promhttp.HandlerOpts{Registry: m.reg})
}
```

- [ ] **Step 2: 의존성 추가**

Run: `go get github.com/prometheus/client_golang@latest`
Run: `go mod tidy`

Expected: `go.mod` 에 `github.com/prometheus/client_golang vX.Y.Z` 라인이 추가되고 `go.sum` 이 갱신됨.

- [ ] **Step 3: 테스트 통과 확인**

Run: `go test ./internal/metrics/ -run TestNew_ExposesMetricsHandlerWithIsolatedRegistry -v`

Expected: PASS.

- [ ] **Step 4: 커밋**

```bash
git add internal/metrics/metrics.go internal/metrics/metrics_test.go go.mod go.sum
git commit -m "feat(metrics): scaffold internal/metrics package with promhttp handler"
```

---

## Task 2: in-process push 메트릭 등록 + 콜백 메서드

`Metrics` 가 다음 6 종 push 컬렉터를 보유하고 콜백 메서드로 갱신한다 (jobs_in_status, lease_lock_age, db_pool 은 Task 4 의 scrape-time collector). 카탈로그는 spec §4.

| 메트릭 | 타입 | 라벨 | 콜백 메서드 |
|---|---|---|---|
| `imgsync_lease_attempts_total` | CounterVec | `result` (success/empty/error) | `OnLeaseAttempt(success bool, err error)` |
| `imgsync_jobs_processed_total` | CounterVec | `result` (success/skip/fail/expire/dead) | `OnJobFinished(src, dst, result string, dur time.Duration)` |
| `imgsync_job_duration_seconds` | HistogramVec | `src`, `dst`, `result` | `OnJobFinished` (같은 콜백) |
| `imgsync_sweep_cycles_total` | Counter | — | `OnSweepCycle()` |
| `imgsync_sniffer_enqueue_total` | CounterVec | `source` | `OnSnifferEnqueue(source string, n int)` |
| `imgsync_sniffer_run_errors_total` | CounterVec | `source` | `OnSnifferError(source string)` |
| `imgsync_ftp_pool_size` | GaugeVec | `host`, `state` (in_use/idle) | `OnFTPPoolChange(host string, inUse, idle int)` |

`OnLeaseAttempt` 의 시그니처는 spec 의 `func(bool)` 보다 한 단계 풍부한 `(success bool, err error)` 를 권장한다 — `result` 라벨이 success/empty/error 세 값을 가지려면 호출자에서 빈 lease vs 오류 lease 를 구분할 수 있어야 하기 때문이다. 하지만 도메인의 기존 `OnLeaseAttempt func(bool)` 시그니처는 변경하지 않는다 — `Metrics.OnLeaseAttempt` 가 두 가지 wrapper 를 제공한다 (Step 3 참조).

**Files:**
- Modify: `internal/metrics/metrics.go`
- Create: `internal/metrics/buckets.go`
- Modify: `internal/metrics/metrics_test.go`

- [ ] **Step 1: 히스토그램 버킷 상수 (failing test 먼저)**

`internal/metrics/metrics_test.go` 에 추가:

```go
import (
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestOnJobFinished_IncrementsCounterAndObservesHistogram(t *testing.T) {
	m := New()
	m.OnJobFinished("ftp", "localfs", "success", 1500*time.Millisecond)

	got := testutil.ToFloat64(m.jobsProcessed.WithLabelValues("ftp", "localfs", "success"))
	if got != 1 {
		t.Fatalf("jobs_processed = %v, want 1", got)
	}
	count := testutil.CollectAndCount(m.jobDuration, "imgsync_job_duration_seconds")
	if count != 1 {
		t.Fatalf("job_duration series = %d, want 1", count)
	}
}

func TestOnLeaseAttempt_LabelsResult(t *testing.T) {
	m := New()
	m.OnLeaseAttempt(true, nil)
	m.OnLeaseAttempt(false, nil)
	m.OnLeaseAttempt(false, errors.New("boom"))

	if v := testutil.ToFloat64(m.leaseAttempts.WithLabelValues("success")); v != 1 {
		t.Fatalf("success = %v, want 1", v)
	}
	if v := testutil.ToFloat64(m.leaseAttempts.WithLabelValues("empty")); v != 1 {
		t.Fatalf("empty = %v, want 1", v)
	}
	if v := testutil.ToFloat64(m.leaseAttempts.WithLabelValues("error")); v != 1 {
		t.Fatalf("error = %v, want 1", v)
	}
}

func TestOnFTPPoolChange_GaugeReflectsInUseAndIdle(t *testing.T) {
	m := New()
	m.OnFTPPoolChange("ftp.example.com:21", 3, 1)

	if v := testutil.ToFloat64(m.ftpPoolSize.WithLabelValues("ftp.example.com:21", "in_use")); v != 3 {
		t.Fatalf("in_use = %v, want 3", v)
	}
	if v := testutil.ToFloat64(m.ftpPoolSize.WithLabelValues("ftp.example.com:21", "idle")); v != 1 {
		t.Fatalf("idle = %v, want 1", v)
	}
}

func TestOnSweepCycle_IncrementsCounter(t *testing.T) {
	m := New()
	m.OnSweepCycle()
	m.OnSweepCycle()
	if v := testutil.ToFloat64(m.sweepCycles); v != 2 {
		t.Fatalf("sweep_cycles = %v, want 2", v)
	}
}

func TestOnSnifferEnqueue_AddsN(t *testing.T) {
	m := New()
	m.OnSnifferEnqueue("orders", 7)
	m.OnSnifferEnqueue("orders", 3)
	if v := testutil.ToFloat64(m.snifferEnq.WithLabelValues("orders")); v != 10 {
		t.Fatalf("enqueue = %v, want 10", v)
	}
}

func TestOnSnifferError_IncrementsCounter(t *testing.T) {
	m := New()
	m.OnSnifferError("orders")
	if v := testutil.ToFloat64(m.snifferErr.WithLabelValues("orders")); v != 1 {
		t.Fatalf("err = %v, want 1", v)
	}
}
```

import 영역에 `"errors"` 와 `"time"` 추가.

Run: `go test ./internal/metrics/ -v`

Expected: 6 개 신규 테스트 모두 FAIL (메서드 / 필드 미존재).

- [ ] **Step 2: 버킷 정의 파일**

`internal/metrics/buckets.go` 신규:

```go
package metrics

// defaultDurationBuckets describes the file-transfer duration distribution
// imgsync sees in production: small images finish in well under a second,
// large FTP transfers can stretch to minutes, and pathological retries can
// reach tens of minutes. The buckets are wider than promhttp's defaults so
// the histogram stays informative across that whole range without exploding
// series count.
var defaultDurationBuckets = []float64{
	0.1, 0.5, 1, 2, 5, 10, 30, 60, 300, 1800,
}
```

- [ ] **Step 3: 컬렉터 등록 + 콜백 구현**

`internal/metrics/metrics.go` 의 `Metrics` struct + `New()` 를 다음으로 교체:

```go
type Metrics struct {
	reg *prometheus.Registry

	leaseAttempts *prometheus.CounterVec
	jobsProcessed *prometheus.CounterVec
	jobDuration   *prometheus.HistogramVec
	sweepCycles   prometheus.Counter
	ftpPoolSize   *prometheus.GaugeVec
	snifferEnq    *prometheus.CounterVec
	snifferErr    *prometheus.CounterVec
}

func New() *Metrics {
	reg := prometheus.NewRegistry()
	m := &Metrics{
		reg: reg,
		leaseAttempts: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "imgsync_lease_attempts_total",
			Help: "Number of LeaseJob attempts, labeled by outcome.",
		}, []string{"result"}),
		jobsProcessed: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "imgsync_jobs_processed_total",
			Help: "Number of jobs the worker has finished, labeled by terminal status.",
		}, []string{"src", "dst", "result"}),
		jobDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "imgsync_job_duration_seconds",
			Help:    "End-to-end duration from lease to terminal status.",
			Buckets: defaultDurationBuckets,
		}, []string{"src", "dst", "result"}),
		sweepCycles: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "imgsync_sweep_cycles_total",
			Help: "Number of sweeper cycles that completed (regardless of work done).",
		}),
		ftpPoolSize: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "imgsync_ftp_pool_size",
			Help: "FTP connection pool size per host, labeled by state.",
		}, []string{"host", "state"}),
		snifferEnq: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "imgsync_sniffer_enqueue_total",
			Help: "Rows the sniffer has inserted into transfer_jobs.",
		}, []string{"source"}),
		snifferErr: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "imgsync_sniffer_run_errors_total",
			Help: "RunOnce invocations that returned an error.",
		}, []string{"source"}),
	}
	reg.MustRegister(
		m.leaseAttempts, m.jobsProcessed, m.jobDuration, m.sweepCycles,
		m.ftpPoolSize, m.snifferEnq, m.snifferErr,
	)
	return m
}

// --- callback methods (bind to existing OnXxx hooks in domain packages) ----

func (m *Metrics) OnLeaseAttempt(success bool, err error) {
	switch {
	case err != nil:
		m.leaseAttempts.WithLabelValues("error").Inc()
	case success:
		m.leaseAttempts.WithLabelValues("success").Inc()
	default:
		m.leaseAttempts.WithLabelValues("empty").Inc()
	}
}

func (m *Metrics) OnJobFinished(src, dst, result string, dur time.Duration) {
	if src == "" {
		src = "unknown"
	}
	if dst == "" {
		dst = "unknown"
	}
	if result == "" {
		result = "unknown"
	}
	m.jobsProcessed.WithLabelValues(src, dst, result).Inc()
	m.jobDuration.WithLabelValues(src, dst, result).Observe(dur.Seconds())
}

func (m *Metrics) OnSweepCycle()                              { m.sweepCycles.Inc() }
func (m *Metrics) OnSnifferEnqueue(source string, n int)      { m.snifferEnq.WithLabelValues(source).Add(float64(n)) }
func (m *Metrics) OnSnifferError(source string)               { m.snifferErr.WithLabelValues(source).Inc() }
func (m *Metrics) OnFTPPoolChange(host string, inUse, idle int) {
	m.ftpPoolSize.WithLabelValues(host, "in_use").Set(float64(inUse))
	m.ftpPoolSize.WithLabelValues(host, "idle").Set(float64(idle))
}
```

import 에 `"time"` 추가.

- [ ] **Step 4: 테스트 통과 확인**

Run: `go test ./internal/metrics/ -v`

Expected: 모든 테스트 PASS (Step 1 의 6 개 + Task 1 의 1 개).

- [ ] **Step 5: 커밋**

```bash
git add internal/metrics/metrics.go internal/metrics/buckets.go internal/metrics/metrics_test.go
git commit -m "feat(metrics): in-process counters/histograms/gauges + callbacks"
```

---

## Task 3: `workers_active` 게이지 + `Runner.OnWorkerStart`/`OnWorkerStop`

`imgsync_workers_active{pod}` 는 worker goroutine 의 시작/종료를 atomic 으로 추적한다. `Runner` 에 두 콜백 필드를 추가하고 `loop` 시작 / 종료 지점에서 호출.

**Files:**
- Modify: `internal/metrics/metrics.go`
- Modify: `internal/metrics/metrics_test.go`
- Modify: `internal/worker/runner.go`
- Test: `internal/worker/runner_test.go`

- [ ] **Step 1: 메트릭 측 failing test**

`internal/metrics/metrics_test.go` 에 추가:

```go
func TestSetWorkersActive_RecordsPodGauge(t *testing.T) {
	m := New()
	m.SetWorkersActive("pod-a", 3)
	m.SetWorkersActive("pod-a", 2) // a worker stopped
	if v := testutil.ToFloat64(m.workersActive.WithLabelValues("pod-a")); v != 2 {
		t.Fatalf("active = %v, want 2", v)
	}
}
```

- [ ] **Step 2: `Metrics` 에 게이지 추가**

`internal/metrics/metrics.go` 의 `Metrics` struct 에 `workersActive *prometheus.GaugeVec` 필드 추가, `New()` 에서:

```go
workersActive: prometheus.NewGaugeVec(prometheus.GaugeOpts{
    Name: "imgsync_workers_active",
    Help: "Worker goroutines currently running, per pod.",
}, []string{"pod"}),
```

`MustRegister` 인자에 `m.workersActive` 추가.

콜백 메서드:

```go
func (m *Metrics) SetWorkersActive(pod string, n int) {
	m.workersActive.WithLabelValues(pod).Set(float64(n))
}
```

Run: `go test ./internal/metrics/ -run TestSetWorkersActive -v`

Expected: PASS.

- [ ] **Step 3: Runner 측 failing test**

`internal/worker/runner_test.go` (기존 파일이라면 어설션 추가, 없으면 신규) — runner 가 시작/종료 시 콜백을 호출하는지 검증:

```go
func TestRunner_InvokesWorkerStartStopCallbacks(t *testing.T) {
	// Use a runner with Workers=2 and a Pool stub. Simplest: create a Runner
	// where SourceFor / TransportFor return ErrUnknownProtocol so loop never
	// processes a real job; LeaseJob requires a *pgxpool.Pool though, so use
	// a dummy pool that always returns "no rows". Simpler: assert via a unit
	// helper that exposes the goroutine count callback.
	//
	// Simpler still: refactor loop() so it calls onStart/onStop locally and
	// test that wrapper directly. (Plan implementer: choose whichever is
	// least invasive given the runner's current structure.)
	t.Skip("see runner.go: assertion strategy decided during impl")
}
```

> **Note for implementer:** `Runner.loop` 은 현재 worker 인덱스를 인자로 받는다. 가장 단순한 방법은 `Runner` 에 `OnWorkerStart func(pod string)` / `OnWorkerStop func(pod string)` 필드를 추가하고 `loop` 의 진입/종료에서 호출하되, 두 콜백을 wrapper 함수로 추출 (`emitWorkerStart`/`emitWorkerStop`) 해 nil 체크 + 단위 테스트가 가능하게 한다. 실제 goroutine 라이프사이클 어설션은 `Run` 에 Workers=1 짧은 backoff + Pool mock 으로 가능하지만, 이 plan 은 wrapper 단위 테스트로 충분히 검증한다.

- [ ] **Step 4: Runner 변경**

`internal/worker/runner.go` 의 `Runner` struct 에 다음 필드 추가:

```go
// OnWorkerStart / OnWorkerStop are invoked when a worker goroutine enters /
// leaves its loop. Both nil-safe. Used by metrics wiring.
OnWorkerStart func(pod string)
OnWorkerStop  func(pod string)
```

`loop` 함수의 시작과 끝에 다음을 추가:

```go
func (r *Runner) loop(ctx context.Context, idx int) {
	if r.OnWorkerStart != nil {
		r.OnWorkerStart(r.PodName)
	}
	defer func() {
		if r.OnWorkerStop != nil {
			r.OnWorkerStop(r.PodName)
		}
	}()
	// ... existing body ...
}
```

(`loop` 의 정확한 함수명은 현재 코드에 맞춰 적용; 이름이 다를 수 있다.)

- [ ] **Step 5: Runner 단위 테스트**

`internal/worker/runner_test.go` 의 `TestRunner_InvokesWorkerStartStopCallbacks` 를 다음으로 교체:

```go
func TestRunner_StartStopCallbacksAreInvokedAroundLoop(t *testing.T) {
	var startedCount, stoppedCount int32
	r := &worker.Runner{
		PodName: "test-pod",
		OnWorkerStart: func(pod string) {
			if pod != "test-pod" {
				t.Errorf("start pod = %q, want test-pod", pod)
			}
			atomic.AddInt32(&startedCount, 1)
		},
		OnWorkerStop: func(pod string) {
			atomic.AddInt32(&stoppedCount, 1)
		},
	}
	// Drive a single iteration of loop manually via the package-internal
	// helper. If runner.go does not expose loop() externally, use a
	// short-lived ctx + Pool mock through Run with Workers=1.
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // ctx cancelled before loop starts: enters and exits immediately.
	_ = r // implementer wires up the test driver here.

	if got := atomic.LoadInt32(&startedCount); got != 1 {
		t.Fatalf("started = %d, want 1", got)
	}
	if got := atomic.LoadInt32(&stoppedCount); got != 1 {
		t.Fatalf("stopped = %d, want 1", got)
	}
}
```

import 에 `"context"`, `"sync/atomic"` 추가.

> 실제 driver 는 runner 의 unexported helper 노출 정도에 따라 다르다. 가장 단순한 방법: `loop` 는 unexported 라 외부에서 못 부르므로 `Run` 호출 + `Workers=1` + `Pool=nil` 가 panic 을 일으킨다면, runner.go 안에 다음 helper 를 추가:
>
> ```go
> // emitStart/emitStop are exported only for tests; the loop body uses them.
> func (r *Runner) emitStart() { if r.OnWorkerStart != nil { r.OnWorkerStart(r.PodName) } }
> func (r *Runner) emitStop()  { if r.OnWorkerStop  != nil { r.OnWorkerStop(r.PodName) } }
> ```
>
> 그리고 `loop` 가 `r.emitStart()` / `defer r.emitStop()` 을 부른다. 테스트는 `Runner{...}.emitStart() / .emitStop()` 을 직접 호출해 콜백 집계만 어설션. 그러면 Pool mocking 없이 테스트가 단순해진다. 이 단순화 경로를 권장.

- [ ] **Step 6: 테스트 통과 확인**

Run: `go test ./internal/metrics/ ./internal/worker/ -v`

Expected: PASS.

- [ ] **Step 7: 커밋**

```bash
git add internal/metrics/metrics.go internal/metrics/metrics_test.go \
        internal/worker/runner.go internal/worker/runner_test.go
git commit -m "feat(worker,metrics): workers_active gauge + OnWorkerStart/Stop hooks"
```

---

## Task 4: scrape-time collectors — queueDepthCollector / dbPoolCollector / leaseLockAge

**Files:**
- Create: `internal/metrics/queue_depth.go`
- Create: `internal/metrics/db_pool.go`
- Create: `internal/metrics/lease_lock_age.go`
- Modify: `internal/metrics/metrics.go`
- Create: `internal/metrics/integration_test.go`

`prometheus.Collector` 인터페이스 직접 구현 — `Describe` / `Collect`. `Collect` 호출 시 SQL 1 회 (2 초 timeout). 실패 시 0 emit + warn 한 줄 + 다음 scrape 시 재시도.

- [ ] **Step 1: queueDepthCollector — failing 통합 테스트 먼저**

`internal/metrics/integration_test.go` 신규:

```go
//go:build integration

package metrics_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/nineking424/imgsync/internal/db"
	"github.com/nineking424/imgsync/internal/metrics"
)

func setupPG(t *testing.T) (*pgxpool.Pool, func()) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	pg, err := postgres.Run(ctx, "postgres:16-alpine",
		postgres.WithDatabase("imgsync"),
		postgres.WithUsername("test"),
		postgres.WithPassword("test"),
	)
	if err != nil {
		t.Fatalf("postgres run: %v", err)
	}
	dsn, err := pg.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("conn string: %v", err)
	}
	if err := db.ApplyMigrations(ctx, dsn, "../../migrations"); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	return pool, func() {
		pool.Close()
		_ = testcontainers.TerminateContainer(pg)
	}
}

func TestQueueDepthCollector_ReflectsPerStatusCount(t *testing.T) {
	pool, cleanup := setupPG(t)
	defer cleanup()

	ctx := context.Background()
	insert := func(status string) {
		t.Helper()
		_, err := pool.Exec(ctx, `INSERT INTO transfer_jobs
		   (trace_id, src, dst, src_protocol, dst_protocol, status)
		   VALUES ($1, 'src://x', 'dst://y', 'localfs', 'localfs', $2)`,
			status+"-trace-"+time.Now().Format("150405.000"), status)
		if err != nil {
			t.Fatalf("insert %s: %v", status, err)
		}
	}
	for i := 0; i < 3; i++ {
		insert("pending")
	}
	insert("succeeded")

	m := metrics.New()
	m.AttachQueueDepth(pool)

	want := strings.NewReader(`
# HELP imgsync_jobs_in_status Number of transfer_jobs rows per status.
# TYPE imgsync_jobs_in_status gauge
imgsync_jobs_in_status{status="pending"} 3
imgsync_jobs_in_status{status="succeeded"} 1
`)
	if err := testutil.GatherAndCompare(m.RegistryForTest(), want,
		"imgsync_jobs_in_status"); err != nil {
		t.Fatalf("gather: %v", err)
	}
}

func TestLeaseLockAge_IsZeroWhenNoLeasedRows(t *testing.T) {
	pool, cleanup := setupPG(t)
	defer cleanup()

	m := metrics.New()
	m.AttachLeaseLockAge(pool)

	mfs, err := m.RegistryForTest().Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	var found bool
	for _, mf := range mfs {
		if mf.GetName() == "imgsync_lease_lock_age_seconds" {
			found = true
			if got := mf.GetMetric()[0].GetGauge().GetValue(); got != 0 {
				t.Fatalf("lease_lock_age = %v, want 0 with no leased rows", got)
			}
		}
	}
	if !found {
		t.Fatalf("lease_lock_age_seconds not exposed")
	}
	_ = prometheus.NewRegistry // keep import live in case future tests need it
}

func TestDBPoolCollector_ExposesInUseIdleMax(t *testing.T) {
	pool, cleanup := setupPG(t)
	defer cleanup()

	m := metrics.New()
	m.AttachDBPool(pool)

	mfs, err := m.RegistryForTest().Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	states := map[string]bool{}
	for _, mf := range mfs {
		if mf.GetName() != "imgsync_db_pool_conns" {
			continue
		}
		for _, m := range mf.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == "state" {
					states[l.GetValue()] = true
				}
			}
		}
	}
	for _, s := range []string{"in_use", "idle", "max"} {
		if !states[s] {
			t.Fatalf("state %q missing from imgsync_db_pool_conns", s)
		}
	}
}
```

> **Note:** `RegistryForTest()` 는 패키지 외부에서 registry 를 Gather 할 수 있게 export 하는 헬퍼다. Step 3 에서 `internal/metrics/metrics.go` 에 추가한다. 별도 export 가 싫다면 unit test 와 통합 test 를 같은 패키지 (`metrics`) 에 두고 `m.reg` 직접 접근 — 이 plan 은 `_test.go` 패키지를 `metrics_test` (외부) 로 두므로 export 헬퍼가 필요.

- [ ] **Step 2: queueDepthCollector 구현**

`internal/metrics/queue_depth.go` 신규:

```go
package metrics

import (
	"context"
	"log"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
)

// queueDepthCollector emits imgsync_jobs_in_status{status} by running
// SELECT status, COUNT(*) FROM transfer_jobs GROUP BY status at scrape time.
// 2-second timeout per Collect. Failures emit 0 metrics + warn log; never
// panics, never blocks. Phase 1.5 adds an index that makes this an index-only
// scan.
type queueDepthCollector struct {
	pool *pgxpool.Pool
	desc *prometheus.Desc
}

func newQueueDepthCollector(pool *pgxpool.Pool) *queueDepthCollector {
	return &queueDepthCollector{
		pool: pool,
		desc: prometheus.NewDesc(
			"imgsync_jobs_in_status",
			"Number of transfer_jobs rows per status.",
			[]string{"status"}, nil,
		),
	}
}

func (c *queueDepthCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.desc
}

func (c *queueDepthCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	rows, err := c.pool.Query(ctx,
		`SELECT status::text, COUNT(*)::bigint FROM transfer_jobs GROUP BY status`)
	if err != nil {
		log.Printf("metrics: queue_depth scrape failed: %v", err)
		return
	}
	defer rows.Close()
	for rows.Next() {
		var status string
		var count int64
		if err := rows.Scan(&status, &count); err != nil {
			log.Printf("metrics: queue_depth scan failed: %v", err)
			return
		}
		ch <- prometheus.MustNewConstMetric(c.desc, prometheus.GaugeValue, float64(count), status)
	}
	if err := rows.Err(); err != nil {
		log.Printf("metrics: queue_depth iter failed: %v", err)
	}
}
```

- [ ] **Step 3: dbPoolCollector + leaseLockAge 구현**

`internal/metrics/db_pool.go` 신규:

```go
package metrics

import (
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
)

// dbPoolCollector wraps pgxpool.Pool.Stat() into imgsync_db_pool_conns{state}.
// Cost: zero — pgxpool keeps these counters in-process.
type dbPoolCollector struct {
	pool *pgxpool.Pool
	desc *prometheus.Desc
}

func newDBPoolCollector(pool *pgxpool.Pool) *dbPoolCollector {
	return &dbPoolCollector{
		pool: pool,
		desc: prometheus.NewDesc(
			"imgsync_db_pool_conns",
			"pgxpool connection counts by state.",
			[]string{"state"}, nil,
		),
	}
}

func (c *dbPoolCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.desc
}

func (c *dbPoolCollector) Collect(ch chan<- prometheus.Metric) {
	s := c.pool.Stat()
	ch <- prometheus.MustNewConstMetric(c.desc, prometheus.GaugeValue, float64(s.AcquiredConns()), "in_use")
	ch <- prometheus.MustNewConstMetric(c.desc, prometheus.GaugeValue, float64(s.IdleConns()), "idle")
	ch <- prometheus.MustNewConstMetric(c.desc, prometheus.GaugeValue, float64(s.MaxConns()), "max")
}
```

`internal/metrics/lease_lock_age.go` 신규:

```go
package metrics

import (
	"context"
	"database/sql"
	"log"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
)

// newLeaseLockAge returns a GaugeFunc that runs the lease-lock-age SQL each
// scrape. Cheap thanks to transfer_jobs_leased_idx (locked_at). Returns 0 if
// no rows are leased (NULL MIN). Errors emit a warn log + 0.
func newLeaseLockAge(pool *pgxpool.Pool) prometheus.Collector {
	return prometheus.NewGaugeFunc(
		prometheus.GaugeOpts{
			Name: "imgsync_lease_lock_age_seconds",
			Help: "Age in seconds of the oldest currently-leased job (0 if none).",
		},
		func() float64 {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			var ageSec sql.NullFloat64
			err := pool.QueryRow(ctx,
				`SELECT EXTRACT(EPOCH FROM NOW() - MIN(locked_at))::double precision
				   FROM transfer_jobs WHERE status='leased'`,
			).Scan(&ageSec)
			if err != nil {
				log.Printf("metrics: lease_lock_age scrape failed: %v", err)
				return 0
			}
			if !ageSec.Valid {
				return 0
			}
			return ageSec.Float64
		},
	)
}
```

- [ ] **Step 4: Metrics 에 Attach 메서드 + RegistryForTest 헬퍼**

`internal/metrics/metrics.go` 에 추가:

```go
// AttachQueueDepth registers the jobs_in_status scrape collector. Idempotent:
// safe to call once per pool. Panics on duplicate registration (caller bug).
func (m *Metrics) AttachQueueDepth(pool *pgxpool.Pool) {
	m.reg.MustRegister(newQueueDepthCollector(pool))
}

// AttachDBPool registers the db_pool_conns collector.
func (m *Metrics) AttachDBPool(pool *pgxpool.Pool) {
	m.reg.MustRegister(newDBPoolCollector(pool))
}

// AttachLeaseLockAge registers the lease_lock_age_seconds gauge.
func (m *Metrics) AttachLeaseLockAge(pool *pgxpool.Pool) {
	m.reg.MustRegister(newLeaseLockAge(pool))
}

// RegistryForTest exposes the underlying registry for assertions in tests
// from external packages. Not for production code.
func (m *Metrics) RegistryForTest() *prometheus.Registry { return m.reg }
```

import 에 `"github.com/jackc/pgx/v5/pgxpool"` 추가.

- [ ] **Step 5: 통합 테스트 통과 확인**

Run: `go test -tags integration -timeout 5m -v ./internal/metrics/`

Expected: PASS (postgres testcontainer 가 뜨고 3 개 통합 테스트 모두 PASS).

- [ ] **Step 6: Makefile 통합 타겟 보강 (선택)**

`Makefile` 의 `test-integration-sniffer` 옆에 `test-integration-metrics` 를 추가하면 CI 에서 분리 실행이 가능.

```makefile
.PHONY: test-integration-metrics
test-integration-metrics: ## Run metrics scrape collector integration tests
	go test -tags integration -timeout 5m -v ./internal/metrics/
```

`make test-integration` 가 두 타겟을 합치는 와이드 타겟이라면 거기에 합류.

- [ ] **Step 7: 커밋**

```bash
git add internal/metrics/queue_depth.go internal/metrics/db_pool.go \
        internal/metrics/lease_lock_age.go internal/metrics/metrics.go \
        internal/metrics/integration_test.go Makefile
git commit -m "feat(metrics): scrape-time queue_depth/db_pool/lease_lock_age collectors"
```

---

## Task 5: `ftp.PoolConfig.OnPoolChange` 콜백

**Files:**
- Modify: `internal/transports/ftp/pool.go`
- Test: `internal/transports/ftp/pool_test.go`

`PoolConfig` 에 `OnPoolChange func(host string, inUse, idle int)` 필드를 추가하고, `Acquire` (idle pop, dial succeed/fail 시) / `release` 모두에서 host 별 inUse / idle 을 호출.

- [ ] **Step 1: failing test**

`internal/transports/ftp/pool_test.go` 에 추가 (또는 신규):

```go
func TestPool_OnPoolChangeFiresOnAcquireAndRelease(t *testing.T) {
	type call struct {
		host  string
		inUse int
		idle  int
	}
	var (
		mu    sync.Mutex
		calls []call
	)
	cb := func(host string, inUse, idle int) {
		mu.Lock()
		calls = append(calls, call{host, inUse, idle})
		mu.Unlock()
	}

	srv := newTestFTPServer(t) // existing helper if present; otherwise stub
	defer srv.Close()

	p := pftp.NewPool(pftp.PoolConfig{
		MaxPerHost:   2,
		IdleTTL:      1 * time.Minute,
		NoopAfter:    1 * time.Minute,
		AuthUser:     "anonymous",
		AuthPassword: "",
		OnPoolChange: cb,
	})
	defer p.Close()

	c, err := p.Acquire(context.Background(), srv.Addr())
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	c.Release(false)

	mu.Lock()
	defer mu.Unlock()
	if len(calls) < 2 {
		t.Fatalf("expected ≥2 callback fires (acquire+release), got %d", len(calls))
	}
	last := calls[len(calls)-1]
	if last.idle != 1 || last.inUse != 0 {
		t.Fatalf("after release: in_use=%d idle=%d, want 0/1", last.inUse, last.idle)
	}
}
```

> 기존 ftp pool 테스트가 사용하는 in-process FTP 서버 헬퍼가 있다면 그대로 사용 (`internal/transports/ftp/pool_test.go` 옆 파일 확인). 없으면 이 테스트는 `t.Skip` 처리하고 단위 테스트는 다음 step 의 콜백 wrapping 헬퍼로 대체한다.

- [ ] **Step 2: PoolConfig + 호출 지점 변경**

`internal/transports/ftp/pool.go` 의 `PoolConfig` 에 추가:

```go
// OnPoolChange, if non-nil, is invoked under the pool mutex whenever a host's
// in_use or idle count changes. Keep callback work O(1) — it runs on the hot
// path of every Acquire/Release. nil-safe.
OnPoolChange func(host string, inUse, idle int)
```

`Pool` 내부에 helper:

```go
// emitChange must be called with p.mu held. It snapshots the host's counts
// and invokes the callback after releasing the lock so callbacks do not block
// other waiters.
func (p *Pool) emitChange(host string) {
	if p.cfg.OnPoolChange == nil {
		return
	}
	hp := p.hosts[host]
	if hp == nil {
		return
	}
	inUse := hp.inUse
	idle := len(hp.idle)
	// Defer the actual call to a goroutine? No — simplest: copy values, drop
	// the lock at the call site, then invoke. The current callers all hold
	// p.mu and can call p.cfg.OnPoolChange(host, inUse, idle) right after
	// p.mu.Unlock() if they snapshot first.
	_ = inUse
	_ = idle
}
```

> **단순화 권장:** `emitChange` 대신 `Acquire` / `release` 의 `p.mu.Unlock()` 직전 (또는 직후) 에 변수에 host 별 `inUse` / `idle` 을 캡처하고, mutex 해제 후 콜백 호출. 콜백이 panic 이나 blocking I/O 를 하면 mutex 가 잡혀 있지 않아도 되므로 안전.

`Acquire` 의 모든 변경 지점 (idle 에서 꺼냄 / 새 dial 성공 / 새 dial 실패 후 inUse-- / 대기 후 깨어나서 다시 시도) 와 `release` 끝에서 `OnPoolChange` 호출. 핵심 패턴:

```go
// release 의 끝부분 예
p.mu.Unlock()
if p.cfg.OnPoolChange != nil {
	p.cfg.OnPoolChange(host, inUseSnapshot, idleSnapshot)
}
return
```

> 정확한 코드 변경은 `pool.go` 의 현재 indent / lock holding 패턴에 맞춘다. 핵심: **inUse 또는 idle 이 변하는 모든 지점에서 콜백 한 번**. 누락된 경로가 없도록 grep 으로 `inUse`, `hp.idle` 쓰기 지점을 모두 확인.

Run: `grep -n "inUse\+\+\|inUse--\|hp.idle =\|hp.idle, " internal/transports/ftp/pool.go`

각 지점이 콜백 호출과 짝이 되도록 검토.

- [ ] **Step 3: 테스트 통과 확인**

Run: `go test ./internal/transports/ftp/ -race -count=1 -v`

Expected: 기존 테스트 + 신규 콜백 테스트 PASS.

- [ ] **Step 4: 커밋**

```bash
git add internal/transports/ftp/pool.go internal/transports/ftp/pool_test.go
git commit -m "feat(ftp): PoolConfig.OnPoolChange callback for metrics wiring"
```

---

## Task 6: `sniffer.Config.OnEnqueue` / `OnError` 콜백

**Files:**
- Modify: `internal/sniffer/sniffer.go`
- Modify: `internal/sniffer/sniffer_test.go`

- [ ] **Step 1: failing test**

`internal/sniffer/sniffer_test.go` 에 추가:

```go
func TestRunOnce_InvokesEnqueueCallbackWithRowCount(t *testing.T) {
	// 기존 RunOnce 테스트 fixture 를 참고. RunOnce 가 (n, nil) 반환하는 케이스
	// 에 OnEnqueue("source-id", n) 가 정확히 한 번 호출되는지 어설션.
	t.Skip("see sniffer.go: assertion uses existing RunOnce test scaffolding")
}

func TestRunOnce_InvokesErrorCallbackOnFailure(t *testing.T) {
	// RunOnce 가 (0, err) 반환 시 OnError("source-id") 가 한 번 호출되는지.
	t.Skip("see sniffer.go: ditto")
}
```

> 기존 `internal/sniffer/sniffer_test.go` 의 RunOnce 픽스처 (mock pool 또는 testcontainers) 를 그대로 차용해 콜백을 꽂는다. 본 plan 은 콜백 시점만 명세.

- [ ] **Step 2: Config 필드 + RunOnce 호출**

`internal/sniffer/sniffer.go` 의 `Config` 에 추가:

```go
// OnEnqueue, if non-nil, is invoked at the end of every successful RunOnce
// with the SourceID and the number of rows just enqueued (0 is allowed).
OnEnqueue func(source string, n int)

// OnError, if non-nil, is invoked when RunOnce returns a non-nil error.
OnError func(source string)
```

`RunOnce` 끝부분:

```go
func (s *Sniffer) RunOnce(ctx context.Context) (int, error) {
	n, err := s.runOnceImpl(ctx) // 기존 본문을 헬퍼로 추출하거나 인라인
	if err != nil {
		if s.cfg.OnError != nil {
			s.cfg.OnError(s.cfg.SourceID)
		}
		return n, err
	}
	if s.cfg.OnEnqueue != nil {
		s.cfg.OnEnqueue(s.cfg.SourceID, n)
	}
	return n, nil
}
```

> 기존 본문이 한 함수 안에 있으므로 `runOnceImpl` 추출이 깔끔. 또는 단순히 모든 `return 0, fmt.Errorf(...)` 직전에 콜백 호출 — 권장은 추출.

- [ ] **Step 3: 테스트 통과 확인**

Run: `go test ./internal/sniffer/ -race -count=1 -v`

Expected: PASS.

- [ ] **Step 4: 커밋**

```bash
git add internal/sniffer/sniffer.go internal/sniffer/sniffer_test.go
git commit -m "feat(sniffer): Config.OnEnqueue/OnError callbacks"
```

---

## Task 7: `worker.Job.Duration()` 메서드

**Files:**
- Modify: `internal/worker/job.go`
- Modify: `internal/worker/job_test.go` (없으면 신규)

`Job.LockedAt *time.Time` 가 이미 채워지므로 (`LeaseJob` RETURNING) `Duration()` 는 단순한 메서드 한 줄.

- [ ] **Step 1: failing test**

`internal/worker/job_test.go` (있으면 추가, 없으면 신규):

```go
package worker

import (
	"testing"
	"time"
)

func TestJob_Duration_NilLockedAt(t *testing.T) {
	j := &Job{}
	if got := j.Duration(); got != 0 {
		t.Fatalf("Duration with nil LockedAt = %v, want 0", got)
	}
}

func TestJob_Duration_TimeSinceLockedAt(t *testing.T) {
	now := time.Now().Add(-3 * time.Second)
	j := &Job{LockedAt: &now}
	got := j.Duration()
	if got < 2900*time.Millisecond || got > 3500*time.Millisecond {
		t.Fatalf("Duration = %v, want ~3s", got)
	}
}
```

Run: `go test ./internal/worker/ -run TestJob_Duration -v`

Expected: FAIL (메서드 미존재).

- [ ] **Step 2: 메서드 추가**

`internal/worker/job.go` 끝부분에:

```go
// Duration returns how long has elapsed since the job was leased. Returns 0
// when the job has no LockedAt (e.g. constructed directly in tests). The
// value is the worker's view of "lease → now"; for the in-DB lease age use
// imgsync_lease_lock_age_seconds.
func (j *Job) Duration() time.Duration {
	if j.LockedAt == nil {
		return 0
	}
	return time.Since(*j.LockedAt)
}
```

- [ ] **Step 3: 테스트 통과 확인**

Run: `go test ./internal/worker/ -v`

Expected: PASS.

- [ ] **Step 4: 커밋**

```bash
git add internal/worker/job.go internal/worker/job_test.go
git commit -m "feat(worker): Job.Duration() — lease→now elapsed"
```

---

## Task 8: `internal/health` Option 패턴 + `WithMetrics`

**Files:**
- Modify: `internal/health/server.go`
- Modify: `internal/health/server_test.go` (없으면 신규)

`NewServer` 시그니처를 `NewServer(pool *pgxpool.Pool, st *Status, opts ...Option) *Server` 로 확장. 옵션이 없으면 기존 동작과 100% 동일. `WithMetrics(http.Handler)` 옵션이 있으면 `/metrics` 가 mux 에 마운트된다.

- [ ] **Step 1: failing test**

`internal/health/server_test.go` (있으면 추가):

```go
func TestNewServer_WithoutMetricsOption_404OnMetricsPath(t *testing.T) {
	pool := mustTestPool(t) // 기존 헬퍼; 없으면 dummy *pgxpool.Pool 패턴 사용
	st := health.NewStatus()
	srv := health.NewServer(pool, st)

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	srv.MuxForTest().ServeHTTP(w, r)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}

func TestNewServer_WithMetricsOption_ServesProvidedHandler(t *testing.T) {
	pool := mustTestPool(t)
	st := health.NewStatus()
	called := false
	hh := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("# HELP test ok\n"))
	})
	srv := health.NewServer(pool, st, health.WithMetrics(hh))

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	srv.MuxForTest().ServeHTTP(w, r)

	if !called {
		t.Fatalf("metrics handler not invoked")
	}
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
}
```

> `MuxForTest()` 는 server.go 가 자체 mux 를 보유하므로 외부에서 ServeHTTP 직접 부르기 위한 export 헬퍼. 기존 server.go 에서 mux 가 함수 안 로컬 변수라면 struct 필드로 끌어올린 뒤 export 헬퍼 추가.

- [ ] **Step 2: server.go 변경**

`internal/health/server.go` 변경:

```go
type Option func(*Server)

func WithMetrics(h http.Handler) Option {
	return func(s *Server) {
		if h == nil {
			return
		}
		s.metrics = h
	}
}

type Server struct {
	pool    *pgxpool.Pool
	status  *Status
	mux     *http.ServeMux
	metrics http.Handler // nil if not configured
	hs      *http.Server
}

func NewServer(pool *pgxpool.Pool, st *Status, opts ...Option) *Server {
	mux := http.NewServeMux()
	s := &Server{pool: pool, status: st, mux: mux}
	for _, opt := range opts {
		opt(s)
	}
	mux.HandleFunc("/livez", s.livez)
	mux.HandleFunc("/readyz", s.readyz)
	mux.HandleFunc("/healthz", s.healthz)
	if s.metrics != nil {
		mux.Handle("/metrics", s.metrics)
	}
	s.hs = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	return s
}

// MuxForTest exposes the internal mux for unit tests that need to drive
// requests directly without binding a TCP socket.
func (s *Server) MuxForTest() http.Handler { return s.mux }
```

기존 `Serve` / `Close` 는 변경 없음.

- [ ] **Step 3: 테스트 통과 확인**

Run: `go test ./internal/health/ -race -count=1 -v`

Expected: PASS.

- [ ] **Step 4: 커밋**

```bash
git add internal/health/server.go internal/health/server_test.go
git commit -m "feat(health): NewServer Option pattern + WithMetrics(/metrics) handler"
```

---

## Task 9: `cmd/imgsync/worker.go` wiring

**Files:**
- Modify: `cmd/imgsync/worker.go`

기존 wiring 에 metrics 통합. 변경 지점만 명시.

- [ ] **Step 1: import 추가**

`cmd/imgsync/worker.go` 의 import 블록에:

```go
"github.com/nineking424/imgsync/internal/metrics"
```

- [ ] **Step 2: pool 생성 후, ftpPool 생성 전에 metrics + ftp pool callback 와이어링**

```go
m := metrics.New()
m.AttachQueueDepth(pool)
m.AttachDBPool(pool)
m.AttachLeaseLockAge(pool)
```

ftpPool 생성 시 `OnPoolChange: m.OnFTPPoolChange` 추가:

```go
ftpPool := pftp.NewPool(pftp.PoolConfig{
	MaxPerHost:   envInt("IMGSYNC_FTP_MAX_PER_HOST", 4),
	IdleTTL:      time.Duration(envInt("IMGSYNC_FTP_IDLE_TTL_SEC", 300)) * time.Second,
	NoopAfter:    time.Duration(envInt("IMGSYNC_FTP_NOOP_AFTER_SEC", 60)) * time.Second,
	AuthUser:     os.Getenv("IMGSYNC_FTP_USER"),
	AuthPassword: os.Getenv("IMGSYNC_FTP_PASSWORD"),
	OnPoolChange: m.OnFTPPoolChange,
})
```

- [ ] **Step 3: Runner 콜백 chain wiring**

기존 한 줄 (`r.OnLeaseAttempt = status.OnLeaseAttempt`) 을 chain 으로:

```go
// Compose: status (existing) + metrics (new). Domain calls a single callback;
// chaining keeps the runner ignorant of multiple consumers.
r.OnLeaseAttempt = func(success bool) {
	status.OnLeaseAttempt(success)
	m.OnLeaseAttempt(success, nil) // worker has no err in this signature; lease errors are logged separately.
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
```

함수 본문 위에 `var workersGauge int32` 선언. import 에 `"sync/atomic"` 추가.

> **참고:** `OnLeaseAttempt` 의 두 번째 인자는 spec 에서 (success, error) 시그니처를 갖지만 도메인의 콜백은 `func(bool)` 단일이라 worker 측에서 lease error 를 따로 분류하지 못한다. **이번 plan 은 lease 의 ErrNoRows 가 success=false → "empty", 그 외 driver 오류는 worker.go 가 별도 path 에서 log + retry 하므로 metric 으로는 잡히지 않음.** 이 한계는 monitoring.md 에 명시한다 (Task 13).

- [ ] **Step 4: sweeper 콜백 chain**

```go
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
```

- [ ] **Step 5: health server 에 metrics handler 마운트**

기존:
```go
hs := health.NewServer(pool, status)
```
변경:
```go
hs := health.NewServer(pool, status, health.WithMetrics(m.Handler()))
```

- [ ] **Step 6: 빌드 + 테스트**

Run: `go build ./cmd/imgsync && go test ./...`

Expected: PASS.

- [ ] **Step 7: 수동 스모크 테스트**

Run (별도 터미널, postgres 띄운 상태):
```bash
IMGSYNC_DSN="postgres://..." IMGSYNC_HEALTH_ADDR=":18080" \
  ./bin/imgsync worker &
WPID=$!
sleep 1
curl -s http://localhost:18080/metrics | grep -E "^imgsync_" | head -20
kill $WPID
```

Expected: `imgsync_lease_attempts_total`, `imgsync_jobs_in_status`, `imgsync_db_pool_conns`, `imgsync_lease_lock_age_seconds`, `imgsync_workers_active`, `imgsync_sweep_cycles_total` 메트릭이 노출됨.

- [ ] **Step 8: 커밋**

```bash
git add cmd/imgsync/worker.go
git commit -m "feat(cmd/worker): wire metrics — runner/sweeper/ftp callbacks + /metrics"
```

---

## Task 10: `internal/cli/sniffer.go` HTTP listener + metrics wiring

현재 sniffer 는 health endpoint 도 K8s probe 도 없다. worker 와 동일한 패턴 (`health.NewServer` + `:8080` listen + `health.WithMetrics`) 을 적용한다.

**Files:**
- Modify: `internal/cli/sniffer.go`
- Test: `internal/cli/sniffer_test.go` (필요 시)

- [ ] **Step 1: failing test (선택)**

기존 `RunSniffer` 통합 테스트가 있으면 거기 어설션을 추가한다. 없으면 이 step 은 skip — wire 자체는 build 와 e2e 로 검증한다.

- [ ] **Step 2: imports + wiring**

`internal/cli/sniffer.go` 의 import 에:

```go
"net"

"github.com/nineking424/imgsync/internal/health"
"github.com/nineking424/imgsync/internal/metrics"
```

`RunSniffer` 의 imgPool / sniffer.New 다음, ticker 시작 전에:

```go
m := metrics.New()
m.AttachQueueDepth(imgPool)
m.AttachDBPool(imgPool)
// lease lock age 는 worker 의 책임. sniffer 에서는 노출하지 않음.

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
```

`sniffer.New(...)` 호출 시 `Config` 에 추가:

```go
OnEnqueue: m.OnSnifferEnqueue,
OnError:   m.OnSnifferError,
```

- [ ] **Step 3: 빌드 확인**

Run: `go build ./cmd/imgsync ./internal/cli/...`

Expected: 빌드 성공.

- [ ] **Step 4: 수동 스모크 테스트 (e2e 환경 없으면 skip — Task 14 에서 검증)**

```bash
SNIFFER_SOURCE_ID=test SNIFFER_SOURCE_DSN=... SNIFFER_IMGSYNC_DSN=... \
SNIFFER_HEALTH_ADDR=":18081" \
SNIFFER_TABLE=... SNIFFER_PK_COLUMN=... SNIFFER_TS_COLUMN=... \
SNIFFER_DST_PATTERN=... SNIFFER_SRC_PATTERN=... \
SNIFFER_SRC_PROTOCOL=ftp SNIFFER_DST_PROTOCOL=localfs \
./bin/imgsync sniffer &
sleep 1
curl -s http://localhost:18081/livez
curl -s http://localhost:18081/metrics | grep -E "^imgsync_sniffer" | head
kill %1
```

Expected: `/livez` 200, `/metrics` 에 `imgsync_sniffer_*` 가 노출.

- [ ] **Step 5: 커밋**

```bash
git add internal/cli/sniffer.go
git commit -m "feat(cli/sniffer): add /livez/readyz/metrics HTTP listener"
```

---

## Task 11: Helm — service selector / sniffer service / sniffer probes / values

**Files:**
- Modify: `deploy/helm/imgsync/values.yaml`
- Modify: `deploy/helm/imgsync/templates/deployment.yaml`
- Modify: `deploy/helm/imgsync/templates/service.yaml`
- Modify: `deploy/helm/imgsync/templates/sniffer-deployment.yaml`
- Create: `deploy/helm/imgsync/templates/sniffer-service.yaml`
- Modify: `deploy/helm/imgsync/tests/template_test.sh`

- [ ] **Step 1: 기존 어설션 (failing 추가)**

`deploy/helm/imgsync/tests/template_test.sh` 의 끝에 추가 (Test 7 다음):

```bash
# ─── Test 8: worker Service selector includes component=worker ──────
echo "==> worker service selector"
awk '/^# Source: imgsync\/templates\/service\.yaml/{p=1} p; p && /^---$/{exit}' \
  "$TMP/t1.yaml" > "$TMP/t1-svc.yaml"
[ -s "$TMP/t1-svc.yaml" ] || { echo "FAIL: no Service rendered"; exit 1; }
grep -q "component: worker" "$TMP/t1-svc.yaml" || \
  { echo "FAIL: worker Service selector missing component: worker"; exit 1; }
grep -q "name: http-metrics" "$TMP/t1-svc.yaml" || \
  { echo "FAIL: worker Service port name http-metrics missing"; exit 1; }

# ─── Test 9: sniffer Service exists when sniffer.enabled=true ───────
echo "==> sniffer service"
helm template t-sniff "$CHART" --set sniffer.enabled=true > "$TMP/t-sniff.yaml"
grep -q "name: .*-sniffer$\|name: imgsync-sniffer" "$TMP/t-sniff.yaml" || \
  { echo "FAIL: sniffer Service missing"; exit 1; }
awk '/^# Source: imgsync\/templates\/sniffer-service\.yaml/{p=1} p; p && /^---$/{exit}' \
  "$TMP/t-sniff.yaml" > "$TMP/t-sniff-svc.yaml"
[ -s "$TMP/t-sniff-svc.yaml" ] || { echo "FAIL: no sniffer Service rendered"; exit 1; }
grep -q "component: sniffer" "$TMP/t-sniff-svc.yaml" || \
  { echo "FAIL: sniffer Service selector missing component: sniffer"; exit 1; }

# ─── Test 10: sniffer-deployment has probes + http port ─────────────
echo "==> sniffer probes"
awk '/^# Source: imgsync\/templates\/sniffer-deployment\.yaml/{p=1} p; p && /^---$/{exit}' \
  "$TMP/t-sniff.yaml" > "$TMP/t-sniff-deploy.yaml"
grep -q "containerPort: 8080" "$TMP/t-sniff-deploy.yaml" || \
  { echo "FAIL: sniffer port 8080 missing"; exit 1; }
grep -q "livenessProbe" "$TMP/t-sniff-deploy.yaml" || \
  { echo "FAIL: sniffer livenessProbe missing"; exit 1; }
grep -q "readinessProbe" "$TMP/t-sniff-deploy.yaml" || \
  { echo "FAIL: sniffer readinessProbe missing"; exit 1; }
```

Run: `make helm-test`

Expected: Test 8/9/10 모두 FAIL (대상 변경 미적용).

- [ ] **Step 2: values.yaml 추가**

`deploy/helm/imgsync/values.yaml` 끝부분에:

```yaml
monitoring:
  serviceMonitor:
    enabled: false
    interval: 30s
    scrapeTimeout: 10s
    labels: {}
    namespace: ""
  podAnnotations: {}

logging:
  format: text
```

- [ ] **Step 3: deployment.yaml — component label 명시**

worker `templates/deployment.yaml` 의 `metadata.labels` / `spec.selector.matchLabels` / `spec.template.metadata.labels` 모두에 `app.kubernetes.io/component: worker` 추가. 헬퍼 (`_helpers.tpl`) 에 `imgsync.workerSelectorLabels` 가 없다면 그대로 인라인.

- [ ] **Step 4: service.yaml — selector + port name**

`templates/service.yaml` 변경:

- selector 에 `app.kubernetes.io/component: worker` 추가
- ports 의 `name: http` → `name: http-metrics` (port 8080 그대로). 외부 컨벤션 (예: ServiceMonitor) 이 `http-metrics` 를 기대하므로.

- [ ] **Step 5: sniffer-service.yaml 신규**

`templates/sniffer-service.yaml` 신규:

```yaml
{{- if .Values.sniffer.enabled -}}
apiVersion: v1
kind: Service
metadata:
  name: {{ include "imgsync.fullname" . }}-sniffer
  labels:
    {{- include "imgsync.labels" . | nindent 4 }}
    app.kubernetes.io/component: sniffer
spec:
  type: ClusterIP
  selector:
    {{- include "imgsync.selectorLabels" . | nindent 4 }}
    app.kubernetes.io/component: sniffer
  ports:
    - name: http-metrics
      port: 8080
      targetPort: http
      protocol: TCP
{{- end }}
```

- [ ] **Step 6: sniffer-deployment.yaml — port + probes + env**

`templates/sniffer-deployment.yaml` 의 spec 에 추가:

- `metadata.labels` 에 `app.kubernetes.io/component: sniffer` 추가 (deployment + pod template 둘 다)
- container `ports`:
  ```yaml
  ports:
    - name: http
      containerPort: 8080
      protocol: TCP
  ```
- container `env` 에 추가:
  ```yaml
  - name: SNIFFER_HEALTH_ADDR
    value: ":8080"
  ```
- container 에 probes 추가:
  ```yaml
  livenessProbe:
    httpGet: { path: /livez, port: http }
    periodSeconds: 10
    failureThreshold: 3
  readinessProbe:
    httpGet: { path: /readyz, port: http }
    periodSeconds: 5
    failureThreshold: 3
  startupProbe:
    httpGet: { path: /readyz, port: http }
    periodSeconds: 5
    failureThreshold: 30
  ```

- [ ] **Step 7: helm template 어설션 통과**

Run: `make helm-test`

Expected: Test 8/9/10 PASS, 기존 Test 1~7 도 여전히 PASS.

- [ ] **Step 8: 커밋**

```bash
git add deploy/helm/imgsync/values.yaml \
        deploy/helm/imgsync/templates/deployment.yaml \
        deploy/helm/imgsync/templates/service.yaml \
        deploy/helm/imgsync/templates/sniffer-deployment.yaml \
        deploy/helm/imgsync/templates/sniffer-service.yaml \
        deploy/helm/imgsync/tests/template_test.sh
git commit -m "feat(helm): worker selector + sniffer service/probes/port + values"
```

---

## Task 12: Helm — `servicemonitor.yaml` + 어설션

**Files:**
- Create: `deploy/helm/imgsync/templates/servicemonitor.yaml`
- Modify: `deploy/helm/imgsync/tests/template_test.sh`

- [ ] **Step 1: failing 어설션 추가**

`tests/template_test.sh` 끝에:

```bash
# ─── Test 11: ServiceMonitor disabled by default ────────────────────
echo "==> ServiceMonitor default off"
if grep -q "kind: ServiceMonitor" "$TMP/t1.yaml"; then
  echo "FAIL: ServiceMonitor rendered with default values (must be opt-in)"
  exit 1
fi

# ─── Test 12: ServiceMonitor enabled produces both endpoints ────────
echo "==> ServiceMonitor enabled"
helm template t-sm "$CHART" \
  --set monitoring.serviceMonitor.enabled=true \
  --set sniffer.enabled=true \
  --api-versions monitoring.coreos.com/v1 > "$TMP/t-sm.yaml"
grep -q "kind: ServiceMonitor" "$TMP/t-sm.yaml" || \
  { echo "FAIL: ServiceMonitor not rendered when enabled=true"; exit 1; }
# Endpoints: one for the worker service, one for the sniffer service
grep -c "port: http-metrics" "$TMP/t-sm.yaml" | grep -q "^[2-9]" || \
  { echo "FAIL: ServiceMonitor missing endpoints (need ≥2 http-metrics refs)"; exit 1; }
```

Run: `make helm-test`

Expected: Test 11 PASS (default off), Test 12 FAIL (servicemonitor 파일 없음).

- [ ] **Step 2: servicemonitor.yaml 신규**

`templates/servicemonitor.yaml` 신규:

```yaml
{{- if and .Values.monitoring.serviceMonitor.enabled (.Capabilities.APIVersions.Has "monitoring.coreos.com/v1") -}}
apiVersion: monitoring.coreos.com/v1
kind: ServiceMonitor
metadata:
  name: {{ include "imgsync.fullname" . }}
  {{- with .Values.monitoring.serviceMonitor.namespace }}
  namespace: {{ . }}
  {{- end }}
  labels:
    {{- include "imgsync.labels" . | nindent 4 }}
    {{- with .Values.monitoring.serviceMonitor.labels }}
    {{- toYaml . | nindent 4 }}
    {{- end }}
spec:
  selector:
    matchExpressions:
      - key: app.kubernetes.io/name
        operator: In
        values: [{{ include "imgsync.name" . }}]
      - key: app.kubernetes.io/component
        operator: In
        values: [worker, sniffer]
  endpoints:
    - port: http-metrics
      path: /metrics
      interval: {{ .Values.monitoring.serviceMonitor.interval | default "30s" }}
      scrapeTimeout: {{ .Values.monitoring.serviceMonitor.scrapeTimeout | default "10s" }}
{{- end }}
```

> selector 가 두 service (worker, sniffer) 를 모두 잡도록 `matchExpressions` 사용. 단일 endpoint 가 두 service 의 같은 port name 을 scrape.

- [ ] **Step 3: 어설션 통과**

Run: `make helm-test`

Expected: Test 11/12 PASS.

- [ ] **Step 4: 커밋**

```bash
git add deploy/helm/imgsync/templates/servicemonitor.yaml \
        deploy/helm/imgsync/tests/template_test.sh
git commit -m "feat(helm): ServiceMonitor template + opt-in toggle"
```

---

## Task 13: Grafana 대시보드 + 문서

**Files:**
- Create: `deploy/helm/imgsync/dashboards/imgsync-overview.json`
- Create: `docs/operating/dashboards.md`
- Modify: `docs/operating/monitoring.md`
- Modify: `docs/operating/runbook.md`

- [ ] **Step 1: 대시보드 JSON 작성**

`deploy/helm/imgsync/dashboards/imgsync-overview.json` 신규. 4 분면 (큐 / 워커 / 처리량 / 실패) 패널을 가진 단순 대시보드. Panel 구성:

| Row | Panel | PromQL |
|---|---|---|
| Queue | Pending jobs | `imgsync_jobs_in_status{status="pending"}` |
| Queue | Leased jobs | `imgsync_jobs_in_status{status="leased"}` |
| Queue | Lease lock age | `imgsync_lease_lock_age_seconds` |
| Workers | Active workers per pod | `sum by (pod) (imgsync_workers_active)` |
| Workers | DB pool in_use vs max | `imgsync_db_pool_conns{state="in_use"}` / `imgsync_db_pool_conns{state="max"}` |
| Workers | FTP pool in_use by host | `sum by (host) (imgsync_ftp_pool_size{state="in_use"})` |
| Throughput | Lease attempts rate | `sum by (result) (rate(imgsync_lease_attempts_total[5m]))` |
| Throughput | Jobs processed rate | `sum by (result) (rate(imgsync_jobs_processed_total[5m]))` |
| Throughput | p95 job duration | `histogram_quantile(0.95, sum by (le, src, dst) (rate(imgsync_job_duration_seconds_bucket[5m])))` |
| Failure | Sniffer error rate | `sum by (source) (rate(imgsync_sniffer_run_errors_total[5m]))` |
| Failure | Sweep cycles | `rate(imgsync_sweep_cycles_total[5m])` |

JSON 골격은 Grafana export 형식 — `dashboard.title="imgsync overview"`, `dashboard.uid="imgsync-overview-v1"`, `dashboard.tags=["imgsync","monitoring"]`, `dashboard.schemaVersion=39`. 패널 좌표는 8x8 그리드, datasource 는 placeholder `${DS_PROMETHEUS}` template var.

> **Note:** 1000+ 줄 JSON 을 plan 안에 직접 박지 않는다. 구현자는 Grafana UI 에서 위 PromQL 로 패널을 만들고 `Dashboard settings → JSON Model → Save to file` 로 export. 또는 빈 JSON 골격 (`{ "title": "imgsync overview", ... "panels": [] }`) 을 넣고 PromQL 만 docs 에 명시 — 운영자가 직접 클릭해 채우는 방식.

이 plan 은 **빈 골격 JSON + PromQL 명세** 를 채택. 정밀한 panel layout 은 `docs/operating/dashboards.md` 의 import 가이드와 함께 문서화한다.

`deploy/helm/imgsync/dashboards/imgsync-overview.json` 최소 골격:

```json
{
  "annotations": { "list": [] },
  "editable": true,
  "graphTooltip": 0,
  "panels": [],
  "schemaVersion": 39,
  "tags": ["imgsync", "monitoring"],
  "templating": {
    "list": [
      {
        "current": {},
        "hide": 0,
        "includeAll": false,
        "multi": false,
        "name": "DS_PROMETHEUS",
        "options": [],
        "query": "prometheus",
        "refresh": 1,
        "regex": "",
        "skipUrlSync": false,
        "type": "datasource"
      }
    ]
  },
  "time": { "from": "now-6h", "to": "now" },
  "timezone": "browser",
  "title": "imgsync overview",
  "uid": "imgsync-overview-v1",
  "version": 1
}
```

- [ ] **Step 2: dashboards.md 신규**

`docs/operating/dashboards.md` 신규 — Grafana import 절차 (UI 경로 + datasource 매핑) + Phase 1.5 ConfigMap 옵션 예고. 명세에 위 PromQL 표를 그대로 붙인다 (panel 마다 PromQL 한 줄).

- [ ] **Step 3: monitoring.md 정식화**

`docs/operating/monitoring.md` 의 "Prometheus 메트릭은 노출하지 않는다 / 향후 계획" 단락을 다음으로 교체:

- 메트릭 카탈로그 표 (spec §4 의 11 종)
- `transfer_events.status ↔ metric` 매핑 표
- 권장 알람 (예: `imgsync_jobs_in_status{status="pending"} > N`, `rate(imgsync_jobs_processed_total{result="fail"}[5m]) > X`, `imgsync_lease_lock_age_seconds > 600`)
- `OnLeaseAttempt` driver-error 한계 명시 (Task 9 Step 3 의 note 참조)

- [ ] **Step 4: runbook.md 보완**

`docs/operating/runbook.md` §7 (SQL 폴링 가이드) 옆에 다음 표 추가:

| 증상 | 메트릭 query | 대응 |
|---|---|---|
| 큐 적체 | `imgsync_jobs_in_status{status="pending"}` 가 SLO 초과 | worker 스케일 / sniffer 폭주 확인 |
| Stuck lease | `imgsync_lease_lock_age_seconds > sweeperThreshold` | sweeper 동작 확인 (`imgsync_sweep_cycles_total` rate) |
| 실패 폭증 | `rate(imgsync_jobs_processed_total{result="fail"}[5m])` 급증 | `transfer_events` SQL 로 detail 조사 |

- [ ] **Step 5: 커밋**

```bash
git add deploy/helm/imgsync/dashboards/imgsync-overview.json \
        docs/operating/dashboards.md \
        docs/operating/monitoring.md \
        docs/operating/runbook.md
git commit -m "docs(monitoring): metrics catalog + alarms + Grafana import guide"
```

---

## Task 14: end-to-end 검증 — kind cluster scrape

**Files:** none (운영 절차)

spec §9 의 검증 시퀀스를 실행. 자동 어설션을 만들지는 않고 PR 본문에 결과 첨부.

- [ ] **Step 1: 로컬 CI 풀 패스**

Run:
```bash
make ci
make helm-test
make test-integration-sniffer
make test-integration-metrics   # Task 4 Step 6 에서 추가했다면
```

Expected: 전부 PASS.

- [ ] **Step 2: kind 클러스터 부팅**

Run: `./scripts/e2e-up.sh`

(이 스크립트가 기본 helm install 까지 한다면) `kubectl get pods -n imgsync` 로 worker / sniffer pod 가 Running 임을 확인.

- [ ] **Step 3: /metrics scrape 확인**

```bash
kubectl port-forward -n imgsync svc/imgsync 18080:8080 &
PFPID=$!
sleep 2
curl -s http://localhost:18080/metrics | grep -E "^imgsync_" | sort -u | head -30
kill $PFPID
```

Expected: 11 종 메트릭 (`imgsync_jobs_in_status`, `imgsync_lease_attempts_total`, `imgsync_jobs_processed_total`, `imgsync_job_duration_seconds_bucket`, `imgsync_sweep_cycles_total`, `imgsync_lease_lock_age_seconds`, `imgsync_db_pool_conns`, `imgsync_ftp_pool_size`, `imgsync_workers_active`, `imgsync_sniffer_enqueue_total`, `imgsync_sniffer_run_errors_total`) 가 모두 노출.

`imgsync-sniffer` service 도 마찬가지로 port-forward 후 `/metrics` 노출 확인.

- [ ] **Step 4: ServiceMonitor 가 Prometheus Operator 가 있는 클러스터에서 잡히는지 (선택)**

운영 클러스터에 직접 배포 전, dev / staging Prom Operator 환경에서:

```bash
helm install imgsync ... --set monitoring.serviceMonitor.enabled=true ...
kubectl get servicemonitor -n imgsync
```

Prometheus Targets 페이지에서 `imgsync` 가 healthy 상태인지 확인.

- [ ] **Step 5: e2e 셧다운**

Run: `./scripts/e2e-down.sh`

- [ ] **Step 6: PR 생성**

```bash
git push -u origin <feature-branch>
gh pr create --title "feat: phase 1 monitoring stack integration (metrics + grafana)" --body "$(cat <<'EOF'
## Summary
- 신규 `internal/metrics` 패키지 (prometheus/client_golang) — 11 종 메트릭
- worker / sniffer 모두 `:8080/metrics` 노출 (sniffer 는 health endpoint 자체가 신설)
- Helm: ServiceMonitor (opt-in), sniffer service / probes, worker service component selector 정정
- Grafana 대시보드 JSON 골격 + import 가이드
- `docs/operating/monitoring.md` 정식화

## Test plan
- [ ] `make ci`
- [ ] `make helm-test`
- [ ] `make test-integration-sniffer`
- [ ] `make test-integration-metrics`
- [ ] kind e2e: `curl /metrics` shows 11 series (worker + sniffer)
- [ ] (optional) staging Prom Operator: ServiceMonitor target healthy

## Follow-up
- [ ] phase 1.5 PR (`docs/superpowers/plans/2026-05-05-monitoring-phase-1-5-status-index.md`) — 동일 sprint 머지 필수

🤖 Generated with [Claude Code](https://claude.com/claude-code)
EOF
)"
```

---

## Notes for the implementer

- **콜백 chain 패턴:** worker.go 와 sniffer cli 양쪽에서 `OnLeaseAttempt`, `OnSweepCycle` 등이 status 와 metrics 두 컨슈머에게 동시에 전달되어야 한다. 무명 함수로 묶는 단순 wrapper 패턴 (Task 9 Step 3) 을 일관되게 사용. Slice 기반 dispatcher 같은 추상화는 만들지 않는다 (YAGNI).
- **글로벌 registry 금지:** `prometheus.MustRegister` (글로벌) 는 절대 호출하지 않는다. 항상 `m.reg.MustRegister`. 테스트 격리를 깨면 race / panic 이 난다.
- **`/metrics` 인증:** 사내 클러스터의 Prometheus 가 같은 namespace 안에서 scrape 한다는 가정하에 별도 인증을 두지 않는다. 외부 노출이 필요하면 NetworkPolicy + Ingress auth 로 처리 (이번 plan 범위 밖).
- **로그와 metric 의 관계:** Phase 1 은 metric 만이다. 기존 평문 로그 키워드 ("lease loop started" 등) 는 그대로 두고, Phase 2 에서 slog 마이그레이션을 하면서 한꺼번에 정리한다.
- **`OnLeaseAttempt` 의 error 라벨:** 현재 worker 의 콜백 시그니처가 `func(success bool)` 이라서 driver error 를 metric "error" 라벨로 분류하지 못한다. 이 한계를 monitoring.md 에 명시하고, Phase 2 에서 시그니처를 `func(success bool, err error)` 로 확장하는 작업을 백로그.
- **Helm component label 도입의 부작용:** 이번 plan 에서 `app.kubernetes.io/component` 를 처음 도입한다. 기존 deployment 의 `selectorLabels` 에 매치되던 sniffer pod 가 이제 worker Service 에서 빠진다 — 이는 의도된 정정. 머지 후 첫 rollout 에서 worker Service 의 endpoints 가 잠깐 흔들릴 수 있으므로 release 노트에 명시 (deployer 가 `kubectl get endpoints` 한 번 확인).
- **ServiceMonitor 로 두 service 한 번에:** `selector.matchExpressions` 가 worker / sniffer 두 component 를 같이 잡도록 했다. 두 service 의 port name 이 똑같이 `http-metrics` 라야 단일 endpoint 정의로 충분. Task 11 Step 4 / Step 5 가 이를 보장.
- **Phase 1.5 와의 머지 순서:** Phase 1 PR 이 먼저 머지되어도 `jobs_in_status` 가 풀 scan 으로 떨어지면 2 초 timeout 으로 0 emit 한다 — 망가지진 않지만 데이터 누락. 운영팀이 Phase 1 머지 즉시 Phase 1.5 머지를 트리거할 수 있도록 두 PR 에 라벨 / cross-link 를 단다.
