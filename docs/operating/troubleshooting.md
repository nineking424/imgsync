# 트러블슈팅

증상 → 원인 → 진단 → 조치 매트릭스. 각 항목에 진단용 한 줄(SQL 또는 kubectl)이 들어 있다. 더 깊은 절차는 해당 항목에서 [런북](runbook.md) 의 절로 점프한다.

## 작업이 영원히 pending 상태

**원인.** 워커가 lease 를 시도하지 못하고 있다. 흔한 이유: pod 가 0개거나, 모두 NotReady 거나, DB pool 이 포화.

**진단.**

```bash
kubectl -n <ns> get pods -l app.kubernetes.io/name=imgsync
```

Ready 가 0/N 이거나 pod 가 아예 없다면 거기서 끝. Ready 인데도 pending 이 줄지 않으면 한 pod 의 `/healthz` 의 `last_lease_attempt_ts` 가 신선한지 본다 — 60초 이상 오래됐다면 lease 루프가 멈춘 것.

**조치.** Pod 가 0개면 `helm upgrade --reuse-values --set replicaCount=N` 으로 띄운다. `last_lease_attempt_ts` 가 멈춰 있으면 해당 pod 를 `kubectl delete pod` 로 재시작한다. DB pool 포화면 [모니터링](monitoring.md) 의 `pool_in_use` 알람을 참고해 `IMGSYNC_WORKERS` 또는 `pgxpool` max 를 조정한다.

## leased 인데 worker 가 안 보임

**원인.** lease 를 잡았던 pod 가 죽었거나 네트워크 분단으로 사라졌다. Sweeper 가 5분 뒤 회수해야 한다.

**진단.**

```sql
SELECT id, locked_by, locked_at, NOW() - locked_at AS held_for
  FROM transfer_jobs
 WHERE status = 'leased' AND locked_at < NOW() - INTERVAL '5 minutes'
 ORDER BY locked_at;
```

`held_for` 가 5분이 한참 넘었는데도 행이 그대로면 sweeper 가 멈췄다는 신호.

