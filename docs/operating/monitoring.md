# 모니터링

imgsync 가 노출하는 health endpoint 와 로그 라인의 의미를 정리한다. 알람을 어디에 걸지 판단하는 기준점이다.

## /livez · /readyz · /healthz 의 차이

세 endpoint 의 책임은 서로 다르다. 같은 포트(`IMGSYNC_HEALTH_ADDR`, 기본 `:8080`)에 함께 뜬다.

| Endpoint | 용도 | Probe 매핑 | 실패가 의미하는 것 |
|---|---|---|---|
| `/livez` | "프로세스가 살아 있는가" | `livenessProbe` | 무조건 200. 실패 = 프로세스가 deadlock 등으로 응답 자체를 못 함 → kubelet 이 컨테이너 재시작 |
| `/readyz` | "트래픽을 받을 준비가 됐나" | `startupProbe` + `readinessProbe` | DB pool ping 을 2초 안에 못하면 503. Pod 가 Service endpoint 에서 빠진다 |
| `/healthz` | deep health (인간이 읽는 진단) | probe 로 쓰지 말 것 | 200 + JSON. 안의 timestamp 가 오래된 경우가 진짜 알람 신호 |

요점: `/livez` 와 `/readyz` 는 Kubernetes 가 "재시작" / "트래픽 차단" 의 의사결정에 쓰는 신호다. 사람이 디버깅용으로 보는 깊은 상태는 모두 `/healthz` 에 있다.

`/livez` 와 `/readyz` 를 deep-health 로 만들고 싶은 유혹을 피해야 한다. DB 가 1초 느려졌다고 모든 pod 가 동시에 NotReady 가 되면 전체 워커 클러스터가 동시에 트래픽에서 빠진다.

## /healthz 응답 구조

```bash
curl -s localhost:8080/healthz | jq
```

```json
{
  "last_lease_attempt_ts": "2026-05-05T08:12:01.234Z",
  "last_lease_success_ts": "2026-05-05T08:11:58.118Z",
  "last_sweep_ts":         "2026-05-05T08:11:42.011Z",
  "pool_in_use":           3,
  "pool_idle":             7,
  "pool_max":              16
}
```

| 필드 | 의미 | 정상 범위 (참고) |
|---|---|---|
| `last_lease_attempt_ts` | lease 쿼리를 마지막으로 시도한 시각 | ≤ 수 초 (idle backoff `MaxDelay` 기본 1초) |
| `last_lease_success_ts` | 마지막으로 작업을 잡은 시각 | 큐에 작업이 있을 때 ≤ 수 초. 큐가 비면 자연스럽게 오래됨 |
| `last_sweep_ts` | sweeper 가 마지막 사이클을 끝낸 시각 | ≤ 60초 (sweep interval = 30초) |
| `pool_in_use` | 사용 중 pgx 커넥션 | `pool_max` 미만이면 정상 |
| `pool_idle` | 유휴 pgx 커넥션 | — |
| `pool_max` | 풀 상한 | `pgxpool` 설정 그대로 |

알람 후보:

