# values.yaml 레퍼런스

!!! warning "유지 비용 주의"
    이 페이지는 `values.yaml` 변경 시 같이 업데이트해야 한다. PR 체크리스트(기여 가이드)에 명시한다. 자동 생성(`helm-docs`) 적용은 후속 과제로 남긴다.

`deploy/helm/imgsync/values.yaml`의 모든 키를 그룹별로 정리한다.

---

## 공통 / 이미지

| 키 | 타입 | 기본값 | 설명 |
|---|---|---|---|
| `replicaCount` | int | `1` | 워커 파드 수. 2 이상이면 `pdb`가 적용된다. |
| `image.repository` | string | `imgsync` | 컨테이너 이미지 저장소 경로. |
| `image.tag` | string | `""` | 이미지 태그. 비우면 `.Chart.AppVersion`을 사용한다. |
| `image.pullPolicy` | string | `IfNotPresent` | 이미지 풀 정책. |
| `imagePullSecrets` | list | `[]` | 프라이빗 레지스트리 인증 Secret 목록. |
| `nameOverride` | string | `""` | 차트 이름 오버라이드. |
| `fullnameOverride` | string | `""` | 전체 릴리스 이름 오버라이드. |

→ 자세한 의미는 [컴포넌트](../concepts/components.md) 참고.

---

## serviceAccount

| 키 | 타입 | 기본값 | 설명 |
|---|---|---|---|
| `serviceAccount.create` | bool | `true` | ServiceAccount 자동 생성 여부. |
| `serviceAccount.name` | string | `""` | 사용할 ServiceAccount 이름. 비우면 릴리스 이름에서 자동 생성한다. |

---

## podAnnotations / podLabels

| 키 | 타입 | 기본값 | 설명 |
|---|---|---|---|
| `podAnnotations` | map | `{}` | 파드에 추가할 annotation. |
| `podLabels` | map | `{}` | 파드에 추가할 label. |

---

## nodeSelector / tolerations / affinity

| 키 | 타입 | 기본값 | 설명 |
|---|---|---|---|
| `nodeSelector` | map | `{}` | 파드 스케줄링 노드 셀렉터. |
| `tolerations` | list | `[]` | 파드 toleration 목록. |
| `affinity` | map | `{}` | 파드 affinity/anti-affinity 규칙. |

---

## dsnSecretRef

| 키 | 타입 | 기본값 | 설명 |
|---|---|---|---|
| `dsnSecretRef.name` | string | `imgsync-dsn` | control DB DSN을 담은 Secret 이름. |
| `dsnSecretRef.key` | string | `dsn` | Secret 내 DSN 값의 키 이름. |

→ Secret 생성 방법은 [Secret 준비](secrets.md) 참고.

---

## ftpSecretRef

| 키 | 타입 | 기본값 | 설명 |
|---|---|---|---|
| `ftpSecretRef.name` | string | `""` | FTP 자격증명 Secret 이름 (예: `imgsync-ftp`). 비우면 FTP 트랜스포트를 사용할 수 없다. |
| `ftpSecretRef.userKey` | string | `user` | Secret 내 FTP 사용자명 키. |
| `ftpSecretRef.passwordKey` | string | `password` | Secret 내 FTP 비밀번호 키. |

→ Secret 생성 방법은 [Secret 준비](secrets.md) 참고.

---

## worker

| 키 | 타입 | 기본값 | 설명 |
|---|---|---|---|
| `worker.workers` | int | `4` | 파드당 고루틴(워커) 수. |
| `worker.idleSleepMin` | string | `50ms` | 큐가 비었을 때 폴링 idle backoff 최솟값. |
| `worker.idleSleepMax` | string | `1s` | 큐가 비었을 때 폴링 idle backoff 최댓값. |
| `worker.ftpHostMaxConns` | int | `8` | 클러스터 전체 FTP 호스트당 최대 동시 연결 수 (advisory lock). |
| `worker.ftpHostPoolMaxIdle` | int | `5` | FTP 연결 풀의 호스트당 최대 유휴 연결 수. |
| `worker.ftpHostPoolIdleTTL` | string | `5m` | FTP 연결 풀 유휴 연결 TTL. |

