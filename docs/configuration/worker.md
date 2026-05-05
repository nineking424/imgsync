# Worker 설정

Worker 서브커맨드는 `transfer_jobs` 큐에서 작업을 꺼내 실제 파일 전송을 수행합니다.
환경 변수 전체 목록은 [환경 변수](environment-variables.md) 페이지를 참고하세요.

## 병렬도 (`IMGSYNC_WORKERS`)

`IMGSYNC_WORKERS` 는 pod 하나가 동시에 처리하는 goroutine 수입니다 (기본값: `4`).

튜닝 지침:

- **CPU 바운드 작업(localfs 복사):** 코어 수의 1~2배가 일반적입니다.
- **IO 바운드 작업(FTP):** 대기 시간이 길기 때문에 코어보다 더 많이 설정해도 됩니다. `IMGSYNC_WORKERS=16` 정도에서 시작해 DB 연결 수(`pgxpool` max_conns)와 FTP 세션 수(`IMGSYNC_FTP_MAX_PER_HOST`)가 병목이 되는지 확인하세요.

```bash
# IO 바운드 예시 — 8코어 노드에서 FTP 전송이 많은 경우
IMGSYNC_WORKERS=20
IMGSYNC_FTP_MAX_PER_HOST=8
```

## FTP 풀 4개 변수

| 변수 | 기본값 | 의미 |
|---|---|---|
| `IMGSYNC_FTP_MAX_PER_HOST` | `4` | pod 내 호스트당 최대 동시 FTP 세션 (in-process pool) |
| `IMGSYNC_FTP_IDLE_TTL_SEC` | `300` | 유휴 세션을 닫기 전 대기 시간(초) |
| `IMGSYNC_FTP_NOOP_AFTER_SEC` | `60` | NOOP 명령으로 세션 유지(keepalive) 주기(초) |
| `IMGSYNC_FTP_HOST_CAP` | `8` | 클러스터-와이드 호스트 동시 처리 cap (advisory lock) |

### 운영 사례

**서버가 짧게 연결을 끊는 경우** — `IMGSYNC_FTP_IDLE_TTL_SEC` 를 줄여 유휴 세션을 먼저 닫아 버리면 서버측 타임아웃에 의한 강제 끊김을 피할 수 있습니다.

```bash
IMGSYNC_FTP_IDLE_TTL_SEC=60   # 기본 300s → 60s로 단축
```

**peak 시간에 NOOP 오버헤드를 줄이고 싶은 경우** — 세션 keepalive 주기를 늘립니다. 단, 너무 길면 유휴 세션이 서버에 의해 끊길 수 있습니다.

```bash
IMGSYNC_FTP_NOOP_AFTER_SEC=120   # 기본 60s → 120s
```

## Idle Backoff (`idleSleepMin` / `idleSleepMax`)

`values.yaml` 의 `worker.idleSleepMin` / `worker.idleSleepMax` 는 큐가 비었을 때 워커가 DB를 재조회하기 전 대기하는 시간의 하한/상한입니다 (기본: `50ms` / `1s`).

```yaml
worker:
  idleSleepMin: "50ms"
  idleSleepMax: "1s"
```

의도: 큐가 비어있는 기간에 DB 부하를 줄이기 위해 지수 백오프합니다. 최대값(`1s`)을 초과하면 첫 번째 새 작업의 처리 지연이 최대 `1s` 늘어나므로, 지연에 민감한 경우 `idleSleepMax` 를 줄이세요.

## 호스트 Cap 과 replicaCount 의 상호작용

`IMGSYNC_FTP_HOST_CAP` 은 PostgreSQL advisory lock 으로 **클러스터 전체** 에서 동일 FTP 호스트에 동시 접속하는 워커 수를 제한합니다.

- `replicaCount` 를 늘려 pod 가 더 생겨도, cap 이 묶기 때문에 **FTP 서버 입장의 동시 연결 수는 바뀌지 않습니다.** 이것은 의도된 동작입니다.
- PDB(`pdb.maxUnavailable`) 와 cap 을 함께 설계할 때: rolling update 중 남은 pod 들이 cap 을 꽉 채워 처리를 이어갈 수 있도록 `replicaCount - pdb.maxUnavailable` 대비 `IMGSYNC_FTP_HOST_CAP` 을 여유있게 설정하세요.

```yaml
# 예: replicaCount=3, pdb.maxUnavailable=1 → 최소 2 pod 보장
# HOST_CAP=8이면 pod 1개당 최대 4 세션까지 사용 가능
replicaCount: 3
pdb:
  maxUnavailable: 1
```

## 관련 페이지

- 전체 환경 변수 → [environment-variables.md](environment-variables.md)
- Helm 설치 파라미터 → [../installation/helm.md](../installation/helm.md)
