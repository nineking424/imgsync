# Helm 설치

이 페이지는 `deploy/helm/imgsync` 차트를 사용해 imgsync를 Kubernetes 클러스터에 설치하는 순서를 설명한다.

---

## 사전 준비

현재 kubectl 컨텍스트를 확인한다.

```bash
kubectl config current-context
```

이 가이드 전체에서 namespace는 `imgsync`를 사용한다. 다른 이름을 쓸 경우 모든 명령에서 일관되게 교체한다.

---

## Step 1: Secret 생성

설치 전에 필수 Secret을 먼저 생성해야 한다.
→ 생성 명령 및 각 Secret의 상세 내용은 **[Secret 준비](secrets.md)** 를 참고한다.

---

## Step 2: 차트 설치

```bash
helm upgrade --install imgsync deploy/helm/imgsync \
  -n imgsync --create-namespace \
  --set image.repository=<your-repo>/imgsync \
  --set image.tag=<your-tag> \
  --set replicaCount=4 \
  --set ftpSecretRef.name=imgsync-ftp
```

!!! warning "0.x 차트에서 올리는 경우"
    chart 1.0 에서 worker / sniffer 의 selector 라벨이 변경됐다. 같은 릴리스를 0.x 에서 그대로 `helm upgrade` 하면 `field is immutable` 로 실패한다. 절차는 [업그레이드 · 롤백 — 차트 스키마 break](../operating/upgrades-and-rollback.md#차트-스키마-break-selector-라벨-변경-chart-10) 를 본다. 새 설치는 영향 없다.

설치 직후 `pre-install` hook으로 `imgsync-migrate` Job이 자동 실행된다. 이 Job은 DB 스키마 마이그레이션을 수행하며, 완료될 때까지 워커 파드 기동이 대기된다.

---

## Step 3: 설치 검증

```bash
# 파드 상태 확인
kubectl -n imgsync get pods

# pre-install hook 마이그레이션 로그 확인
kubectl -n imgsync logs job/imgsync-migrate

# 헬스 엔드포인트 확인
kubectl -n imgsync port-forward svc/imgsync 8080:8080
curl localhost:8080/healthz | jq
```

응답에 `last_lease_attempt_ts`, `last_sweep_ts`, `pool_in_use` 등의 필드가 보이고 `last_sweep_ts` 가 최근(≤ 60초) 갱신되고 있으면 정상이다 — 응답 구조의 자세한 의미는 [모니터링](../operating/monitoring.md#healthz-응답-구조) 을 참고. 단순히 살아있는지만 확인하려면 `curl -fsS localhost:8080/livez` 가 200 을 돌려주는지 본다.

---

## Step 4: 메트릭 노출 확인 (옵션)

차트가 worker 와 sniffer 모두 `:8080/metrics` 에 Prometheus 메트릭을 노출하므로, 클러스터에 Prometheus Operator 가 있다면 `ServiceMonitor` 를 함께 켜는 것이 표준 절차다.

```bash
helm upgrade --install imgsync deploy/helm/imgsync \
  -n imgsync --reuse-values \
  --set monitoring.serviceMonitor.enabled=true
```

확인:

```bash
# port-forward 로 raw metrics 가 나오는지 확인
kubectl -n imgsync port-forward svc/imgsync 8080:8080 &
curl -s localhost:8080/metrics | head
# imgsync_jobs_in_status{...} 같은 라인이 보이면 OK

# ServiceMonitor 가 렌더됐는지 확인 (Operator 가 있을 때만)
kubectl -n imgsync get servicemonitor imgsync
```

`monitoring.serviceMonitor.enabled=true` 인데 `monitoring.coreos.com/v1` CRD 가 없는 클러스터에서는 ServiceMonitor 리소스가 생성되지 않는다 (차트가 capability check 로 조용히 스킵).

전체 메트릭 카탈로그와 알람 후보는 [운영 — 모니터링](../operating/monitoring.md) 을 본다. Grafana 대시보드 import 절차는 [운영 — 대시보드](../operating/dashboards.md) 에 있다.

---

## Step 5: 첫 작업 enqueue

→ 첫 작업 enqueue 명령은 **[운영 매뉴얼 §1](../operating/runbook.md)** 의 enqueue 절을 참고한다.

---

## 업그레이드

```bash
helm upgrade imgsync deploy/helm/imgsync \
  --reuse-values \
  --set image.tag=<new-tag>
```

!!! warning "`--reuse-values` 필수"
    `--reuse-values`를 생략하면 이전에 `--set`으로 지정했던 모든 값(secret 이름, replicaCount 등)이 차트 기본값으로 되돌아간다.

---

## 언인스톨

```bash
helm uninstall imgsync -n imgsync
```

!!! note "DB 스키마는 삭제되지 않는다"
    imgsync의 마이그레이션은 forward-only 방식이다. `helm uninstall` 후에도 DB 스키마는 의도적으로 그대로 남는다. 데이터를 보호하기 위한 설계이며, 재설치 시 마이그레이션 Job이 멱등하게 다시 실행된다.
