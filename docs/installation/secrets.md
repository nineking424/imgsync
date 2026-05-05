# Secret 준비

imgsync는 DB 연결 정보와 FTP 자격증명을 Kubernetes Secret으로 관리한다. 아래 표는 각 Secret의 키, `values.yaml` 참조 경로, 필수 여부를 정리한 것이다.

| Secret | 키(들) | values.yaml 참조 | 필수 여부 |
|---|---|---|---|
| `imgsync-dsn` | `dsn` | `dsnSecretRef.{name,key}` | 필수 |
| `imgsync-ftp` | `user`, `password` | `ftpSecretRef.{name,userKey,passwordKey}` | FTP 사용 시 |
| `imgsync-source-dsn` | `SNIFFER_SOURCE_DSN` | `sniffer.secrets.sourceDSNSecretRef` | `sniffer.enabled=true` 시 |
| `imgsync-db-dsn` | `SNIFFER_IMGSYNC_DSN` | `sniffer.secrets.imgsyncDSNSecretRef` | `sniffer.enabled=true` 시 |

---

## Secret 생성 명령

네임스페이스(`-n imgsync`)는 차트 설치 전에 미리 생성하거나, `helm upgrade --install ... --create-namespace`로 함께 만든다.

```bash
# 1. control DB DSN (필수)
kubectl -n imgsync create secret generic imgsync-dsn \
  --from-literal=dsn='postgres://imgsync:pw@pg:5432/imgsync?sslmode=require'

# 2. FTP 자격증명 (LocalFS만 사용할 경우 생략 가능)
kubectl -n imgsync create secret generic imgsync-ftp \
  --from-literal=user=imgsync \
  --from-literal=password='...'

# 3. Sniffer (sniffer.enabled=true 인 경우만)
kubectl -n imgsync create secret generic imgsync-source-dsn \
  --from-literal=SNIFFER_SOURCE_DSN='postgres://...'
kubectl -n imgsync create secret generic imgsync-db-dsn \
  --from-literal=SNIFFER_IMGSYNC_DSN='postgres://...'
```

!!! note "Sniffer DSN 분리 권장"
    `imgsync-db-dsn`은 sniffer 전용 control DB DSN이다. 동일한 DB를 가리키더라도 `imgsync-dsn`을 재사용하지 않고 별도로 만드는 것을 권장한다. 향후 접근 권한을 분리하거나 DSN을 독립적으로 회전할 수 있다.

---

## Secret 회전 (Rotation)

자격증명 갱신이 필요한 경우 `kubectl` 로 Secret을 업데이트한 뒤 파드를 롤링 재시작한다.

```bash
# 예: control DB DSN 교체
kubectl -n imgsync create secret generic imgsync-dsn \
  --from-literal=dsn='postgres://imgsync:newpw@pg:5432/imgsync?sslmode=require' \
  --dry-run=client -o yaml | kubectl apply -f -

# 워커 파드 롤링 재시작
kubectl -n imgsync rollout restart deploy/imgsync
```

재시작 후 `kubectl -n imgsync rollout status deploy/imgsync` 로 완료를 확인한다.
