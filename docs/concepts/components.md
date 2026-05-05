# 컴포넌트

imgsync 를 구성하는 세 가지 런타임 컴포넌트를 설명합니다.

## Worker

**역할**: `transfer_jobs` 에서 작업을 lease 해 실제 파일 전송을 수행하는 컴포넌트입니다. scale-out 단위는 파드이며, `replicaCount` 개수만큼 동시에 실행됩니다.

### Lease 루프

각 워커 파드는 환경 변수 `IMGSYNC_WORKERS` 개수만큼 goroutine 을 띄웁니다. 각 goroutine 은 `repo.LeaseOne` 으로 다음 `pending` 작업을 가져오는 루프를 돕니다. 큐가 비어 있으면 `backoff.NewIdle` 로 구현된 idle backoff 가 동작해 DB 폴링 주기를 점진적으로 늘립니다 (최대 수십 초). 새 작업이 enqueue 되면 다음 폴링 사이클에서 즉시 집어갑니다.

### FTP Host Cap

동일 FTP 호스트에 대한 클러스터 전체 동시 처리 상한을 강제합니다. `hostcap.Wrap` 이 `pg_try_advisory_lock` 기반 advisory lock 을 잡아 클러스터-와이드 한도를 초과하는 FTP 동시 세션을 방지합니다. 락을 잡지 못한 goroutine 은 backoff 후 재시도합니다. 자세한 내용은 [용어집 — FTP host cap](glossary.md) 을 참고하세요.

### 종료 신호

파드가 SIGTERM 을 받으면 새 lease 를 중단하고 in-flight 전송이 끝날 때까지 대기합니다. Helm chart 의 `terminationGracePeriodSeconds` 기본값은 60초입니다. 60초 안에 in-flight 이 완료되지 않으면 파드가 강제 종료되고 Sweeper 가 해당 lease 를 회수합니다.

## Sniffer

**역할**: Source DB 를 폴링해 새 레코드를 감지하고, control DB 에 작업을 enqueue 하는 컴포넌트입니다.

### shadow 모드

`SNIFFER_SHADOW=true` 로 켜면 Sniffer 는 enqueue 대신 감사(audit) 로그만 남깁니다. 신규 Source DB 의 쿼리가 올바른지, 예상되는 레코드가 검출되는지 확인할 때 사용합니다. shadow 모드에서는 `transfer_jobs` 에 행이 삽입되지 않으므로 워커가 실제 전송을 시도하지 않습니다.

### High-watermark 기반 증분

Sniffer 는 Source DB 에서 마지막으로 읽은 위치를 `sniffer_state` 테이블의 `(timestamp, pk)` 쌍으로 관리합니다. 재시작해도 이 위치부터 이어서 폴링하므로 중복 enqueue 나 누락 없이 증분 처리가 가능합니다.

### 데몬 / 벌크 모드

- **데몬 모드** (기본): 주기적으로 Source DB 를 폴링하며 장기 실행합니다.
- **벌크 모드**: 지정된 범위를 한 번 읽고 종료합니다. 백필(backfill) 작업이나 일회성 마이그레이션에 씁니다.

v1 의 Sniffer 는 단일 인스턴스만 지원합니다. Helm chart 가 `sniffer.replicas > 1` 을 거부하고(`fail`), Deployment 의 `strategy: Recreate` 로 두 sniffer 가 동시에 돌지 않도록 막습니다. `sniffer_state` 에 advisory lock 이 없기 때문에 동시 실행 시 watermark 경합으로 행이 재enqueue 될 수 있습니다 — 수평 확장은 v2 의 과제로 남겨두었습니다.

## Sweeper

**역할**: stuck lease 를 회수해 작업이 영원히 `leased` 상태에 머무는 것을 방지합니다.

### Threshold / Interval

- **threshold** (기본 5분): lease 획득 후 이 시간이 지나도 finalize 되지 않으면 stuck 으로 판단합니다.
- **interval** (기본 30초): Sweeper 가 stuck lease 를 스캔하는 주기입니다.

stuck lease 를 발견하면 `transfer_events` 에 `expire` 이벤트를 기록하고 `status` 를 `pending` 으로 되돌립니다.

### Cycle 직렬화

Sweeper goroutine 은 모든 worker pod 에서 돌지만, 매 cycle 시작 시 `pg_try_advisory_xact_lock(hashtext("imgsync_sweeper"))` 를 시도해 한 pod 만 실제 회수 작업을 수행합니다. 락을 못 잡은 pod 는 그 cycle 을 건너뛰고 다음 tick 에서 다시 시도합니다. 영구 leader 가 따로 있는 게 아니라 매 cycle 마다 누가 락을 잡느냐로 결정됩니다 — 결과적으로 `replicaCount > 1` 이어도 같은 lease 가 두 번 회수되는 일은 없습니다.

### Health 연동

Sweeper 는 각 사이클(`OnCycle`) 완료 시 `last_sweep_ts` 를 갱신합니다. `/healthz` 엔드포인트가 이 값을 노출하므로, 값이 오래된 경우 Sweeper 가 정상적으로 실행되지 않고 있다는 신호입니다.
