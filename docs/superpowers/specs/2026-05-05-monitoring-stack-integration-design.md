# imgsync 표준 관측 스택 통합 — Design Spec

**Status:** Brainstorming complete, awaiting user review before plan write-out
**Date:** 2026-05-05
**Branch:** docs/site-2026-05-05 (작업 시 새 브랜치 분기)
**Repo:** nineking424/imgsync
**Author:** nineking (with Claude collaboration via `/superpowers:brainstorming`)

**Related artifacts:**
- imgsync v1 design doc (rev 4 APPROVED): `~/.gstack/projects/nineking424-imgsync/nineking-main-design-20260427-031601.md`
- Shadow sniffer spec: `docs/superpowers/specs/2026-04-27-imgsync-shadow-sniffer-design.md`
- 운영 모니터링 가이드: `docs/operating/monitoring.md` (현재 "Prometheus 메트릭은 노출하지 않는다 / 향후 계획" 으로 명시)
- 런북: `docs/operating/runbook.md`

---

## Context

imgsync v1 의 운영 가시성은 현재 세 채널뿐이다:

1. `/healthz` JSON (lease/sweep timestamp + pgxpool stat)
2. 표준출력 평문 로그 ("lease loop started", "sniffer enqueued N new jobs" 등)
3. 운영자가 직접 치는 SQL (`transfer_events` 폴링, 런북 §7)

이 조합으로는 "지금 몇 개 작업이 대기 중인가 / 워커가 정상 동작하는가 / 처리량과 실패율이 어떻게 변하는가" 같은 1차 운영 질문에 즉답하기 어렵다. 알람도 SQL 기반이라 외부 모니터링 시스템과 분리된다.

이 spec 은 imgsync 가 표준 관측 스택 — **Prometheus / Grafana / OpenSearch / OpenTelemetry** — 와 통합되어 메트릭·로그·트레이스 세 신호를 모두 흘리도록 한다. 자체 웹 대시보드를 만들지 않는다 (운영 부담 + 표준 도구 중복).

원하는 결과: Grafana 대시보드 한 페이지에서 **큐 / 워커 / 처리량 / 실패** 의 4 분면이 보이고, 알람은 Prometheus / Alertmanager 가 발화하며, OpenSearch 에서 `trace_id` 로 로그·트레이스를 가로지를 수 있다.

---

## Section 1: Architecture

**Decision:** Hybrid 라우팅 — 각 신호가 백엔드의 native 경로를 탄다.

```
┌─ imgsync (cobra subcmds) ──────────────────────────────────────────────┐
│                                                                        │
│  ┌─ worker pod ─────────────┐    ┌─ sniffer pod ──────────────┐        │
│  │  internal/metrics  ─────►│    │  internal/metrics  ───────►│        │
│  │  internal/worker         │    │  internal/sniffer          │        │
│  │  internal/sweeper        │    │                            │        │
│  │  internal/transports/ftp │    │                            │        │
│  │                          │    │  log: stdout JSON          │        │
│  │  log: stdout JSON        │    │  trace: OTLP 출력           │        │
│  │  trace: OTLP 출력         │    │                            │        │
│  │                          │    │  http :8080                │        │
│  │  http :8080              │    │   /livez /readyz           │  ◄── Phase 1 신설
│  │   /livez /readyz /healthz│    │   /metrics                 │        │
│  │   /metrics  ◄── Phase 1  │    │                            │        │
│  └──────────────────────────┘    └────────────────────────────┘        │
└────────────────────────────────────────────────────────────────────────┘
       │                  │                 │
       │ scrape           │ stdout pipe     │ OTLP gRPC
       │ (Phase 1)        │ (Phase 2)       │ (Phase 3)
       ▼                  ▼                 ▼
┌───────────────┐  ┌─────────────┐   ┌─────────────────┐
│  Prometheus   │  │ Fluent Bit  │   │ OTel Collector  │
│ (ServiceMon.) │  │ (DaemonSet) │   │ (Deployment)    │
└──────┬────────┘  └──────┬──────┘   └────────┬────────┘
       │                  │                   │
       │                  ▼                   ▼
       │           ┌────────────────────────────────┐
       │           │      OpenSearch                │
       │           │  · logs index                  │
       │           │  · traces index (Data Prepper) │
       │           └──────────────┬─────────────────┘
       │                          │
       └──────────────┬───────────┘
                      ▼
              ┌───────────────┐
              │    Grafana    │
              │  Prom + OS DS │
              └───────────────┘
```

