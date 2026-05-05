# 아키텍처 심화

상위 다이어그램 / 컴포넌트 책임은 [개념 — 아키텍처](../concepts/architecture.md) 에 있다. 이 페이지는 코드를 처음 읽을 때 어디부터 들어가야 하는지, 어떤 인터페이스가 외부 확장 포인트인지, 동시성 제어가 어디서 일어나는지를 정리한다.

## 패키지 맵

| 경로 | 역할 |
|---|---|
| `cmd/imgsync` | 단일 바이너리. 서브커맨드(`worker`, `sniffer`, `migrate`, `enqueue`) 를 디스패치한다. |
| `internal/worker` | 잡 lease + 1건 처리 루프. `process.go` 가 단일 잡 라이프사이클의 진실 소스. |
| `internal/transfer` | `Source` / `Transport` 인터페이스. 모든 프로토콜 어댑터의 외부 경계. |
| `internal/sources` | `Source` 구현체 (`localfs`, `ftp`, …). |
| `internal/transports` | `Transport` 구현체. 특히 `transports/ftp/pool.go` 에 호스트별 커넥션 풀이 있다. |
| `internal/sniffer` | source DB 의 `updated_at` 윈도우를 폴링해서 잡을 enqueue 한다. |
| `internal/sweeper` | leased 인 채로 멎은 잡을 회수해 `pending` 으로 되돌린다. advisory lock 으로 cycle 마다 한 pod 만 회수 작업을 수행. |
| `internal/db` | pgx 연결 풀, 마이그레이션 러너, schema 상수. |
| `internal/health` | `/healthz` / `/readyz` 핸들러와 헬스 시그널. |
| `internal/hostcap` | FTP 같은 외부 호스트의 동시 접속 상한을 advisory lock 으로 강제한다. |
| `internal/backoff` | 지수 backoff + jitter. retry 정책의 단일 출처. |
| `internal/eval` | sniffer 의 윈도우 평가 / bias 보정 등 공유 평가 로직. |

## 인터페이스 경계

