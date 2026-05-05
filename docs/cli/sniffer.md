# imgsync sniffer

source DB 를 폴링해 transfer_jobs 로 enqueue 한다.

## 사용법

```text
imgsync sniffer [flags]
```

## 플래그

없음. 모든 설정은 환경 변수로 전달한다.

## 환경 변수

| 변수 | 필수 | 설명 |
|---|---|---|
| `IMGSYNC_DSN` | 선택 | PostgreSQL 연결 문자열 (대체: `SNIFFER_IMGSYNC_DSN`) |
| `SNIFFER_SOURCE_DSN` | 필수 | 폴링할 source DB 연결 문자열 |
| `SNIFFER_IMGSYNC_DSN` | 필수 | enqueue 대상 imgsync DB 연결 문자열 |
| `SNIFFER_SOURCE_ID` | 필수 | source 식별자 (watermark key) |
| `SNIFFER_TABLE` | 필수 | 폴링할 테이블명 |
| `SNIFFER_PK_COLUMN` | 필수 | 기본 키 컬럼명 |
| `SNIFFER_TS_COLUMN` | 필수 | watermark 기준 타임스탬프 컬럼명 |
| `SNIFFER_DST_PATTERN` | 필수 | 목적지 URI Go template |
| `SNIFFER_SRC_PATTERN` | 필수 | 소스 URI Go template |
| `SNIFFER_SRC_PROTOCOL` | 필수 | 소스 프로토콜 (e.g. `localfs`, `ftp`) |
| `SNIFFER_DST_PROTOCOL` | 필수 | 목적지 프로토콜 |
| `SNIFFER_EXTRA_COLUMNS` | 선택 | template 에 노출할 추가 컬럼 목록 (쉼표 구분) |
| `SNIFFER_SHADOW` | 선택 | `true` 이면 enqueue 없이 감사 로그만 기록 |
| `SNIFFER_BATCH_SIZE` | 선택 | 1회 SELECT 에서 가져올 최대 row 수 |
| `SNIFFER_BIAS_SEC` | 선택 | watermark 후방 여유 시간(초) |
| `SNIFFER_INTERVAL_SEC` | 선택 | 폴링 간격(초) |

자세한 표는 [환경 변수](../configuration/environment-variables.md)를 참고.

## 예시

데몬 모드 (Helm chart 가 자동 배포):

```bash
# Deployment 는 Helm chart 가 관리.
helm install imgsync deploy/helm/imgsync --set image.tag=<tag>
```

shadow 모드로 enqueue 결과 검증:

```bash
# 실제 enqueue 없이 감사 로그만 남겨 패턴 렌더링 확인
SNIFFER_SHADOW=true \
SNIFFER_SOURCE_DSN=postgres://src-host/srcdb \
SNIFFER_IMGSYNC_DSN=postgres://imgsync-host/imgsync \
SNIFFER_TABLE=images \
SNIFFER_PK_COLUMN=id \
SNIFFER_TS_COLUMN=created_at \
SNIFFER_SRC_PROTOCOL=ftp \
SNIFFER_DST_PROTOCOL=localfs \
SNIFFER_SRC_PATTERN='ftp://host/{{.path}}' \
SNIFFER_DST_PATTERN='file:///mnt/share/{{.id}}' \
imgsync sniffer
```

> oneshot 옵션(`--oneshot`)은 현재 구현되어 있지 않다. 추후 추가 예정.

## 동작

`SNIFFER_INTERVAL_SEC` 마다 source DB 의 high-watermark(`sniffer_state` 테이블 저장) 이후 row 들을 SELECT 한다. 각 row 에 대해 `SNIFFER_SRC_PATTERN` / `SNIFFER_DST_PATTERN` 을 Go template 으로 렌더링한 뒤 `transfer_jobs` 에 enqueue 한다. `SNIFFER_SHADOW=true` 이면 enqueue 대신 감사 로그만 기록한다. 매 폴링 완료 후 watermark 를 `sniffer_state` 에 갱신한다.
