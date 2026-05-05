# Sweeper 설정

Sweeper 는 만료된 lease 를 주기적으로 회수해 갇힌 작업을 `pending` 으로 되돌립니다.
설정은 `internal/sweeper/sweeper.go` 의 `Config` 구조체로 관리됩니다.

## Config 구조

```go
type Config struct {
    Threshold time.Duration // lease 만료 판단 기준 시간; 기본 5m
    Interval  time.Duration // sweep 루프 주기; 기본 30s
    OnCycle   func()        // 각 cycle 후 호출되는 훅 (healthz 갱신 용도)
}
```

- **`Threshold`**: `transfer_jobs.locked_at < NOW() - Threshold` 인 `leased` 상태 작업을 `pending` 으로 되돌립니다.
- **`Interval`**: Sweep 을 실행하는 주기입니다. 각 cycle 은 별도 context timeout (`2 * Interval`) 을 가지므로, 한 cycle 이 멈춰도 다음 cycle 이 차단되지 않습니다.
- **`OnCycle`**: 성공한 cycle 마다 호출되며, `/healthz` 의 `last_sweep_ts` 갱신에 사용됩니다.

## Threshold 튜닝

### 너무 짧으면

정상적으로 처리 중인 작업(in-flight) 까지 회수됩니다. 워커가 아직 파일을 전송하는 도중에 lease 가 만료되면 같은 작업이 다른 워커에도 할당되어 **중복 전송** 이 발생할 수 있습니다.

### 너무 길면

워커가 OOM 이나 네트워크 장애로 갑자기 죽었을 때 해당 작업이 회수되기까지 오래 걸립니다. threshold 동안은 작업이 `leased` 상태에서 대기합니다.

### 권장값 (현재 기본 5분)

현재 기본값 `5m` 은 **p99 전송시간이 ~1분 미만** 이라는 가정 하에 설정되었습니다.

- 수백 MB 이상의 대용량 파일을 전송한다면 threshold 를 전송 소요 시간의 p99 보다 넉넉하게 설정하세요.
- 반대로 파일이 작고 빠른 회복이 필요하다면 threshold 를 줄일 수 있습니다. 단, 짧게 설정할수록 중복 처리 위험이 높아집니다.

```yaml
# values.yaml 예시 (향후 sweeper 섹션 추가 시)
sweeper:
  threshold: "10m"   # 대용량 파일 환경
  interval: "30s"
```

## 단일 리더 보장 (Advisory Lock)

Sweeper 는 각 cycle 시작 시 `pg_try_advisory_xact_lock(hashtext('imgsync_sweeper'))` 를 획득하려 시도합니다.

- lock 획득 성공: 해당 pod 만 이번 cycle 에서 row 를 회수합니다.
- lock 획득 실패: 다른 pod 가 이미 sweep 중. 0 rows 로 조용히 종료합니다 (에러 없음).

lock 은 트랜잭션 종료(COMMIT 또는 ROLLBACK) 시 자동으로 해제되므로 별도 cleanup 이 필요 없습니다.

**결과:** `replicaCount > 1` 환경에서도 같은 작업이 여러 번 회수(`expire` 이벤트 중복 삽입)되지 않습니다.

## 관련 페이지

- 전체 환경 변수 → [environment-variables.md](environment-variables.md)
- 아키텍처 개요 → [../concepts/architecture.md](../concepts/architecture.md)
