---
title: E2E 매뉴얼 검증 가이드
---

# E2E 매뉴얼 검증 가이드

이 페이지는 imgsync 의 핵심 시나리오(C7 처리량, F5a/b/c 회복, C5' sniffer)를 kind 클러스터에서 수동으로 검증하는 절차서다. 자동화된 E2E 는 `make e2e-throughput` / `make e2e-dirty-state` / `make e2e-sniffer` 로 실행된다 — 이 매뉴얼은 자동화가 잡아내지 못하는 운영 시나리오를 사람의 눈으로 확인할 때 쓴다.

`go test -tags e2e ./e2e/...` 가 자동으로 수행하는 시나리오를, 사람이 손으로
따라가며 동일한 불변식(invariant)을 직접 확인하는 절차다. 자동 테스트를 못 돌리는
환경 (CI 토큰 만료, 격리망, 인프라 실험) 또는 새 시나리오를 시운전할 때 쓴다.

각 시나리오 끝의 **검증 체크리스트**를 모두 통과하면 자동 테스트의 PASS 와 동치다.
하나라도 실패하면 회귀(regression) 의심하고 보고할 것.

---

## 0. 사전 준비

### 0.1 도구 버전

| 도구       | 권장 버전 | 확인 명령                            |
|------------|-----------|--------------------------------------|
| Go         | ≥ 1.25    | `go version`                         |
| Docker     | ≥ 24.0    | `docker version --format {{.Server.Version}}` |
| kind       | ≥ 0.24    | `kind version`                       |
| kubectl    | ≥ 1.30    | `kubectl version --client=true`      |
| helm       | ≥ 3.14    | `helm version --short`               |
| psql       | ≥ 14      | `psql --version` (선택, SQL 직타용)  |

### 0.2 디스크 / 메모리

- 호스트 `/tmp/imgsync-e2e-localfs` 가 비어 있어야 한다. 잔여물이 있으면 새 fixture
  생성이 skip 되어 결과가 흐려진다.
  ```bash
  sudo rm -rf /tmp/imgsync-e2e-localfs && mkdir -p /tmp/imgsync-e2e-localfs
  ```
- C7 throughput 시나리오는 1000개 × 10MB ≈ 10GB 의 파일을 host 디스크에 쓴다.
  여유 공간 ≥ 15GB 권장. C5' / F5 만 돌릴 거면 1KB×1000 = 2MB 면 충분하다.
- kind cluster 는 control-plane 1 + worker 3 노드를 띄운다.
  컨테이너 메모리 합산 ≈ 4GB 잡고 출발.

### 0.3 리포지토리 위치

이 문서의 모든 명령은 리포지토리 루트(`make` 가 동작하는 디렉토리)에서 실행한다고
가정한다.

```bash
cd /path/to/imgsync
git status   # working tree clean 권장 (uninstall/reinstall 시 불필요한 diff 방지)
```

---

## 1. 클러스터 부트스트랩

이미지 빌드 + kind 클러스터 생성 + Helm install 까지 한 번에 떨어진다. 시나리오를
바꿔 가며 여러 번 돌려도 idempotent.

```bash
make e2e-up
```

내부에서 일어나는 일 (디버깅 시 참고):

1. `kind create cluster --name imgsync-e2e --config e2e/kind_config.yaml`
   - 4-node cluster (1 cp + 3 worker), 모든 노드가 host `/tmp/imgsync-e2e-localfs`
     를 `/srv/imgsync` 로 마운트
2. `docker build -t imgsync:e2e .` → `kind load docker-image imgsync:e2e`
3. `kubectl apply -f e2e/manifests/{nfs-pv,postgres,source-postgres}.yaml`
4. `imgsync-dsn`, `imgsync-db-dsn`, `imgsync-source-dsn` Secret 생성
5. `helm upgrade --install imgsync deploy/helm/imgsync` (replicas=2, sniffer 활성)

### 1.1 부트스트랩 검증

```bash
# 모든 Deployment 가 ready 인가
kubectl -n imgsync-e2e get deploy
# 기대: postgres / source-postgres / imgsync 모두 READY 1/1, imgsync 는 2/2
```

```bash
# pre-install migrate Job 의 흔적 (성공 시 hook-succeeded 로 reaped 되어 보이지 않을 수도 있음)
kubectl -n imgsync-e2e get jobs
# 기대: 빈 출력 또는 imgsync-migrate Completed
```

```bash
# 워커가 살아 있는가
kubectl -n imgsync-e2e logs -l app.kubernetes.io/name=imgsync --tail=30
# 기대: "lease loop started" / "no jobs to lease" 같은 line
```

### 1.2 DB 핸들 만들기 (시나리오 공통 준비)

이후 시나리오들은 control-plane DB 와 source DB 두 곳을 본다. 두 개 별도 터미널에서
포트포워드를 띄워두는 게 가장 편하다.

```bash
# 터미널 A — imgsync 의 control DB
kubectl -n imgsync-e2e port-forward svc/postgres 5433:5432
```
```bash
# 터미널 B — sniffer 가 보는 source DB (C5' 시나리오에서만 필요)
kubectl -n imgsync-e2e port-forward svc/source-postgres 5434:5432
```

연결 확인:
```bash
psql 'postgres://imgsync:imgsync@127.0.0.1:5433/imgsync?sslmode=disable' -c '\dt'
# 기대: transfer_jobs, transfer_events, schema_migrations, sniffer_state ...
```

---

## 2. 시나리오 C7 — 처리량 선형성 (replicas 2 → 8)

자동 테스트: `e2e/throughput_test.go::TestC7_ThroughputScaleOut`
make 타깃: `make e2e-throughput`

### 2.1 목적

레플리카를 4배 늘렸을 때 처리량도 ≥ 3.2× 늘어나는지 (linear scaling 의 80% 이상)
확인한다.

### 2.2 절차

#### Phase A — replicas=2 로 1000 jobs 처리

1. fixture 1000개 (10MB × 1000 = 10GB) 가 호스트에 준비됐는지 확인:
   ```bash
   ls /tmp/imgsync-e2e-localfs/src | wc -l
   # 기대: 1000 (없으면 자동 테스트가 처음 1회 채워준다 — 매뉴얼이라면 아래 시드 헬퍼 사용)
   ```
   수동 시드용:
   ```bash
   mkdir -p /tmp/imgsync-e2e-localfs/{src,dst}
   for i in $(seq -w 00001 01000); do
     dd if=/dev/zero of=/tmp/imgsync-e2e-localfs/src/file-${i}.bin \
        bs=1M count=10 status=none 2>/dev/null || true
   done
   ```

2. 깨끗한 출발 — 잔여 jobs 와 dst 정리:
   ```bash
   psql 'postgres://imgsync:imgsync@127.0.0.1:5433/imgsync?sslmode=disable' -c \
     'TRUNCATE transfer_jobs, transfer_events RESTART IDENTITY CASCADE'
   rm -rf /tmp/imgsync-e2e-localfs/dst && mkdir -p /tmp/imgsync-e2e-localfs/dst
   ```

3. replicas=2 보장:
   ```bash
   helm -n imgsync-e2e upgrade --install imgsync deploy/helm/imgsync \
     --set image.repository=imgsync --set image.tag=e2e \
     --set image.pullPolicy=IfNotPresent --set replicaCount=2 \
     --wait --timeout 5m
   kubectl -n imgsync-e2e rollout status deploy/imgsync
   ```

4. 1000 jobs 일괄 enqueue:
   ```bash
   psql 'postgres://imgsync:imgsync@127.0.0.1:5433/imgsync?sslmode=disable' <<'SQL'
   INSERT INTO transfer_jobs
     (trace_id, src, dst, src_protocol, dst_protocol, status, attempts, max_attempts, payload, next_run_at)
   SELECT
     'phaseA-' || lpad(i::text, 5, '0'),
     '/srv/imgsync/src/file-' || lpad(i::text, 5, '0') || '.bin',
     '/srv/imgsync/dst/file-' || lpad(i::text, 5, '0') || '.bin',
     'localfs', 'localfs', 'pending', 0, 5, '{}'::jsonb, NOW()
   FROM generate_series(1, 1000) AS i
   ON CONFLICT (trace_id, dst) DO NOTHING;
   SQL
   ```

5. 시작 시각 기록 + 완료 폴링:
   ```bash
   START_A=$(date +%s)
   while true; do
     N=$(psql -At 'postgres://imgsync:imgsync@127.0.0.1:5433/imgsync?sslmode=disable' -c \
         "SELECT count(*) FROM transfer_jobs WHERE status='succeeded'")
     echo "$(date +%T) succeeded=$N"
     [ "$N" -ge 1000 ] && break
     sleep 1
   done
   END_A=$(date +%s)
   DUR_A=$(( END_A - START_A ))
   TPUT_A=$(awk "BEGIN{printf \"%.2f\", 1000 / $DUR_A}")
   echo "Phase A: 1000 jobs in ${DUR_A}s → ${TPUT_A} jobs/sec"
   ```

#### Phase B — replicas=8 로 동일 부하 재실행

6. 다시 깨끗한 출발:
   ```bash
   psql 'postgres://imgsync:imgsync@127.0.0.1:5433/imgsync?sslmode=disable' -c \
     'TRUNCATE transfer_jobs, transfer_events RESTART IDENTITY CASCADE'
   rm -rf /tmp/imgsync-e2e-localfs/dst && mkdir -p /tmp/imgsync-e2e-localfs/dst
   ```

7. 8 replica 로 스케일아웃 + 5분 SLO 측정:
   ```bash
   SCALE_START=$(date +%s)
   helm -n imgsync-e2e upgrade --install imgsync deploy/helm/imgsync \
     --set image.repository=imgsync --set image.tag=e2e \
     --set image.pullPolicy=IfNotPresent --set replicaCount=8 \
     --wait --timeout 5m
   kubectl -n imgsync-e2e rollout status deploy/imgsync
   SCALE_END=$(date +%s)
   echo "Scale 2→8 ready in $((SCALE_END - SCALE_START))s"   # 기대: ≤ 300
   ```

8. Phase B enqueue + 측정 (Phase A와 동일하되 prefix 만 `phaseB-` 로 바꾼다):
   ```bash
   psql 'postgres://imgsync:imgsync@127.0.0.1:5433/imgsync?sslmode=disable' <<'SQL'
   INSERT INTO transfer_jobs
     (trace_id, src, dst, src_protocol, dst_protocol, status, attempts, max_attempts, payload, next_run_at)
   SELECT
     'phaseB-' || lpad(i::text, 5, '0'),
     '/srv/imgsync/src/file-' || lpad(i::text, 5, '0') || '.bin',
     '/srv/imgsync/dst/file-' || lpad(i::text, 5, '0') || '.bin',
     'localfs', 'localfs', 'pending', 0, 5, '{}'::jsonb, NOW()
   FROM generate_series(1, 1000) AS i
   ON CONFLICT (trace_id, dst) DO NOTHING;
   SQL
   START_B=$(date +%s)
   # (Phase A 와 같은 폴링 루프)
   END_B=$(date +%s)
   DUR_B=$(( END_B - START_B ))
   TPUT_B=$(awk "BEGIN{printf \"%.2f\", 1000 / $DUR_B}")
   RATIO=$(awk "BEGIN{printf \"%.2f\", $TPUT_B / $TPUT_A}")
   echo "Phase B: ${DUR_B}s → ${TPUT_B} jobs/sec, ratio=${RATIO}"
   ```

### 2.3 검증 체크리스트

- [ ] Phase A 가 15분 안에 1000건 모두 `succeeded`
- [ ] Phase B 가 15분 안에 1000건 모두 `succeeded`
- [ ] `RATIO = tputB / tputA ≥ 3.20` (선형성 80% 이상)
- [ ] Scale 2→8 ready 가 5분 이내
- [ ] 종료 시점 `dead = 0`
  ```sql
  SELECT count(*) FROM transfer_jobs WHERE status='dead'; -- 0
  ```

---

## 3. 시나리오 F5a — Mid-flight 워커 강제 종료 후 sweeper 회복

자동 테스트: `e2e/dirty_state_test.go::TestF5_DirtyStateRecovery/F5a_mid_flight_kill`

### 3.1 목적

워커 한 대를 SIGKILL 로 떨어뜨려도 sweeper 가 leased→pending 으로 회복시켜
모든 잡이 결국 `succeeded` 로 끝나는지 확인. 사라진 잡이 0, 좀비 leased 도 0.

### 3.2 절차

1. replicas=2 보장 + 깨끗한 출발 (§2.2 단계 2,3 동일).

2. 100건 enqueue:
   ```bash
   psql 'postgres://imgsync:imgsync@127.0.0.1:5433/imgsync?sslmode=disable' <<'SQL'
   INSERT INTO transfer_jobs
     (trace_id, src, dst, src_protocol, dst_protocol, status, attempts, max_attempts, payload, next_run_at)
   SELECT
     'f5a-' || lpad(i::text, 5, '0'),
     '/srv/imgsync/src/file-' || lpad(i::text, 5, '0') || '.bin',
     '/srv/imgsync/dst/file-' || lpad(i::text, 5, '0') || '.bin',
     'localfs', 'localfs', 'pending', 0, 5, '{}'::jsonb, NOW()
   FROM generate_series(1, 100) AS i
   ON CONFLICT (trace_id, dst) DO NOTHING;
   SQL
   ```

3. 적어도 1건이 `leased` 가 될 때까지 기다린 뒤, leased id 들을 스냅샷:
   ```bash
   while true; do
     L=$(psql -At 'postgres://imgsync:imgsync@127.0.0.1:5433/imgsync?sslmode=disable' -c \
         "SELECT count(*) FROM transfer_jobs WHERE status='leased'")
     [ "$L" -gt 0 ] && break
     sleep 0.2
   done
   psql 'postgres://imgsync:imgsync@127.0.0.1:5433/imgsync?sslmode=disable' -c \
     "SELECT id, locked_by FROM transfer_jobs WHERE status='leased'"
   ```

4. 워커 한 대 강제 종료 (kubelet 우회, --grace-period=0):
   ```bash
   POD=$(kubectl -n imgsync-e2e get pods -l app.kubernetes.io/name=imgsync \
         -o jsonpath='{.items[0].metadata.name}')
   kubectl -n imgsync-e2e delete pod "$POD" --grace-period=0 --force
   ```
   (이걸 stash 해 둬야 step 6 의 비교가 의미 있다)

5. **시간 단축 트릭** — sweeper 의 기본 5분 임계를 기다리는 대신 leased 들의
   `locked_at` 을 6분 전으로 점프시킨다. (자동 테스트가 동일하게 한다)
   ```bash
   psql 'postgres://imgsync:imgsync@127.0.0.1:5433/imgsync?sslmode=disable' -c "
     UPDATE transfer_jobs
        SET locked_at = NOW() - INTERVAL '6 minutes'
      WHERE status='leased'"
   ```

6. 5분 안에 100건 모두 `succeeded` 가 될 때까지 폴링 (§2.2 단계 5 와 동일).

### 3.3 검증 체크리스트

- [ ] 5분 내 모든 100건이 `succeeded`
  ```sql
  SELECT count(*) FROM transfer_jobs WHERE status='succeeded'; -- 100
  ```
- [ ] `dead = 0`, `leased = 0`
  ```sql
  SELECT status, count(*) FROM transfer_jobs GROUP BY status;
  ```
- [ ] sweeper 가 회수한 잡이 1건 이상 + 그 잡들의 `attempts = 0` (재시도 카운터에
      찍히지 않아야 한다 — sweeper expire 는 retry 로 세지 않는 invariant):
  ```sql
  SELECT count(*) FROM transfer_jobs j
   WHERE j.status='succeeded' AND j.attempts=0
     AND EXISTS (
       SELECT 1 FROM transfer_events e
        WHERE e.trace_id=j.trace_id AND e.job_id=j.id AND e.status='expire');
  -- 기대: ≥ 1
  ```

---

## 4. 시나리오 F5b — 잘못된 helm upgrade → rollback 회복

자동 테스트: `e2e/dirty_state_test.go::TestF5_DirtyStateRecovery/F5b_bad_upgrade_then_rollback`

### 4.1 목적

존재하지 않는 이미지 태그로 helm upgrade 했을 때, `helm rollback` 만으로
이전 정상 상태로 돌아오고 in-flight job 이 잃지 않고 완료되는지 확인.

### 4.2 절차

1. 깨끗한 출발 + replicas=2:
   ```bash
   psql 'postgres://imgsync:imgsync@127.0.0.1:5433/imgsync?sslmode=disable' -c \
     'TRUNCATE transfer_jobs, transfer_events RESTART IDENTITY CASCADE'
   rm -rf /tmp/imgsync-e2e-localfs/dst && mkdir -p /tmp/imgsync-e2e-localfs/dst
   helm -n imgsync-e2e upgrade --install imgsync deploy/helm/imgsync \
     --set image.repository=imgsync --set image.tag=e2e \
     --set image.pullPolicy=IfNotPresent --set replicaCount=2 \
     --wait --timeout 5m
   ```

2. 50건 enqueue (§3.2 step 2 와 동일, `f5a-` 대신 `f5b-` 사용).

3. 좋은 빌드에서 10건 이상 성공할 때까지 기다리기 (warm-up):
   ```bash
   while true; do
     N=$(psql -At 'postgres://imgsync:imgsync@127.0.0.1:5433/imgsync?sslmode=disable' -c \
         "SELECT count(*) FROM transfer_jobs WHERE status='succeeded'")
     echo "warm-up succeeded=$N"
     [ "$N" -ge 10 ] && break
     sleep 0.5
   done
   PRE_BAD=$N
   ```

4. 망가진 upgrade — 존재하지 않는 태그로 push, 30s 후 timeout:
   ```bash
   helm -n imgsync-e2e upgrade --install imgsync deploy/helm/imgsync \
     --set image.repository=imgsync --set image.tag=does-not-exist \
     --set image.pullPolicy=IfNotPresent --set replicaCount=2 \
     --wait --timeout 30s || true   # 실패 정상
   ```
   상태 점검 (디버깅용):
   ```bash
   kubectl -n imgsync-e2e get pods -l app.kubernetes.io/name=imgsync
   # 기대: ImagePullBackOff 또는 ErrImagePull 인 신규 pod
   ```

5. rollback:
   ```bash
   helm -n imgsync-e2e rollback imgsync --wait --timeout 3m
   kubectl -n imgsync-e2e rollout status deploy/imgsync
   ```

6. 50건이 모두 `succeeded` 될 때까지 폴링 (5분 budget).

### 4.3 검증 체크리스트

- [ ] `helm history imgsync` 에서 bad revision 이 `failed`/`superseded`,
      직전 good revision 이 `deployed` 로 보임
- [ ] 5분 내 50건 모두 `succeeded`
- [ ] `dead = 0`, `leased = 0`

---

## 5. 시나리오 F5c — uninstall → reinstall 멱등 마이그레이션

자동 테스트: `e2e/dirty_state_test.go::TestF5_DirtyStateRecovery/F5c_uninstall_reinstall_idempotent_migration`

### 5.1 목적

`helm uninstall` 은 DB 를 건드리지 않는다. 잡 30건을 enqueue 한 뒤 워커가 일을
다 끝내기 전에 uninstall 했다가, 다시 install 하면 pre-install hook 이 멱등하게
migrate 를 다시 돌리고 잔여 잡을 워커가 마저 처리하는지 확인.

### 5.2 절차

1. 깨끗한 출발 + 30건 enqueue (`f5c-` prefix).
2. 빠른 uninstall (잡이 끝나기 전에):
   ```bash
   helm -n imgsync-e2e uninstall imgsync --wait --timeout 2m
   ```
3. uninstall 직후 DB 상태 캡처:
   ```bash
   psql 'postgres://imgsync:imgsync@127.0.0.1:5433/imgsync?sslmode=disable' -c "
     SELECT status, count(*) FROM transfer_jobs GROUP BY status"
   # 기대: pending+leased+succeeded == 30 (분포는 워커 속도에 따라 다름)
   ```
4. orphan leased 빠른 회복용 시간 점프 (자동 테스트와 동일):
   ```bash
   psql 'postgres://imgsync:imgsync@127.0.0.1:5433/imgsync?sslmode=disable' -c "
     UPDATE transfer_jobs SET locked_at = NOW() - INTERVAL '6 minutes'
      WHERE status='leased'"
   ```
5. reinstall (pre-install hook 이 migrate 를 다시 실행 — schema_migrations 의
   기존 row 와 부딪히지만 멱등이어야 한다):
   ```bash
   helm -n imgsync-e2e upgrade --install imgsync deploy/helm/imgsync \
     --set image.repository=imgsync --set image.tag=e2e \
     --set image.pullPolicy=IfNotPresent --set replicaCount=2 \
     --wait --timeout 5m
   ```
6. 5분 내 30건 모두 `succeeded` 폴링.

### 5.3 검증 체크리스트

- [ ] uninstall 직후: `pending+leased+succeeded == 30`, `dead = 0`
- [ ] reinstall 시 pre-install Job 이 `Completed` 로 종료
  ```bash
  kubectl -n imgsync-e2e get jobs
  ```
- [ ] 5분 내 30건 `succeeded`
- [ ] `dead = 0`, `leased = 0`

---

## 6. 시나리오 C5' — Sniffer 자가 감사

자동 테스트: `e2e/sniffer_test.go::TestC5Prime_SnifferSelfAudit`
make 타깃: `make e2e-sniffer`

### 6.1 목적

source DB 에 1000 행을 넣으면 sniffer 가 정확히 1000건을 `transfer_jobs` 로
enqueue 하고, `trace_id` 가 모두 distinct 하며, 워커가 shadow path 로 모두
복사하여 `dead = 0` 이 되는지 확인.

### 6.2 절차

1. source-postgres 포트포워드 띄우기 (§1.2 터미널 B):
   ```bash
   kubectl -n imgsync-e2e port-forward svc/source-postgres 5434:5432
   ```

2. helm 을 sniffer 용 설정으로 upgrade (`scripts/e2e-up-sniffer.sh` 가 같은 일을
   wrapper 로 해 준다 — 매뉴얼이라면 직접 실행):
   ```bash
   helm -n imgsync-e2e upgrade --install imgsync deploy/helm/imgsync \
     --set image.repository=imgsync --set image.tag=e2e \
     --set image.pullPolicy=IfNotPresent --set replicaCount=2 \
     --set sniffer.enabled=true \
     --set sniffer.config.intervalSec=5 \
     --set sniffer.config.shadow=true \
     --set sniffer.config.srcProtocol=localfs \
     --set sniffer.config.dstProtocol=localfs \
     --set "sniffer.config.srcPattern=/srv/imgsync/src/{{.file_path}}.bin" \
     --set "sniffer.config.dstPattern=/srv/imgsync/dst/{{.file_path}}.bin" \
     --wait --timeout 5m
   kubectl -n imgsync-e2e rollout status deploy/imgsync
   kubectl -n imgsync-e2e logs -l app.kubernetes.io/component=sniffer --tail=20
   # 기대: "sniffer started", interval=5s
   ```
   주의: 기본 chart values 의 srcProtocol/dstProtocol 은 `localfs` 로 박혀 있다
   (PR #6 에서 fix). `fs` 로 보이면 worker 에 등록 안 된 프로토콜이라 1000건이
   `dead` 로 떨어진다.

3. 깨끗한 출발 — control DB 와 sniffer watermark 모두 초기화:
   ```bash
   psql 'postgres://imgsync:imgsync@127.0.0.1:5433/imgsync?sslmode=disable' -c \
     'TRUNCATE transfer_jobs, transfer_events RESTART IDENTITY CASCADE'
   psql 'postgres://imgsync:imgsync@127.0.0.1:5433/imgsync?sslmode=disable' -c \
     'TRUNCATE sniffer_state'
   ```
   sniffer pod 한 번 재기동해서 새 watermark 로 다시 출발하게 함:
   ```bash
   kubectl -n imgsync-e2e rollout restart deploy/imgsync
   kubectl -n imgsync-e2e rollout status deploy/imgsync
   ```

4. 1KB fixture 미리 깔기 (worker 가 src 파일을 못 찾으면 `skipped` 로 빠진다):
   ```bash
   mkdir -p /tmp/imgsync-e2e-localfs/src
   for i in $(seq -w 00001 01000); do
     dd if=/dev/zero of=/tmp/imgsync-e2e-localfs/src/file-${i}.bin \
        bs=1024 count=1 status=none
   done
   ```

5. source DB 에 schema 와 1000행 insert (`updated_at` 은 sniffer 의 기본
   bias_sec=5 보다 크게 — 10s 전):
   ```bash
   psql 'postgres://source:source@127.0.0.1:5434/source?sslmode=disable' <<'SQL'
   CREATE TABLE IF NOT EXISTS images (
     id         BIGSERIAL PRIMARY KEY,
     updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
     file_path  TEXT        NOT NULL
   );
   TRUNCATE images RESTART IDENTITY;
   INSERT INTO images (updated_at, file_path)
   SELECT NOW() - INTERVAL '10 seconds',
          'file-' || lpad(i::text, 5, '0')
     FROM generate_series(1, 1000) AS i;
   SQL
   ```

6. drain 폴링 — sniffer 는 5초 간격으로 source 를 읽어 `transfer_jobs` 에
   enqueue 한다. 그리고 워커가 그것을 비운다. 두 조건 모두 만족할 때까지 폴링:
   ```bash
   while true; do
     read ENQ PEN DEAD <<<$(psql -At -F' ' \
       'postgres://imgsync:imgsync@127.0.0.1:5433/imgsync?sslmode=disable' -c "
       SELECT count(*),
              count(*) FILTER (WHERE status='pending'),
              count(*) FILTER (WHERE status='dead')
         FROM transfer_jobs")
     echo "$(date +%T) enqueued=$ENQ pending=$PEN dead=$DEAD"
     [ "$ENQ" -ge 1000 ] && [ "$PEN" -eq 0 ] && break
     sleep 3
   done
   ```

### 6.3 검증 체크리스트

- [ ] `enqueued = 1000`
  ```sql
  SELECT count(*) FROM transfer_jobs;
  ```
- [ ] `count(distinct trace_id) = 1000` (idempotent enqueue 가 깨지지 않았다는 뜻)
  ```sql
  SELECT count(DISTINCT trace_id) FROM transfer_jobs;
  ```
- [ ] `dead = 0`
- [ ] dst 가 shadow suffix 와 함께 실제로 존재:
  ```bash
  ls /tmp/imgsync-e2e-localfs/dst/file-00001.bin.imgsync_shadow_v1
  # 기대: 파일 존재 (size 1024)
  ```

### 6.4 추가 검증 — sniffer 의 멱등성 (선택)

같은 source 데이터로 sniffer 를 다시 한 바퀴 더 돌렸을 때 새 잡이 안 생기는지:

1. sniffer pod 만 살아있는 상태에서 60초 대기 (기본 interval 5s × 12회 폴 보장).
2. `transfer_jobs` 갯수가 여전히 1000 인지 확인.
3. `transfer_events` 에 동일 trace_id 의 enqueue 가 1회뿐인지 확인:
   ```sql
   SELECT trace_id, count(*) FROM transfer_events
    WHERE status='enqueue' GROUP BY trace_id HAVING count(*) > 1;
   -- 기대: 0 rows
   ```

---

## 7. 사후 정리

```bash
make e2e-down   # kind 삭제 + /tmp/imgsync-e2e-localfs 제거
```

수동으로 부분 정리만 하고 싶으면:
```bash
helm -n imgsync-e2e uninstall imgsync
kubectl delete namespace imgsync-e2e
kind delete cluster --name imgsync-e2e
sudo rm -rf /tmp/imgsync-e2e-localfs
```

---

## 8. 트러블슈팅

| 증상 | 의심 | 조치 |
|------|------|------|
| `helm install` 이 pre-install Job 에서 멈춤 | `imgsync` ServiceAccount 가 없음 | `e2e-up.sh` 가 SA 를 미리 만든다. 수동 진행 시 §0.3 의 SA YAML 을 apply |
| 모든 잡이 `dead` 로 빠짐 | `src_protocol`/`dst_protocol` 이 워커에 등록 안 됨 (`fs`) | `localfs` 또는 `ftp` 만 등록됨. helm `--set` 값 확인 |
| sniffer 가 enqueue 안 함 | `sniffer_state.last_updated_at` 이 너무 미래 | `TRUNCATE sniffer_state` 후 sniffer pod 재기동 |
| sniffer 가 일부만 enqueue 함 | `bias_sec` 윈도우에 가려짐 | source 의 `updated_at` 을 `NOW() - INTERVAL '10 seconds'` 이상으로 |
| `port-forward` 가 죽음 | 포드 재기동 후 svc 매핑 잠시 끊김 | 포트포워드 명령 재실행 (자동 테스트의 watchdog 이 같은 동작) |
| C7 ratio 가 3.2 미만 | 호스트 디스크가 한계 (모든 파드가 동일 hostPath I/O) | 호스트 SSD 확보, 또는 fixture 크기를 1MB 로 줄여 disk-bound 영향 축소 |

---

## 9. 참고

- 자동 E2E 의 정확한 SQL/타이밍은 `e2e/helpers.go`, `e2e/{sniffer,dirty_state,throughput}_test.go` 가
  최종 진실이다. 본 문서의 SQL 은 거기서 그대로 추출/번역한 것.
- 오퍼레이터 일상 운영은 [운영 런북](../operating/runbook.md) 참고.
- 최근 자동 E2E 결과는 CI 산출물 / 별도 보고서로 관리되며 본 리포에는 포함되지 않는다. 직접 재현하려면 위 §1–§8 의 절차를 그대로 따른다.
