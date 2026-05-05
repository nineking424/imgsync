# kind + Helm 빠른 시작

실제 K8s 토폴로지에서 imgsync 가 도는 모습을 본다. ~15분.

## 사전 준비

| 도구 | 권장 버전 |
|---|---|
| kind | 0.23+ |
| kubectl | 1.30+ |
| helm | 3.14+ |
| Docker | 24+ |
| 디스크 여유 | 10 GB |

## 1. 클러스터 부트

```bash
make e2e-up
```

다음이 일어난다:

1. `e2e/kind_config.yaml` 로 단일 노드 kind 클러스터 생성
2. 컨테이너 이미지를 빌드해 kind 노드로 load
3. control DB(`postgres`) + source DB(`source-postgres`) 배포
4. `helm upgrade --install imgsync deploy/helm/imgsync ...` 실행
5. pre-install hook 으로 `imgsync migrate` Job 이 먼저 돌고, 끝나면 worker / sniffer Deployment 가 ready

## 2. 상태 확인

> 참고: `make e2e-up` 으로 띄운 kind 클러스터의 namespace 는 `imgsync-e2e` 다.
> 실제 운영용 Helm 설치 시에는 `helm install -n imgsync ...` 처럼 원하는 namespace 를 지정한다.

```bash
kubectl -n imgsync-e2e get pods
# imgsync-...           2/2 Running
# postgres-0            1/1 Running
# source-postgres-0     1/1 Running

kubectl -n imgsync-e2e logs deploy/imgsync -c imgsync --tail=20
# "lease loop started" / "no jobs to lease"
```

## 3. 작업 enqueue 와 처리 확인

작업 한 건을 큐에 넣는다:

```bash
kubectl -n imgsync-e2e exec -it deploy/imgsync -c imgsync -- \
  imgsync enqueue --trace-id=demo \
    --src=file:///tmp/foo --dst=file:///tmp/bar \
    --src-protocol=localfs --dst-protocol=localfs
```

> 위 경로(`/tmp/foo`)는 컨테이너 안에 존재하지 않으므로 작업은 `skipped` 로 종결된다(`ErrSkippable`).
> CLI 동작 확인용 데모로 충분하며, 실제 전송을 보려면 [E2E 매뉴얼](../developer/e2e-manual.md) 의 시드 스크립트를 사용한다.

별도 터미널에서 control DB 에 직접 붙어 결과를 확인한다(`port-forward` 는 블로킹):

```bash
kubectl -n imgsync-e2e port-forward svc/postgres 5432:5432
```

```bash
psql -h 127.0.0.1 -U imgsync -c \
  "select id, status, attempts from transfer_jobs where trace_id='demo';"
```

> 참고: control DB Service 는 `postgres` 라는 이름으로 노출된다(`svc/imgsync` 가 아니라).

## 4. 정리

```bash
make e2e-down
```

## 더 깊이

- 처리량 선형성, 강제종료 회복, sniffer 자가 감사 등 시나리오별 검증 →
  [E2E 매뉴얼](../developer/e2e-manual.md)
- 운영 환경(실제 K8s)에 올리는 단계 → [Helm 설치](../installation/helm.md)
