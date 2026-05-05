# 업그레이드 · 롤백

이미지 태그를 새 버전으로 올리고, 문제가 보이면 helm 으로 되돌리는 절차. 마이그레이션 정책이 함께 결정되어 있다는 점이 중요하다.

## 릴리스 절차

```bash
# 1. 새 이미지 태그로 helm upgrade
helm upgrade imgsync deploy/helm/imgsync \
  -n <ns> --reuse-values \
  --set image.tag=<new-tag>

# 2. 마이그레이션 Job 이 완료됐는지 확인 (pre-install/pre-upgrade hook)
kubectl -n <ns> get jobs -l app.kubernetes.io/component=migrate

# 3. /healthz 모니터로 새 pod 가 lease 루프에 진입했는지 확인
kubectl -n <ns> port-forward svc/imgsync 8080:8080
curl -s localhost:8080/healthz | jq
```

기대 동작:

1. helm 이 `imgsync-migrate-<revision>` Job 을 먼저 띄운다 (pre-upgrade hook).
2. Job 이 성공해야 Deployment 가 rolling update 된다 — 마이그레이션 실패 시 새 pod 는 뜨지 않는다.
3. RollingUpdate 전략으로 한 pod 씩 교체. `terminationGracePeriodSeconds=60s` 안에 in-flight 작업이 정리된다.
4. 새 pod 의 `/healthz` 의 `last_lease_success_ts` 가 갱신되면 정상.

배포 이후 30분 정도는 [모니터링](monitoring.md) 의 알람 신호 (last_sweep_ts / pool_in_use / pending 누적) 를 같이 본다.

## 차트 스키마 break: selector 라벨 변경 (chart 1.0+)

차트 1.0 부터 worker 와 sniffer 의 `Deployment.spec.selector.matchLabels` 에 `app.kubernetes.io/component` 라벨이 추가됐다. selector 는 Kubernetes 가 immutable 로 강제하는 필드라, **0.x 에서 올라가는 모든 helm upgrade 가 다음 에러로 실패한다.**

```text
Error: UPGRADE FAILED: cannot patch "imgsync" with kind Deployment:
Deployment.apps "imgsync" is invalid: spec.selector: Invalid value: ...:
field is immutable
```

복구 절차 (PVC / Service / ConfigMap / Secret 은 영향 없음):

```bash
# 1) 기존 Deployment 만 삭제
kubectl -n <ns> delete deploy imgsync imgsync-sniffer

# 2) helm upgrade 재실행
helm upgrade imgsync deploy/helm/imgsync -n <ns> --reuse-values \
  --set image.tag=<new-tag>
```

또는 `helm uninstall imgsync && helm install imgsync ...` 로 한 번에 처리해도 된다 (control DB / NFS PVC 등 영구 자원이 차트 밖에 있다는 전제).

신규 설치는 영향 없다. 영향 범위는 **이미 0.x 차트로 설치된 클러스터** 만이며, 한 번 1.0 으로 올린 뒤에는 추가 조치가 필요하지 않다.

> NOTES.txt 의 `UPGRADE CAVEAT` 블록도 같은 내용을 안내하며, `kubectl ... delete deploy` 명령을 릴리스 네임스페이스 / 풀네임으로 자동 채워 보여준다.

---

## 롤백

```bash
helm history -n <ns> imgsync       # 마지막 양호한 revision 찾기
helm rollback -n <ns> imgsync <N>  # revision N 으로 롤백
```

revision 번호는 helm history 가 보여주는 1-based 정수다. `<N>` 을 생략하면 직전 revision 으로 돌아간다.

배드 릴리스가 crashloop 으로 helm rollback 자체를 막는 경우:

```bash
kubectl -n <ns> delete pod -l app.kubernetes.io/name=imgsync --force
helm rollback -n <ns> imgsync <N>
```

