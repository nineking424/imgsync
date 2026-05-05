# imgsync Monitoring Phase 1.5 — `transfer_jobs.status` Index Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Phase 1 의 `imgsync_jobs_in_status` scrape SQL 이 사용하는 `SELECT status, COUNT(*) FROM transfer_jobs GROUP BY status` 쿼리를 index-only scan 으로 떨어뜨리기 위해 `transfer_jobs.status` 단일 b-tree 인덱스를 추가한다.

**Architecture:** SQL 마이그레이션 단 한 개 (`0003_jobs_status_index.up.sql` + `.down.sql`) 만 추가한다. imgsync 의 `migrate up` 은 `*.up.sql` 을 lexical 순서로 자동 적용하므로 Go 코드 / Helm 변경은 없다. 별도 PR 로 분리되어 마이그레이션 단독 리뷰 / 롤백 단순화를 보장한다.

**Tech Stack:** PostgreSQL 16 (project default), `internal/db.ApplyMigrations` (기존 forward-only migrator), helm pre-install/pre-upgrade hook (`templates/migrate-job.yaml`).

---

## Spec reference

원본 요구는 `docs/superpowers/specs/2026-05-05-monitoring-stack-integration-design.md` Section 2 (Phase 1.5) / Section 4 (Scrape 비용). **Phase 1 머지와 동일 sprint 안에 머지되어야 한다** — Phase 1 의 `jobs_in_status` collector 가 succeeded/skipped 누적 행에서 풀 scan 을 돌면 scrape SQL 이 무거워질 수 있기 때문이다. 인덱스가 깔린 후 GROUP BY 는 index-only scan 으로 떨어진다.

## File structure

| 파일 | 변경 |
|---|---|
| `migrations/0003_jobs_status_index.up.sql` | 신규 — `transfer_jobs (status)` b-tree 인덱스 생성, `schema_migrations` 행 INSERT |
| `migrations/0003_jobs_status_index.down.sql` | 신규 — 인덱스 DROP, `schema_migrations` 행 삭제 |

**그 외 파일 변경 없음.** `internal/db/migrate.go` 가 디렉터리에서 `*.up.sql` 을 lexical 순서로 적용하고, helm `templates/migrate-job.yaml` 의 `pre-install,pre-upgrade` hook 이 새 마이그레이션을 자동으로 실행한다. Go 코드, Helm 템플릿, values, 테스트 스크립트 모두 변경 없음.

---

## Task 1: 인덱스 마이그레이션 작성

**Files:**
- Create: `migrations/0003_jobs_status_index.up.sql`
- Create: `migrations/0003_jobs_status_index.down.sql`

- [ ] **Step 1: up 마이그레이션 작성**

`migrations/0003_jobs_status_index.up.sql` 신규 작성:

```sql
-- imgsync v1 phase 1.5: transfer_jobs.status 단일 b-tree 인덱스
-- 목적: imgsync_jobs_in_status scrape SQL
--   SELECT status, COUNT(*) FROM transfer_jobs GROUP BY status
-- 가 succeeded/skipped 누적 행에서 풀 heap scan 으로 떨어지는 것을 방지.
-- 단일 컬럼 b-tree 면 PG 가 index-only scan + HashAggregate 로 처리한다.
-- Spec: docs/superpowers/specs/2026-05-05-monitoring-stack-integration-design.md §4

BEGIN;

CREATE INDEX transfer_jobs_status_idx ON transfer_jobs (status);

INSERT INTO schema_migrations (version) VALUES ('0003_jobs_status_index');

COMMIT;
```

- [ ] **Step 2: down 마이그레이션 작성**

`migrations/0003_jobs_status_index.down.sql` 신규 작성:

```sql
DROP INDEX IF EXISTS transfer_jobs_status_idx;
DELETE FROM schema_migrations WHERE version = '0003_jobs_status_index';
```

- [ ] **Step 3: 파일 생성 확인**

Run: `ls -la migrations/0003_*`

Expected:
```
-rw-r--r-- ... migrations/0003_jobs_status_index.down.sql
-rw-r--r-- ... migrations/0003_jobs_status_index.up.sql
```

- [ ] **Step 4: SQL 구문 정적 검증**

Run: `cat migrations/0003_jobs_status_index.up.sql | grep -E "BEGIN|CREATE INDEX|INSERT|COMMIT" | wc -l`

Expected: `4` (네 키워드가 한 번씩 등장)

추가:

Run: `grep -n "transfer_jobs_status_idx" migrations/0003_jobs_status_index.up.sql migrations/0003_jobs_status_index.down.sql`

Expected: 두 파일에서 한 번씩 등장 (CREATE 와 DROP).

- [ ] **Step 5: 정렬 순서 확인 (lexical apply)**

`internal/db/migrate.go` 의 `ApplyMigrations` 가 `sort.Strings(files)` 로 정렬 후 적용하므로 `0003_*` 가 `0002_*` 다음에 자동으로 실행된다.

Run: `ls migrations/*.up.sql | sort`

