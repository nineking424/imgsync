---
date: 2026-05-03
branch: docs/e2e-real-cluster-2026-05-03
sha: 0612277
plan: docs/superpowers/plans/2026-05-03-imgsync-e2e-real-cluster.md
guide: docs/e2e-real-cluster-guide.md
operator: nineking424@gmail.com
---

# imgsync — Real-cluster E2E Test Report (2026-05-03)

수동 검증으로 5개 시나리오를 admin@talos-homelab 클러스터에서 실행한 결과.

## Environment

| 항목                | 값                                              |
|---------------------|-------------------------------------------------|
| kubectl context     | admin@talos-homelab (SSH tunnel via 16443)      |
| k8s server          | v1.36.0                                         |
| nodes               | 3 control-plane + 2 worker                      |
| storage class       | nfs-client (NFS subdir provisioner, RWX)        |
| imgsync image       | ghcr.io/nineking424/imgsync:e2e-ae59775         |
| chart               | deploy/helm/imgsync (replicaCount default 2)    |
| namespace           | imgsync-e2e-real (PSS=baseline)                 |
| postgres            | postgres:16.4 + source-postgres:16.4 (NFS-backed PVC) |
| sniffer             | enabled (src=localfs, dst=localfs)              |

## Scenario Results

| ID    | Name                                            | Status | Notes                                                                                  |
|-------|-------------------------------------------------|--------|----------------------------------------------------------------------------------------|
| C5'   | Sniffer self-audit                              | PASS   | 1000/1000 succeeded; distinct trace_id=1000; dead=0; shadow file 1024 bytes            |
| F5a   | Mid-flight worker kill → sweeper recovery       | PASS   | 100/100 succeeded; dead=0; 5 born-leased rows expired by sweeper; attempts unchanged   |
| F5b   | Bad helm upgrade → rollback recovery            | PASS   | rev 2 ImagePullBackOff → rollback to rev 3 deployed; 50/50 succeeded; dead=0           |
| F5c   | uninstall → reinstall idempotent migrations     | PASS   | helm uninstall preserved 30 jobs; reinstall REVISION=1; dead=0; migrate hook idempotent |
| C7    | Throughput scale-out (replica 2→8)              | PASS   | A=21s/47.62 jobs/sec, B=9s/111.11 jobs/sec, 2.33× ratio, dead=0                       |

전 시나리오 PASS. 5/5.

## Numbers

### C7 — Throughput

| Phase | replica | duration | jobs/sec | dead |
|-------|---------|----------|----------|------|
| A     | 2       | 21s      | 47.62    | 0    |
| B     | 8       | 9s       | 111.11   | 0    |

- scale 2→8 wall-clock: 8s (예산 5min 대비 충분)
- ratio = 2.33× (4× replica 에 sub-linear, 정상 패턴)
- 1MB × 1000 건 모두 dead=0

### F5a — Sweeper Recovery

- 100건 정상 시드, 100/100 succeeded
- 별도 5건 `born-leased` (locked_at = NOW()-6min, 'ghost-pod-killed') 삽입 →
  sweeper threshold(5min) 초과로 5건 모두 expired (status=pending)
- attempts 는 그대로 0 → leased→pending 단순 회수, 카운터 증가 없음 (의도된 동작)

### F5b — Rollback

- 1차 helm upgrade `--set image.tag=invalid-tag-foo` → ImagePullBackOff
- `helm rollback imgsync 1` → REVISION=3 deployed
- 50/50 jobs succeeded post-rollback, dead=0

### F5c — uninstall + reinstall

- 30 pending jobs 시드 후 `helm uninstall` → DB rows 30 그대로 보존
- `make e2e-up-real` 재실행 → migrate Job hook-succeeded → 새 REVISION=1
- 30/30 succeeded, dead=0, leased=0

## Known Limitations / Caveats

1. **F5a/F5b/F5c 의 mid-flight 캡처 어려움** — 1KB 짜리 작업이 NFS+8 worker 환경에서
   초당 100건 단위로 드레인되기 때문에 leased 상태를 자연스레 캡처할 수 없음.
   F5a 는 born-leased 시뮬레이션으로, F5b/F5c 는 사후 invariants 검증으로 우회.
2. **macOS 15.2 Local Network Privacy** — Go 바이너리(kubectl 일부 빌드)가 사설망
   엔드포인트를 차단당해 SSH tunnel(localhost:16443 → 192.168.2.100:6443)을
   임시 사용. Apple-signed `/usr/bin/ssh` 만 허용됨.
3. **seeder/wipe Pod ownership** — alpine root 가 만든 dst 디렉터리에 worker(uid
   65532) 가 쓰지 못해 1000 jobs 가 한 번에 dead 로 떨어진 사례 있음. seeder Job 과
   모든 wipe Pod 에 `chown 65532:65532 /srv/imgsync/dst` 강제 (가이드 §1.3, §3, §7).

## How to Reproduce

전체 절차는 [docs/e2e-real-cluster-guide.md](../e2e-real-cluster-guide.md) 참고.
요약:

```bash
# 환경 부트스트랩
make e2e-up-real

# 시나리오 실행 (각 §3~§7 참조)
./scripts/e2e-seed-real.sh 1000 1024
# ... 가이드 절차 따라 INSERT/poll ...

# 정리
make e2e-down-real
```

## Final Teardown

본 리포트 작성 완료 후 `make e2e-down-real` 실행하여 PVC 포함 전 리소스 회수.