새 프로토콜을 추가하는 유일한 외부 확장 포인트는 [`internal/transfer/interfaces.go`](https://github.com/nineking424/imgsync/blob/main/internal/transfer/interfaces.go) 의 두 인터페이스다.

```go
type Source interface {
    Open(ctx context.Context, src string) (body io.ReadCloser, size int64, err error)
}

type Transport interface {
    Send(
        ctx context.Context,
        dst string,
        body io.Reader,
        expectedSize int64,
    ) (writtenBytes int64, sha256Hex string, err error)
}
```

핵심 계약:

- **버퍼링 금지.** 두 인터페이스 모두 본문(body) 을 메모리에 쌓으면 안 된다. 이는 `scripts/check-streaming.sh` 가 CI 에서 강제한다.
- **`size = -1`** 은 "소스가 길이를 알리지 않음" 을 의미한다. 워커는 D6 사이즈 검증을 사용 가능할 때만 켠다.
- **sha256 은 Transport 가 계산.** 흘려보낸 바이트의 해시여야 하고, 외부에서 받은 값이 아니어야 한다.

`Source.Open` 이 `transfer.ErrSkippable` 을 돌리면 잡은 dead 가 아니라 skip 으로 종료된다. LocalFS 는 `os.IsNotExist` 를 이 오류로 매핑해 두었다 (Week 2A 의 C3 회귀 방지).

## 동시성 모델

PostgreSQL advisory lock 두 곳이 전체 동시성의 척추다.

### Sweeper — 단일 활성 인스턴스

`internal/sweeper/sweeper.go` 는 `pg_try_advisory_xact_lock(hashtext("imgsync_sweeper"))` 를 매 사이클 시작 시 잡는다.

- 트랜잭션-스코프 락이라 sweeper 가 죽거나 commit 하면 자동 해제된다.
- 동일 키이므로 여러 파드가 동시에 sweeper 를 돌려도 한 번에 하나만 회수 작업을 진행한다 — leader election 을 별도로 두지 않는다.
- 락을 못 잡으면 그 사이클은 no-op 으로 빠진다. 다음 tick 에 재시도.

### FTP host cap — 호스트별 슬롯

`internal/hostcap/hostcap.go` 는 호스트당 N개 동시 슬롯을 advisory lock 으로 강제한다.

```go
func slotKey(host string, slot int) string {
    return fmt.Sprintf("ftp_host_%s_%d", host, slot)
}
```

- 키는 호스트 + 슬롯 번호의 조합. 슬롯 0 .. N-1 을 순회하며 첫 성공한 슬롯을 점유한다.
- `pg_try_advisory_lock` (세션 스코프, 별도 pgx 커넥션에 핀 고정) 으로 비동기 시도 — 실패하면 `false` 를 반환하고 다음 슬롯을 시도한다. 잡힌 lock 은 `Send` 가 끝날 때까지 유지되고 `defer pg_advisory_unlock` 으로 풀어준다.
- 결과적으로 같은 FTP 호스트에 대한 동시 업로드 수가 클러스터 전역에서 N 으로 묶인다. 워커 파드 수와 무관.

## 스트리밍 가드

`scripts/check-streaming.sh` 는 `internal/sources` / `internal/transports` / `internal/transfer` 세 디렉토리의 비-테스트 `.go` 파일을 검사해서 두 패턴을 금지한다.

```text
\b(io|ioutil)\.ReadAll\b
bytes\.NewBuffer\b.*\bbody\b
```

- 주석(`//` 으로 시작하는 라인)은 false-positive 회피용으로 빼준다.
- `_test.go` 는 제외 — 테스트 픽스처 빌더는 메모리에 들고 있어도 되기 때문.
- 알려진 갭: 변수명이 `body` 가 아니면 (`payload`, `buf`) 두 번째 패턴이 못 잡는다. 코드 리뷰가 마지막 보루.

이 가드는 D-class 회귀(메모리 OOM) 를 사전에 차단하기 위한 정적 검사다. 깨진다고 곧장 런타임 버그는 아니지만, 깨졌다는 사실 자체가 인터페이스 계약 위반이다.

## 코드를 처음 읽을 때

다음 순서로 읽으면 잡 한 건이 어떻게 흘러가는지가 빠르게 잡힌다.

1. [`cmd/imgsync/worker.go`](https://github.com/nineking424/imgsync/blob/main/cmd/imgsync/worker.go) — 워커 부트스트랩. config / DB / runner 가 어떻게 조립되는지.
2. [`internal/worker/process.go`](https://github.com/nineking424/imgsync/blob/main/internal/worker/process.go) — 잡 1건의 라이프사이클 (lease → open source → send → mark done/dead).
3. [`internal/transfer/interfaces.go`](https://github.com/nineking424/imgsync/blob/main/internal/transfer/interfaces.go) — 외부 확장 포인트.
4. [`internal/transports/ftp/pool.go`](https://github.com/nineking424/imgsync/blob/main/internal/transports/ftp/pool.go) — 실전 Transport 구현. 커넥션 풀, host cap, 스트리밍 송신이 어떻게 함께 도는지.

이 네 파일을 읽고 나면, 나머지 패키지는 "어디서 호출되는지" 를 grep 으로 따라가는 것만으로도 의도가 잡힌다.

## `internal/metrics` 패키지

Phase 1 모니터링에서 추가된 패키지로, **워커/스니퍼 코드가 Prometheus 라이브러리에 직접 의존하지 않도록** 콜백 어댑터를 모아둔 layer 다.

### 파일 구조

| 파일 | 책임 |
|---|---|
| `metrics.go` | `imgsync_jobs_processed_total`, `imgsync_lease_attempts_total`, `imgsync_workers_active`, `imgsync_ftp_pool_size`, `imgsync_sweep_cycles_total`, `imgsync_sniffer_*` counter/gauge 정의 + 외부 노출용 콜백 (`OnFinish`, `OnLeaseAttempt`, `OnWorkerStart/Stop`, `OnPoolChange`, `OnCycle`, `OnEnqueue`, `OnError`) |
| `buckets.go` | `imgsync_job_duration_seconds` histogram bucket 상수 (`[0.1, 0.5, 1, 2, 5, 10, 30, 60, 300, 1800]` 초) |
| `db_pool.go` | `pgxpool.Stat()` 을 scrape 시점에 읽어 `imgsync_db_pool_conns{state}` 로 변환 |
| `lease_lock_age.go` | `SELECT EXTRACT(EPOCH FROM NOW()-MIN(locked_at))` 을 scrape 시점에 실행 (2초 timeout) |
| `queue_depth.go` | `SELECT status, COUNT(*) FROM transfer_jobs GROUP BY status` 을 scrape 시점에 실행 |

### Push vs scrape 두 패턴

| 패턴 | 트리거 | 라이브러리 | 적용 메트릭 |
|---|---|---|---|
| **Push (in-process)** | 작업 / 콜백 발생 시점 | `prometheus.CounterVec`, `HistogramVec`, `GaugeVec` | `imgsync_jobs_processed_total`, `imgsync_lease_attempts_total`, `imgsync_workers_active`, `imgsync_ftp_pool_size`, `imgsync_sweep_cycles_total`, `imgsync_sniffer_*`, `imgsync_job_duration_seconds` |
| **Scrape-time** | `/metrics` GET 요청 시 | `prometheus.Collector` 인터페이스 직접 구현 | `imgsync_jobs_in_status`, `imgsync_db_pool_conns`, `imgsync_lease_lock_age_seconds` |

scrape-time 메트릭은 매 GET 마다 DB 쿼리를 한 번 더 던지므로 `interval` 이 너무 짧으면 control DB 가 영향을 받는다. 기본 `30s` (ServiceMonitor 기본값) 이 안전선이다.

### 새 메트릭을 추가할 때

1. `metrics.go` (push) 또는 새 collector 파일 (scrape-time) 에 정의를 추가한다.
2. 라벨 카디널리티가 폭발하는지 사전 점검한다 — `src`, `dst`, `result` 처럼 enum 성격 필드만 라벨에 둔다. `trace_id` / `path` 등 unbounded 필드는 절대 라벨에 넣지 않는다.
3. `metrics_test.go` 에 노출 형식 단위 테스트를, scrape 형이라면 `integration_test.go` 에 testcontainer 기반 통합 테스트를 추가한다.
4. emit 지점 (worker / sniffer / sweeper / FTP pool) 에서 콜백을 받아 호출한다. **Prometheus import 가 emit 지점 코드로 새지 않도록 주의** — 항상 `internal/metrics` 가 단일 진입점이어야 한다.
5. [모니터링 — 메트릭 카탈로그](../operating/monitoring.md#메트릭-카탈로그) 표와 [대시보드 — 패널 명세](../operating/dashboards.md#패널-명세) 표를 같이 갱신한다.

### Health 서버 wiring

`internal/health.NewServer` 는 functional option 패턴으로 바뀌었으며, `WithMetrics(handler http.Handler)` 옵션이 `/metrics` 를 같은 포트에 mount 한다. CLI(`cmd/imgsync/worker`, `cmd/imgsync/sniffer`) 는 `metrics.HTTPHandler()` 를 받아 이 옵션에 연결한다 — 별도 HTTP 서버를 띄우지 않는다.
