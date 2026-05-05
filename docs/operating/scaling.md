# 스케일링

처리량을 늘리고 줄이는 두 축은 **replicaCount**(pod 개수)와 **per-pod IMGSYNC_WORKERS**(pod 안 goroutine 수)다. 어느 쪽을 어떻게 움직일지 결정하는 기준을 정리한다.

## 기본: replicaCount 늘리기

가장 흔한 시나리오. helm 으로 in-place 업그레이드한다.

```bash
helm upgrade imgsync deploy/helm/imgsync \
  --reuse-values --set replicaCount=8
```

확인 절차:

1. `kubectl -n <ns> get pods -l app.kubernetes.io/name=imgsync -w` 로 새 pod 가 Ready 가 되는지 본다. warm cache 면 1분 이내, cold image 면 5분 이내가 기대값이다.
2. 한 pod 에 port-forward 해서 `/healthz` 의 `last_lease_success_ts` 가 신선해지는지 확인한다 — [모니터링](monitoring.md#healthz-응답-구조).
3. `transfer_jobs` 의 `pending` 카운트가 단조 감소로 돌아서는지 — [런북 §7](runbook.md#7-sql) 의 상태별 카운트 SQL 참고.

스케일 업 자체는 무중단이다. 새 pod 는 기존 pod 와 동일한 lease 루프를 돌 뿐이며, `SELECT FOR UPDATE SKIP LOCKED` 가 중복 처리를 막는다.

## per-pod `IMGSYNC_WORKERS` vs replicaCount 트레이드오프

같은 동시 처리량 N 을 만드는 방법은 두 가지다.

| 방식 | 예시 | 장점 | 단점 |
|---|---|---|---|
| pod 수를 늘림 | `replicaCount=8`, `IMGSYNC_WORKERS=4` | 격리(한 pod OOM 이 다른 pod 에 영향 없음), 노드 분산이 자연스럽다, 롤링 배포 시 안전 | DB 커넥션이 pod 수 × pool size 로 곱해진다 |
| pod 안 goroutine 수를 늘림 | `replicaCount=2`, `IMGSYNC_WORKERS=16` | DB pool 효율 좋음, 메모리/이미지 풋프린트 작음 | 한 pod 가 죽으면 한꺼번에 N 개 lease 가 expire 처리 대상이 됨 |

권장 출발점:

- **CPU 바운드 이미지 처리가 많은 워크로드**: pod 단위 격리가 더 가치 있음. `replicaCount` 위주로 늘린다.
- **IO 바운드 (FTP) 워크로드**: 한 pod 안에서 goroutine 으로 동시성을 높이는 것이 효율적. `IMGSYNC_WORKERS=16~32` 까지 시도해 본 뒤 DB pool 또는 FTP host cap 이 병목이 되는지 확인한다.

`IMGSYNC_WORKERS` 의 자세한 가이드는 [Worker 설정](../configuration/worker.md) 을 본다.

## FTP host cap 과 동시 처리량

FTP 호스트 한 대에 imgsync 클러스터가 동시에 띄울 수 있는 세션 수는 `IMGSYNC_FTP_HOST_CAP` (기본 `8`) 가 advisory lock 으로 강제한다. 이 값이 **클러스터-와이드** 라는 점이 중요하다.

- replicaCount 를 8 로 늘려도, FTP host 가 1대뿐이고 cap=8 이라면 **모든 pod 합쳐서** 동시 8 세션이 상한이다. 9번째 lease 는 락을 잡지 못해 backoff 후 재시도한다.
- 처리량을 더 늘리려면 (a) cap 을 호스트가 받아낼 수 있는 만큼 올리거나, (b) FTP source 를 여러 호스트로 분산해야 한다.

E2E throughput 게이트(C7)는 LocalFS-only 환경에서 2→8 replicas 로 ≥3.2× 선형성을 검증한다. FTP 가 섞이면 cap 이 천장이 되므로 순수 LocalFS 보다 ratio 가 떨어진다 — 이는 결함이 아니라 cap 의 의도된 동작이다.

`IMGSYNC_FTP_HOST_CAP` 자체는 host 단위 (host 별로 advisory lock key 가 다름) 이므로, 호스트 N 개 × cap M 이면 클러스터 전체로 N × M 동시 세션이 가능하다.

## 스케일 다운 시 graceful drain

replicaCount 를 줄이면 Kubernetes 가 SIGTERM 을 보낸다. 워커는 in-flight `Source.Open → Transport.Send` 가 끝날 때까지 기다린 뒤 종료한다 (`terminationGracePeriodSeconds = 60s`).

진행 중 작업을 완전히 종료시키고 깨끗이 비우고 싶다면:

```bash
# 1. 새 lease 수락 중지
helm upgrade imgsync deploy/helm/imgsync --reuse-values --set replicaCount=0

# 2. 60초 grace + 5분 sweeper threshold 합산 대기
sleep 360

# 3. 다시 띄움
helm upgrade imgsync deploy/helm/imgsync --reuse-values --set replicaCount=4
```

`sleep 360` 의 근거는 [런북 §6](runbook.md#6-drain) 에 같이 적혀 있다. 짧게 끊으면 회수되지 못한 lease 가 다음 release 에서 stuck 으로 보인다.

drain 없이 단순히 replicaCount 를 줄이는 경우라도, 손실되는 작업은 없다. 잡혀 있던 lease 는 60초 grace 안에 끝나거나, 못 끝나면 sweeper 가 5분 뒤 회수해 다른 pod 가 다시 lease 한다 (idempotent ETag-aware Send 덕분에 부분 전송분은 유실되지 않는다).

## PDB 와 함께 스케일 다운하기

차트는 `podDisruptionBudget` 을 기본 활성화한다. `kubectl drain` 같은 voluntary disruption 시 PDB 를 위반하지 않도록 한 번에 빠지는 pod 수가 제한된다 — 의도된 동작이다. PDB 가 helm upgrade 진행을 막는 것 같으면 [트러블슈팅](troubleshooting.md) 의 "PDB 때문에 helm upgrade 가 진행되지 않음" 항목을 본다.
