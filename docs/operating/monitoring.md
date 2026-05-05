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
| `last_lease_attempt_ts` | lease 쿼리를 마지막으로 시도한 시각 | idle backoff 최대 주기 이내 (보통 ≤ 30초) |
| `last_lease_success_ts` | 마지막으로 작업을 잡은 시각 | 큐에 작업이 있을 때 ≤ 수 초. 큐가 비면 자연스럽게 오래됨 |
| `last_sweep_ts` | sweeper 가 마지막 사이클을 끝낸 시각 | ≤ 60초 (sweep interval = 30초) |
| `pool_in_use` | 사용 중 pgx 커넥션 | `pool_max` 미만이면 정상 |
| `pool_idle` | 유휴 pgx 커넥션 | — |
| `pool_max` | 풀 상한 | `pgxpool` 설정 그대로 |

알람 후보:

- `now() - last_sweep_ts > 5분` → sweeper 가 멈췄거나 leader 락을 못 잡고 있다 → [런북 §3](runbook.md#3-stuck) 으로 점프.
- `pool_in_use ≈ pool_max` 가 지속 → DB pool 포화. `IMGSYNC_WORKERS` / FTP host cap / DB max_connections 의 균형이 깨진 신호.
- `now() - last_lease_attempt_ts > 60초` → worker 의 lease 루프가 멈춤. probe 가 통과하더라도 사실상 일을 안 하는 상태.

## 로그

imgsync 워커 / sweeper 는 standard logger 로 표준출력에 라인을 흘린다. 운영에서 자주 보게 되는 키 이벤트와 의미는 다음과 같다 (정규식이 아니라 "이런 키워드가 들어 있다" 수준):

| 라인 키워드 | 컴포넌트 | 의미 |
|---|---|---|
| `lease loop started` | worker | 워커 goroutine 이 lease 루프 진입. pod 당 `IMGSYNC_WORKERS` 만큼 찍혀야 한다 |
| `no jobs to lease` | worker | `pending` 행이 없어 idle backoff 대기. 큐가 비어 있을 때 정상 |
| `lease acquired` | worker | `SELECT FOR UPDATE SKIP LOCKED` 로 한 행 잡음. 직후 `Source.Open → Transport.Send` 진입 |
| `lease expired` | sweeper | sweep cycle 에서 5분 넘게 잡혀 있던 lease 를 회수. 일시적으로는 정상, 빈번하면 worker 가 죽어 있다는 신호 |
| `sniffer enqueued N new jobs` | sniffer | 한 사이클에서 `transfer_jobs` 에 INSERT 된 행 수 |
| `sniffer run error` | sniffer | sniffer 사이클이 실패. detail 에 source/transport 식별자가 붙는다 |

JSON 구조 로그가 아니라 grep 으로 보는 사람-친화 라인이라는 점에 주의한다. 운영 환경에서 키-밸류 추출이 필요하면 sidecar (예: vector / fluent-bit) 로 후처리하는 것을 권장한다.

## 메트릭 (Prometheus)

현재 imgsync 는 Prometheus 메트릭을 노출하지 **않는다**. 2026-05 시점의 상태는 다음과 같다:

- 진단의 1차 채널은 `/healthz` JSON + 표준출력 로그.
- 큐 깊이 / 처리량 / 실패율 같은 시계열 지표가 필요하면 `transfer_events` 테이블을 직접 폴링한다 — [런북 §7](runbook.md#7-sql) 의 SQL 컬렉션을 참고.
- 향후 계획: `/metrics` 엔드포인트에서 `imgsync_lease_attempts_total`, `imgsync_lease_success_total`, `imgsync_sweep_cycles_total`, `imgsync_pool_in_use` 등을 노출하는 안이 검토 중이다. 디자인 문서가 확정되기 전까지는 SQL + `/healthz` 를 표준으로 본다.

알람을 거는 짧은 가이드:

- 즉시 page: `/readyz` 503, `now() - last_sweep_ts > 5분`, `dead` 행이 분당 X 건 초과.
- 다음 영업일: `pending` 누적이 임계값 이상 1시간 지속, `pool_in_use ≈ pool_max` 가 10분 이상 지속.