**왜 Hybrid 인가** (대안 OTel-first 기각 사유):

- Prometheus pull `/metrics` 는 OTel SDK → Collector → Prometheus 보다 단순하고, K8s 에서 ServiceMonitor 로 이미 검증된 경로다.
- 로그는 Fluent Bit DaemonSet 이 stdout 을 알아서 OpenSearch 로 보낸다 — imgsync 코드 변경 없음.
- OTel 의 가장 큰 가치는 **트레이싱**이며, `transfer_jobs.trace_id` 가 이미 1 등 시민이라 자연스럽게 매핑된다.
- OTel Collector 가 단일 control plane 이 되면 SPOF 위험이 늘고, 신호 하나 망가지면 전체가 흔들린다. 신호별 분리가 더 견고하다.

**Sniffer 의 HTTP listener 신설**: 현재 sniffer pod 는 health 포트가 없어 K8s probe 도 받지 못한다. 이번에 worker 와 동일한 패턴으로 `:8080/livez /readyz /metrics` 를 띄우고, 동시에 readiness/liveness probe 를 helm 에 추가한다.

---

## Section 2: Phase 분할

### Phase 1 — Metrics + Grafana (이번 plan 의 주 대상)

- `/metrics` HTTP 엔드포인트 (worker + sniffer)
- `internal/metrics` 신규 패키지
- 도메인 패키지에 콜백 hook 연결 (worker / sweeper / sniffer / ftp pool)
- Helm: ServiceMonitor + sniffer service · probes
- Grafana 대시보드 JSON 1 장
- `docs/operating/monitoring.md` 정식화

### Phase 1.5 — 인덱스 추가 (필수)

`jobs_in_status` 메트릭의 scrape SQL 이 succeeded/skipped 누적 행에 대해 GROUP BY 를 돌면서 무거워질 수 있다. **Phase 1 머지와 동일 sprint 안에 머지되어야 한다.** Phase 1 의 plan 과 Phase 1.5 의 plan 을 같은 시점에 작성한다.

- `migrations/0003_jobs_status_index.up.sql` — `transfer_jobs.status` 단일 b-tree index
- 적용 후 GROUP BY 가 index-only scan 으로 떨어진다
- 별도 PR 로 분리 — 마이그레이션 단독 리뷰 / 롤백 단순화

### Phase 2 — Logs (별도 spec/plan)

- `slog` (Go 1.21+, 현재 1.25 사용 중) 기반 stdout JSON 전환
- `IMGSYNC_LOG_FORMAT=json|text` 토글 (기본 text 유지, K8s 배포는 json)
- 기존 키워드 라인 ("lease loop started", "lease acquired" 등) 모두 structured field 화
- 테스트 어설션 영향 점검 (현재 일부 테스트가 로그 라인을 보는지 확인 후 마이그레이션)
- Fluent Bit 설정 예시는 imgsync repo 가 아닌 운영 클러스터 매니페스트로 (`docs/operating/logs.md` 가이드만 추가)

### Phase 3 — Traces (별도 spec/plan)

