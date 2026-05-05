# imgsync worker

transfer_jobs 큐를 드레인한다.

## 사용법

```text
imgsync worker [flags]
```

## 플래그

없음. 모든 설정은 환경 변수로 전달한다.

## 환경 변수

| 변수 | 필수 | 설명 |
|---|---|---|
| `IMGSYNC_DSN` | 필수 | PostgreSQL 연결 문자열 |
| `IMGSYNC_WORKERS` | 선택 | 동시 lease 수 |
| `IMGSYNC_POD_NAME` | 선택 | lease 소유자 식별자 (K8s `metadata.name`) |
| `IMGSYNC_FTP_*` | 선택 | FTP 연결 파라미터 6개 |
| `IMGSYNC_HEALTH_ADDR` | 선택 | health 리스너 바인드 (기본 `:8080`). `/livez`, `/readyz`, `/healthz`, `/metrics` 가 모두 같은 포트에 뜬다. |

자세한 표는 [환경 변수](../configuration/environment-variables.md)를 참고.

## 예시

Docker 단독 실행:

```bash
docker run --rm \
  --env IMGSYNC_DSN=postgres://user:pass@host/imgsync \
  <repo>/imgsync:<tag> worker
```

K8s (Helm chart 가 자동 배포):

```bash
# Deployment 는 Helm chart 가 관리하므로 운영자가 직접 실행할 필요 없음.
helm install imgsync deploy/helm/imgsync --set image.tag=<tag>
```

로컬 개발:

```bash
# docker-compose 로 PG + worker 를 함께 띄움
make dev-up
```

## 동작

lease loop 를 실행한다. 컨텍스트가 살아있는 동안 `SELECT FOR UPDATE SKIP LOCKED` 로 `transfer_jobs` 에서 작업을 lease 하고, `Source.Open → Transport.Send` 를 수행한 후 결과를 `transfer_events` 에 기록한다. SIGTERM 을 받으면 in-flight 작업이 끝날 때까지 기다린 후 graceful 종료한다. `/healthz` 엔드포인트를 기본 `:8080` 에 노출한다.
