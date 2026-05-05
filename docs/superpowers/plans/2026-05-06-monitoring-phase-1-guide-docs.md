# 모니터링 Phase 1 / 1.5 — 가이드 문서 갭 보강 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 두 커밋(`6df49a08` PR #13 status index, `2e23aa49` PR #14 monitoring stack) 이 코드/Helm/일부 운영 문서까지만 반영했고 — 공개 가이드(MkDocs Material) 의 `installation/`, `configuration/`, `cli/`, `concepts/`, `developer/`, `operating/scaling.md`, `operating/troubleshooting.md` 페이지에는 변경이 닿지 않았다. 이 plan 은 그 갭을 채워 공개 가이드가 새 메트릭/엔드포인트/Helm 키/마이그레이션과 일관되게 만든다.

**Architecture:** 문서-only PR. 새 코드/테스트 없음. 검증은 `make docs-build` (`mkdocs build --strict`) 가 깨진 링크나 nav 참조를 잡는 것으로 대체한다. 한국어 톤은 기존 페이지(짧은 문장, 운영자 관점, 코드 인용) 를 그대로 따른다.

**Tech Stack:**
- MkDocs Material → ReadTheDocs theme (`mkdocs.yml`)
- `pymdownx.superfences` + `mermaid2.fence_mermaid` (단, 이 plan 의 추가 콘텐츠는 mermaid 미사용)
- 빌드: `make docs-build` = `mkdocs build --strict` (`requirements-docs.txt` venv 필요)

**Non-goals (이번 plan 에서 다루지 않음):**
- `docs/operating/monitoring.md`, `docs/operating/dashboards.md`, `docs/operating/runbook.md` — PR #14 에서 이미 갱신됨. 이 plan 은 이 세 파일을 **수정하지 않는다.**
- 신규 페이지 추가. 기존 페이지에 섹션을 추가하는 것으로 충분하다.
- 메트릭/Helm 동작 변경. 코드/차트는 손대지 않는다.

**커밋 규약:** 각 태스크 끝에서 `docs(<area>): <subject>` 메시지로 1 커밋. 모든 커밋은 끝에 `Co-Authored-By: Claude Opus 4.7 <noreply@anthropic.com>` 를 붙인다.

**전제 조건:** 로컬에서 `make docs-install` 이 한 번 끝났거나 `.venv-docs/` 가 활성화돼 있어야 `make docs-build` 가 동작한다. 미활성이면 `python -m venv .venv-docs && source .venv-docs/bin/activate && pip install -r requirements-docs.txt` 한 번 실행한다.

---

## File Structure

이번 plan 이 손대는 파일 목록. 신규 파일은 없다.

| 파일 | 변경 성격 | 어떤 커밋에서 유래 |
|---|---|---|
| `docs/installation/values-reference.md` | 신규 섹션 (monitoring, logging) | PR #14 — `values.yaml` 에 `monitoring.*` / `logging.format` 추가 |
| `docs/installation/helm.md` | 짧은 caveat + 링크 | PR #14 — selector 불변성 |
| `docs/operating/upgrades-and-rollback.md` | 신규 섹션 (selector 불변성) | PR #14 — `Deployment.spec.selector` 변경 |
| `docs/configuration/sniffer.md` | 신규 섹션 (health/metrics 엔드포인트) | PR #14 — sniffer 가 `:8080/livez,/readyz,/metrics` 노출 |
| `docs/configuration/environment-variables.md` | 행 추가 + 메모 | PR #14 — `SNIFFER_HEALTH_ADDR`, `/metrics` 동거 |
| `docs/cli/sniffer.md` | 환경 변수 표 + 검증 명령 | PR #14 |
| `docs/cli/worker.md` | `IMGSYNC_HEALTH_ADDR` 행 보강 | PR #14 |
| `docs/concepts/components.md` | 메트릭 emission 노트 (worker/sniffer/sweeper) | PR #14 |
| `docs/developer/architecture-deep-dive.md` | 신규 섹션 (`internal/metrics` 패키지, push vs scrape) | PR #14 |
| `docs/operating/scaling.md` | 메트릭 기반 신호 추가 | PR #14 |
| `docs/operating/troubleshooting.md` | 메트릭 진단 흐름 추가 | PR #14 |
| `docs/cli/migrate.md` | 마이그레이션 목록 표 추가 | PR #13 — `0003_jobs_status_index` |
| `docs/concepts/job-queue-model.md` | 상태 인덱스 노트 | PR #13 |

각 파일은 **추가만** 한다. 기존 문장은 손대지 않는다 (글로벌 CLAUDE.md §3 surgical changes). 만약 추가 위치 옆 문장이 새 내용과 충돌한다면 **사용자에게 물어보고** 수정한다 — 임의 rewrite 금지.

---

## 사전 작업 (모든 태스크 시작 전 1회)

- [ ] **Step P1: 현재 브랜치 / 클린 상태 확인**

```bash
git status -s
git rev-parse --abbrev-ref HEAD
```

Expected: workspace 가 clean 하거나 의도된 변경만 있어야 한다. 새 작업 브랜치를 쓰려면:

```bash
git checkout -b docs/monitoring-phase-1-guide-2026-05-06
```

- [ ] **Step P2: `make docs-build` 가 baseline 으로 통과하는지 확인**

```bash
source .venv-docs/bin/activate 2>/dev/null || python -m venv .venv-docs && source .venv-docs/bin/activate
pip install -r requirements-docs.txt --quiet
make docs-build
```

Expected: `mkdocs build --strict` 가 `INFO -  Documentation built` 로 끝난다. 실패하면 (이번 plan 과 무관하게) 먼저 고쳐야 한다 — 사용자에게 보고.

---

## Task 1: values.yaml 신규 키 (monitoring, logging) 를 values-reference 에 추가

**Files:**
- Modify: `docs/installation/values-reference.md` (파일 끝에 두 섹션 추가)

**왜 필요:** PR #14 가 `values.yaml` 에 `monitoring.serviceMonitor.*`, `monitoring.podAnnotations`, `logging.format` 11 키를 추가했다. values-reference 페이지 상단 admonition 이 "values.yaml 변경 시 같이 업데이트해야 한다" 고 명시하므로 이 키들이 누락되면 정책 위반이다.

- [ ] **Step 1: 파일 끝에 monitoring + logging 섹션 추가**

`docs/installation/values-reference.md` 의 마지막 라인 (현재 sniffer 표의 마지막 행) 직후에 아래 두 섹션을 append.

````markdown

---

## monitoring

