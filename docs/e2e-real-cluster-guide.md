# imgsync — Real-Cluster E2E 매뉴얼 검증 가이드

`docs/e2e-manual-guide.md` 의 자매 문서. kind 가 아닌 운영자(homelab)에 이미
띄워진 실제 Kubernetes 클러스터(`admin@talos-homelab`)에 직접 imgsync 를
설치해서 동일한 5개 시나리오 — C7, F5a, F5b, F5c, C5' — 의 invariant 를
손으로 확인한다.

## 0. 사전 준비

### 0.1 클러스터 / kubectl

```bash
kubectl config current-context   # 기대: admin@talos-homelab (또는 동등 클러스터)
kubectl get nodes                # 기대: 모두 Ready
kubectl get sc                   # 기대: nfs-client (default) 가 존재
```

### 0.2 도구 버전

| 도구    | 권장 버전 | 확인                                |
|---------|-----------|-------------------------------------|
| Go      | ≥ 1.25    | `go version`                        |
| Docker  | ≥ 24      | `docker version --format '{{.Server.Version}}'` |
| kubectl | ≥ 1.30    | `kubectl version --client=true`     |
| helm    | ≥ 3.14    | `helm version --short`              |
| psql    | ≥ 14      | `psql --version`                    |
| gh      | ≥ 2.40    | `gh version`                        |

### 0.3 ghcr.io 로그인 (1회만)

이미지를 ghcr.io 에 push 해서 클러스터가 그걸 pull 하는 구조다. 다른 사람이
같은 클러스터에 이미 띄워둔 게 있다면 step 1 (이미지 push) 를 건너뛰고
바로 step 2 부터 시작해도 된다.

```bash
gh auth login --scopes write:packages   # PAT 가 이미 있으면 건너뛰기
gh auth token | docker login ghcr.io -u nineking424 --password-stdin
```

`Login Succeeded` 가 나오면 OK.

### 0.4 작업 디렉토리

이 문서의 모든 명령은 imgsync 리포지토리 루트에서 실행한다고 가정한다.

```bash
cd /path/to/imgsync
git status   # working tree clean 권장
```

## 1. 클러스터 부트스트랩

### 1.1 이미지 빌드 + push

```bash
make e2e-push-real
```

내부 동작:
1. `docker build -t ghcr.io/nineking424/imgsync:e2e-<sha> .`
2. `docker push ghcr.io/nineking424/imgsync:e2e-<sha>`

이 step 은 imgsync 코드가 바뀐 직후에만 다시 돌리면 된다.

### 1.2 환경 부트스트랩

```bash
make e2e-up-real
```

내부 동작:
1. namespace `imgsync-e2e-real` 생성
2. shared-localfs PVC + postgres + source-postgres apply, READY 대기
3. DSN secret 3개 생성 (control / sniffer-imgsync / sniffer-source)
4. ServiceAccount `imgsync` 사전 생성 (pre-install hook 의 SA reference 충족)
5. `helm upgrade --install imgsync deploy/helm/imgsync` (replicas=2, sniffer 활성)

### 1.3 부트스트랩 검증

```bash
kubectl -n imgsync-e2e-real get deploy
# 기대: postgres 1/1, source-postgres 1/1, imgsync 2/2
```

```bash
kubectl -n imgsync-e2e-real get pvc
# 기대: imgsync-localfs (RWX), postgres-data (RWO), source-postgres-data (RWO) 모두 Bound
```

```bash
kubectl -n imgsync-e2e-real logs -l app.kubernetes.io/name=imgsync --tail=20
# 기대: "lease loop started" / "no jobs to lease"
```

### 1.4 DB 핸들 (시나리오 공통 준비)

이후 시나리오는 control DB 와 source DB 두 곳을 본다. 별도 터미널 두 개에서
포트포워드를 띄워둔다.

```bash
# 터미널 A — imgsync control DB
kubectl -n imgsync-e2e-real port-forward svc/postgres 5433:5432
```

```bash
# 터미널 B — sniffer 가 보는 source DB (C5' 시나리오에서만 필요)
kubectl -n imgsync-e2e-real port-forward svc/source-postgres 5434:5432
```

