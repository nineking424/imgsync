# Source · Transport

imgsync 가 파일을 읽고 쓰는 두 가지 인터페이스와 현재 구현체를 설명합니다.

## 인터페이스 정의

```go
// internal/transfer/interfaces.go
type Source interface {
    Open(ctx context.Context, src string) (body io.ReadCloser, size int64, err error)
}

type Transport interface {
    Send(ctx context.Context, dst string, body io.Reader, expectedSize int64) (
        writtenBytes int64, sha256Hex string, err error)
}
```

새 스토리지 백엔드를 추가하려면 이 두 인터페이스 중 하나 또는 둘 다를 구현합니다. 아키텍처 전반의 확장 포인트 설명은 [아키텍처 — 확장 포인트](architecture.md#확장-포인트) 를 참고하세요.

## 프로토콜 매트릭스

| 프로토콜 | Source 구현 | Transport 구현 | 용도 |
|---|---|---|---|
| `localfs` | `internal/sources/localfs` | `internal/transports/localfs` | 개발/테스트, on-disk 마운트 |
| `ftp` | `internal/sources/ftp` | `internal/transports/ftp` (세션 풀) | 운영 default |
| `s3` | 예정 | 예정 | 로드맵 |

## 에러 정책

`Source.Open` 또는 `Transport.Send` 에서 반환하는 에러는 두 종류로 나뉩니다.

- **`ErrSkippable`**: 소스 파일이 없거나 영구적으로 읽을 수 없는 경우처럼, 재시도해도 성공할 수 없는 에러입니다. Worker 는 이를 감지하면 `MarkSkipped` 를 호출해 `status = 'skipped'` 로 종결합니다.
- **일반 에러**: 일시적인 네트워크 오류 등 재시도하면 성공할 가능성이 있는 에러입니다. Worker 는 `MarkFailed` 를 호출하고 `attempts < max_attempts` 이면 작업을 다시 `pending` 으로 되돌립니다.

LocalFS Source 의 `Open` 은 `os.ErrNotExist` 를 `ErrSkippable` 로 변환합니다. 이 패턴이 필요한 이유는 운영 중 자주 만나는 race condition 때문입니다 — Sniffer 가 Source DB 에서 파일 경로를 읽은 직후 원본 파일이 삭제되거나 이동하는 경우, 재시도를 반복해도 파일은 돌아오지 않습니다. `ErrSkippable` 로 마킹해 불필요한 재시도를 막고 `max_attempts` 를 소진하는 대신 즉시 `skipped` 로 종결합니다.

## 스트리밍 계약

`Source.Open` 은 `io.ReadCloser` 를 반환하고, `Transport.Send` 는 `io.Reader` 를 소비합니다. 두 인터페이스 모두 **전체 본문을 메모리에 버퍼링하는 것을 금지**합니다. Worker 는 `Source.Open` 의 결과를 `Transport.Send` 에 직접 파이프합니다.

이 계약은 CI 레벨에서 `scripts/check-streaming.sh` 가 강제합니다. 이 스크립트는 `bytes.NewBuffer.*body` 나 `io.ReadAll` 같이 본문을 메모리에 통째로 읽는 패턴을 감지하면 빌드를 실패시킵니다. 새 Source / Transport 를 구현할 때 이 게이트를 통과해야 합니다.