- `internal/tracing` 신규 패키지
- OTel SDK + OTLP gRPC exporter
- Span 체계: `sniffer.enqueue` → `worker.lease` → `process` (`Source.Open` → `Transport.Send`)
- `transfer_jobs.trace_id` ↔ W3C trace context 매핑 — `trace_id` 칼럼이 32 자 hex 면 그대로 OTel TraceID 로 해석. 현재 `internal/sniffer/traceid.go` 의 생성 규약 확인 후 정렬 (필요 시 phase 3 spec 에서 transitional 변환 정의)
- OTel Collector 배포는 별도 docs

이 spec 은 phase 2 / 3 의 architecture 자리만 박는다. plan 작성 시점은 phase 1 + 1.5 머지 이후.

---

## Section 3: Components (Phase 1)

### 의존성

- 신규: `github.com/prometheus/client_golang` (메트릭 primitives + `promhttp.Handler`)
- 변경 없음: 기존 모든 의존성 그대로
- OTel SDK 는 phase 3 에서만 추가

### `internal/metrics` 패키지

```go
package metrics

// Metrics 는 모든 imgsync_* 컬렉터를 보유. 프로세스 당 1 인스턴스.
// 메서드 값 (m.OnLeaseAttempt 등) 을 도메인 패키지의 기존 콜백에 그대로 전달.
type Metrics struct {
    reg *prometheus.Registry

    leaseAttempts *prometheus.CounterVec   // result="success|empty|error"
    jobsProcessed *prometheus.CounterVec   // result="success|skip|fail|expire|dead"
    jobDuration   *prometheus.HistogramVec // labels: src, dst, result
    sweepCycles   prometheus.Counter
    workersActive *prometheus.GaugeVec     // labels: pod
    leaseLockAge  prometheus.GaugeFunc     // scrape-time SQL
    jobsInStatus  *queueDepthCollector     // scrape-time SQL (Collector iface)
    ftpPoolSize   *prometheus.GaugeVec     // labels: host, state
    snifferEnq    *prometheus.CounterVec   // labels: source
    snifferErr    *prometheus.CounterVec   // labels: source
    dbPoolConns   *dbPoolCollector         // wraps pgxpool.Stat()
}

func New() *Metrics
func (m *Metrics) Handler() http.Handler

// 콜백 메서드 (도메인 패키지의 OnXxx 에 그대로 바인딩)
func (m *Metrics) OnLeaseAttempt(success bool)
func (m *Metrics) OnSweepCycle()
func (m *Metrics) OnJobFinished(src, dst, result string, dur time.Duration)
func (m *Metrics) OnSnifferEnqueue(source string, n int)
func (m *Metrics) OnSnifferError(source string)
func (m *Metrics) OnFTPPoolChange(host string, inUse, idle int)
func (m *Metrics) SetWorkersActive(pod string, n int)

// Attach 류 — scrape-time collector 를 registry 에 묶음
func (m *Metrics) AttachQueueDepth(pool *pgxpool.Pool)
func (m *Metrics) AttachDBPool(pool *pgxpool.Pool)
func (m *Metrics) AttachFTPPool(p *ftp.Pool)
```

**설계 원칙:**

- 글로벌 registry 안 씀. 테스트는 매번 새 `Metrics{}` 를 만들어 격리.
- 도메인 패키지 (`internal/worker`, `internal/sweeper`, `internal/sniffer`, `internal/transports/ftp`) 는 `internal/metrics` 를 **import 하지 않는다**. 기존 `OnLeaseAttempt func(bool)` / `OnCycle func()` / `OnFinish func(*Job)` 콜백 패턴을 그대로 재사용 → 의존성 단방향 유지.
- Scrape-time vs push 구분 — 큐 깊이 / DB pool / FTP pool / lease lock age 는 `prometheus.Collector` 구현체로 scrape 시 산출. 카운터 / 히스토그램은 in-process push.

### `internal/health` 일반화

```go
// WithMetrics 옵션이 있으면 같은 mux 의 /metrics 에 마운트.
// 옵션이 없으면 /metrics 는 노출되지 않는다.
func NewServer(pool *pgxpool.Pool, st *Status, opts ...Option) *Server
func WithMetrics(h http.Handler) Option
```

