# Docker Compose 빠른 시작

5분 안에 imgsync 의 동작을 노트북에서 확인한다.

## 사전 준비

- macOS / Linux
- Docker 24+
- Make
- 8 GB RAM 여유, 5 GB 디스크 여유
- 리포지토리 클론 (`github.com/nineking424/imgsync.git` 가정)

## 1. 컨테이너 빌드

```bash
make docker-build
```

> ⏱️ 처음 빌드 ~2분. 이후는 캐시.

## 2. 스택 기동

```bash
make dev-up
```

postgres + 인메모리 ftpd + 워커 1대가 docker-compose 로 뜬다.

확인:

```bash
docker compose ps
# postgres: healthy, ftpd: running, worker: running
```

## 3. 작업 enqueue

```bash
make dev-seed
```

LocalFS 기반 smoke job 10건이 큐에 들어간다.

## 4. 처리 확인

```bash
make dev-smoke
```

10건 모두 `succeeded` 인지 검사한다. 통과하면 다음과 같이 출력된다.

```text
ok: 10 jobs succeeded
```

## 5. 정리

```bash
make dev-down
```

볼륨까지 같이 지운다.

## 다음으로

- 클러스터 토폴로지를 보고 싶다면 → [kind + Helm 빠른 시작](quickstart-kind.md)
- DB 안의 데이터를 들여다보고 싶다면 → [작업 큐 모델](../concepts/job-queue-model.md)
- 무엇이 막혔을 때 → [트러블슈팅](../operating/troubleshooting.md)