Expected:
```
migrations/0001_initial.up.sql
migrations/0002_sniffer_state.up.sql
migrations/0003_jobs_status_index.up.sql
```

---

## Task 2: 마이그레이션 적용 → 테스트로 인덱스 존재 확인

**Files:**
- Test: `internal/db/migrate_integration_test.go` (기존 통합 테스트가 있다면 어설션 추가, 없으면 신규)

> **참고:** imgsync 의 기존 통합 테스트 컨벤션은 `-tags integration` + testcontainers-go postgres 다 (`internal/sniffer/integration_test.go` 패턴). 만약 `internal/db/` 에 통합 테스트가 아직 없다면 이 task 에서 신규로 만든다. 있다면 기존 파일에 어설션을 추가한다.

- [ ] **Step 1: 기존 통합 테스트 위치 확인**

Run: `ls internal/db/*_test.go 2>/dev/null && grep -l "tags integration" internal/db/*.go 2>/dev/null`

기존 파일이 있으면 그 파일에 어설션을 추가하고 Step 2 의 신규 파일 생성은 건너뛴다. 없으면 Step 2 로 진행.

- [ ] **Step 2: 신규 통합 테스트 파일 (없을 때만)**

`internal/db/migrate_integration_test.go` 신규 작성:

```go
//go:build integration

package db_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/nineking424/imgsync/internal/db"
)

// TestMigrate_0003_StatusIndex 는 0003 마이그레이션 적용 후
// transfer_jobs (status) 인덱스가 존재하고, GROUP BY status 쿼리가
// index-only / heap-not-touched plan 을 사용하는지 검증한다.
func TestMigrate_0003_StatusIndex(t *testing.T) {
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
	t.Cleanup(func() { _ = testcontainers.TerminateContainer(pg) })

	dsn, err := pg.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("conn string: %v", err)
	}

	if err := db.ApplyMigrations(ctx, dsn, "../../migrations"); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}

	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close(ctx)

	var idxName string
	err = conn.QueryRow(ctx,
		`SELECT indexname FROM pg_indexes
		   WHERE schemaname = 'public'
		     AND tablename  = 'transfer_jobs'
		     AND indexname  = 'transfer_jobs_status_idx'`,
	).Scan(&idxName)
	if err != nil {
		t.Fatalf("status index missing: %v", err)
	}
	if idxName != "transfer_jobs_status_idx" {
		t.Fatalf("got idx %q, want transfer_jobs_status_idx", idxName)
	}
}
```

- [ ] **Step 3: 통합 테스트가 처음에 실패하는지 확인 (인덱스 미생성 상태)**

먼저 0003 마이그레이션 파일을 임시로 비활성화한 채 테스트를 실행한다.

Run: `mv migrations/0003_jobs_status_index.up.sql /tmp/0003.up.sql.bak`

Run: `go test -tags integration -run TestMigrate_0003_StatusIndex -v ./internal/db/`

Expected: FAIL — `status index missing` 메시지.

- [ ] **Step 4: 마이그레이션 복원 후 테스트 통과 확인**

Run: `mv /tmp/0003.up.sql.bak migrations/0003_jobs_status_index.up.sql`

Run: `go test -tags integration -run TestMigrate_0003_StatusIndex -v ./internal/db/`

Expected: PASS.

---

## Task 3: 쿼리 plan 검증 — index-only scan 확인

**Files:** none (CLI 검증)

이 task 는 spec §9 검증 항목 6 (`EXPLAIN ANALYZE SELECT status, COUNT(*) FROM transfer_jobs GROUP BY status` 가 index-only 인지) 을 만족시키기 위한 수동 / 반자동 단계다. 자동 어설션은 PG 버전 / 통계 영향이 커서 Task 2 의 인덱스 존재 어설션으로 충분하지만, 머지 직전 운영 DB 와 유사한 데이터 분포에서 plan 을 한 번 찍어 확인한다.

- [ ] **Step 1: 운영 / staging DB 에 fixture 분포 확보**

대표 분포 예시 (운영 평균 대략): pending 100, leased 5, succeeded 1,000,000, skipped 50,000, dead 200. 실 운영 DB 에 0003 을 적용하기 전에 staging 에서 다음을 확인.

Run (staging psql):

```sql
EXPLAIN ANALYZE
  SELECT status, COUNT(*) FROM transfer_jobs GROUP BY status;
```

Expected (인덱스 적용 후 plan 발췌):
```
HashAggregate  (... rows=5 ...)
  Group Key: status
  ->  Index Only Scan using transfer_jobs_status_idx on transfer_jobs
        Heap Fetches: <낮음>
```

핵심:
- `Index Only Scan` 키워드가 plan 에 등장한다.
- `Seq Scan on transfer_jobs` 는 등장하지 않는다.

- [ ] **Step 2: 인덱스 적용 전 / 후 비교 (선택)**

Run (staging, 인덱스 drop 한 임시 세션):

```sql
BEGIN;
DROP INDEX transfer_jobs_status_idx;
EXPLAIN ANALYZE SELECT status, COUNT(*) FROM transfer_jobs GROUP BY status;
ROLLBACK;
```

