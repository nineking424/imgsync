# Helm 설치

이 페이지는 `deploy/helm/imgsync` 차트를 사용해 imgsync를 Kubernetes 클러스터에 설치하는 순서를 설명한다.

---

## 사전 준비

현재 kubectl 컨텍스트를 확인한다.

```bash
kubectl config current-context
```

이 가이드 전체에서 namespace는 `imgsync`를 사용한다. 다른 이름을 쓸 경우 모든 명령에서 일관되게 교체한다.

---

## Step 1: Secret 생성

설치 전에 필수 Secret을 먼저 생성해야 한다.
→ 생성 명령 및 각 Secret의 상세 내용은 **[Secret 준비](secrets.md)** 를 참고한다.

---

## Step 2: 차트 설치

```bash
helm upgrade --install imgsync deploy/helm/imgsync \
  -n imgsync --create-namespace \
  --set image.repository=<your-repo>/imgsync \
  --set image.tag=<your-tag> \
  --set replicaCount=4 \
  --set ftpSecretRef.name=imgsync-ftp
```

설치 직후 `pre-install` hook으로 `imgsync-migrate` Job이 자동 실행된다. 이 Job은 DB 스키마 마이그레이션을 수행하며, 완료될 때까지 워커 파드 기동이 대기된다.

---

## Step 3: 설치 검증

```bash
# 파드 상태 확인
kubectl -n imgsync get pods

# pre-install hook 마이그레이션 로그 확인
kubectl -n imgsync logs job/imgsync-migrate

# 헬스 엔드포인트 확인
kubectl -n imgsync port-forward svc/imgsync 8080:8080
curl localhost:8080/healthz | jq
```

`/healthz` 응답이 `{"status":"ok"}` 형태면 정상이다.

---

## Step 4: 첫 작업 enqueue

→ 첫 작업 enqueue 명령은 **[운영 매뉴얼 §1](../operating/runbook.md)** 의 enqueue 절을 참고한다.

---

## 업그레이드

```bash
helm upgrade imgsync deploy/helm/imgsync \
  --reuse-values \
  --set image.tag=<new-tag>
```

!!! warning "`--reuse-values` 필수"
    `--reuse-values`를 생략하면 이전에 `--set`으로 지정했던 모든 값(secret 이름, replicaCount 등)이 차트 기본값으로 되돌아간다.

---

## 언인스톨

```bash
helm uninstall imgsync -n imgsync
```

!!! note "DB 스키마는 삭제되지 않는다"
    imgsync의 마이그레이션은 forward-only 방식이다. `helm uninstall` 후에도 DB 스키마는 의도적으로 그대로 남는다. 데이터를 보호하기 위한 설계이며, 재설치 시 마이그레이션 Job이 멱등하게 다시 실행된다.