→ 자세한 의미는 [워커 설정](../configuration/worker.md) 참고.

---

## health

| 키 | 타입 | 기본값 | 설명 |
|---|---|---|---|
| `health.port` | int | `8080` | 헬스 HTTP 서버 포트. |
| `health.livenessProbe.httpGet.path` | string | `/livez` | Liveness probe 경로. |
| `health.livenessProbe.periodSeconds` | int | `10` | Liveness probe 주기(초). |
| `health.livenessProbe.timeoutSeconds` | int | `2` | Liveness probe 타임아웃(초). |
| `health.livenessProbe.failureThreshold` | int | `3` | Liveness probe 실패 허용 횟수. |
| `health.readinessProbe.httpGet.path` | string | `/readyz` | Readiness probe 경로. |
| `health.readinessProbe.periodSeconds` | int | `5` | Readiness probe 주기(초). |
| `health.readinessProbe.timeoutSeconds` | int | `2` | Readiness probe 타임아웃(초). |
| `health.readinessProbe.failureThreshold` | int | `2` | Readiness probe 실패 허용 횟수. |
| `health.startupProbe.httpGet.path` | string | `/readyz` | Startup probe 경로. |
| `health.startupProbe.periodSeconds` | int | `2` | Startup probe 주기(초). |
| `health.startupProbe.failureThreshold` | int | `30` | Startup probe 실패 허용 횟수. period 2s × 30 = 60s grace period. |

→ 자세한 의미는 [컴포넌트](../concepts/components.md) 참고.

---

## podSecurityContext

| 키 | 타입 | 기본값 | 설명 |
|---|---|---|---|
| `podSecurityContext.fsGroup` | int | `65532` | 파드 볼륨의 파일시스템 GID. |
| `podSecurityContext.runAsNonRoot` | bool | `true` | root로 실행 금지. |
| `podSecurityContext.runAsUser` | int | `65532` | 파드 프로세스 UID. |
| `podSecurityContext.seccompProfile.type` | string | `RuntimeDefault` | Seccomp 프로파일 유형. |

---

## securityContext

| 키 | 타입 | 기본값 | 설명 |
|---|---|---|---|
| `securityContext.allowPrivilegeEscalation` | bool | `false` | 권한 상승 금지. |
| `securityContext.capabilities.drop` | list | `["ALL"]` | 드롭할 Linux capability 목록. |
| `securityContext.readOnlyRootFilesystem` | bool | `true` | 루트 파일시스템 읽기 전용. |
| `securityContext.runAsNonRoot` | bool | `true` | root로 실행 금지. |
| `securityContext.runAsUser` | int | `65532` | 컨테이너 프로세스 UID. |

---

## resources

| 키 | 타입 | 기본값 | 설명 |
|---|---|---|---|
| `resources.requests.cpu` | string | `100m` | CPU request. |
| `resources.requests.memory` | string | `128Mi` | 메모리 request. |
| `resources.limits.cpu` | string | `500m` | CPU limit. |
| `resources.limits.memory` | string | `256Mi` | 메모리 limit. |

---

## pdb

| 키 | 타입 | 기본값 | 설명 |
|---|---|---|---|
| `pdb.maxUnavailable` | int | `1` | 동시에 허용되는 최대 비가용 파드 수. `replicaCount >= 2`일 때만 PodDisruptionBudget이 렌더된다. |

---

## service

| 키 | 타입 | 기본값 | 설명 |
|---|---|---|---|
| `service.type` | string | `ClusterIP` | Kubernetes Service 타입. 헬스 모니터링 전용이며 앱 트래픽은 없다. |
| `service.port` | int | `8080` | Service 포트. |

---

## migrationJob

