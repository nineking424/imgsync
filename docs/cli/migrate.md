# imgsync migrate

마이그레이션 SQL 을 idempotent 하게 적용한다.

## 사용법

```text
imgsync migrate [flags]
```

## 플래그

| 플래그 | 타입 | 기본값 | 설명 |
|---|---|---|---|
| `--dir` | string | `/etc/imgsync/migrations` | 마이그레이션 SQL 파일(`*.up.sql`)이 있는 디렉터리 |

## 환경 변수

| 변수 | 필수 | 설명 |
|---|---|---|
| `IMGSYNC_DSN` | 필수 | PostgreSQL 연결 문자열 |
| `IMGSYNC_MIGRATIONS_DIR` | 선택 | `--dir` 플래그의 환경 변수 형태 |

자세한 표는 [환경 변수](../configuration/environment-variables.md)를 참고.

## 예시

로컬에서 직접 실행:

```bash
IMGSYNC_DSN=postgres://localhost/imgsync imgsync migrate --dir ./migrations
```

K8s (Helm pre-install hook 으로 자동 실행):

```bash
# 일반적으로 사용자가 직접 부를 일은 없음.
# helm install / helm upgrade 시 pre-install Job 이 자동으로 호출함.
helm install imgsync deploy/helm/imgsync --set image.tag=<tag>
```

## 동작

`migrations/*.up.sql` 파일을 파일명 정렬 순(숫자 prefix)으로 읽어 순서대로 적용한다. 이미 적용된 마이그레이션은 skip 하므로 여러 번 실행해도 안전하다(idempotent). Helm 차트의 pre-install/pre-upgrade hook Job 도 동일한 서브커맨드를 호출하므로, 운영 환경에서 사용자가 직접 실행할 경우는 거의 없다.

## 현재 마이그레이션 목록

`migrations/` 디렉터리에 들어 있는 SQL 파일 (2026-05 시점):

| 파일 | 도입 PR | 목적 |
|---|---|---|
| `0001_initial.up.sql` | v1 — Week 1 | `transfer_jobs`, `transfer_events`, `sniffer_state`, `schema_migrations` 테이블 생성 |
| `0002_add_extra_columns.up.sql` | v1 — Week 2 | `transfer_jobs` 에 `attempts`, `last_error`, `last_attempt_at` 등 운영 칼럼 추가 |
| `0003_jobs_status_index.up.sql` | Phase 1.5 모니터링 | `transfer_jobs(status)` b-tree 인덱스. `imgsync_jobs_in_status` scrape SQL (`SELECT status, COUNT(*) GROUP BY status`) 가 succeeded/skipped 누적 행에서 풀 heap scan 으로 떨어지는 것을 방지한다 — index-only scan + HashAggregate 로 처리. |

각 파일은 idempotent 하므로 같은 버전이 이미 `schema_migrations` 에 기록돼 있으면 skip 된다. 새 마이그레이션을 추가할 때는 짝이 되는 `*.down.sql` 도 같이 만든다 (자동 실행되지 않지만 비상 시 수동 적용용).

> 이전 버전이 새 스키마와 호환되어야 한다는 forward-only 정책의 정확한 의미는 [업그레이드 · 롤백 — 마이그레이션 정책](../operating/upgrades-and-rollback.md#마이그레이션-정책) 을 본다.
