# 빌드와 테스트

이 페이지는 imgsync 를 처음 클론한 직후 어떻게 빌드하고 테스트를 돌리는지, 어디서 자주 막히는지를 한 곳에 모아둔다. 더 깊은 내부 구조는 [아키텍처 심화](architecture-deep-dive.md), kind 클러스터 위에서 직접 검증하는 절차는 [E2E 매뉴얼 검증 가이드](e2e-manual.md) 를 본다.

## 사전 준비

| 도구 | 권장 버전 | 용도 |
|---|---|---|
| Go | 1.25 | 빌드 / 테스트 |
| Docker | 24.0+ | 컨테이너 빌드, 통합 테스트의 ephemeral 의존 |
| golangci-lint | 1.61+ | `make lint` |
| kind | 0.24+ | E2E 클러스터 |
| helm | 3.14+ | 차트 설치 / E2E |
| kubectl | 1.30+ | E2E 검증 |

`go install` 로 받은 도구는 `$HOME/go/bin` 에 들어간다. `PATH` 에 포함되어 있는지 먼저 확인한다.

```bash
export PATH="$HOME/go/bin:$PATH"
go version          # ≥ 1.25
golangci-lint --version
kind version
helm version --short
```

## 한 줄 시작

클론 직후 한 줄로 모든 게이트를 돌린다.

```bash
git clone https://github.com/nineking424/imgsync && cd imgsync
make ci   # = lint + streaming-check + test
```

`make ci` 는 [`Makefile`](https://github.com/nineking424/imgsync/blob/main/Makefile) 의 다음 정의다.

```makefile
ci: lint streaming-check test
```

- `lint` — `golangci-lint run` (서브셋 룰셋, [`.golangci.yml`](https://github.com/nineking424/imgsync/blob/main/.golangci.yml))
- `streaming-check` — `scripts/check-streaming.sh` (스트리밍 핫패스에서 `io.ReadAll` / `bytes.NewBuffer(...body...)` 금지)
- `test` — `go test ./... -race -count=1`

## 테스트 분류

| 종류 | 명령 | build tag | 외부 의존 | 1회 소요 |
|---|---|---|---|---|
| 단위 (unit) | `go test ./...` | 없음 | 없음 | 수 초 ~ 30초 |
| 통합 (integration) | `make test-integration-sniffer` 등 | `integration` | Docker (ephemeral postgres / ftp) | 1 ~ 5 분 |
| E2E — 처리량 | `make e2e-throughput` | `e2e` | kind + helm + 환경변수 `IMGSYNC_E2E=1` | 20 ~ 35 분 |
| E2E — Dirty state 회복 | `make e2e-dirty-state` | `e2e` | kind + helm | ~ 30 분 |
| E2E — Sniffer C5' | `make e2e-sniffer` | `e2e` | kind + helm | ~ 20 분 |

build tag 가 붙은 테스트는 `go test ./...` 로는 안 도는 것이 의도된 동작이다. 통합 / E2E 는 인프라 부팅 비용이 큰 테스트만 격리해 두기 위함.

`-race` 는 항상 켠다. 단위 테스트는 race free 가 기본 조건이고, race 검출이 실패하면 그 자체가 회귀 신호다.

## 빌드

프로덕션 컨테이너 빌드는 다음 한 줄.

```bash
make docker-build           # IMAGE=imgsync:$(VERSION) 태그
```

`VERSION` 은 `git describe --tags --always --dirty` 에서 자동 산출된다. 자세한 규칙은 [릴리스 프로세스](release-process.md).

로컬 바이너리만 필요할 때는,

```bash
make build                  # ./bin/imgsync
./bin/imgsync --help
```

## 자주 막히는 곳

- **`gopls` 가 `internal/...` 를 못 찾는다** — 워크스페이스 루트가 `imgsync` 리포 루트인지 확인. IDE 가 부모 디렉토리에서 열려 있으면 모듈 경계가 어긋난다.
- **`golangci-lint: command not found`** — `go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest` 후 `$HOME/go/bin` 을 `PATH` 에 추가.
- **`docker: permission denied`** — Linux 라면 `usermod -aG docker $USER` + 재로그인. macOS Docker Desktop 은 보통 별도 설정 불필요.
- **`kind create cluster` 가 디스크 부족으로 실패** — 컨테이너 이미지 + e2e fixture 합산 ≥ 15GB 가 필요하다. `docker system prune -a` 로 비운 뒤 재시도. C7 은 1000개 × 10MB 를 호스트에 쓴다.
- **E2E 가 즉시 skip 으로 끝남** — `IMGSYNC_E2E=1` 환경변수가 빠졌다. `make e2e-throughput` 는 Makefile 에서 자동 export 하지만, `go test` 를 직접 호출할 때는 잊기 쉽다.

## 테스트 작성 컨벤션

- 어서션은 [`testify`](https://github.com/stretchr/testify) (`require` 우선, `assert` 는 보강용).
- `t.Parallel()` 은 race 가 안전하면 켠다. DB / FTP fixture 를 공유하는 테스트는 끄거나 별도 schema 를 쓴다.
- 통합 테스트는 파일 상단에 `//go:build integration` 빌드 태그를 명시한다. E2E 는 `//go:build e2e`.
- 픽스처는 `t.TempDir()` 를 우선. 호스트 경로(`/tmp/imgsync-e2e-localfs`) 는 E2E 처럼 클러스터에서도 봐야 하는 경우에만.
- 외부 명령(`kubectl`, `helm`, `kind`) 호출은 `e2e/helpers.go` 의 헬퍼를 통해서만. 직접 `exec.Command` 를 쓰면 retry / timeout 정책이 깨진다.

## 다음

- 코어 패키지가 어떻게 맞물려 있는지 — [아키텍처 심화](architecture-deep-dive.md).
- 실제 클러스터에서 손으로 검증할 때 — [E2E 매뉴얼 검증 가이드](e2e-manual.md).
- PR 을 올리기 전에 — [기여 가이드](contributing.md).
