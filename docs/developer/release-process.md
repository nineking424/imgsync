# 릴리스 프로세스

이 페이지는 imgsync 의 한 릴리스(태그 → 컨테이너 이미지 → Helm chart) 가 어떻게 만들어지는지를 적는다. PR 머지 절차 자체는 [기여 가이드](contributing.md) 를 본다.

## 버전 산출

`Makefile` 첫 줄이 진실의 출처다.

```makefile
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
IMAGE   ?= imgsync:$(VERSION)
```

- 가장 가까운 annotated tag (`vX.Y.Z`) 가 베이스.
- 그 이후의 커밋이 있으면 `vX.Y.Z-<n>-g<sha>` 형태로 늘어난다.
- 워킹 트리가 dirty 하면 끝에 `-dirty` 가 붙는다 — 정식 릴리스 빌드에서는 절대 나오면 안 된다.
- 태그가 하나도 없으면 `dev`.

이 값이 `docker-build` 시 컨테이너 태그로, 바이너리 빌드 시 `--build-arg VERSION` 으로 그대로 흘러간다. 별도 버전 파일을 두지 않는 이유.

## 절차

semver `vX.Y.Z` 기준이다.

1. **main 머지 + 태그.** `main` 이 green 인지 확인 후,

    ```bash
    git checkout main && git pull
    git tag -a vX.Y.Z -m "release vX.Y.Z"
    git push origin vX.Y.Z
    ```

2. **CI 가 컨테이너 빌드 + push.** 태그 push 가 release 워크플로우의 트리거다 (자동화 시점). 수동으로 돌릴 일이 생기면 동일하게 `make docker-build IMAGE=...` 후 레지스트리에 push.

3. **Helm chart bump.** `deploy/helm/imgsync/Chart.yaml` 의 `appVersion` 과 `version` 을 같이 올린다.
    - `appVersion` — 컨테이너 이미지 태그를 따라간다 (`vX.Y.Z`).
    - `version` — Helm chart 자체의 semver. chart 만 바뀌고 코드가 그대로면 `version` 만 patch bump.
    - bump 커밋 하나를 별도 PR 로 올리고 머지한다.

4. **변경 이력 반영.** 사용자 가시 변경(behavior, 플래그, breaking change) 을 변경 이력에 한 줄씩 적는다. 라이브러리 의존성 bump 처럼 영향이 없는 변경은 생략 가능.

## 호환성 정책

semver 의 의미를 다음과 같이 좁혀 둔다.

- **MAJOR** — 워커 / sniffer 의 외부 계약이 깨질 때. 환경변수 / CLI 플래그 / DB 스키마 / 메트릭 이름이 비호환 변경되면 메이저 bump.
- **MINOR** — 기능 추가, 새 플래그, 새 메트릭. 기존 사용자의 동작은 보존.
- **PATCH** — 버그 수정, 의존성 bump, 문서 / 테스트.

마이그레이션은 **forward-only** 다. down 마이그레이션은 짝으로 두지만 (개발 / 롤백 시 사용), 운영 환경에서 down 이 수동으로 돌아가는 시나리오를 보장하지 않는다. 운영의 롤백은 [업그레이드 · 롤백](../operating/upgrades-and-rollback.md) 절차를 따른다.

같은 메이저 안에서 워커 N 과 DB schema N+1 의 일시적 공존은 지원한다. rolling update 중에 짧게 발생하는 상태이고, 이 시기에 새 컬럼을 NULLable 로 추가해 두는 식의 가산 변경(additive migration) 으로 해결한다. 같은 메이저 안에서 schema N+2 와 워커 N 의 공존은 지원하지 않는다 — 두 단계 이상 벌어지기 전에 워커도 따라가야 한다.

## 다음

- 업그레이드 / 롤백을 운영 측에서 어떻게 절차화하는지 — [운영 — 업그레이드 · 롤백](../operating/upgrades-and-rollback.md).
- 태그 직전에 한 번 더 도는 검증은 — [E2E 매뉴얼 검증 가이드](e2e-manual.md).