worker / sniffer 모두 같은 health.Server 를 사용한다. sniffer 는 `Status` 를 더 작은 변종으로 사용 (현재 worker 의 `Status` 는 lease/sweep timestamp 를 보유하지만 sniffer 는 sweep 이 없음 — sniffer 용 변종 또는 nil 허용 결정은 plan 단계에서).

### Wiring (cmd/imgsync/worker.go 변경 부분)

```go
m := metrics.New()
m.AttachQueueDepth(pool)
m.AttachDBPool(pool)
m.AttachFTPPool(ftpPool)

r.OnLeaseAttempt = m.OnLeaseAttempt
r.OnFinish = func(j *worker.Job) {
    m.OnJobFinished(j.SrcProtocol, j.DstProtocol, j.Status, j.Duration())
}

sw := sweeper.Config{ Threshold: 5*time.Minute, Interval: 30*time.Second,
                       OnCycle: m.OnSweepCycle }

hs := health.NewServer(pool, status, health.WithMetrics(m.Handler()))
```

`internal/cli/sniffer.go` 의 `RunSniffer` 도 같은 패턴 — sniffer 인스턴스 생성 후 health server 를 띄우고 `m.OnSnifferEnqueue` / `m.OnSnifferError` 를 sniffer 의 신규 콜백에 연결.

---

## Section 4: Data flow

### 메트릭 카탈로그 (Phase 1, 11 종)

| 메트릭 | 모드 | 산출 | 라벨 (카디널리티) |
|---|---|---|---|
| `imgsync_jobs_in_status` | scrape | `SELECT status, COUNT(*) FROM transfer_jobs GROUP BY status` (2 초 timeout) | `status` (5) |
| `imgsync_lease_attempts_total` | push | `worker.Runner.OnLeaseAttempt` | `result` (success/empty/error) |
| `imgsync_jobs_processed_total` | push | `worker.OnFinish` | `result` (success/skip/fail/expire/dead) |
| `imgsync_job_duration_seconds` | push | `worker.OnFinish` (histogram, lease→Send 완료. buckets: `[0.1, 0.5, 1, 2, 5, 10, 30, 60, 300, 1800]` 초 — file transfer 도메인) | `src`, `dst`, `result` (~45) |
| `imgsync_sweep_cycles_total` | push | `sweeper.Config.OnCycle` | — |
| `imgsync_lease_lock_age_seconds` | scrape | `SELECT EXTRACT(EPOCH FROM NOW()-MIN(locked_at)) WHERE status='leased'` | — |
| `imgsync_db_pool_conns` | scrape | `pgxpool.Stat()` | `state` (in_use/idle/max) |
| `imgsync_ftp_pool_size` | push | `ftp.Pool` checkout/release | `host`, `state` (in_use/idle) |
| `imgsync_workers_active` | push | `Runner` goroutine start/stop | `pod` |
| `imgsync_sniffer_enqueue_total` | push | `Sniffer.RunOnce` 결과 n 만큼 Add | `source` |
| `imgsync_sniffer_run_errors_total` | push | `Sniffer.RunOnce` err 발생 시 Inc | `source` |

최악 가정 (워커 20 pod · FTP 호스트 30 · sniffer 소스 10) 으로도 약 250 시리즈. Prometheus 부하 무시 가능.

### `transfer_events.status` ↔ metric 매핑 (truth source)

| `transfer_events.status` | 대응 metric & label |
|---|---|
| `enqueue` | `imgsync_sniffer_enqueue_total{source}` |
| `lease` | `imgsync_lease_attempts_total{result=success}` + `imgsync_jobs_in_status{status=leased}` 변동 |
| `success` | `imgsync_jobs_processed_total{result=success}` + `imgsync_job_duration_seconds` observe |
| `skip` | `imgsync_jobs_processed_total{result=skip}` |
| `fail` | `imgsync_jobs_processed_total{result=fail}` |
| `expire` | `imgsync_jobs_processed_total{result=expire}` (sweeper 회수 시) |
| `dead` | `imgsync_jobs_processed_total{result=dead}` |

