# 용어집

작업 (Job)
:   `transfer_jobs` 의 한 행. 한 파일의 한 번 전송 시도.

trace_id
:   외부에서 부여하는 작업 단위 식별자. 동일 `(trace_id, dst)` 는 멱등 enqueue 된다.

lease
:   워커가 작업을 점유한 상태. `status='leased'`, `locked_by`, `locked_at` 셋이 같이 세팅된다.

sweeper
:   임의 시간(threshold) 이상 leased 상태인 작업의 lease 를 회수해 pending 으로 돌리는 백그라운드 루프.

shadow 모드
:   sniffer 가 enqueue 대신 감사(audit) 로그만 남기는 모드. 신규 source DB 의 쿼리 검증용.

skippable
:   소스 부재처럼 "재시도해도 의미 없는 영구적 부재"를 의미하는 에러 카테고리. `ErrSkippable` 로 마킹된 작업은 `status='skipped'` 로 종결된다.

FTP host cap
:   동일 FTP 호스트에 대한 클러스터 전체 동시 처리 상한. `hostcap.Wrap` 이 advisory lock 으로 강제하며, 상한을 초과하는 FTP 세션 생성을 차단한다.

advisory lock
:   PostgreSQL 의 `pg_try_advisory_lock` 기반 협력 잠금. imgsync 는 sniffer/sweeper 단일 리더 보장과 FTP host cap 강제에 사용한다.

high-watermark
:   sniffer 가 source DB 에서 마지막으로 본 `(timestamp, pk)` 위치. `sniffer_state` 테이블에 저장되며, 재시작 후 이 위치부터 증분 폴링을 재개한다.

source
:   `Source` 인터페이스 구현. `Open(ctx, src)` 로 파일 본문을 `io.ReadCloser` 스트림으로 연다. 현재 구현: `localfs`, `ftp`.

transport
:   `Transport` 인터페이스 구현. `Send(ctx, dst, body, size)` 로 스트림을 목적지에 쓰고, 실제 기록 바이트 수와 SHA-256 해시를 반환한다. 현재 구현: `localfs`, `ftp`.

connector
:   PRD 에서 정의된 작업 목록 수집기의 추상 개념. sniffer 는 source DB 를 폴링하는 DB connector 의 한 구현이다.