| 키 | 타입 | 기본값 | 설명 |
|---|---|---|---|
| `monitoring.serviceMonitor.enabled` | bool | `false` | Prometheus Operator 의 `ServiceMonitor` 리소스를 렌더할지 여부. 클러스터에 `monitoring.coreos.com/v1` CRD 가 없으면 옵트인해도 무시된다. |
| `monitoring.serviceMonitor.interval` | string | `30s` | 스크랩 간격. Prometheus duration 문법 (`30s`, `1m`). |
| `monitoring.serviceMonitor.scrapeTimeout` | string | `10s` | 한 번의 스크랩이 허용하는 최대 시간. `interval` 보다 짧아야 한다. |
| `monitoring.serviceMonitor.labels` | map | `{}` | 생성되는 `ServiceMonitor` 에 붙일 추가 라벨. Prometheus Operator 의 `serviceMonitorSelector` 가 요구하는 라벨이 있으면 여기에 넣는다. |
| `monitoring.serviceMonitor.namespace` | string | `""` | `ServiceMonitor` 를 둘 네임스페이스. 비우면 릴리스 네임스페이스에 생성된다. Prometheus 가 다른 네임스페이스에서 watch 하도록 설정돼 있다면 그쪽 이름을 적는다. |
| `monitoring.podAnnotations` | map | `{}` | 메트릭 스크랩 환경에서 worker/sniffer 파드에 추가할 annotation. Prometheus Operator 가 아닌 in-pod scrape (e.g. `prometheus.io/scrape: "true"`) 를 쓸 때 사용한다. |