이 매핑은 phase 3 OTel span attributes 의 vocabulary 가 된다.

### 라벨 카디널리티 가드

- `src` / `dst` 라벨은 protocol 만 (`ftp`, `localfs`). host:port / 경로 절대 안 씀.
- `host` 라벨이 들어가는 곳은 `imgsync_ftp_pool_size` 한 곳뿐 — FTP 호스트 수에 비례 (운영 환경 수십 단위 예상).
- `pod` 는 deployment scale 에 비례 — K8s 가 자체 한계.
- `source` 는 sniffer SourceID 수 (운영팀이 구성, few).

### Scrape 비용

- **`jobs_in_status`** — succeeded/skipped 누적 행이 수백만이 되면 GROUP BY 가 무거워질 수 있다. **Phase 1.5 의 status 인덱스가 이 비용을 index-only scan 으로 떨어뜨린다.**
- **`lease_lock_age_seconds`** — 기존 `transfer_jobs_leased_idx (locked_at)` 활용, MIN 은 첫 행만 → cheap.
- 나머지 scrape 컬렉터는 `pool.Stat()` 또는 in-process counter 라 비용 0.

---

## Section 5: 오류 처리

| 시나리오 | 처리 |
|---|---|
| scrape SQL 실패 (timeout / DB 오류) | collector 가 0 metric emit + warn 한 줄 로그. `/metrics` 응답은 partial 200 (Prometheus 컨벤션). panic 안 함 |
| registry duplicate registration | `Metrics.New()` 가 cmd wiring 에서 한 번만 호출되도록 보장. 테스트는 매번 fresh. duplicate 시 panic = bug, 가드 안 함 |
| sniffer health/metrics bind 실패 | sniffer 부팅 자체 실패 (worker 의 기존 정책과 동일). silent fallback 없음 |
| nil callback | 도메인 패키지의 기존 패턴 그대로 — `if r.OnLeaseAttempt != nil { ... }`. metrics 와이어링 안 한 환경 (테스트 등) 에서도 동작 |
| label 값 unknown | `unknown` 으로 폴백. silently drop 하지 않는다 |

---

## Section 6: 테스트 전략 (Phase 1)

| 레벨 | 무엇을 검증하나 | 위치 |
|---|---|---|
| Unit | `Metrics` 콜백 호출 → `testutil.ToFloat64` 로 카운터/게이지 값. `testutil.CollectAndCompare` 로 노출 형식 (1 ~ 2 anchor metric) | `internal/metrics/*_test.go` |
| Domain hook | fake `OnLeaseAttempt` / `OnFinish` / `OnCycle` 를 꽂아 호출 횟수 · 인자 어설션 | 각 도메인 `*_test.go` |
| Scrape collector | testcontainers-go postgres + 마이그레이션 + fixture insert 후 `Collect()` 결과. `-tags integration` | `internal/metrics/integration_test.go` (신규) |
| Health server option | `WithMetrics` 설정 / 미설정 → `/metrics` 200 / 404 | `internal/health/server_test.go` |
| Helm template | `scripts/template_test.sh` 에 ServiceMonitor 토글, sniffer service 존재, sniffer probe 정의, port name `http-metrics` 어설션 | `deploy/helm/imgsync/tests/` |
| 스트리밍 가드 | `promhttp` 자체가 `io.Copy` 기반. `scripts/check-streaming.sh` 통과 확인 | CI |

eval 패키지 (audit invariants, fixture suite 등) 의 contract 추가는 Phase 2 이후로 미룬다.

---

## Section 7: Helm 변경 (Phase 1)

### `values.yaml` 추가