Expected: `Seq Scan on transfer_jobs` plan, execution time 이 인덱스 있을 때보다 한 자릿수 이상 길다. ROLLBACK 으로 인덱스 복구.

- [ ] **Step 3: 검증 결과 PR 본문에 첨부**

PR 본문에 두 EXPLAIN ANALYZE 출력 (인덱스 있음 / 없음) 을 붙여 리뷰어가 효과를 확인할 수 있게 한다.

---

## Task 4: 커밋 + PR

- [ ] **Step 1: 커밋**

```bash
git add migrations/0003_jobs_status_index.up.sql \
        migrations/0003_jobs_status_index.down.sql \
        internal/db/migrate_integration_test.go
git commit -m "$(cat <<'EOF'
feat(db): add transfer_jobs.status b-tree index (0003)

Phase 1.5 of monitoring-stack-integration. The phase 1 metric
imgsync_jobs_in_status scrapes
  SELECT status, COUNT(*) FROM transfer_jobs GROUP BY status
which falls back to a seq scan once succeeded/skipped rows accumulate.
Adding a single-column b-tree on status lets PG resolve the GROUP BY
via index-only scan + HashAggregate, keeping the 2s scrape budget cheap.

- migrations/0003_jobs_status_index.up.sql / down.sql
- internal/db/migrate_integration_test.go: assert idx exists post-migrate
EOF
)"
```

(Step 2 의 신규 파일 생성을 건너뛴 케이스라면 `internal/db/...` 경로는 빼고 add.)

- [ ] **Step 2: PR 생성**

```bash
gh pr create --title "feat(db): add transfer_jobs.status b-tree index (phase 1.5)" --body "$(cat <<'EOF'
## Summary
- Adds 0003 migration creating `transfer_jobs_status_idx` b-tree.
- Required follow-up to phase 1 metrics (`imgsync_jobs_in_status` scrape SQL).
- Must merge in the same sprint as the phase 1 PR — the metric scrape gets expensive without this index once succeeded/skipped rows accumulate.

## Test plan
- [ ] `make test` (unit) — no impact, but sanity check
- [ ] `go test -tags integration -run TestMigrate_0003_StatusIndex -v ./internal/db/`
- [ ] Staging EXPLAIN ANALYZE confirms `Index Only Scan using transfer_jobs_status_idx` (output pasted below)
- [ ] Roll forward + roll back validated against staging clone

## EXPLAIN ANALYZE (paste here)

\`\`\`
-- with index
HashAggregate ...
  ->  Index Only Scan using transfer_jobs_status_idx on transfer_jobs ...
\`\`\`

\`\`\`
-- without index (control)
HashAggregate ...
  ->  Seq Scan on transfer_jobs ...
\`\`\`

🤖 Generated with [Claude Code](https://claude.com/claude-code)
EOF
)"
```

---

## Notes for the implementer

- **순서 보장:** `0003_*` 라는 파일명만 지키면 `internal/db.ApplyMigrations` 의 `sort.Strings(files)` 가 알아서 마지막에 실행한다. 새 SQL 안에서 다른 마이그레이션 결과에 의존하지 마라 — 단일 인덱스 생성뿐이라 의존 자체가 없지만 원칙으로 기억.
- **DDL 는 트랜잭션 안:** PG 는 DDL 도 transactional 이라 `BEGIN; ... COMMIT;` 안에 두는 기존 컨벤션 (`0001_initial.up.sql`, `0002_sniffer_state.up.sql`) 을 따랐다. 부분 실패 시 `schema_migrations` 가 깨지지 않는다.
- **CONCURRENTLY 안 씀:** `CREATE INDEX CONCURRENTLY` 는 BEGIN/COMMIT 안에서 못 쓰고 (PG 제약), 이번 인덱스는 `transfer_jobs` 의 단일 컬럼이라 운영 테이블에서도 짧은 잠금 (수 백 ms 미만 추정 — 행 수 100 만 단위) 으로 끝난다. 인덱스 잠김 시간이 불안하면 별도 운영 절차로 `CREATE INDEX CONCURRENTLY` 를 수동 실행 후 `INSERT INTO schema_migrations` 만 마이그레이션이 채우는 변종을 고려하라. **이번 plan 은 단순한 single-statement DDL 로 진행.**
- **롤백 순서:** down 은 인덱스만 DROP. 다른 데이터 변형 없음. 마이그레이션 자체가 forward-only 이므로 (`internal/db/migrate.go`) down 파일은 운영자가 수동으로 돌릴 때만 의미가 있다.
- **Phase 1 plan 과의 머지 순서:** Phase 1 (메트릭) PR 이 먼저 머지되어도 무방하다 — `jobs_in_status` 가 풀 scan 으로 떨어져도 2 초 timeout 이 있어 `/metrics` 가 망가지지는 않는다. 다만 같은 sprint 안에 Phase 1.5 도 머지해서 scrape 비용을 정상화하는 것이 spec 의 결정 사항이다.