→ ServiceMonitor 를 켜기 전에 클러스터 측 Prometheus 가 이 차트의 라벨 (`app.kubernetes.io/name=imgsync`, `component∈{worker, sniffer}`) 을 selector 로 매칭하는지 확인한다. 노출되는 메트릭 카탈로그는 [모니터링 — 메트릭 카탈로그](../operating/monitoring.md#메트릭-카탈로그) 를 본다.

---

## logging

| 키 | 타입 | 기본값 | 설명 |
|---|---|---|---|
| `logging.format` | string | `text` | 컨테이너 로그 포맷. 현재는 `text` 만 지원하며 차후 `json` 추가 시 같은 키에서 토글한다. 운영 환경에서 키-밸류 추출이 필요하면 sidecar (vector / fluent-bit) 로 후처리하는 것을 권장한다. |
````

- [ ] **Step 2: `make docs-build` 로 strict 빌드 통과 확인**

```bash
make docs-build
```

Expected: PASS. 새 헤더가 등록되고 `monitoring.md` 앵커(`#메트릭-카탈로그`) 링크가 깨지지 않아야 한다.

- [ ] **Step 3: 커밋**

```bash
git add docs/installation/values-reference.md
git commit -m "$(cat <<'EOF'
docs(values): add monitoring + logging blocks to values reference

PR #14 added monitoring.serviceMonitor.* / monitoring.podAnnotations
and logging.format to values.yaml. The values-reference page's own
warning admonition requires these keys be documented when values.yaml
changes — this commit fills that gap.

Co-Authored-By: Claude Opus 4.7 <noreply@anthropic.com>
EOF
)"
```

---

## Task 2: Selector 불변성 caveat 를 upgrades-and-rollback 에 추가

**Files:**
- Modify: `docs/operating/upgrades-and-rollback.md` (적절한 위치에 새 섹션 삽입)

**왜 필요:** PR #14 가 `Deployment.spec.selector.matchLabels` 에 `app.kubernetes.io/component` 을 추가. selector 는 immutable 이므로 이전 차트에서 `helm upgrade` 시 "field is immutable" 로 실패한다. NOTES.txt 에 caveat 가 있지만 운영자가 release notes / 가이드를 먼저 보는 흐름에서도 같은 정보가 필요하다.

- [ ] **Step 1: "롤백" 섹션 직전에 새 섹션 삽입**

`docs/operating/upgrades-and-rollback.md` 에서 `## 롤백` 헤더 **바로 위**에 다음 블록을 삽입한다.

````markdown
## 차트 스키마 break: selector 라벨 변경 (chart 1.0+)

차트 1.0 부터 worker 와 sniffer 의 `Deployment.spec.selector.matchLabels` 에 `app.kubernetes.io/component` 라벨이 추가됐다. selector 는 Kubernetes 가 immutable 로 강제하는 필드라, **0.x 에서 올라가는 모든 helm upgrade 가 다음 에러로 실패한다.**

```text
Error: UPGRADE FAILED: cannot patch "imgsync" with kind Deployment:
Deployment.apps "imgsync" is invalid: spec.selector: Invalid value: ...:
field is immutable
```

복구 절차 (PVC / Service / ConfigMap / Secret 은 영향 없음):

```bash
# 1) 기존 Deployment 만 삭제
kubectl -n <ns> delete deploy imgsync imgsync-sniffer

# 2) helm upgrade 재실행
helm upgrade imgsync deploy/helm/imgsync -n <ns> --reuse-values \
  --set image.tag=<new-tag>
```

또는 `helm uninstall imgsync && helm install imgsync ...` 로 한 번에 처리해도 된다 (control DB / NFS PVC 등 영구 자원이 차트 밖에 있다는 전제).

신규 설치는 영향 없다. 영향 범위는 **이미 0.x 차트로 설치된 클러스터** 만이며, 한 번 1.0 으로 올린 뒤에는 추가 조치가 필요하지 않다.

> NOTES.txt 의 `UPGRADE CAVEAT` 블록도 같은 내용을 안내하며, `kubectl ... delete deploy` 명령을 릴리스 네임스페이스 / 풀네임으로 자동 채워 보여준다.

---

````

- [ ] **Step 2: `make docs-build` 통과 확인**

```bash
make docs-build
```

Expected: PASS.

- [ ] **Step 3: 커밋**

```bash
git add docs/operating/upgrades-and-rollback.md
git commit -m "$(cat <<'EOF'
docs(upgrades): selector immutability caveat for chart 1.0 upgrade

PR #14 added app.kubernetes.io/component to Deployment.spec.selector
on both the worker and the sniffer. Operators upgrading from any 0.x
chart hit "field is immutable". Document the recovery procedure
(delete deploy + re-upgrade, or uninstall+install) here so it
surfaces alongside the standard upgrade flow, mirroring the NOTES.txt
caveat shipped with the chart.

Co-Authored-By: Claude Opus 4.7 <noreply@anthropic.com>
EOF
)"
```

---

## Task 3: Helm 설치 가이드에 caveat 링크 + ServiceMonitor 토글 step 추가

**Files:**
- Modify: `docs/installation/helm.md` (Step 2 옆에 알림 추가, "검증" 뒤에 옵션 step 추가)

**왜 필요:** 신규 설치자도 `monitoring.serviceMonitor.enabled=true` 로 켜는 방법을 첫 화면에서 한 번 보고 가야 한다. 또한 0.x 사용자가 이 페이지로 들어왔을 때 caveat 페이지로 점프할 링크가 필요하다.

- [ ] **Step 1: Step 2 helm 명령 다음 admonition 추가**

`docs/installation/helm.md` 의 `## Step 2: 차트 설치` 섹션, 첫 `helm upgrade --install` 코드 블록 **바로 뒤** ("설치 직후 ...rolling update 된다." 문장 앞) 에 다음 admonition 을 삽입한다.

````markdown
!!! warning "0.x 차트에서 올리는 경우"
    chart 1.0 에서 worker / sniffer 의 selector 라벨이 변경됐다. 같은 릴리스를 0.x 에서 그대로 `helm upgrade` 하면 `field is immutable` 로 실패한다. 절차는 [업그레이드 · 롤백 — 차트 스키마 break](../operating/upgrades-and-rollback.md#차트-스키마-break-selector-라벨-변경-chart-10) 를 본다. 새 설치는 영향 없다.

````

- [ ] **Step 2: 검증 step 뒤에 "Step 4: 메트릭 노출 확인" 추가**

`docs/installation/helm.md` 의 `## Step 3: 설치 검증` 섹션 끝, 다음 헤더 (`## ` 또는 파일 끝) **직전**에 아래 블록을 추가한다.

````markdown

---

## Step 4: 메트릭 노출 확인 (옵션)

차트가 worker 와 sniffer 모두 `:8080/metrics` 에 Prometheus 메트릭을 노출하므로, 클러스터에 Prometheus Operator 가 있다면 `ServiceMonitor` 를 함께 켜는 것이 표준 절차다.

```bash
helm upgrade --install imgsync deploy/helm/imgsync \
  -n imgsync --reuse-values \
  --set monitoring.serviceMonitor.enabled=true
```

확인:

```bash
# port-forward 로 raw metrics 가 나오는지 확인
kubectl -n imgsync port-forward svc/imgsync 8080:8080 &
curl -s localhost:8080/metrics | head
# imgsync_jobs_in_status{...} 같은 라인이 보이면 OK

# ServiceMonitor 가 렌더됐는지 확인 (Operator 가 있을 때만)
kubectl -n imgsync get servicemonitor imgsync
```

`monitoring.serviceMonitor.enabled=true` 인데 `monitoring.coreos.com/v1` CRD 가 없는 클러스터에서는 ServiceMonitor 리소스가 생성되지 않는다 (차트가 capability check 로 조용히 스킵).

전체 메트릭 카탈로그와 알람 후보는 [운영 — 모니터링](../operating/monitoring.md) 을 본다. Grafana 대시보드 import 절차는 [운영 — 대시보드](../operating/dashboards.md) 에 있다.
````

- [ ] **Step 3: `make docs-build` 통과 확인**

```bash
make docs-build
```

Expected: PASS. `../operating/upgrades-and-rollback.md#차트-스키마-break-...` 앵커가 Task 2 의 헤더와 일치해야 한다 (slugify case=lower).

- [ ] **Step 4: 커밋**

```bash
git add docs/installation/helm.md
git commit -m "$(cat <<'EOF'
docs(helm): note 0.x→1.0 selector caveat + Step 4 metrics enablement

Adds an inline warning pointing 0.x→1.0 upgraders at the new caveat
section, and an optional Step 4 covering ServiceMonitor opt-in plus
a port-forward sanity check that surfaces /metrics output.

Co-Authored-By: Claude Opus 4.7 <noreply@anthropic.com>
EOF
)"
```

---

## Task 4: Sniffer 설정 페이지에 health/metrics 엔드포인트 섹션 추가

**Files:**
- Modify: `docs/configuration/sniffer.md` (신규 섹션을 "관련 페이지" 또는 파일 끝에 추가)

**왜 필요:** PR #14 가 sniffer 에 `:8080` 리스너 + `/livez`, `/readyz`, `/metrics` 를 추가하고 sniffer Deployment 에 probe 3종을 붙였다. 기존 sniffer 설정 페이지에는 이 정보가 전혀 없다.

- [ ] **Step 1: 파일 끝(또는 마지막 섹션 직전) 에 새 섹션 추가**

`docs/configuration/sniffer.md` 의 마지막 줄에 아래 블록을 append.

````markdown

## Health / Metrics 엔드포인트

Sniffer 는 `SNIFFER_HEALTH_ADDR` (기본 `:8080`) 에 다음 엔드포인트를 노출합니다. Helm 차트는 이 주소를 자동으로 설정하고 컨테이너에 `livenessProbe` / `readinessProbe` / `startupProbe` 를 붙입니다.

| 경로 | 용도 | 응답 |
|---|---|---|
| `/livez` | 프로세스 liveness | 항상 `200 OK`. 응답 자체가 안 나오면 deadlock 으로 본다. |
| `/readyz` | 트래픽 ready | source DB / control DB ping 이 2초 안에 성공하면 `200`, 그렇지 않으면 `503`. |
| `/metrics` | Prometheus scrape | sniffer push 메트릭 (`imgsync_sniffer_enqueue_total{source}`, `imgsync_sniffer_run_errors_total{source}`) + Go runtime 기본 메트릭. |

`SNIFFER_HEALTH_ADDR` 는 `:port` 또는 `host:port` 형식을 받습니다. 비워두면 핸들러가 등록되지 않아 probe 가 실패하므로 운영 환경에서는 항상 비워두지 않습니다.

> Helm 차트는 `containerPort: 8080` 을 노출하고 `imgsync-sniffer` Service 에 `port-name: http-metrics` 를 매핑합니다. ServiceMonitor 가 이 포트 이름으로 scrape 하므로 차트 외부에서 포트를 임의로 바꾸면 메트릭 수집이 끊깁니다.

샘플:

```bash
# 로컬에서 sniffer 단독으로 띄우고 /metrics 확인
SNIFFER_HEALTH_ADDR=":8080" \
SNIFFER_SOURCE_DSN=... SNIFFER_IMGSYNC_DSN=... \
imgsync sniffer &
curl -s localhost:8080/metrics | grep imgsync_sniffer_
```

## 메트릭 emission 훅 (코드 레벨)

`internal/sniffer/sniffer.go` 의 `Config` 는 외부에서 메트릭에 연결할 수 있도록 두 콜백을 노출합니다. CLI(`cmd/imgsync/sniffer`) 가 `internal/metrics` 와 wiring 합니다 — 직접 호출할 일은 보통 없습니다.

```go
type Config struct {
    // ... 기존 필드 생략
    OnEnqueue func(source string, n int)  // RunOnce 결과 enqueue 된 행 수
    OnError   func(source string)         // RunOnce err 발생 시 1회
}
```
````

- [ ] **Step 2: `make docs-build` 통과 확인**

```bash
make docs-build
```

Expected: PASS.

- [ ] **Step 3: 커밋**

```bash
git add docs/configuration/sniffer.md
git commit -m "$(cat <<'EOF'
docs(sniffer): document /livez, /readyz, /metrics + Config callbacks

PR #14 wired sniffer to a SNIFFER_HEALTH_ADDR listener with the
standard probe paths plus /metrics, and added OnEnqueue/OnError
callbacks to internal/sniffer.Config for metrics hookup. Document
both surfaces here so operators and contributors don't have to
read the chart templates.

Co-Authored-By: Claude Opus 4.7 <noreply@anthropic.com>
EOF
)"
```

---

## Task 5: 환경 변수 페이지에 SNIFFER_HEALTH_ADDR 추가 + IMGSYNC_HEALTH_ADDR 의 /metrics 표기

**Files:**
- Modify: `docs/configuration/environment-variables.md`

**왜 필요:** 환경 변수 페이지가 모든 운영 환경 변수의 canonical reference. PR #14 의 `SNIFFER_HEALTH_ADDR` 가 누락돼 있고, `IMGSYNC_HEALTH_ADDR` 설명도 `/metrics` 가 같은 포트에 뜬다는 사실을 반영하지 않았다.

- [ ] **Step 1: `IMGSYNC_HEALTH_ADDR` 행 설명 보강**

`docs/configuration/environment-variables.md` line 18 (`| `IMGSYNC_HEALTH_ADDR` | worker | `:8080` | /healthz 바인드 주소 |`) 의 설명 셀을 다음으로 교체:

```markdown
| `IMGSYNC_HEALTH_ADDR` | worker | `:8080` | health/metrics 리스너 바인드. `/livez`, `/readyz`, `/healthz`, `/metrics` 가 모두 같은 포트에 뜬다. |
```

- [ ] **Step 2: 같은 표 (또는 sniffer 표) 에 `SNIFFER_HEALTH_ADDR` 행 추가**

먼저 sniffer 환경 변수 표가 어디 있는지 확인:

```bash
grep -n "^| \`SNIFFER_" docs/configuration/environment-variables.md | head -5
```

표의 마지막 sniffer 행 (예: `SNIFFER_INTERVAL_SEC`) 다음 줄에 다음 행을 추가:

```markdown
| `SNIFFER_HEALTH_ADDR` | sniffer | `:8080` | sniffer health/metrics 리스너 바인드. `/livez`, `/readyz`, `/metrics` 노출. 비우면 probe 실패. |
```

표 컬럼 수가 위 IMGSYNC 행과 다르면 `(변수, 컴포넌트, 기본값, 설명)` 4열 표 컨벤션을 따라 컬럼 수를 맞춰 삽입한다 (예: 5열 표라면 `필수` 컬럼에 `선택` 을 넣는다).

- [ ] **Step 3: `make docs-build` 통과 확인**

```bash
make docs-build
```

Expected: PASS.

- [ ] **Step 4: 커밋**

```bash
git add docs/configuration/environment-variables.md
git commit -m "$(cat <<'EOF'
docs(env): add SNIFFER_HEALTH_ADDR + note /metrics on health port

PR #14 wires both worker and sniffer to expose /metrics on the same
health-addr listener. Reflect that in the canonical env-var
reference so operators don't need to grep the chart.

Co-Authored-By: Claude Opus 4.7 <noreply@anthropic.com>
EOF
)"
```

---

## Task 6: Sniffer CLI 페이지에 SNIFFER_HEALTH_ADDR + /metrics 검증 명령 추가

**Files:**
- Modify: `docs/cli/sniffer.md`

**왜 필요:** CLI 페이지의 환경 변수 표가 sniffer 운영 환경 변수의 빠른 참조다. `SNIFFER_HEALTH_ADDR` 가 없으면 운영자가 차트 외부에서 sniffer 를 단독 기동할 때 probe 가 안 떠서 실패한다.

- [ ] **Step 1: 환경 변수 표에 SNIFFER_HEALTH_ADDR 행 추가**

`docs/cli/sniffer.md` 의 환경 변수 표에서 마지막 행 (`SNIFFER_INTERVAL_SEC`) 다음에 추가:

```markdown
| `SNIFFER_HEALTH_ADDR` | 선택 | `:8080` | `/livez` · `/readyz` · `/metrics` 리스너 바인드 주소 |
```

- [ ] **Step 2: 예시 섹션에 단독 검증 명령 추가**

`docs/cli/sniffer.md` 의 마지막 예시 (shadow 모드) 코드 블록 **다음**, 파일 끝까지 사이에 다음 블록을 삽입한다 (만약 파일 끝까지 다른 헤더가 없다면 단순 append).

````markdown

메트릭 / probe 단독 확인:

```bash
SNIFFER_HEALTH_ADDR=":8080" \
SNIFFER_SOURCE_DSN=... \
SNIFFER_IMGSYNC_DSN=... \
imgsync sniffer &

curl -s localhost:8080/livez       # → 200 OK
curl -s localhost:8080/readyz      # → 200 OK / 503 (DB ping 실패 시)
curl -s localhost:8080/metrics | grep imgsync_sniffer_
```
````

- [ ] **Step 3: `make docs-build` 통과 확인**

```bash
make docs-build
```

Expected: PASS.

- [ ] **Step 4: 커밋**

```bash
git add docs/cli/sniffer.md
git commit -m "$(cat <<'EOF'
docs(cli/sniffer): document SNIFFER_HEALTH_ADDR + curl probes

Co-Authored-By: Claude Opus 4.7 <noreply@anthropic.com>
EOF
)"
```

---

## Task 7: Worker CLI 페이지의 IMGSYNC_HEALTH_ADDR 설명 보강

**Files:**
- Modify: `docs/cli/worker.md`

**왜 필요:** worker 의 `/metrics` 가 같은 포트에서 뜬다는 사실이 worker CLI 페이지에서 빠져 있다. 운영자가 worker 만 보고 `:8080/metrics` 를 expect 하지 못한다.

- [ ] **Step 1: 환경 변수 표의 `IMGSYNC_HEALTH_ADDR` 행 교체**

`docs/cli/worker.md` 에서 `| \`IMGSYNC_HEALTH_ADDR\` | 선택 | \`/healthz\` 수신 주소 (기본 \`:8080\`) |` 행을 다음으로 교체:

```markdown
| `IMGSYNC_HEALTH_ADDR` | 선택 | health 리스너 바인드 (기본 `:8080`). `/livez`, `/readyz`, `/healthz`, `/metrics` 가 모두 같은 포트에 뜬다. |
```

- [ ] **Step 2: `make docs-build` 통과 확인**

```bash
make docs-build
```

Expected: PASS.

- [ ] **Step 3: 커밋**

```bash
git add docs/cli/worker.md
git commit -m "$(cat <<'EOF'
docs(cli/worker): note /metrics shares the IMGSYNC_HEALTH_ADDR port

Co-Authored-By: Claude Opus 4.7 <noreply@anthropic.com>
EOF
)"
```

---

## Task 8: 컴포넌트 컨셉 페이지에 메트릭 emission 노트 추가

**Files:**
- Modify: `docs/concepts/components.md`

**왜 필요:** Worker / Sniffer / Sweeper 가 어떤 메트릭을 어떻게 내보내는지 (push vs scrape) 가 운영 가이드 단계가 아닌 **개념 단계** 에서 한 번 짚어져야 한다. 그래야 metrics catalog 표가 "왜 이 라벨인가" 를 자체 설명할 수 있다.

- [ ] **Step 1: Worker 섹션 끝(`## Sniffer` 헤더 직전) 에 짧은 노트 추가**

`docs/concepts/components.md` 의 `### 종료 신호` 섹션 다음, `## Sniffer` 헤더 직전에 추가:

````markdown
### 메트릭 emission

워커는 lease 시도 / 작업 완료 / FTP 풀 변동 / lease 루프 활성 4 가지 시점에 in-process 콜백 (`OnLeaseAttempt`, `OnFinish`, `OnPoolChange`, `OnWorkerStart/Stop`) 을 부르고, `internal/metrics` 가 이 콜백을 받아 Prometheus counter / histogram / gauge 로 변환합니다. 즉 워커는 메트릭 라이브러리에 직접 의존하지 않고, 콜백 시그니처만 알고 있습니다. 카탈로그는 [모니터링 — 메트릭 카탈로그](../operating/monitoring.md#메트릭-카탈로그) 를 보세요.

````

- [ ] **Step 2: Sniffer 섹션 끝 (`## Sweeper` 헤더 직전) 에 노트 추가**

`docs/concepts/components.md` 의 sniffer 섹션 마지막 (예: `## Sweeper` 직전) 에 다음을 삽입:

````markdown
### 메트릭 emission

Sniffer 는 `RunOnce` 가 끝날 때마다 `OnEnqueue(source, n)` 와 (오류 시) `OnError(source)` 를 부릅니다. CLI (`cmd/imgsync/sniffer`) 가 이 콜백을 `imgsync_sniffer_enqueue_total{source}` / `imgsync_sniffer_run_errors_total{source}` 에 연결합니다. 또한 `:8080/metrics` 에 worker 와 동일한 Prometheus 핸들러를 띄워 ServiceMonitor 가 같은 포트 이름(`http-metrics`)으로 scrape 합니다.

````

- [ ] **Step 3: Sweeper 섹션 끝 (파일 끝 또는 다음 `##` 직전) 에 노트 추가**

`docs/concepts/components.md` 의 sweeper 섹션 끝부분 (`OnCycle` 언급 근처) 에 짧은 노트를 추가하거나, 해당 단락에 `imgsync_sweep_cycles_total` 한 줄을 끼워 넣는다. **간단히 마지막 단락 끝에 다음 한 문장을 append:**

```markdown

또한 `OnCycle` 콜백은 `imgsync_sweep_cycles_total` counter 의 emission 지점이기도 합니다 — 외부 모니터링이 sweeper 가 살아 있는지 확인할 때 이 메트릭의 `rate(...)` 가 0 이 아닌지 본다.
```

- [ ] **Step 4: `make docs-build` 통과 확인**

```bash
make docs-build
```

Expected: PASS.

- [ ] **Step 5: 커밋**

```bash
git add docs/concepts/components.md
git commit -m "$(cat <<'EOF'
docs(components): note metrics emission per worker/sniffer/sweeper

Explain the callback-driven metrics model at the concepts layer so
the operating monitoring page can reference these without re-stating
the architecture.

Co-Authored-By: Claude Opus 4.7 <noreply@anthropic.com>
EOF
)"
```

---

## Task 9: 개발자 아키텍처 deep-dive 에 `internal/metrics` 패키지 섹션 추가

**Files:**
- Modify: `docs/developer/architecture-deep-dive.md`

**왜 필요:** 새 패키지 (`internal/metrics`) 가 들어왔는데 deep-dive 가 이걸 모른다. 컨트리뷰터가 새 메트릭을 추가할 때 어디에 무엇을 넣어야 하는지 한 번 짚어 둘 곳이 필요하다.

- [ ] **Step 1: 파일 끝에 새 섹션 추가**

`docs/developer/architecture-deep-dive.md` 의 마지막 줄에 다음 블록을 append.

````markdown

## `internal/metrics` 패키지

Phase 1 모니터링에서 추가된 패키지로, **워커/스니퍼 코드가 Prometheus 라이브러리에 직접 의존하지 않도록** 콜백 어댑터를 모아둔 layer 다.

### 파일 구조

| 파일 | 책임 |
|---|---|
| `metrics.go` | `imgsync_jobs_processed_total`, `imgsync_lease_attempts_total`, `imgsync_workers_active`, `imgsync_ftp_pool_size`, `imgsync_sweep_cycles_total`, `imgsync_sniffer_*` counter/gauge 정의 + 외부 노출용 콜백 (`OnFinish`, `OnLeaseAttempt`, `OnWorkerStart/Stop`, `OnPoolChange`, `OnCycle`, `OnEnqueue`, `OnError`) |
| `buckets.go` | `imgsync_job_duration_seconds` histogram bucket 상수 (`[0.1, 0.5, 1, 2, 5, 10, 30, 60, 300, 1800]` 초) |
| `db_pool.go` | `pgxpool.Stat()` 을 scrape 시점에 읽어 `imgsync_db_pool_conns{state}` 로 변환 |
| `lease_lock_age.go` | `SELECT EXTRACT(EPOCH FROM NOW()-MIN(locked_at))` 을 scrape 시점에 실행 (2초 timeout) |
| `queue_depth.go` | `SELECT status, COUNT(*) FROM transfer_jobs GROUP BY status` 을 scrape 시점에 실행 |

### Push vs scrape 두 패턴

| 패턴 | 트리거 | 라이브러리 | 적용 메트릭 |
|---|---|---|---|
| **Push (in-process)** | 작업 / 콜백 발생 시점 | `prometheus.CounterVec`, `HistogramVec`, `GaugeVec` | `imgsync_jobs_processed_total`, `imgsync_lease_attempts_total`, `imgsync_workers_active`, `imgsync_ftp_pool_size`, `imgsync_sweep_cycles_total`, `imgsync_sniffer_*`, `imgsync_job_duration_seconds` |
| **Scrape-time** | `/metrics` GET 요청 시 | `prometheus.Collector` 인터페이스 직접 구현 | `imgsync_jobs_in_status`, `imgsync_db_pool_conns`, `imgsync_lease_lock_age_seconds` |

scrape-time 메트릭은 매 GET 마다 DB 쿼리를 한 번 더 던지므로 `interval` 이 너무 짧으면 control DB 가 영향을 받는다. 기본 `30s` (ServiceMonitor 기본값) 이 안전선이다.

### 새 메트릭을 추가할 때

1. `metrics.go` (push) 또는 새 collector 파일 (scrape-time) 에 정의를 추가한다.
2. 라벨 카디널리티가 폭발하는지 사전 점검한다 — `src`, `dst`, `result` 처럼 enum 성격 필드만 라벨에 둔다. `trace_id` / `path` 등 unbounded 필드는 절대 라벨에 넣지 않는다.
3. `metrics_test.go` 에 노출 형식 단위 테스트를, scrape 형이라면 `integration_test.go` 에 testcontainer 기반 통합 테스트를 추가한다.
4. emit 지점 (worker / sniffer / sweeper / FTP pool) 에서 콜백을 받아 호출한다. **Prometheus import 가 emit 지점 코드로 새지 않도록 주의** — 항상 `internal/metrics` 가 단일 진입점이어야 한다.
5. [모니터링 — 메트릭 카탈로그](../operating/monitoring.md#메트릭-카탈로그) 표와 [대시보드 — 패널 명세](../operating/dashboards.md#패널-명세) 표를 같이 갱신한다.

### Health 서버 wiring

`internal/health.NewServer` 는 functional option 패턴으로 바뀌었으며, `WithMetrics(handler http.Handler)` 옵션이 `/metrics` 를 같은 포트에 mount 한다. CLI(`cmd/imgsync/worker`, `cmd/imgsync/sniffer`) 는 `metrics.HTTPHandler()` 를 받아 이 옵션에 연결한다 — 별도 HTTP 서버를 띄우지 않는다.
````

- [ ] **Step 2: `make docs-build` 통과 확인**

```bash
make docs-build
```

Expected: PASS.

- [ ] **Step 3: 커밋**

```bash
git add docs/developer/architecture-deep-dive.md
git commit -m "$(cat <<'EOF'
docs(dev): add internal/metrics architecture section

Document the push/scrape split, file layout, label-cardinality rule,
and the health-server WithMetrics option pattern so contributors
adding new metrics know where to put what.

Co-Authored-By: Claude Opus 4.7 <noreply@anthropic.com>
EOF
)"
```

---

## Task 10: Scaling 가이드에 메트릭 기반 신호 추가

**Files:**
- Modify: `docs/operating/scaling.md`

**왜 필요:** 현재 scaling 가이드는 `pending` 카운트를 SQL 로 보라고 안내한다. 메트릭이 들어왔으니 동일 신호를 PromQL 로도 받을 수 있어야 운영팀이 알람으로 자동화한다.

- [ ] **Step 1: 첫 SQL 신호 언급 직후 메트릭 미러 행 추가**

`docs/operating/scaling.md` 18행 (`3. transfer_jobs 의 pending ...`) **그대로 두고**, 그 단락 또는 섹션 끝에 다음 블록을 추가:

````markdown

같은 신호를 Prometheus 로 보고 있다면 SQL 폴링 대신 PromQL 로 동일 결과를 얻을 수 있다.

| SQL 신호 | PromQL 대체 |
|---|---|
| `SELECT status, count(*) FROM transfer_jobs WHERE status='pending'` | `imgsync_jobs_in_status{status="pending"}` |
| `last_lease_success_ts` 가 늦어짐 | `(time() - imgsync_workers_active offset 1m) > 0` 가 아닌, `rate(imgsync_lease_attempts_total{result="success"}[5m])` 가 0 으로 떨어지는지 |
| 풀 포화 (healthz 의 `pool_in_use ≈ pool_max`) | `imgsync_db_pool_conns{state="in_use"} / imgsync_db_pool_conns{state="max"} > 0.9` |
| FTP host cap 적중 빈도 | `sum by (host) (imgsync_ftp_pool_size{state="in_use"})` 가 `ftpHostMaxConns` 에 붙어 있는지 |

스케일 결정에 쓰는 신호이므로 **임계값과 지속시간(for=10m) 을 묶어 알람으로 거는 것을 권장한다.** 임계 후보 목록은 [모니터링 — 권장 알람](monitoring.md#권장-알람) 을 본다.
````

- [ ] **Step 2: `make docs-build` 통과 확인**

```bash
make docs-build
```

Expected: PASS.

- [ ] **Step 3: 커밋**

```bash
git add docs/operating/scaling.md
git commit -m "$(cat <<'EOF'
docs(scaling): add PromQL mirror for SQL-based scale signals

Co-Authored-By: Claude Opus 4.7 <noreply@anthropic.com>
EOF
)"
```

---

## Task 11: Troubleshooting 가이드에 메트릭 진단 흐름 추가

**Files:**
- Modify: `docs/operating/troubleshooting.md`

**왜 필요:** troubleshooting 페이지가 healthz JSON / SQL 컬렉션을 1차 도구로 안내한다. 메트릭이 들어왔으니 "어느 메트릭부터 보면 되나" 를 한 번 짚어줘야 한다.

- [ ] **Step 1: 파일 구조 확인**

```bash
grep -n "^##" docs/operating/troubleshooting.md
```

가장 마지막 `##` 섹션 직전이나 파일 끝에 새 섹션을 추가할 위치를 정한다.

- [ ] **Step 2: 새 섹션 append**

`docs/operating/troubleshooting.md` 파일 끝에 다음 섹션을 추가:

````markdown

## 메트릭 우선 진단 (Phase 1+)

`/metrics` 가 떠 있는 환경에서는 healthz/SQL 보다 다음 PromQL 4 종을 먼저 본다. 같은 신호를 더 빠르게, history 와 함께 받을 수 있다.

| 증상 | 1차 PromQL | 의미 / 후속 |
|---|---|---|
| 큐가 안 빠진다 | `imgsync_jobs_in_status{status="pending"}` 와 `rate(imgsync_jobs_processed_total[5m])` 비교 | pending 만 증가하면 enqueue 폭증, processed rate 가 0 이면 워커 스톱 |
| Stuck lease | `imgsync_lease_lock_age_seconds` | sweeper threshold 초과면 [런북 §3](runbook.md#3-stuck) 으로 점프 |
| 실패 폭증 | `sum by (result) (rate(imgsync_jobs_processed_total[5m]))` | `fail` / `dead` 가 갑자기 올라가면 `transfer_events` SQL 로 detail 조사 |
| sniffer 가 일을 안 한다 | `rate(imgsync_sniffer_enqueue_total[10m])` | 0 이면 sniffer 사이클이 안 돌거나 source 가 비어 있음 — `imgsync_sniffer_run_errors_total` rate 도 같이 본다 |

PromQL 만 보고 결론을 내지 않고, 항상 짝이 되는 SQL ([런북 §7](runbook.md#7-sql)) 으로 한 번 더 검증한 뒤 후속 액션을 취한다 — 메트릭은 라벨 카디널리티 한계상 일부 디테일을 잘라낸다.
````

- [ ] **Step 3: `make docs-build` 통과 확인**

```bash
make docs-build
```

Expected: PASS.

- [ ] **Step 4: 커밋**

```bash
git add docs/operating/troubleshooting.md
git commit -m "$(cat <<'EOF'
docs(troubleshooting): add metrics-first diagnostic table

Co-Authored-By: Claude Opus 4.7 <noreply@anthropic.com>
EOF
)"
```

---

## Task 12: migrate CLI 페이지에 마이그레이션 목록 표 추가

**Files:**
- Modify: `docs/cli/migrate.md`

**왜 필요:** PR #13 가 `0003_jobs_status_index` 를 추가. 운영자가 "지금 적용돼야 할 마이그레이션이 몇 개인가" 를 가이드에서 한 번에 보고 싶다 — 통합 테스트 (`migrate_test.go`) 의 count 도 3 으로 올라갔다.

- [ ] **Step 1: 파일 끝(`## 동작` 섹션 다음) 에 마이그레이션 목록 섹션 추가**

`docs/cli/migrate.md` 의 마지막 `## 동작` 섹션 다음, 파일 끝에 추가:

````markdown

## 현재 마이그레이션 목록

`migrations/` 디렉터리에 들어 있는 SQL 파일 (2026-05 시점):

| 파일 | 도입 PR | 목적 |
|---|---|---|
| `0001_initial.up.sql` | v1 — Week 1 | `transfer_jobs`, `transfer_events`, `sniffer_state`, `schema_migrations` 테이블 생성 |
| `0002_add_extra_columns.up.sql` | v1 — Week 2 | `transfer_jobs` 에 `attempts`, `last_error`, `last_attempt_at` 등 운영 칼럼 추가 |
| `0003_jobs_status_index.up.sql` | Phase 1.5 모니터링 | `transfer_jobs(status)` b-tree 인덱스. `imgsync_jobs_in_status` scrape SQL (`SELECT status, COUNT(*) GROUP BY status`) 가 succeeded/skipped 누적 행에서 풀 heap scan 으로 떨어지는 것을 방지한다 — index-only scan + HashAggregate 로 처리. |

각 파일은 idempotent 하므로 같은 버전이 이미 `schema_migrations` 에 기록돼 있으면 skip 된다. 새 마이그레이션을 추가할 때는 짝이 되는 `*.down.sql` 도 같이 만든다 (자동 실행되지 않지만 비상 시 수동 적용용).

> 이전 버전이 새 스키마와 호환되어야 한다는 forward-only 정책의 정확한 의미는 [업그레이드 · 롤백 — 마이그레이션 정책](../operating/upgrades-and-rollback.md#마이그레이션-정책) 을 본다.
````

- [ ] **Step 2: `make docs-build` 통과 확인**

```bash
make docs-build
```

Expected: PASS. `../operating/upgrades-and-rollback.md#마이그레이션-정책` 앵커가 존재하는지 확인 (해당 섹션은 PR #14 와 무관하게 이미 있음).

- [ ] **Step 3: 커밋**

```bash
git add docs/cli/migrate.md
git commit -m "$(cat <<'EOF'
docs(cli/migrate): list migrations 0001/0002/0003 with intent

PR #13 introduced 0003_jobs_status_index. Surface the full migration
inventory in the CLI page so operators have a one-stop reference
for "what should have run by now."

Co-Authored-By: Claude Opus 4.7 <noreply@anthropic.com>
EOF
)"
```

---

## Task 13: job-queue-model 컨셉 페이지에 status 인덱스 노트 추가

**Files:**
- Modify: `docs/concepts/job-queue-model.md`

**왜 필요:** 모델 페이지가 `transfer_jobs` 의 컬럼과 제약을 설명한다. PR #13 의 `transfer_jobs(status)` 인덱스가 이 페이지에서 한 번도 언급되지 않으면, 컨트리뷰터가 향후 `status` 를 다루는 쿼리를 짤 때 인덱스 활용을 모를 수 있다.

- [ ] **Step 1: `(trace_id, dst)` UNIQUE 설명 단락 (line 60 근처) 직후, 또는 `transfer_jobs 컬럼` 표 직후에 인덱스 노트 추가**

`docs/concepts/job-queue-model.md` 에서 멱등성 키 단락 (`transfer_jobs 에는 (trace_id, dst) UNIQUE constraint 가 있습니다.`) 의 끝에 다음 단락을 append:

````markdown

`transfer_jobs` 에는 운영을 위한 보조 인덱스가 하나 더 있습니다 — `transfer_jobs_status_idx` (단일 컬럼 b-tree, `status`). 이 인덱스는 모니터링 scrape SQL `SELECT status, COUNT(*) FROM transfer_jobs GROUP BY status` 가 `succeeded`/`skipped` 누적 행에서 풀 heap scan 으로 떨어지는 것을 막기 위해 도입됐습니다 — index-only scan + HashAggregate 로 처리됩니다. lease 경로의 `pending` 후보 선정은 별도의 부분 인덱스 (`(status, ready_at) WHERE status='pending'`) 가 담당하므로, 새 status-only 인덱스는 모니터링용 read-side 만 가속합니다.
````

- [ ] **Step 2: `make docs-build` 통과 확인**

```bash
make docs-build
```

Expected: PASS.

- [ ] **Step 3: 커밋**

```bash
git add docs/concepts/job-queue-model.md
git commit -m "$(cat <<'EOF'
docs(model): note transfer_jobs_status_idx for monitoring scrapes

Explains why the 0003 migration ships a status-only b-tree index
even though lease selection already has a partial index.

Co-Authored-By: Claude Opus 4.7 <noreply@anthropic.com>
EOF
)"
```

---

## Task 14: 최종 검증 — 전체 사이트 빌드 + 변경된 페이지 spot-check

**Files:** 전체 `docs/` (수정 없음, 검증만)

- [ ] **Step 1: 깨끗한 strict build**

```bash
make docs-clean
make docs-build
```

Expected: `INFO -  Documentation built` 로 끝. WARNING 가 새로 생기지 않았는지 (이전 기준선과 비교) 확인한다.

- [ ] **Step 2: 로컬 미리보기로 spot-check (수동)**

```bash
make docs-serve
```

브라우저에서 다음 페이지를 직접 열어 (1) 새 섹션이 렌더되는지, (2) 표가 깨지지 않았는지, (3) 앵커 링크가 동작하는지 본다:

- `http://localhost:8000/installation/values-reference/` — monitoring / logging 섹션
- `http://localhost:8000/operating/upgrades-and-rollback/` — selector 불변성 caveat
- `http://localhost:8000/installation/helm/` — Step 4 추가 + 0.x 경고 admonition
- `http://localhost:8000/configuration/sniffer/` — health/metrics 섹션
- `http://localhost:8000/configuration/environment-variables/` — `SNIFFER_HEALTH_ADDR` 행
- `http://localhost:8000/cli/sniffer/` / `cli/worker/` — env var 표
- `http://localhost:8000/concepts/components/` — 메트릭 emission 단락 3 곳
- `http://localhost:8000/developer/architecture-deep-dive/` — `internal/metrics` 섹션
- `http://localhost:8000/operating/scaling/` — PromQL mirror 표
- `http://localhost:8000/operating/troubleshooting/` — 메트릭 우선 진단
- `http://localhost:8000/cli/migrate/` — 마이그레이션 목록
- `http://localhost:8000/concepts/job-queue-model/` — status 인덱스 단락

각 페이지 안에서 클릭한 cross-link 가 모두 200 으로 연결되어야 한다.

- [ ] **Step 3: 커밋 히스토리 확인**

```bash
git log --oneline main..HEAD
```

13 개 커밋 (Task 1~13) 이 단정한 `docs(<area>): ...` 형식으로 나열되는지 확인한다. 너무 짧으면 squash, 너무 길면 split — 그러나 기본은 그대로 두는 것을 권장.

- [ ] **Step 4: PR 생성 (사용자 승인 후)**

사용자가 명시적으로 PR 생성을 요청하면:

```bash
git push -u origin docs/monitoring-phase-1-guide-2026-05-06
gh pr create --title "docs: reflect monitoring phase 1 + 1.5 in public guide" --body "$(cat <<'EOF'
## Summary
- Fill the public-guide gaps left by PR #13 (transfer_jobs.status index) and PR #14 (monitoring stack).
- 13 commits, all docs-only. No chart/code/test changes.
- Pages touched: installation/{values-reference,helm}, configuration/{sniffer,environment-variables}, cli/{sniffer,worker,migrate}, concepts/{components,job-queue-model}, developer/architecture-deep-dive, operating/{upgrades-and-rollback,scaling,troubleshooting}.
- Already-updated by PR #14 (operating/{monitoring,dashboards,runbook}, mkdocs.yml) are NOT modified again.

## Test plan
- [x] `make docs-build` (strict mode) passes locally
- [ ] Manual spot-check via `make docs-serve` for each of the 13 touched pages
- [ ] CI builds the site and the readthedocs preview renders the new sections

🤖 Generated with [Claude Code](https://claude.com/claude-code)
EOF
)"
```

PR 생성은 **사용자 명시 요청 후에만** 실행한다.

---

## 자가 점검 (plan 작성 후 1회)

**Spec coverage:**

- PR #13 변경 (3 파일):
  - `migrations/0003_jobs_status_index.up.sql` → Task 12 (migrate CLI 목록), Task 13 (job-queue-model 노트) 에서 다룸 ✓
  - `migrations/0003_jobs_status_index.down.sql` → 자동 실행되지 않음, Task 12 에서 down.sql 정책 언급 ✓
  - `internal/db/migrate_integration_test.go` (테스트) → 가이드 문서에 노출할 surface 없음 (의도적 누락) ✓
- PR #14 변경 카테고리:
  - `internal/metrics/*` (신규 패키지) → Task 9 (deep-dive) ✓
  - `internal/worker/*` (Job.Duration, runner emit) → Task 8 (components), Task 9 (deep-dive) ✓
  - `internal/sniffer/sniffer.go` (OnEnqueue/OnError) → Task 4 (config/sniffer), Task 8 (components) ✓
  - `internal/cli/sniffer.go` (/livez,/readyz,/metrics) → Task 4, Task 5, Task 6 ✓
  - `internal/health/server.go` (Option 패턴, WithMetrics) → Task 9 (deep-dive 의 wiring 단락) ✓
  - `internal/transports/ftp/pool.go` (OnPoolChange) → Task 8 (components 워커 단락에 콜백 목록), Task 9 ✓
  - `cmd/imgsync/worker.go` (wiring) → Task 7 (cli/worker), Task 9 ✓
  - Helm: `deployment.yaml`, `sniffer-deployment.yaml`, `service.yaml`, `sniffer-service.yaml`, `servicemonitor.yaml`, `NOTES.txt` UPGRADE CAVEAT, `values.yaml` 신규 키, `dashboards/imgsync-overview.json` →
    - selector 변경 → Task 2 (upgrades), Task 3 (helm) ✓
    - sniffer-service / port-name `http-metrics` → Task 4 (sniffer config 의 노트), Task 9 (deep-dive) ✓
    - servicemonitor 토글 → Task 1 (values), Task 3 (helm Step 4) ✓
    - dashboards JSON → 이미 `operating/dashboards.md` 가 import 절차를 설명. 추가 변경 불필요 ✓
    - values.yaml monitoring/logging → Task 1 ✓
  - 운영 docs (이미 갱신됨) → 이번 plan 에서 손대지 않음 ✓
  - `mkdocs.yml` (대시보드 nav) → 이미 갱신됨 ✓
  - `Makefile` (testcontainers 관련) → 가이드 surface 없음, 누락 의도적 ✓
  - `go.mod` / `go.sum` → 가이드 surface 없음, 누락 의도적 ✓

**Placeholder scan:** 모든 코드/마크다운 블록은 정확한 컨텐츠로 채워져 있다. "TBD", "TODO", "...", "appropriate", "similar to" 등 검색 결과 없음 (Task 본문 내).

**Type / 식별자 일관성:**
- `SNIFFER_HEALTH_ADDR` 는 Task 4, 5, 6 에서 모두 동일 표기.
- `IMGSYNC_HEALTH_ADDR` 는 Task 5, 7 에서 동일.
- `monitoring.serviceMonitor.{enabled,interval,scrapeTimeout,labels,namespace}` 와 `monitoring.podAnnotations` 는 Task 1 에서 정의, Task 3 에서 사용 — 일치.
- `imgsync_jobs_in_status`, `imgsync_jobs_processed_total`, `imgsync_lease_attempts_total`, `imgsync_workers_active`, `imgsync_db_pool_conns`, `imgsync_lease_lock_age_seconds`, `imgsync_ftp_pool_size`, `imgsync_sweep_cycles_total`, `imgsync_sniffer_enqueue_total`, `imgsync_sniffer_run_errors_total`, `imgsync_job_duration_seconds` — 11 개 메트릭 이름이 모든 Task 에서 동일 표기로 등장.
- 콜백 이름 (`OnFinish`, `OnLeaseAttempt`, `OnWorkerStart/Stop`, `OnPoolChange`, `OnCycle`, `OnEnqueue`, `OnError`) 도 Task 4, 8, 9 에서 동일.
- 차트 templates 의 서비스 포트 이름 `http-metrics` 는 Task 4, 9 에서 동일.

**앵커 링크 검증 (strict mode 가 잡지만 미리 매핑):**
- `../operating/monitoring.md#메트릭-카탈로그` → 기존 헤더 (PR #14 commit) ✓
- `../operating/monitoring.md#권장-알람` → 기존 헤더 (PR #14 commit) ✓
- `../operating/dashboards.md#패널-명세` → 기존 헤더 (PR #14 commit) ✓
- `../operating/upgrades-and-rollback.md#차트-스키마-break-selector-라벨-변경-chart-10` → Task 2 가 만드는 새 헤더. slugify (case=lower) 로 한국어 + `:` + `(`,`)` 가 어떻게 변환되는지 strict build 가 검증해야 한다. 만약 빌드가 실패하면 헤더를 `## Selector 라벨 변경 (chart 1.0+)` 같은 단순 형태로 다듬는다.
- `runbook.md#3-stuck`, `runbook.md#7-sql`, `runbook.md#8-incident` → 기존 헤더 (PR #14 commit) ✓

→ 빌드 시 헤더 slug 충돌이 일어나면 Task 2 의 헤더를 단순화한 뒤 Task 3 의 admonition 링크를 같이 수정한다 (두 곳만 영향).

---

## Execution Handoff

**Plan complete and saved to `docs/superpowers/plans/2026-05-06-monitoring-phase-1-guide-docs.md`. Two execution options:**

1. **Subagent-Driven (recommended)** — fresh subagent per task (Task 1~13), 두 단계 review (코드 리뷰 + spec 매핑 리뷰), 빠른 iteration. 14 개 task 가 독립적이라 병렬화 가능 — Task 1, 2, 4, 5, 6, 7, 12, 13 은 서로 다른 파일에 손대므로 동시에 실행해도 충돌 없음. Task 3 은 Task 2 의 헤더 slug 에 의존하므로 Task 2 직후 실행.

2. **Inline Execution** — 이 세션에서 Task 1~14 를 순차 실행. 중간 4 / 9 / 14 에서 사용자 review checkpoint.

**어떤 방식으로 가시겠어요?**
