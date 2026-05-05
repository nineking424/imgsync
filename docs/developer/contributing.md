# 기여 가이드

이 페이지는 imgsync 에 PR 을 올리기 전에 한번 훑는 체크리스트다. 빌드 / 테스트 절차는 [빌드와 테스트](build-and-test.md) 에 있다.

## 워크플로우

1. **이슈** — 작업 전 GitHub 이슈로 변경 의도를 적는다. 작은 버그 수정은 PR 본문으로 대체 가능.
2. **브랜치** — `main` 에서 분기. 이름은 `feat/...`, `fix/...`, `docs/...` 같은 prefix 권장.
3. **로컬 검증** — `make ci` 가 통과해야 한다. (`lint` + `streaming-check` + `test`)
4. **PR** — 제목은 Conventional Commits 형식(아래 참조). 본문에 변경 의도 + 영향 범위 + 테스트 방법.
5. **Review** — 최소 1명의 승인 + CI green 이 머지 조건.
6. **Merge** — squash merge 권장. 머지 후 브랜치는 삭제.

## PR 체크리스트

- [ ] `make ci` 가 로컬에서 통과한다.
- [ ] 새로 추가한 exported 심볼에는 godoc 주석이 있다 (`revive` 의 `exported` 룰).
- [ ] 새 에러 메시지는 lowercase 로 시작하고 마침표를 붙이지 않는다 (`error-strings` 룰).
- [ ] 환경 변수 / `values.yaml` 키 / CLI 플래그를 추가했다면 다음 페이지도 같이 갱신했다.
    - [환경 변수](../configuration/environment-variables.md)
    - [values.yaml 레퍼런스](../installation/values-reference.md)
    - 해당 [CLI 페이지](../cli/index.md)
- [ ] DB 스키마를 바꿨다면 up + down 마이그레이션을 짝으로 추가했다.
- [ ] 사용자 가시 변경(behavior 변경, 새 플래그, breaking change) 은 변경 이력에 한 줄을 남겼다.
- [ ] 새 패키지를 만들었다면 [아키텍처 심화 — 패키지 맵](architecture-deep-dive.md#패키지-맵) 도 갱신했다.

## 커밋 메시지 컨벤션

[Conventional Commits](https://www.conventionalcommits.org/) 를 따른다.

```text
feat(sniffer): add bias-sec window override
fix(ftp): release advisory lock on Send error
docs(developer): clarify streaming guard scope
chore(deps): bump pgx to v5.7
```

scope 는 패키지 이름이나 `e2e`, `helm`, `docs` 처럼 큰 영역이면 그대로 쓴다. PR 제목도 같은 형식을 쓰면 squash 머지 시 자동으로 깔끔한 히스토리가 된다.

자주 쓰는 type:

| type | 의미 |
|---|---|
| `feat` | 사용자 가시 새 기능 |
| `fix` | 버그 수정 |
| `docs` | 문서만 변경 |
| `refactor` | 행동 변화 없는 리팩토링 |
| `test` | 테스트만 추가 / 수정 |
| `chore` | 의존성 / 빌드 / CI |
| `style` | 포맷팅 (코드 의미 변화 없음) |

## 문서 스타일

코드를 건드리는 PR 이 아니라도 문서만 손보는 PR 도 받는다. 톤은 페이지 종류에 따라 갈린다 — 같은 페이지 안에서 섞지만 않으면 된다.

- **개념 / 매뉴얼 / 개발자 가이드** (`concepts/`, `cli/`, `developer/`, `installation/`, `getting-started/`) — `한다체`. 사실 진술 위주.
- **운영 / 런북 / FAQ** (`operating/`, `faq.md`) — `합니다체`. 사용자에게 행동을 요청하는 톤이라 더 정중하게.
- **인덱스 / 랜딩 / 카드 텍스트** — 둘 다 허용. 기존 페이지 톤을 따라간다.

새 페이지를 만들 때는 인접 디렉토리 안의 기존 페이지를 한 번 훑고 시작하면 된다.

## 코드 스타일

전부 자동화되어 있다. 사람이 외울 것은 거의 없다.

- **포맷팅** — `gofmt` + `goimports`. golangci-lint 의 formatter 단계에서 강제.
- **린트** — `golangci-lint run`. 룰셋은 [`.golangci.yml`](https://github.com/nineking424/imgsync/blob/main/.golangci.yml) 에 박혀 있다. 현재 enable 된 룰의 핵심:
    - `bodyclose` — `http.Response.Body` 누락 close 차단.
    - `misspell` — 영어 오탈자.
    - `revive` 의 `exported` / `var-naming` / `error-return` / `error-strings` 서브셋.
- **스트리밍 가드** — `scripts/check-streaming.sh` 가 `internal/sources` / `internal/transports` / `internal/transfer` 에서 `io.ReadAll` / `bytes.NewBuffer(...body...)` 를 차단한다. 자세한 정책은 [아키텍처 심화](architecture-deep-dive.md#스트리밍-가드).

새 룰을 켜고 싶거나 끄고 싶으면 별도 PR 로 분리한다 — 코드 변경과 룰 변경이 같은 PR 에 섞이면 review 가 어려워진다.

## 코드 리뷰 관점

PR 작성자가 셀프 리뷰할 때, 그리고 리뷰어가 봐줄 때 자주 나오는 항목.

- 새로 추가한 hot path 가 본문을 메모리에 쌓진 않는가? (스트리밍 가드가 못 잡는 변수명 이슈 포함)
- 에러 래핑은 `fmt.Errorf("scope: %w", err)` 형태인가? sentinel 비교가 깨지지 않게.
- 새 advisory lock 키를 추가했다면 이름 충돌이 없는가? ([아키텍처 심화 — 동시성 모델](architecture-deep-dive.md#동시성-모델))
- 마이그레이션은 idempotent 인가? 같은 마이그레이션이 두 번 돌아도 깨지지 않아야 한다.
- 테스트 추가가 행동 변화에 비례하는가? 새 분기를 만들었으면 그 분기의 단위 테스트가 있어야 한다.

## 다음

- 처음이라면 — [빌드와 테스트](build-and-test.md) 부터.
- 깊은 내부 구조 — [아키텍처 심화](architecture-deep-dive.md).
- 태그 / 릴리스 절차 — [릴리스 프로세스](release-process.md).
