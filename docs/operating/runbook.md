# 운영 런북

on-call 치트 시트. 막힌 전송을 디버깅하기 위해 필요한 모든 명령은 이 페이지 안에 있다.

## 0. 이 페이지의 사용법

증상부터 골라서 해당 절로 점프한다. 모르는 용어가 나오면 [용어집](../concepts/glossary.md) 을 같이 연다.

| 증상 | 보는 절 |
|---|---|
| 새 작업을 직접 큐에 넣어야 한다 | [§1 enqueue](#1-enqueue) |
| 어떤 trace_id 가 어디까지 갔는지 알고 싶다 | [§2 audit](#2-audit) |
| 작업이 `leased` 인 채 안 끝난다 | [§3 stuck](#3-stuck) |
| 처리량이 부족하다 / 너무 많다 | [§4 scale](#4-scale) |
| 방금 배포한 버전이 이상하다 | [§5 rollback](#5-rollback) |
| 롤링 재시작 전에 깨끗이 비우고 싶다 | [§6 drain](#6-drain) |
| SQL 한 줄로 상태를 보고 싶다 | [§7 SQL 컬렉션](#7-sql) |
| 사후 회고를 적어야 한다 | [§8 incident 템플릿](#8-incident) |

`<ns>` 는 운영 네임스페이스 (`imgsync` / `imgsync-e2e` 등), `<dsn>` 은 PostgreSQL 접속 문자열로 치환한다.

## 1. 작업을 수동으로 enqueue {#1-enqueue}

```bash
kubectl -n <ns> run --rm -it imgsync-cli \
  --image=<repo>/imgsync:<tag> --restart=Never \
  --env=IMGSYNC_DSN=<dsn> -- \
  enqueue \
    --trace-id=<trace> \
    --src=ftp://host/path/to/file \
    --dst=file:///mnt/share/dst/file \
    --src-protocol=ftp \
    --dst-protocol=localfs
```

같은 `(trace_id, dst)` 쌍이 이미 존재하면 INSERT 가 멱등하게 무시된다 (NULL 반환). enqueue CLI 의 자세한 인자는 [CLI — enqueue](../cli/enqueue.md) 를 본다.

## 2. 단일 작업 audit (한 줄 SQL) {#2-audit}

```sql
SELECT j.id, j.status, j.attempts, e.status AS event, e.ts, e.detail
  FROM transfer_jobs j LEFT JOIN transfer_events e ON j.id = e.job_id
 WHERE j.trace_id = '<trace>' AND j.dst = '<dst>'
 ORDER BY e.ts;
```

`transfer_jobs.id = transfer_events.job_id` 로 join 하는 것이 핵심이다. `trace_id` 만으로 join 하면 재enqueue 된 `(trace_id, dst)` 쌍 사이에서 fan-out 이 일어나 실제로 일어난 이벤트보다 훨씬 많은 행이 나온다.

`transfer_jobs.status` 의 의미:

- `pending` — 워커가 lease 하기를 기다리는 중
- `leased` — 어떤 워커가 잡고 전송 진행 중
- `succeeded` — 종료: 성공
- `skipped` — 종료: 소스가 없거나 읽을 수 없음 (`ErrSkippable`)
- `dead` — 종료: 영속적 오류 또는 max_attempts 초과

`transfer_events.status` 의 값: `enqueue`, `lease`, `success`, `skip`, `fail` (일시적), `expire`, `dead`.

## 3. 멈춘 작업 찾기 {#3-stuck}

```sql
-- sweeper 임계값(5분)보다 오래 leased 상태인 작업
SELECT id, trace_id, locked_by, locked_at, NOW() - locked_at AS held_for
  FROM transfer_jobs
 WHERE status = 'leased'
   AND locked_at < NOW() - INTERVAL '5 minutes'
 ORDER BY locked_at;
```

Sweeper cycle 은 모든 pod 에서 돌지만 PostgreSQL advisory lock (`pg_try_advisory_xact_lock`) 으로 매 cycle 한 pod 만 실제 회수 작업을 수행한다 — 영구 leader 가 따로 있는 게 아니라 매 tick 마다 누가 lock 을 잡느냐로 결정된다. 위 쿼리가 행을 반환하면 어느 pod 도 cycle 을 끝까지 돌리지 못한다는 뜻이다. 모든 pod 의 stderr 에서 `sweeper: cycle timeout` / `sweeper: cycle error` 가 보이는지 확인하고, [§7 SQL 컬렉션](#7-sql) 의 `last_sweep_ts` 쿼리로 마지막 사이클 시각을 본다.

## 4. 스케일 업 / 다운 {#4-scale}

처리량을 늘리려면 replicaCount 를 올린다.

```bash
helm upgrade imgsync deploy/helm/imgsync \
  --reuse-values --set replicaCount=8
```

기대 동작: 8번째 pod 가 5분 이내(이미지 cold) 또는 1분 이내(warm cache)에 lease 를 시작한다. C7 throughput 게이트는 2→8 replicas 에서 ≥3.2× 선형성을 보장한다 — 자세한 권장 패턴은 [스케일링](scaling.md) 페이지를 본다.

```bash
# fullname 헬퍼는 release 이름이 "imgsync" 를 포함하면 release 이름으로 collapse 한다.
# 그렇지 않으면 service 는 "<release>-imgsync" 가 된다. 확실하지 않으면:
#   kubectl -n <ns> get svc -l app.kubernetes.io/name=imgsync
kubectl -n <ns> port-forward svc/imgsync 8080:8080
curl localhost:8080/healthz | jq
```

`/healthz` 응답 필드의 의미는 [모니터링](monitoring.md#healthz-응답-구조) 에 정리되어 있다.

## 5. 잘못된 릴리스 롤백 {#5-rollback}

```bash
helm history -n <ns> imgsync       # 마지막 양호한 revision 찾기
helm rollback -n <ns> imgsync <N>  # revision N 으로 롤백
```

마이그레이션은 forward-only 다. 차트를 롤백해도 스키마는 되돌아가지 않는다 — 이전 버전 바이너리는 새 스키마를 우아하게 읽도록 작성되어 있다 (새 컬럼은 NULL 또는 합리적 기본값을 가진다). 정책의 자세한 근거는 [업그레이드 · 롤백](upgrades-and-rollback.md#마이그레이션-정책) 을 본다.

배드 릴리스가 crashloop 으로 롤백을 막고 있다면:

```bash
kubectl -n <ns> delete pod -l app.kubernetes.io/name=imgsync --force
helm rollback -n <ns> imgsync <N>
```

## 6. 롤링 재시작 전 drain (드물게 필요) {#6-drain}

```bash
# replicaCount=0 으로 새 lease 수락 중지
helm upgrade imgsync deploy/helm/imgsync --reuse-values --set replicaCount=0

# in-flight 종료 대기 (≤ terminationGracePeriodSeconds = 60s) + sweeper 윈도우
sleep 360

# 다시 띄우기
helm upgrade imgsync deploy/helm/imgsync --reuse-values --set replicaCount=4
```

`sleep 360` 은 60s grace period + 5min sweeper threshold 의 합이다. 더 일찍 끊으면 미처 회수되지 못한 lease 가 다음 release 에서 stuck 으로 보인다.

## 7. 주요 SQL 컬렉션 {#7-sql}

큐 전체의 헬스를 빠르게 보는 한 줄들. 모두 read-only 다.

### 상태별 카운트

```sql
SELECT status, COUNT(*) FROM transfer_jobs GROUP BY status ORDER BY 1;
```

`pending` 이 시간에 따라 단조 증가하면 처리량이 enqueue 속도를 못 따라간다는 신호 — [스케일링](scaling.md) 으로 간다. `dead` 가 갑자기 튀면 [트러블슈팅](troubleshooting.md) 의 FTP/transport 항목을 본다.

### 시간대별 처리량 (최근 1시간, 분 단위)

```sql
SELECT date_trunc('minute', e.ts) AS bucket,
       SUM(CASE WHEN e.status = 'success' THEN 1 ELSE 0 END) AS ok,
       SUM(CASE WHEN e.status = 'fail'    THEN 1 ELSE 0 END) AS fail,
       SUM(CASE WHEN e.status = 'expire'  THEN 1 ELSE 0 END) AS expire
  FROM transfer_events e
 WHERE e.ts > NOW() - INTERVAL '1 hour'
 GROUP BY 1
 ORDER BY 1 DESC;
```

### 최근 실패 N 건

```sql
SELECT j.trace_id, j.dst, j.attempts, e.ts, e.detail
  FROM transfer_events e JOIN transfer_jobs j ON j.id = e.job_id
 WHERE e.status IN ('fail', 'dead', 'expire')
 ORDER BY e.ts DESC
 LIMIT 50;
```

`detail` 의 `stage` 필드가 1차 분류 단서다. 실제로 코드가 emit 하는 값은 `source-factory` (source 인스턴스 생성 실패), `transport-factory` (transport 인스턴스 생성 실패), `open` (`Source.Open` 실패), `transport` (`Transport.Send` 실패), `verify` (size mismatch — 자주 보이는 terminal 사유), `source_close` (전송 후 source close 실패) 다.

### 마지막 sweep 사이클

```sql
-- /healthz 의 last_sweep_ts 와 동일한 정보를 DB 만 보고 추정하고 싶을 때
SELECT MAX(e.ts) FROM transfer_events e WHERE e.status = 'expire';
```

만약 ‘expire’ 이벤트가 없을 만큼 트래픽이 적다면 `/healthz` 의 `last_sweep_ts` 를 직접 본다 — [모니터링](monitoring.md#healthz-응답-구조) 참고.

### Sniffer enqueue 추세 (과거 1시간)

```sql
SELECT date_trunc('minute', e.ts) AS bucket, COUNT(*) AS enqueued
  FROM transfer_events e
 WHERE e.status = 'enqueue'
   AND e.ts > NOW() - INTERVAL '1 hour'
 GROUP BY 1 ORDER BY 1 DESC;
```

## 8. Incident response 템플릿 (5 whys) {#8-incident}

장애를 닫을 때 한 페이지로 적어 둔다. 5 Whys 의 핵심은 "사람"이 아니라 "시스템 안의 약한 보호장치"를 찾는 것이다.

```markdown
## Incident: <한 줄 제목>

- 발생 시각 (UTC): <YYYY-MM-DD HH:MM>
- 감지: <알람 / on-call 직접 발견 / 사용자 보고>
- 영향: <pending 누적 N건 / dead N건 / 다운타임 N분>
- 1차 대응자: <handle>
- 종료 시각 (UTC): <YYYY-MM-DD HH:MM>
- 총 영향 시간: <N분>

### 타임라인 (UTC)
- HH:MM — <이벤트>
- HH:MM — <이벤트>
- HH:MM — 완화 적용
- HH:MM — 회복 확인 (/healthz 정상, pending 감소)

### 5 Whys
1. 왜 발생했나? <증상 수준의 원인>
2. 왜 그게 일어났나? <한 단계 더 깊은 원인>
3. 왜 그건 막히지 않았나? <누락된 가드>
4. 왜 그 가드가 없었나? <설계/리뷰의 빈 곳>
5. 왜 그게 보이지 않았나? <관측성/문서의 빈 곳>

### 무엇이 잘 작동했나
- <탐지/완화 중 빠르게 작동한 보호장치>

### 무엇이 잘 작동하지 않았나
- <대응을 늦춘 부분>

### 액션 아이템 (담당자, 마감)
- [ ] <조치> — @handle, MM/DD
- [ ] <문서/모니터링 보강> — @handle, MM/DD
- [ ] <테스트 커버리지 보강> — @handle, MM/DD
```

작성이 끝나면 PR 로 `docs/superpowers/incidents/<YYYY-MM-DD>-<slug>.md` 에 추가하고, 액션 아이템은 별도 이슈로 분리해 추적한다.
