# imgsync enqueue

단일 작업을 큐에 삽입한다 (멱등 — `(trace_id, dst)`).

## 사용법

```text
imgsync enqueue [flags]
```

## 플래그

| 플래그 | 타입 | 기본값 | 설명 |
|---|---|---|---|
| `--trace-id` | string | (필수) | 외부 부여 식별자 |
| `--src` | string | (필수) | 소스 URI |
| `--dst` | string | (필수) | 목적지 URI |
| `--src-protocol` | string | (필수) | 소스 프로토콜 (e.g. `localfs`, `ftp`) |
| `--dst-protocol` | string | (필수) | 목적지 프로토콜 |
| `--max-attempts` | int | `5` | 재시도 예산 |

## 환경 변수

| 변수 | 필수 | 설명 |
|---|---|---|
| `IMGSYNC_DSN` | 필수 | PostgreSQL 연결 문자열 |

자세한 표는 [환경 변수](../configuration/environment-variables.md)를 참고.

## 예시

localfs → localfs (스모크 테스트):

```bash
kubectl -n <ns> run --rm -it imgsync-cli \
  --image=<repo>/imgsync:<tag> --restart=Never \
  --env=IMGSYNC_DSN=<dsn> -- \
  enqueue \
    --trace-id=smoke-001 \
    --src=file:///data/input/sample.jpg \
    --dst=file:///mnt/share/output/sample.jpg \
    --src-protocol=localfs \
    --dst-protocol=localfs
```

ftp → localfs (runbook §1):

```bash
kubectl -n <ns> run --rm -it imgsync-cli \
  --image=<repo>/imgsync:<tag> --restart=Never \
  --env=IMGSYNC_DSN=<dsn> -- \
  enqueue \
    --trace-id=<trace> \
    --src=ftp://host/path/to/file \
    --dst=file:///mnt/share/dst/file \
    --src-protocol=ftp \
    --dst-protocol=localfs
```

## 동작

`INSERT INTO transfer_jobs ... ON CONFLICT (trace_id, dst) DO NOTHING` 를 실행한다. 동일한 `(trace_id, dst)` 가 이미 존재하면 silently skip 하므로 여러 번 실행해도 안전하다(idempotent). 새로 삽입된 row 는 `status=pending` 으로 들어가 다음 worker 가 lease 한다. 수동 재처리나 스크립트 기반 스모크 테스트에 사용한다.