연결 확인:
```bash
psql 'postgres://imgsync:imgsync@127.0.0.1:5433/imgsync?sslmode=disable' -c '\dt'
# 기대: transfer_jobs, transfer_events, schema_migrations, sniffer_state ...
```

### 1.5 시드 fixture (시나리오 공통 준비)

worker pod 가 읽을 source 파일을 NFS PVC 에 깔아둔다.

```bash
# C5' / F5a / F5b / F5c 용 — 1KB × 1000 = 2MB
make e2e-seed-real

# C7 throughput 용 — 1MB × 1000 = 1GB (별도)
./scripts/e2e-seed-real.sh 1000 1048576
```

검증:
```bash
kubectl -n imgsync-e2e-real run --rm -i --restart=Never \
  --image=alpine:3.20 ls-fixtures \
  --overrides='{"spec":{"containers":[{"name":"l","image":"alpine:3.20","command":["sh","-c","ls /srv/imgsync/src | wc -l"],"volumeMounts":[{"name":"v","mountPath":"/srv/imgsync"}]}],"volumes":[{"name":"v","persistentVolumeClaim":{"claimName":"imgsync-localfs"}}]}}' \
  --
# 기대: 1000
```

---

## 2. 시나리오 별 절차

각 시나리오는 별도 섹션으로 분리되어 있다 — 이 가이드의 §3~§7 참고.

본 가이드는 §3 부터 §7 까지 채워나가는 살아 있는 문서다. 새 시나리오를
추가할 때는 같은 형식 (목적 / 절차 / 검증 체크리스트) 을 유지한다.

(섹션 §3~§7 은 이 plan 의 Task 11~14 가 채운다.)

---

## 8. 사후 정리

```bash
make e2e-down-real
```

PVC 까지 (NFS 데이터 포함) 모두 회수한다 (`reclaimPolicy=Delete`). 부분 정리만
하고 싶으면:

```bash
helm -n imgsync-e2e-real uninstall imgsync
# (namespace 와 PVC 는 유지)
```

다음 번 시나리오 사이에 깨끗한 출발만 원하면:

```bash
psql 'postgres://imgsync:imgsync@127.0.0.1:5433/imgsync?sslmode=disable' -c \
  'TRUNCATE transfer_jobs, transfer_events RESTART IDENTITY CASCADE'

kubectl -n imgsync-e2e-real run --rm -i --restart=Never \
  --image=alpine:3.20 wipe-dst \
  --overrides='{"spec":{"containers":[{"name":"w","image":"alpine:3.20","command":["sh","-c","rm -rf /srv/imgsync/dst && mkdir -p /srv/imgsync/dst"],"volumeMounts":[{"name":"v","mountPath":"/srv/imgsync"}]}],"volumes":[{"name":"v","persistentVolumeClaim":{"claimName":"imgsync-localfs"}}]}}' \
  --
```

---

## 9. 트러블슈팅

| 증상 | 의심 | 조치 |
|------|------|------|
| pod ImagePullBackOff | ghcr.io 패키지가 private | `gh api -X PATCH /user/packages/container/imgsync/visibility -f visibility=public` |
| pre-install Job 멈춤 | SA `imgsync` 누락 | `e2e-up-real.sh` Step 4 의 SA YAML 다시 apply |
| 모든 잡 dead | `srcProtocol`/`dstProtocol` 가 `fs` | values-real.yaml 의 protocol 값을 `localfs` 로 |
| sniffer enqueue 안 함 | `sniffer_state.last_updated_at` 미래 | `TRUNCATE sniffer_state` 후 sniffer pod 재기동 |
| port-forward 끊김 | 포드 재기동 | port-forward 재실행 |
| C7 ratio 낮음 | NFS 대역폭 한계 | 본 cluster 에서 C7 는 smoke 만 (3.2x 미달성 정상) — `dead = 0` 만 확인 |

---

## 10. 참고

- 자동 e2e (kind) 의 정확한 SQL/타이밍은 `e2e/helpers.go`, `e2e/{sniffer,dirty_state,throughput}_test.go` 가 진실의 소스
- 운영자 일상은 `docs/runbook.md`
- 이 가이드의 자매 문서 (kind+helm 시나리오): `docs/e2e-manual-guide.md`
