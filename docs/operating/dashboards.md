# Grafana 대시보드

imgsync 의 큐 / 워커 / 처리량 / 실패 4분면 overview 대시보드 import 절차와 패널 PromQL 명세를 정리한다.

## Import 절차

1. Grafana 사이드바에서 **Dashboards → Import** 를 선택한다.
2. **Upload dashboard JSON file** 버튼을 클릭하고 `deploy/helm/imgsync/dashboards/imgsync-overview.json` 을 선택한다.
3. **Prometheus** datasource 선택 항목에서 `${DS_PROMETHEUS}` 변수에 매핑할 Prometheus 인스턴스를 고른다.
4. **Import** 를 클릭한다.

JSON 파일은 `panels: []` 빈 골격 상태다. Import 후 아래 **패널 명세** 표의 PromQL 을 참고해 패널을 직접 추가한다 (Add panel → Time series → PromQL 입력).

## 패널 명세

아래 표의 PromQL 은 datasource template variable `${DS_PROMETHEUS}` 를 사용한다고 가정한다.

| Row | Panel | PromQL |
|---|---|---|
| Queue | Pending jobs | `imgsync_jobs_in_status{status="pending"}` |
| Queue | Leased jobs | `imgsync_jobs_in_status{status="leased"}` |
| Queue | Lease lock age | `imgsync_lease_lock_age_seconds` |
| Workers | Active workers per pod | `sum by (pod) (imgsync_workers_active)` |
| Workers | DB pool in_use vs max | `imgsync_db_pool_conns{state="in_use"}` / `imgsync_db_pool_conns{state="max"}` |
| Workers | FTP pool in_use by host | `sum by (host) (imgsync_ftp_pool_size{state="in_use"})` |
| Throughput | Lease attempts rate | `sum by (result) (rate(imgsync_lease_attempts_total[5m]))` |
| Throughput | Jobs processed rate | `sum by (result) (rate(imgsync_jobs_processed_total[5m]))` |
| Throughput | p95 job duration | `histogram_quantile(0.95, sum by (le, src, dst) (rate(imgsync_job_duration_seconds_bucket[5m])))` |
| Failure | Sniffer error rate | `sum by (source) (rate(imgsync_sniffer_run_errors_total[5m]))` |
| Failure | Sweep cycles | `rate(imgsync_sweep_cycles_total[5m])` |

각 메트릭의 의미와 라벨 카디널리티는 [모니터링 — 메트릭 카탈로그](monitoring.md#메트릭-카탈로그) 를 참고한다.

## 다음 단계 (Phase 1.5)

현재 대시보드 JSON 은 `deploy/helm/imgsync/dashboards/` 에 파일로만 존재하며, 클러스터 내 Grafana 에 자동으로 반영되지 않는다.

Phase 1.5 에서는 `grafana_dashboard=1` 레이블을 가진 ConfigMap 으로 JSON 을 포장하고, Grafana sidecar (`grafana/grafana:sidecar-dashboard`) 가 해당 ConfigMap 을 watch 해 자동 로드하는 방식을 추가할 예정이다. 구현은 별도 plan 에서 처리한다.
