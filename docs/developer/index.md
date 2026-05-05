# 개발

클론 후 `make ci` 한 줄로 시작한다. 린트 + streaming 가드 + 테스트가 모두 돈다.

```text
$ git clone https://github.com/nineking424/imgsync && cd imgsync
$ make ci   # = lint + streaming-check + test
```

| 항목 | 페이지 |
|---|---|
| 빌드 / 테스트 절차 | [빌드와 테스트](build-and-test.md) |
| 코어 내부 동작 | [아키텍처 심화](architecture-deep-dive.md) |
| 실 클러스터 검증 | [E2E 매뉴얼 검증 가이드](e2e-manual.md) |
| PR / 코드 스타일 | [기여 가이드](contributing.md) |
| 버전 / 태그 / 이미지 | [릴리스 프로세스](release-process.md) |
