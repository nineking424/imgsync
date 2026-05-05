# CLI

imgsync 는 단일 바이너리로 모든 컴포넌트를 실행한다. 서브커맨드만 다르다.

```text
$ imgsync --help
imgsync — Go + PostgreSQL file-transfer queue

Usage:
  imgsync [command]

Available Commands:
  enqueue   수동 enqueue (스크립트/스모크용)
  migrate   DB 스키마 마이그레이션
  sniffer   Source DB 폴링 → 큐 enqueue
  worker    작업 큐에서 pull → 처리

(자세한 출력은 Task 7에서 갱신됩니다.)
```

| 서브커맨드 | 한 줄 설명 | 페이지 |
|---|---|---|
| `migrate` | DB 스키마를 최신 버전으로 올린다 | [imgsync migrate](migrate.md) |
| `worker` | 큐에서 작업을 lease 해 처리한다 | [imgsync worker](worker.md) |
| `sniffer` | Source DB 를 폴링해 큐에 enqueue 한다 | [imgsync sniffer](sniffer.md) |
| `enqueue` | 단발성 작업을 큐에 직접 넣는다 | [imgsync enqueue](enqueue.md) |