- `now() - last_sweep_ts > 5분` → 어느 pod 도 sweeper cycle 을 끝까지 돌지 못하고 있다 (advisory lock 으로 직렬화됨) → [런북 §3](runbook.md#3-stuck) 으로 점프.
- `pool_in_use ≈ pool_max` 가 지속 → DB pool 포화. `IMGSYNC_WORKERS` / FTP host cap / DB max_connections 의 균형이 깨진 신호.
- `now() - last_lease_attempt_ts > 60초` → worker 의 lease 루프가 멈춤. probe 가 통과하더라도 사실상 일을 안 하는 상태.

## 로그

imgsync 는 의도적으로 **로그가 적다**. 정상 동작은 `transfer_events` 테이블에 행으로 남고, 표준 에러로 흘리는 라인은 운영자의 주의가 필요한 예외 상황으로 한정된다.

| 라인 키워드 | 컴포넌트 | 의미 | 출처 |
|---|---|---|---|
| `imgsync worker: lease error (...)` | worker | `LeaseOne` DB 호출이 실패. 일시적이면 짧은 sleep 후 재시도, 반복되면 control DB 점검 신호 | `internal/worker/runner.go` |
| `sweeper: cycle timeout (...)` | sweeper | sweep cycle 이 cycleTimeout 안에 끝나지 못함. lease 가 너무 많거나 DB 가 느린 신호 | `internal/sweeper/sweeper.go` |
| `sweeper: cycle error: ...` | sweeper | sweep cycle 에서 DB 에러. 다음 tick 에서 재시도 | `internal/sweeper/sweeper.go` |
| `sniffer enqueued N new jobs` | sniffer | 한 사이클에서 `transfer_jobs` 에 INSERT 된 행 수 (`log.Printf`) | `internal/cli/sniffer.go` |
| `sniffer run error: ...` | sniffer | sniffer 사이클이 실패. source DB 또는 control DB 측 문제 가능성 | `internal/cli/sniffer.go` |

**상태 추적은 로그가 아니라 DB.** lease/success/skip/fail 같은 상태 전이는 `transfer_events` 에 행으로 기록된다. lease 가 잡혔는지 / 워커가 일하고 있는지 확인하려면 [런북 §7](runbook.md#7-sql) 의 SQL 컬렉션 또는 [런북 §2](runbook.md#2-audit) 의 단일 작업 감사 쿼리를 사용한다.

운영 환경에서 키-밸류 추출이 필요하면 sidecar (예: vector / fluent-bit) 로 후처리하는 것을 권장한다.

## 메트릭 (Prometheus)

imgsync 는 health endpoint 와 같은 포트(`IMGSYNC_HEALTH_ADDR` / `SNIFFER_HEALTH_ADDR`, 기본 `:8080`)의 `/metrics` 경로에서 Prometheus 메트릭을 노출한다. 아래는 메트릭 카탈로그, `transfer_events` 매핑, 권장 알람, 그리고 `OnLeaseAttempt` 의 알려진 한계를 정리한 것이다.

### 메트릭 카탈로그

| 메트릭 | 모드 | 산출 | 라벨 (카디널리티) |
|---|---|---|---|
| `imgsync_jobs_in_status` | scrape | `SELECT status, COUNT(*) FROM transfer_jobs GROUP BY status` (2초 timeout) | `status` (5) |
| `imgsync_lease_attempts_total` | push | `worker.Runner.OnLeaseAttempt` | `result` (success/empty/error) |
| `imgsync_jobs_processed_total` | push | `worker.OnFinish` | `src`, `dst`, `result` (success/skip/fail/expire/dead) |
| `imgsync_job_duration_seconds` | push | `worker.OnFinish` (histogram, lease→Send 완료. buckets: `[0.1, 0.5, 1, 2, 5, 10, 30, 60, 300, 1800]` 초) | `src`, `dst`, `result` (~45) |
| `imgsync_sweep_cycles_total` | push | `sweeper.Config.OnCycle` | — |
| `imgsync_lease_lock_age_seconds` | scrape | `SELECT EXTRACT(EPOCH FROM NOW()-MIN(locked_at)) WHERE status='leased'` | — |
| `imgsync_db_pool_conns` | scrape | `pgxpool.Stat()` | `state` (in_use/idle/max) |
| `imgsync_ftp_pool_size` | push | `ftp.Pool` checkout/release | `host`, `state` (in_use/idle) |
| `imgsync_workers_active` | push | `Runner` goroutine start/stop | `pod` |
| `imgsync_sniffer_enqueue_total` | push | `Sniffer.RunOnce` 결과 n 만큼 Add | `source` |
| `imgsync_sniffer_run_errors_total` | push | `Sniffer.RunOnce` err 발생 시 Inc | `source` |

### `transfer_events.status` ↔ metric 매핑

| `transfer_events.status` | 대응 metric & label |
|---|---|
| `enqueue` | `imgsync_sniffer_enqueue_total{source}` |
| `lease` | `imgsync_lease_attempts_total{result=success}` + `imgsync_jobs_in_status{status=leased}` 변동 |
| `success` | `imgsync_jobs_processed_total{result=success}` + `imgsync_job_duration_seconds` observe |
| `skip` | `imgsync_jobs_processed_total{result=skip}` |
| `fail` | `imgsync_jobs_processed_total{result=fail}` |
| `expire` | `imgsync_jobs_processed_total{result=expire}` (sweeper 회수 시) |
| `dead` | `imgsync_jobs_processed_total{result=dead}` |

### 권장 알람

| PromQL | 임계값 | 의미 |
|---|---|---|
| `imgsync_jobs_in_status{status="pending"}` | SLO 초과 지속 | 큐 적체 — 워커 스케일 또는 sniffer 폭주 확인 |
| `rate(imgsync_jobs_processed_total{result="fail"}[5m])` | > X (운영 환경별 설정) | 실패율 급증 — `transfer_events` SQL 로 detail 조사 |
| `imgsync_lease_lock_age_seconds` | > 600 (10분) | Stuck lease — sweeper 동작 확인 (`imgsync_sweep_cycles_total` rate) |

### `OnLeaseAttempt` 의 한계

`OnLeaseAttempt` 콜백은 `func(bool)` 단일 인자 시그니처로, 성공(true) / 빈 큐(false) 두 가지만 구분한다. 그 결과 DB driver 오류는 콜백 바깥의 별도 경로에서 log + retry 처리되며, `imgsync_lease_attempts_total{result="error"}` 로는 잡히지 않는다.

운영 의미: lease error 가 반복될 경우 이 메트릭만 보면 놓칠 수 있다. DB 커넥션 이상은 `imgsync_db_pool_conns` scrape, pgx 드라이버 로그, `/readyz` probe 의 조합으로 감지해야 한다. 콜백 시그니처 확장은 향후 plan 에서 처리한다.