자세한 절차는 [런북 §5](runbook.md#5-rollback) 에 같이 정리되어 있다.

## 마이그레이션 정책

imgsync 의 마이그레이션은 **forward-only** 다. 이 정책의 의미를 정확히 이해해야 안전한 롤백이 가능하다.

### 정책 요점

- `up.sql` 만 정상 경로에서 실행된다. helm 의 pre-install / pre-upgrade hook 에서 `imgsync migrate up` 이 idempotent 하게 돈다.
- `down.sql` 은 비상 시 **수동 적용용** 이지 자동 롤백 도구가 아니다. helm rollback 은 스키마를 되돌리지 않는다.
- 새 버전이 배포되면 그 버전의 모든 변경(컬럼 추가, 인덱스 추가, 디폴트 값 등)은 영구적으로 본다.

### 이전 버전이 새 스키마와 호환되어야 한다

forward-only 가 성립하려면 **N+1 워커 코드와 N 워커 코드가 동일한 스키마(N+1 의 스키마)에서 안전하게 동작** 해야 한다. 즉:

- 새로 추가하는 컬럼은 NULLABLE 이거나 합리적 기본값을 가진다 — 옛 INSERT 가 실패하면 안 된다.
- 기존 컬럼의 의미를 바꾸지 않는다 — 새 컬럼을 추가하고 단계적으로 데이터를 옮기는 식으로만 변경한다.
- 인덱스 / 제약은 옛 워커가 만들어내는 데이터 형태를 거부하지 않아야 한다.

이 규칙을 어기는 마이그레이션(예: NOT NULL + default 없는 컬럼 추가, 컬럼 rename, 제약 강화)은 코드 변경을 최소 두 릴리스로 분리하는 expand → contract 패턴으로 푼다.

### `down.sql` 의 위치

- repo 안에 저장은 한다. 검토 / 코드리뷰 흔적을 남기기 위해서다.
- 자동 실행되지 않는다. helm rollback 도, CI 도 down.sql 을 호출하지 않는다.
- 운영 환경에서 down 을 적용해야 하는 시나리오가 있다면 (예: 의도치 않게 적용된 `up` 을 되돌려야 함) **on-call 이 직접 psql 로** 적용한다. 이건 incident 수준의 결정으로 보고, 적용 후에는 [런북 §8 incident 템플릿](runbook.md#8-incident) 으로 회고를 남긴다.

## 호환성 매트릭스 (워커 N+1 ↔ DB N)

| 시나리오 | 안전한가 | 메모 |
|---|---|---|
| 워커 N + DB N | yes | 정상 운영 |
| 워커 N+1 + DB N+1 | yes | helm upgrade 중간 상태이자 정상 종착점 |
| 워커 N + DB N+1 | yes (정책 강제) | helm rollback 직후 또는 rolling 중 옛 pod. 새 컬럼이 NULLABLE 이라는 정책이 이걸 보장한다 |
| 워커 N+1 + DB N | no | 마이그레이션 hook 이 먼저 돌도록 helm 이 보장한다. 직접 `kubectl set image` 로 우회하면 깨질 수 있음 |

정책 한 단락으로: **마이그레이션 Job 이 먼저, deployment 가 그 다음** 이 helm hook 으로 강제되며, deployment 만 따로 띄우는 경로는 만들지 않는다.

## 알아두기

- 차트의 `pre-install` 과 `pre-upgrade` 양쪽에 migrate hook 이 걸려 있어 첫 설치와 업그레이드 모두에서 자동으로 마이그레이션이 돈다.
- 같은 revision 으로 한 번 더 helm upgrade 를 호출해도 `migrate up` 이 idempotent 이므로 안전하다 — 적용된 버전은 다시 적용되지 않는다.
- 큰 데이터 마이그레이션(테이블 rewrite 등)은 hook Job 의 활성 시간 안에 끝나야 한다. 끝나지 않으면 hook 이 timeout 되고 helm upgrade 가 실패한다 — 이런 경우는 expand → contract 의 첫 단계로 분리해 hook 시간을 짧게 유지한다.
