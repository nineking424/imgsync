# CLI

imgsync 는 단일 바이너리로 모든 컴포넌트를 실행한다. 서브커맨드만 다르다.

```text
$ imgsync --help
imgsync: file transfer queue (Go + PostgreSQL)

Usage:
  imgsync [command]

Available Commands:
  completion  Generate the autocompletion script for the specified shell
  enqueue     Insert a transfer job (idempotent on trace_id, dst)
  help        Help about any command
  migrate     Apply forward-only SQL migrations from a directory
  sniffer     Poll a source DB and enqueue new transfer jobs
  worker      Drain the transfer_jobs queue

Flags:
  -h, --help      help for imgsync
  -v, --version   version for imgsync
```

| 서브커맨드 | 한 줄 설명 | 페이지 |
|---|---|---|
| `migrate` | 마이그레이션 SQL 을 idempotent 하게 적용한다 | [imgsync migrate](migrate.md) |
| `worker` | transfer_jobs 큐를 드레인한다 | [imgsync worker](worker.md) |
| `sniffer` | source DB 를 폴링해 transfer_jobs 로 enqueue 한다 | [imgsync sniffer](sniffer.md) |
| `enqueue` | 단일 작업을 큐에 삽입한다 (멱등) | [imgsync enqueue](enqueue.md) |
| `completion` | 셸 자동완성 스크립트를 생성한다 | — |

각 서브커맨드의 상세 — 플래그, env 변수, 예시 — 는 좌측 메뉴의 개별 페이지를 참고.