**조치.** [런북 §3](runbook.md#3-stuck) 절차로 점프. `/healthz` 의 `last_sweep_ts` 가 오래된 경우 leader 락을 잡고 있는 pod 를 찾아 재시작한다 (sweeper 는 pod 하나에서만 돈다).

## FTP 연결 실패가 반복됨

**원인.** FTP 호스트의 일시적 거부 / 인증 만료 / host cap 초과 / 네트워크 ACL 변경.

**진단.**

```sql
SELECT j.dst, e.detail, COUNT(*)
  FROM transfer_events e JOIN transfer_jobs j ON j.id = e.job_id
 WHERE e.status = 'fail' AND e.ts > NOW() - INTERVAL '15 minutes'
 GROUP BY j.dst, e.detail
 ORDER BY 3 DESC;
```

`detail` 의 `stage` 가 `source-factory` / `transport-factory` 면 인증 또는 connect 단계, `open` / `send` 면 데이터 전송 단계. 같은 host 가 반복되면 호스트 단위 문제.

**조치.** 인증이면 `imgsync-ftp` Secret 갱신 후 pod 재시작. ACL 이면 네트워크팀과 협의. host cap 이 의심되면 [스케일링](scaling.md) 의 FTP host cap 절을 본다.

## sniffer 가 같은 행을 반복 enqueue

**원인.** sniffer 가 high-watermark 를 진척시키지 못한다 — sink 가 INSERT 를 idempotent 하게 무시(unique violation)하고 있어 sniffer 입장에서는 정상이지만, 결과적으로 의도와 다르게 같은 입력을 계속 본다.

**진단.**

```sql
-- 최근 1시간 동안 같은 (trace_id, dst) 가 enqueue 이벤트를 두 번 이상 만들었는지
SELECT j.trace_id, j.dst, COUNT(*) AS enqueue_attempts
  FROM transfer_events e JOIN transfer_jobs j ON j.id = e.job_id
 WHERE e.status = 'enqueue' AND e.ts > NOW() - INTERVAL '1 hour'
 GROUP BY j.trace_id, j.dst HAVING COUNT(*) > 1
 ORDER BY 3 DESC LIMIT 50;
```

`enqueue` 이벤트는 unique violation 이 일어나도 기록되지 않으므로 행이 잡힌다는 건 sink 가 실제로 새 INSERT 를 만들었다는 뜻이다. 그런데도 input shape 이 동일하다면 sniffer 의 watermark 또는 dedup key 설정을 의심한다.

**조치.** `sniffer` Config 의 watermark 컬럼/키가 입력의 단조성을 보존하는지 확인 — [Sniffer 설정](../configuration/sniffer.md). dedup 키가 너무 좁으면 같은 dst 가 다른 trace 로 반복된다.

## migrate Job 이 멈춤 / 실패

**원인.** 마이그레이션이 lock 을 기다리거나, SQL 자체가 실패하거나, image pull 이 안 됨.

**진단.**

```bash
kubectl -n <ns> logs job/imgsync-migrate-<revision> --tail=200
```

`pq: ... is being accessed by other users` 면 누가 advisory lock 을 잡고 있는 것. SQL 에러면 그대로 표시된다. ImagePullBackOff 면 `kubectl describe job` 의 events 를 본다.

**조치.** lock 충돌이면 다른 migrate 가 끝나길 기다린다 (실패하지 않게 두는 것이 안전). SQL 에러는 forward-only 정책상 새 패치를 만들어 다음 helm upgrade 에서 적용한다 — 자세한 가이드는 [업그레이드 · 롤백](upgrades-and-rollback.md#마이그레이션-정책) 을 본다.

## /readyz 가 통과하지 않음

**원인.** `/readyz` 는 DB pool ping 을 2초 안에 못 끝내면 503 을 낸다. DB 가 느리거나, pool 이 포화이거나, network policy 가 막혔거나.

**진단.**

```bash
kubectl -n <ns> exec deploy/imgsync -- curl -s -o /dev/null -w "%{http_code}\n" localhost:8080/readyz
```

503 이 일관되게 나오는지 vs 간헐적인지 본다. 일관되면 연결 자체 문제, 간헐적이면 부하 문제.

**조치.** Pool 포화면 `IMGSYNC_WORKERS` 또는 pgxpool max 조정. DB 가 느리면 DB 측 slow query / 락 경합 조사. NetworkPolicy 가 의심되면 `kubectl describe networkpolicy -n <ns>` 로 egress 룰 확인.

## PDB 때문에 helm upgrade 가 진행되지 않음

**원인.** PodDisruptionBudget 이 voluntary disruption 한도를 강제한다. 차트의 기본 PDB 가 일정 수의 pod 가 NotReady 일 때 추가 evict 를 막는다.

**진단.**

```bash
kubectl -n <ns> get pdb,pods -l app.kubernetes.io/name=imgsync
```

`ALLOWED DISRUPTIONS = 0` 이면 helm 이 새 pod 를 띄울 수 없는 상태. NotReady pod 가 있는지 같이 본다.

**조치.** NotReady pod 의 원인을 먼저 푼다 (이미지 / readiness / DB ping). 진짜 stuck 이면 `kubectl delete pod --force` 로 강제 삭제한 뒤 helm upgrade 를 재시도한다. PDB 자체를 풀고 싶다면 차트 values 에서 `podDisruptionBudget.enabled=false` 로 한 번만 끄고, 그 위험성을 incident 로 기록한다.

## 디스크가 가득 차서 transport 가 죽음

**원인.** dst (LocalFS) 가 가리키는 PV 또는 노드 디스크가 가득 찼다. Send 가 ENOSPC 로 떨어진다.

**진단.**

```bash
kubectl -n <ns> exec deploy/imgsync -- df -h /mnt/share
```

`Use%` 가 100% 거나 임계치 근처면 그게 원인. 같은 시간대 `transfer_events.detail` 에 `no space` 또는 `ENOSPC` 키워드가 같이 보인다.

**조치.** 디스크를 비우거나 PV 를 키운다. PV 확장은 `StorageClass` 의 `allowVolumeExpansion=true` 가 전제. 단기 완화로는 오래된 출력 파일을 외부 저장소로 옮기고 imgsync pod 를 재시작해 in-memory 핸들을 갱신한다.

## race detector 로 테스트 실행이 너무 느림

**원인.** `go test ./... -race` 는 normal 빌드보다 5–10× 느리다. race 자체는 CI 게이트라 끄지 않는 게 원칙이지만, 로컬 반복 시 답답할 수 있다.

**진단.**

```bash
go test -race -v ./... 2>&1 | tail -20
```

특정 패키지가 유난히 느린지 확인한다. `internal/worker` 처럼 goroutine 이 많은 패키지가 race overhead 가 크다.

**조치.** 로컬 반복은 `go test ./internal/<pkg>/...` 로 좁힌다. PR 직전에는 `make ci` 로 race 포함 풀 게이트를 돌린다. CI 가 느린 경우는 [빌드와 테스트](../developer/build-and-test.md) 의 병렬화 가이드를 본다.

## kind 클러스터가 ImagePull 에서 실패

**원인.** 로컬에서 빌드한 이미지가 kind 노드에 없다. kind 는 호스트의 docker daemon 과 분리된 별도의 containerd 라서 push/load 가 필요하다.

**진단.**

```bash
kind load docker-image <repo>/imgsync:<tag> --name <cluster-name> --quiet || echo "load failed"
```

`failed` 가 뜨면 클러스터 이름이 다르거나 이미지 태그가 틀린 것.

**조치.** `kind get clusters` 로 클러스터 이름을 확인한 뒤 `kind load docker-image` 로 재로드한다. CI 환경에서는 `imagePullPolicy: IfNotPresent` 가 기본이므로 같은 태그로 reload 한 뒤 deployment 를 재시작해야 새 이미지가 잡힌다 — `kubectl rollout restart deploy/imgsync -n <ns>`.