| 키 | 타입 | 기본값 | 설명 |
|---|---|---|---|
| `migrationJob.enabled` | bool | `true` | `pre-install` hook으로 마이그레이션 Job 실행 여부. |
| `migrationJob.backoffLimit` | int | `2` | Job 실패 시 재시도 횟수. |
| `migrationJob.ttlSecondsAfterFinished` | int | `600` | Job 완료 후 자동 삭제까지 대기 시간(초). 기본 10분. |
| `migrationJob.resources.requests.cpu` | string | `100m` | 마이그레이션 Job CPU request. |
| `migrationJob.resources.requests.memory` | string | `64Mi` | 마이그레이션 Job 메모리 request. |
| `migrationJob.resources.limits.cpu` | string | `200m` | 마이그레이션 Job CPU limit. |
| `migrationJob.resources.limits.memory` | string | `128Mi` | 마이그레이션 Job 메모리 limit. |

→ 자세한 의미는 [컴포넌트](../concepts/components.md) 참고.

---

## sniffer

| 키 | 타입 | 기본값 | 설명 |
|---|---|---|---|
| `sniffer.enabled` | bool | `true` | Sniffer 배포 여부. |
| `sniffer.replicas` | int | `1` | Sniffer 파드 수. v1에서는 단일 파드만 지원한다. |
| `sniffer.resources.requests.cpu` | string | `50m` | Sniffer CPU request. |
| `sniffer.resources.requests.memory` | string | `64Mi` | Sniffer 메모리 request. |
| `sniffer.resources.limits.cpu` | string | `500m` | Sniffer CPU limit. |
| `sniffer.resources.limits.memory` | string | `256Mi` | Sniffer 메모리 limit. |
| `sniffer.config.sourceID` | string | `main-source-db.images` | 소스 식별자. Sniffer 내부 구분에 사용. |
| `sniffer.config.table` | string | `images` | 감시할 소스 DB 테이블 이름. |
| `sniffer.config.pkColumn` | string | `id` | 기본 키 컬럼 이름. |
| `sniffer.config.tsColumn` | string | `updated_at` | 변경 감지에 사용하는 타임스탬프 컬럼. |
| `sniffer.config.extraColumns` | string | `file_path` | 패턴 렌더링에 필요한 추가 컬럼(쉼표 구분). |
| `sniffer.config.dstPattern` | string | `/incoming/{{.file_path}}` | destination 경로 패턴. Go `text/template` 문법으로 row 컬럼을 참조한다. Helm이 아닌 sniffer 런타임이 렌더한다. |
| `sniffer.config.srcPattern` | string | `src://images/{{.id}}` | source 경로 패턴. Go `text/template` 문법. |
| `sniffer.config.srcProtocol` | string | `localfs` | source 프로토콜 (`localfs`, `ftp` 등). |
| `sniffer.config.dstProtocol` | string | `localfs` | destination 프로토콜 (`localfs`, `ftp` 등). |
| `sniffer.config.shadow` | bool | `true` | `true`이면 감사(audit)만 하고 실제 enqueue는 하지 않는다. |
| `sniffer.config.batchSize` | string | `500` | 한 번에 처리할 최대 행 수. |
| `sniffer.config.biasSec` | string | `5` | 타임스탬프 비교 시 적용할 클럭 편차 허용값(초). |
| `sniffer.config.intervalSec` | string | `60` | Sniffer 폴링 주기(초). |
| `sniffer.secrets.sourceDSNSecretRef` | string | `imgsync-source-dsn` | 소스 DB DSN을 담은 Secret 이름 (`SNIFFER_SOURCE_DSN` 키). |
| `sniffer.secrets.imgsyncDSNSecretRef` | string | `imgsync-db-dsn` | Sniffer 전용 control DB DSN Secret 이름 (`SNIFFER_IMGSYNC_DSN` 키). |

→ Secret 생성 방법은 [Secret 준비](secrets.md) 참고. Sniffer 동작 원리는 [컴포넌트](../concepts/components.md) 참고.