```yaml
monitoring:
  serviceMonitor:
    enabled: false              # Prom Operator 가 있는 클러스터에서만 true
    interval: 30s
    scrapeTimeout: 10s
    labels: {}                  # Prometheus 가 select 하는 라벨
    namespace: ""               # 비우면 Release ns
  podAnnotations: {}            # annotation-based scrape 호환

logging:
  format: text                  # phase 2 prep. K8s 배포는 json 권장
```

### 신규 / 변경 템플릿

| 파일 | 변경 |
|---|---|
| `templates/servicemonitor.yaml` | 신규. `{{- if and .Values.monitoring.serviceMonitor.enabled (.Capabilities.APIVersions.Has "monitoring.coreos.com/v1") }}` 게이트. worker · sniffer 두 endpoint |
| `templates/sniffer-service.yaml` | 신규. `component=sniffer` selector, port `http` :8080 |
| `templates/sniffer-deployment.yaml` | `ports: [{name: http, containerPort: 8080}]` 추가. `livenessProbe(/livez)`, `readinessProbe(/readyz)`, `startupProbe(/readyz)` 추가. `SNIFFER_HEALTH_ADDR` env (default `:8080`) |
| `templates/service.yaml` | selector 에 `component: worker` 추가 (현재는 sniffer pod 도 매치될 위험). port name `http-metrics` 추가 |
| `templates/deployment.yaml` | label `component: worker` 명시. port name 정렬 |
| `dashboards/imgsync-overview.json` | 신규. helm 패키지에 둠. ConfigMap 자동 import 옵션은 phase 1.5 |

### Helm template 어설션 (`tests/template_test.sh`)

- `monitoring.serviceMonitor.enabled=true` 시 ServiceMonitor 생성, `false` 시 미생성
- `sniffer.enabled=true` 시 `imgsync-sniffer` Service 생성
- sniffer-deployment 에 `livenessProbe`/`readinessProbe` 존재
- worker Service selector 에 `component: worker` 존재

---

## Section 8: 문서 업데이트

| 파일 | 변경 |
|---|---|
| `docs/operating/monitoring.md` | "Prometheus 메트릭은 노출하지 않는다" 단락을 정식 카탈로그로 교체. metric 표 + 권장 알람 + Grafana import 가이드 |
| `docs/operating/runbook.md` | SQL 폴링 가이드 옆에 metric 기반 알람 표 추가 (대체가 아니라 보완) |
| `docs/operating/dashboards.md` | 신규. Grafana JSON import 절차 + ConfigMap 옵션 (phase 1.5) |

---

## Section 9: 검증 (end-to-end)

Phase 1 머지 후 다음 절차로 동작 검증:

1. `make ci` — lint + streaming check + unit
2. `make test-integration-sniffer` — `-tags integration` (testcontainers postgres)
3. `make helm-test` — template 어설션 (ServiceMonitor / sniffer service / probes)
4. kind 클러스터 (`scripts/e2e-up.sh`) 에 helm install → `kubectl port-forward svc/imgsync :8080` → `curl :8080/metrics` 로 메트릭 노출 확인
5. (선택) Prom Operator 설치된 클러스터에서 ServiceMonitor 가 잡혀 Prometheus target healthy

Phase 1.5 머지 후:

6. `EXPLAIN ANALYZE SELECT status, COUNT(*) FROM transfer_jobs GROUP BY status` 가 index-only scan 임을 확인

---

## 진행 절차

1. 이 spec 의 사용자 리뷰
2. 승인 후 `superpowers:writing-plans` 로 Phase 1 구현 plan 작성 → `docs/superpowers/plans/2026-05-05-monitoring-phase-1.md`
3. Phase 1.5 plan 동시에 작성 → `docs/superpowers/plans/2026-05-05-monitoring-phase-1-5-status-index.md`
4. 두 plan 의 PR 들이 같은 sprint 안에 머지

Phase 2 / 3 의 plan 은 phase 1 + 1.5 머지 후 별도로 시작.
