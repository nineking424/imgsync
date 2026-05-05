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

```bash
kubectl -n imgsync get pods
# imgsync-...           2/2 Running
# postgres-0            1/1 Running
# source-postgres-0     1/1 Running

kubectl -n imgsync logs deploy/imgsync -c imgsync --tail=20
# "lease loop started" / "no jobs to lease"
```

## 3. 작업 enqueue 와 처리 확인

```bash
kubectl -n imgsync exec -it deploy/imgsync -c imgsync -- \
  imgsync enqueue --trace-id=demo \
    --src=file:///tmp/foo --dst=file:///tmp/bar \
    --src-protocol=localfs --dst-protocol=localfs

kubectl -n imgsync port-forward svc/postgres 5432:5432  # 또는 별도 psql
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
