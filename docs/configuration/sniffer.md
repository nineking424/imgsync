# Sniffer 설정

Sniffer 는 source DB 를 주기적으로 폴링해 신규 행을 `transfer_jobs` 에 enqueue 합니다.
환경 변수 전체 목록은 [환경 변수](environment-variables.md) 페이지를 참고하세요.

## Config 구조 (`internal/sniffer/sniffer.go`)

```go
type Config struct {
    SourceID    string        // sniffer_state 테이블의 키
    Query       Query         // 폴링 쿼리 파라미터
    Dst         DstTemplate   // dstPattern 렌더러
    SrcPattern  string        // text/template 본문
    SrcProtocol string        // transfer_jobs.src_protocol
    DstProtocol string        // transfer_jobs.dst_protocol
    ImgsyncPool *pgxpool.Pool // control DB
    SourcePool  *pgxpool.Pool // source DB
}
```

`Query` 구조에는 `Table`, `PKColumn`, `TSColumn`, `ExtraColumns`, `BatchSize`, `BiasDuration` 이 들어있습니다.

## 패턴 렌더 메커니즘

`SrcPattern` 과 `DstPattern` 은 Go [`text/template`](https://pkg.go.dev/text/template) 문법을 사용합니다. 폴링된 각 row 의 컬럼 값이 `{{.컬럼명}}` 으로 치환됩니다.

**예시:**

```yaml
# values.yaml
sniffer:
  config:
    pkColumn: "id"
    tsColumn: "updated_at"
    extraColumns: "file_path"
    dstPattern: "/incoming/{{.file_path}}"
    srcPattern: "ftp://nas.example.com/images/{{.file_path}}"
    srcProtocol: "ftp"
    dstProtocol: "localfs"
```

row `{id: 42, updated_at: ..., file_path: "a/b.png"}` 가 폴링되면:

- `src` → `ftp://nas.example.com/images/a/b.png`
- `dst` → `/incoming/a/b.png`

이 값들이 `transfer_jobs.src` / `transfer_jobs.dst` 에 저장되고 worker 가 실제 전송에 사용합니다.

### `extraColumns` 에 무엇을 넣어야 하나

SELECT 쿼리는 `pkColumn` + `tsColumn` + `extraColumns` 만 가져옵니다. **패턴에서 참조하는 컬럼이 `pkColumn`/`tsColumn` 외에 있다면 반드시 `extraColumns` 에 추가해야** 합니다. 빠진 컬럼은 빈 문자열로 치환되어 잘못된 경로가 생성됩니다.

```bash
# dstPattern 이 {{.file_path}} 와 {{.bucket}} 을 참조하는 경우
SNIFFER_EXTRA_COLUMNS="file_path,bucket"
```

## Shadow 모드와 단계적 전개

`SNIFFER_SHADOW=true` (기본값) 이면 sniffer 는 쿼리를 실행하고 로그를 남기지만 **실제로 enqueue 하지 않습니다.** 신규 source DB 를 안전하게 연결할 때 사용하는 절차:

1. **shadow=true 로 배포** — 로그/감사로 쿼리 결과와 패턴 렌더 출력을 검증합니다.
2. **검증 완료 후 shadow=false 로 전환** — 실제 enqueue 가 시작됩니다.

```yaml
# 1단계: 감사 모드
sniffer:
  config:
    shadow: true

# 2단계: 실제 전송 시작
sniffer:
  config:
    shadow: false
```

## 폴링 파라미터 튜닝

| 파라미터 | 환경 변수 | 기본값 | 트레이드오프 |
|---|---|---|---|
| `batchSize` | `SNIFFER_BATCH_SIZE` | `500` | 크게 → 처리량↑, 폴링당 메모리·지연↑ |
| `biasSec` | `SNIFFER_BIAS_SEC` | `5` | 크게 → clock skew 흡수↑, 지연↑ |
| `intervalSec` | `SNIFFER_INTERVAL_SEC` | `60` | 짧게 → 지연↓, source DB 부하↑ |

**`batchSize`:** 한 번 폴링에 SELECT 하는 row 수의 상한입니다. 워터마크는 배치가 모두 enqueue 된 후에만 전진하므로, 배치 도중 에러가 나면 배치 전체가 재시도됩니다. 배치가 클수록 재시도 비용도 커집니다.

**`biasSec`:** `NOW() - biasSec` 이내의 행은 폴링에서 제외합니다. source DB 와 sniffer 간 clock skew, 또는 같은 timestamp 로 bulk insert 된 행들을 안전하게 포함하기 위한 마진입니다. 실시간성보다 중복 누락 방지가 중요한 경우 늘리세요.

**`intervalSec`:** 폴링 주기입니다. 짧을수록 지연은 줄어들지만 source DB 에 쿼리가 더 자주 날아갑니다. `batchSize` 가 꽉 찰 정도로 데이터가 쏟아지는 경우에는 `intervalSec` 를 줄이기보다 배치를 빠르게 소진할 수 있도록 `IMGSYNC_WORKERS` 를 늘리는 것이 효과적입니다.

## 관련 페이지

- Sniffer 개념 → [../concepts/components.md](../concepts/components.md)
- 전체 환경 변수 → [environment-variables.md](environment-variables.md)
- 프로토콜 설정 → [protocols.md](protocols.md)
