# 프로토콜

imgsync 는 `src_protocol` / `dst_protocol` 값으로 전송 드라이버를 선택합니다.
현재 지원하는 프로토콜은 `localfs` 와 `ftp` 이며, `s3` 는 예정입니다.

## URL 표기 규칙

### `localfs`

컨테이너 파일시스템의 절대 경로를 사용합니다. [RFC 8089](https://datatracker.ietf.org/doc/html/rfc8089) 의 `file://` 스킴 또는 절대 경로(`/` 로 시작) 모두 허용됩니다.

```
file:///abs/path/to/file.png   ← 슬래시 3개 (RFC 8089)
/mnt/nas/images/a/b.png        ← 절대 경로 형식도 허용
```

### `ftp`

```
ftp://host:21/path/to/file.png
```

포트 기본값은 `21` 입니다. 경로는 FTP 서버의 절대 경로입니다.

### `s3` (예정)

```
s3://bucket-name/path/to/key
```

## 인증 방법

| 프로토콜 | 인증 방법 |
|---|---|
| `localfs` | 파일시스템 권한 (아래 "알려진 제약" 참고) |
| `ftp` | `IMGSYNC_FTP_USER` / `IMGSYNC_FTP_PASSWORD` 환경 변수 |
| `s3` (예정) | AWS SDK 표준 credential chain (IAM Role / env var) |

FTP 자격증명은 반드시 K8s Secret 으로 주입하세요. `values.yaml` 의 `ftpSecretRef` 참고:

```yaml
ftpSecretRef:
  name: "imgsync-ftp"
  userKey: "user"
  passwordKey: "password"
```

## 알려진 제약

### `localfs`

- **컨테이너 내 가시 경로여야 합니다.** NAS 나 공유 스토리지는 PersistentVolume 으로 마운트해야 합니다.
- **권한:** 컨테이너는 `runAsUser: 65532` 로 실행됩니다. 마운트된 PV 의 파일/디렉터리가 이 UID 로 읽기·쓰기 가능한지 확인하세요 (`fsGroup: 65532` 설정으로 그룹 권한 부여 가능).

```yaml
podSecurityContext:
  fsGroup: 65532
  runAsUser: 65532
```

### `ftp`

- **Passive mode only.** Active FTP 는 지원하지 않습니다. Passive 모드는 방화벽 친화적이며 대부분의 현대 FTP 서버가 지원합니다.
- FTP 세션 풀 튜닝은 [worker.md](worker.md) 의 FTP 풀 섹션을 참고하세요.

## 멀티 프로토콜 매트릭스

소스(`src_protocol`) × 목적지(`dst_protocol`) 조합 지원 현황:

| | **dst=localfs** | **dst=ftp** | **dst=s3 (예정)** |
|---|---|---|---|
| **src=localfs** | ✅ | ✅ | 예정 |
| **src=ftp** | ✅ | ✅ | 예정 |
| **src=s3 (예정)** | 예정 | 예정 | 예정 |

## 관련 페이지

- 전체 환경 변수 → [environment-variables.md](environment-variables.md)
- Sniffer 패턴 설정 → [sniffer.md](sniffer.md)
- Helm 설치 파라미터 → [../installation/helm.md](../installation/helm.md)
