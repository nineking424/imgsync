# 환경 변수

imgsync 의 모든 런타임 설정은 환경 변수로 주입됩니다. 아래 표는 전체 목록입니다.

## 전체 목록

| 변수 | 적용 서브커맨드 | 기본값 | 설명 |
|---|---|---|---|
| `IMGSYNC_DSN` | worker, sniffer, enqueue, migrate | (필수) | control DB DSN |
| `IMGSYNC_WORKERS` | worker | `4` | per-pod goroutine 수 |
| `IMGSYNC_POD_NAME` | worker | (호스트네임) | lease `locked_by` 식별자 (K8s 의 metadata.name) |
| `IMGSYNC_FTP_MAX_PER_HOST` | worker | `4` | per-pod FTP 동시 세션 수 (in-process pool) |
| `IMGSYNC_FTP_IDLE_TTL_SEC` | worker | `300` | FTP 세션 idle TTL |
| `IMGSYNC_FTP_NOOP_AFTER_SEC` | worker | `60` | FTP NOOP 주기 |
| `IMGSYNC_FTP_USER` | worker | (없음) | FTP 자격증명 |
| `IMGSYNC_FTP_PASSWORD` | worker | (없음) | FTP 자격증명 |
| `IMGSYNC_FTP_HOST_CAP` | worker | `8` | 클러스터-와이드 host 동시 처리 cap (advisory lock) |
| `IMGSYNC_HEALTH_ADDR` | worker | `:8080` | health/metrics 리스너 바인드. `/livez`, `/readyz`, `/healthz`, `/metrics` 가 모두 같은 포트에 뜬다. |
| `IMGSYNC_MIGRATIONS_DIR` | migrate | `/etc/imgsync/migrations` | 마이그레이션 SQL 경로 |
| `SNIFFER_SOURCE_DSN` | sniffer | (필수) | source DB DSN |
| `SNIFFER_IMGSYNC_DSN` | sniffer | (필수) | sniffer 가 enqueue 할 control DB DSN |
| `SNIFFER_SOURCE_ID` | sniffer | (필수) | source 식별자 (`sniffer_state` 키) |
| `SNIFFER_TABLE` | sniffer | (필수) | 폴링할 source DB 테이블 |
| `SNIFFER_PK_COLUMN` | sniffer | (필수) | high-watermark 의 primary key 컬럼 |
| `SNIFFER_TS_COLUMN` | sniffer | (필수) | high-watermark 의 timestamp 컬럼 |
| `SNIFFER_DST_PATTERN` | sniffer | (필수) | 목적지 URI Go template (`{{.col}}` 치환) |
| `SNIFFER_SRC_PATTERN` | sniffer | (필수) | 소스 URI Go template |
| `SNIFFER_SRC_PROTOCOL` | sniffer | (필수) | source 프로토콜 (e.g., `localfs`, `ftp`) |
| `SNIFFER_DST_PROTOCOL` | sniffer | (필수) | destination 프로토콜 |
| `SNIFFER_EXTRA_COLUMNS` | sniffer | (없음) | 패턴 렌더에 추가로 SELECT 할 컬럼들 (콤마 구분) |
| `SNIFFER_SHADOW` | sniffer | `true` | 감사만 (실제 enqueue X) |
| `SNIFFER_BATCH_SIZE` | sniffer | `500` | 한 번 폴링당 SELECT row cap |
| `SNIFFER_BIAS_SEC` | sniffer | `5` | high-watermark 뒤로 빼는 안전 마진(초) |
| `SNIFFER_INTERVAL_SEC` | sniffer | `60` | 폴링 주기(초) |
| `SNIFFER_HEALTH_ADDR` | sniffer | `:8080` | sniffer health/metrics 리스너 바인드. `/livez`, `/readyz`, `/metrics` 노출. 비우면 probe 실패. |

## 관련 페이지

- Worker 튜닝 세부 사항 → [worker.md](worker.md)
- Sniffer 설정 세부 사항 → [sniffer.md](sniffer.md)
- Sweeper 설정 → [sweeper.md](sweeper.md)
- 프로토콜별 URL 형식 → [protocols.md](protocols.md)

---

!!! note "검증 방법"
    새 env 변수를 추가하면 이 표를 같이 갱신한다. PR 시 `grep -rn 'os.Getenv\|envInt\|envBool' cmd/ internal/cli/` 결과와 표가 일치하는지 검토자가 확인.
