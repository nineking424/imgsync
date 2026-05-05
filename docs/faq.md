# FAQ

운영자와 신규 컨트리뷰터가 자주 묻는 질문을 모았습니다. 본문 페이지의 해당 섹션으로 바로 이동할 수 있도록 링크를 걸어두었으니, 더 자세한 배경이 필요하면 따라가세요.

## 설계 / 아키텍처

### 왜 메시지 브로커(Kafka/RabbitMQ) 가 아니라 PostgreSQL 인가?

ops 의존성 최소화가 목표였습니다. `transfer_jobs` + `transfer_events` 두 테이블만으로 enqueue / lease / finalize / audit 이 모두 해결되며, 추가 컴포넌트(브로커, ZK, etcd)가 늘어나면 그만큼 장애 면적과 운영 부담도 커집니다. 자세한 설계 의도는 [아키텍처 — 의도적으로 하지 않은 것](concepts/architecture.md#의도적으로-하지-않은-것) 을 참고하세요.

### 워커가 동시에 같은 행을 lease 하지 않는다는 보장은?

`SELECT ... FOR UPDATE SKIP LOCKED` 로 점유합니다. 같은 행을 두 워커가 동시에 잡으려 하면 한쪽만 행을 받고 다른 쪽은 다음 후보로 넘어가므로 DB 트랜잭션 수준에서 동시 lease 가 차단됩니다. 자세한 SQL 형태는 [아키텍처 §2 Lease](concepts/architecture.md#2-lease) 를, 상태 전이는 [작업 큐 모델 — 상태 전이도](concepts/job-queue-model.md#상태-전이도) 를 참고하세요.

### 멱등성은 어떻게 보장되나?

`transfer_jobs` 의 `(trace_id, dst)` UNIQUE 제약과 `INSERT ... ON CONFLICT DO NOTHING` 패턴으로 보장합니다. Sniffer 가 재시작되거나 source DB 에서 같은 레코드를 두 번 읽어도 중복 작업이 만들어지지 않습니다. 자세한 동작은 [작업 큐 모델 — 멱등성 키](concepts/job-queue-model.md#멱등성-키) 를 보세요.

## 운영 / 튜닝

### sweeper threshold 를 더 짧게 줄여도 되나?

기본 5분이며 줄이는 건 권장하지 않습니다. threshold 가 p99 전송시간보다 짧으면 정상적으로 처리 중인 in-flight 작업이 회수돼 다른 워커에 재할당되고, 결과적으로 **중복 전송** 이 발생할 수 있습니다. 트레이드오프와 권장값 결정 기준은 [Sweeper — Threshold 튜닝](configuration/sweeper.md#threshold-튜닝) 를 참고하세요.

### shadow sniffer 는 언제 끄나?

기본값은 `SNIFFER_SHADOW=true` 입니다. 신규 source DB 를 연결하면 우선 shadow 로 두고 (1) 쿼리 결과가 기대대로 나오는지, (2) `pattern` 렌더 결과가 운영하는 dst 경로와 맞는지를 로그로 충분히 확인한 뒤 `false` 로 전환합니다. 단계적 전개 절차는 [Sniffer — Shadow 모드와 단계적 전개](configuration/sniffer.md#shadow-모드와-단계적-전개) 를 보세요.

### 다중 namespace 에 같은 차트를 설치해도 되나?

가능합니다. 차트는 namespace-scoped 로 작성돼 있어 같은 클러스터에 여러 namespace 로 병렬 설치할 수 있습니다. 단, 상태 격리는 **control DB 분리에 달려 있습니다** — namespace 별로 별도 control DB(또는 별도 schema)를 쓰지 않으면 작업 큐가 섞입니다. Secret 분리 전략은 [Secret 준비](installation/secrets.md) 를 참고하세요.

### helm uninstall 후 DB 데이터는?

기본적으로 **삭제되지 않습니다.** imgsync 차트는 control DB(PostgreSQL StatefulSet)를 직접 관리하지 않고 외부 DB 를 가정하므로 helm uninstall 은 imgsync 의 Deployment / Job / ConfigMap 만 제거합니다. 차트가 의존성으로 PostgreSQL 을 함께 깐 경우는 PVC retain 정책에 따라 다르므로 별도 확인이 필요합니다. 마이그레이션이 forward-only 인 이유와 함께 [Helm 설치 — 언인스톨](installation/helm.md#언인스톨) 에 정리돼 있습니다.

## 마이그레이션 / 업그레이드

### migration down 이 자동으로 돌지 않는 이유는?

imgsync 는 **forward-only** 정책이며, `down.sql` 은 의도적으로 자동 실행 경로를 만들지 않았습니다. helm rollback 은 deployment 만 되돌리고 스키마는 그대로 둡니다. 새 컬럼이 NULLABLE 이라는 호환성 규칙으로 옛 워커가 새 스키마 위에서도 안전하게 도는 게 정책의 핵심입니다. 자세한 근거는 [업그레이드 · 롤백 — 마이그레이션 정책](operating/upgrades-and-rollback.md#마이그레이션-정책) 을 참고하세요.

### 그럼 down.sql 은 언제 / 어떻게 적용하나?

incident 수준의 결정으로 보고 on-call 이 직접 `psql` 로 적용합니다. 자동 경로(helm rollback, CI)는 down 을 호출하지 않으며, 적용 후에는 [런북 §8 incident 템플릿](operating/runbook.md#8-incident) 으로 회고를 남깁니다. 위치와 운영 규약은 [업그레이드 · 롤백 — `down.sql` 의 위치](operating/upgrades-and-rollback.md#downsql-의-위치) 에 있습니다.

### 워커 N+1 과 DB N 의 조합은 안전한가?

호환성 매트릭스에 따르면 **워커 N + DB N+1** 은 안전(정책 강제), **워커 N+1 + DB N** 은 안전하지 않습니다. helm hook 이 마이그레이션 Job → Deployment 순서를 강제하므로 정상 경로에서는 후자 조합이 만들어지지 않습니다. 표 전체는 [업그레이드 · 롤백 — 호환성 매트릭스](operating/upgrades-and-rollback.md#호환성-매트릭스-워커-n1--db-n) 에서 확인하세요.

## 배포 / 토폴로지

### on-prem K8s 가 아니면 안 되나? docker-compose 만으로 운영 가능한가?

기능적으로는 가능합니다. Sweeper 의 cycle 직렬화는 PostgreSQL advisory lock 기반이라 단일 호스트에서도 정확히 동작하고, Sniffer 는 v1 에서 단일 인스턴스만 지원하므로 docker-compose 1 컨테이너로도 의도와 일치합니다. 다만 워커 재시작·롤링 업그레이드·secret 회전 등의 운영 자동화는 K8s + Helm 경로에 맞춰져 있어 docker-compose 는 **개발 / 단일 노드 검증용** 으로 권장합니다. 비교는 [Docker Compose 빠른 시작](getting-started/quickstart-docker-compose.md) 과 [Helm 설치](installation/helm.md) 를 보세요.

### worker 를 K8s 가 아닌 systemd 로 돌릴 수 있나?

가능합니다. worker 는 단일 정적 바이너리이고 환경 변수만 주면 동작하므로 systemd unit 으로 띄워도 문제가 없습니다. 단, replicaCount 스케일링·rolling restart·OOMKilled 후 재기동·secret 회전 같은 운영 동작은 직접 챙겨야 합니다. 환경 변수 목록은 [환경 변수](configuration/environment-variables.md), 상시 운영 절차는 [런북](operating/runbook.md) 을 참고하세요.

## 확장

### FTP 외에 다른 프로토콜은 언제 지원되나?

S3 가 로드맵에 있고, 그 외 프로토콜은 수요와 우선순위에 따라 추가됩니다. 신규 백엔드 추가는 `transfer.Source` / `transfer.Transport` 인터페이스 구현 + 프로토콜 레지스트리 등록만으로 끝나도록 설계돼 있어 DB 스키마나 Worker 루프를 건드릴 필요가 없습니다. 인터페이스와 매트릭스는 [Source · Transport](concepts/sources-and-transports.md) 와 [아키텍처 — 확장 포인트](concepts/architecture.md#확장-포인트) 를 보세요.

### 메모리에 본문을 통째로 읽어도 되나?

안 됩니다. `Source.Open` → `Transport.Send` 는 스트림을 직접 파이프해야 하며 전체 본문 버퍼링은 금지입니다. CI 의 `scripts/check-streaming.sh` 가 `bytes.NewBuffer.*body`, `io.ReadAll` 같은 패턴을 감지하면 빌드를 실패시킵니다. 계약 전체는 [Source · Transport — 스트리밍 계약](concepts/sources-and-transports.md#스트리밍-계약) 에 있습니다.
