# imgsync Shadow Sniffer — Design Spec

**Status:** Brainstorming complete, awaiting user review before plan write-out
**Date:** 2026-04-27
**Branch:** main
**Repo:** nineking424/imgsync
**Author:** nineking (with Claude collaboration via `/superpowers:brainstorming`)

**Related artifacts:**
- imgsync v1 design doc (rev 4 APPROVED): `~/.gstack/projects/nineking424-imgsync/nineking-main-design-20260427-031601.md`
- Test plan: `~/.gstack/projects/nineking424-imgsync/nineking-main-eng-review-test-plan-20260427-040000.md`
- PRD: `/Users/nineking/workspace/app/imgsync/PRD.txt`

---

## Context

imgsync v1 (Two-Table Minimal: `transfer_jobs` + `transfer_events`) replaces an internal NiFi file-transfer pipeline. The v1 design covers: enqueue CLI, worker dispatch, lease/heartbeat sweeper, FTP/LocalFS source/transport, terminal states (succeeded/skipped/dead), 2GB streaming, audit invariants.

**Open Question item 3 ★ (only blocking pre-Week-3 unknown):** how does imgsync get triggered during shadow mode? NiFi triggers off a source DB; imgsync needs an equivalent trigger to run alongside without depending on NiFi.

This spec resolves that question with a **Polling Sniffer**: a new `imgsync sniffer` subcommand that polls the same source DB NiFi reads, deterministically generates trace_ids, and enqueues jobs into `transfer_jobs` — completely independent of NiFi.

This same sniffer doubles as the **v2 Connector first iteration** (PRD's DB Connector pattern with timestamp_column / extract_range / bias polling).

---

## Section 1: Architecture

**Decision:** A1 Polling Sniffer.

- New subcommand: `imgsync sniffer` on the existing single binary.
- Deployed as a separate pod in the Helm chart (`replicas: 1`).
- Polls the source DB on a cron interval (configurable, default 1 min).
- Reads rows in a windowed range, computes `trace_id` deterministically, enqueues into `transfer_jobs` with `ON CONFLICT (trace_id, dst) DO NOTHING`.
- No NiFi knowledge. No NiFi observation. Reads source DB directly with read-only credentials.

**Why polling vs CDC/triggers:**
- Source DB is shared infrastructure; CDC requires DBA buy-in (slow path).
- Polling matches NiFi's existing pattern (low risk of behavioral divergence).
- Doubles as the v2 Connector — work isn't thrown away.

**Why single binary subcommand vs separate service:**
- Single Docker image, single Helm chart.
- Shares config/secrets/pgx pool patterns with worker.
- One deployable artifact reduces ops burden.

---

## Section 2: trace_id design

**Decision:** Deterministic `trace_id = "${source_table}-${pk}"`.

Examples: `images-12345`, `documents-uuid-abc-def`.

**Properties:**
- Deterministic — same source row always produces same trace_id.
- Idempotent — re-sniffing the same row hits `UNIQUE(trace_id, dst)` and is rejected by `ON CONFLICT DO NOTHING`.
- Independent of NiFi — imgsync owns trace_id generation.
- Audit-friendly — operators can grep `images-12345` across sniffer logs, transfer_jobs, transfer_events to trace any single source row's full history.

**dst path generation:** Mirrors NiFi's existing 1:1 row-to-path mapping (TBD: verify NiFi DSL during Week 4 — see Open Question OQ1).

---

## Section 3: Sniffer state

**Decision:** Single-row state table `sniffer_state`.

```sql
CREATE TABLE sniffer_state (
  source_id   TEXT PRIMARY KEY,         -- e.g. 'main-source-db.images'
  last_run_ts TIMESTAMPTZ NOT NULL,     -- watermark from previous sniff
  last_run_pk TEXT,                     -- tie-break for same-ts rows across batches
  updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
```

**Why a state row vs in-memory:**
- Sniffer pod can crash/restart without losing watermark.
- Polling overlap (extract_range with bias) handles brief watermark inaccuracy; state table prevents catastrophic loss.

**Why `last_run_pk` (tie-break):**
- Multiple source rows may share `updated_at` to the second/ms.
- Without tie-break, batch boundaries split same-ts rows arbitrarily → lost rows.
- With tie-break, the next batch resumes from `(last_run_ts, last_run_pk)` and never skips.

**Why TEXT for last_run_pk:**
- Source DB pk type (BIGINT vs UUID vs composite) is unknown until Week 4 (OQ1). TEXT serializes any.

**Polling query shape (illustrative):**
```sql
SELECT id, updated_at, file_path
  FROM <source_table>
  WHERE (updated_at, id::TEXT) > (:last_run_ts, :last_run_pk)
    AND updated_at <= NOW() - INTERVAL '5 seconds'  -- bias to avoid in-flight tx
  ORDER BY updated_at, id
  LIMIT :batch_size;
```

After processing the batch, update `sniffer_state` with the **last** row's `(updated_at, id)`.

---

## Section 4: Reconcile data flow — imgsync self-validation only

**Decision:** Drop NiFi observation entirely. imgsync's own `transfer_events` audit is the source of truth.

### What runs during shadow

```
[source DB]
   │
   ├──→ NiFi (production, runs untouched, imgsync ignores it)
   │       └→ FTP target/<canonical_path>          ← real production file
   │
   └──→ imgsync sniffer (shadow, independent polling)
           └→ FTP target/<canonical_path>.imgsync_shadow_v1   ← imgsync's shadow file
```

- Both systems independently query the same source DB.
- `.imgsync_shadow_v1` suffix exists **only** to prevent imgsync from overwriting NiFi's production output (operational safety, NOT for cross-system reconcile).
- imgsync looks at its own `transfer_events` only.

### Cutover criteria (replaces design doc SC#1)

| ID | Criterion | Measurement |
|---|---|---|
| **C1** | Sniffer enqueues every source DB row idempotently | sniffer_state.last_run_pk progresses; re-sniff hits UNIQUE conflict (no duplicates) |
| **C2** | Every enqueued job reaches a terminal status (no pending leak) | `SELECT COUNT(*) FROM transfer_jobs WHERE status NOT IN ('succeeded','skipped','dead')` == 0 after 24h shadow |
| **C3** | Dead/skipped ratio at or below NiFi's historical OOM rate | `COUNT(status='dead')/COUNT(*)` < internal NiFi 30-day OOM rate equivalent |
| **C4** | Zero on-call OOM tickets over 30 days (= existing SC#5) | on-call ticket counter |

C1~C4 all pass → cut NiFi off.

### Reconcile table dropped

The existing design doc proposed `reconcile (trace_id, dst, sha256, src_system, status, ts)` for cross-system parity. **Remove it.** `transfer_events` already records the full per-job history.

### Design doc impact

- **SC#1 rewrite:** "NiFi vs imgsync sha256 set-equality 24h+24h" → "C1~C4 imgsync self-audit 24h+24h"
- **C5 (Test Plan Shadow mode reconcile) rewrite:** drop cross-system compare, replace with imgsync-only self-audit invariant (see Section 6 C5')
- **`.imgsync_shadow_v1` suffix:** retain as operational safety, document its non-reconcile purpose

⚠️ SC#1 was locked through 2 prior adversarial reviews. Changing it requires design doc rev 5.

---

## Section 5: Cutover sequence + failure modes

### Phases

```
[Phase 0] Now
  NiFi → FTP target/<path>          (production)
  imgsync = none

[Phase 1] Shadow start (Week 4)
  NiFi → FTP target/<path>          (continues)
  imgsync sniffer → enqueue → worker → FTP target/<path>.imgsync_shadow_v1
  → check imgsync's own transfer_events only

[Phase 2] Shadow 24h+24h passes
  C1~C4 all pass → cutover decision

[Phase 3] Cutover (Week 6 expected)
  Stop NiFi (manual stop, kept rollback-ready)
  Remove .imgsync_shadow_v1 suffix from imgsync dst config (config change, single redeploy)
  imgsync → FTP target/<path>      (sole production)

[Phase 4] Soak 7 days
  on-call counter stays at 0; dead/skipped ratio stays at baseline
  → permanently retire NiFi (delete binary)
```

**Cutover style:** single redeploy (no canary). Justification: shadow already validated worker behavior under full load for 48h+.

### Shadow-period failure modes

| ID | Mode | Detection | Response |
|---|---|---|---|
| **F1** | Sniffer skips source row (window misses new insert) | `COUNT(transfer_jobs WHERE created_at > T) < COUNT(source row WHERE ts BETWEEN T-window AND T)` | widen sniffer extract_range overlap (e.g. 10min → 30min); idempotency absorbs duplicate enqueues |
| **F2** | Sniffer enqueues same row twice (polling overlap, expected) | UNIQUE(trace_id, dst) ON CONFLICT DO NOTHING | none — proves idempotency works |
| **F3** | Worker slower than NiFi, backlog grows | `COUNT(transfer_jobs WHERE status='pending') > N` alert | scale replicas (test plan C7 proves ≥3.2x scale-out) |
| **F4** | FTP target disk fills with `.imgsync_shadow_v1` files | shadow-cleanup cron (separate `imgsync shadow-cleanup --older-than 7d` subcommand) | bulk `find target/ -name '*.imgsync_shadow_v1' -mtime +7 -delete` after shadow ends |
| **F5** | Post-cutover: source row exists in NiFi history but not imgsync | user complains (file missing) | `imgsync replay --trace-id X --dst Y` manual single-row enqueue. **v1.1 scope** (out of v1) |

### Sniffer self-failure modes

| ID | Mode | Detection | Response |
|---|---|---|---|
| **S1** | Sniffer pod crash mid-batch, source DB gets new rows | restart sweeps from last_run_ts, overlap catches new rows | auto-recovery; proven by Integration test S1 |
| **S2** | Source DB query timeout | sniffer query timeout = 30s, retry 3× w/ backoff | on failure, sniffer pod restarts; last_run_ts not advanced (next run re-sweeps) |
| **S3** | sniffer_state.last_run_pk corrupted (tie-break stuck) | metric `sniffer_progress_lag_seconds` (last_run_ts vs NOW()) | manual reset: `UPDATE sniffer_state SET last_run_pk=NULL WHERE source_id='X'` |

### Rollback plan

If critical issue during Phase 4 (7-day soak):
1. `kubectl scale --replicas=0 imgsync-worker imgsync-sniffer`
2. Restart NiFi (binary preserved)
3. NiFi resumes from its own watermark (catches up via its polling)
4. Re-add `.imgsync_shadow_v1` suffix to imgsync config → shadow mode restored

NiFi binary stays physically preserved through Phase 4. Permanent retirement only after shadow + cutover + soak all pass.

---

## Section 6: Sniffer testing plan

Existing eng-review test plan covers worker/source/transport. Sniffer additions only.

### Coverage matrix (sniffer-specific)

| ID | Scenario | Unit | Integration | E2E |
|---|---|---|---|---|
| **S-U1** | extract_range window calc (last_run_ts + overlap) | ✓ | — | — |
| **S-U2** | tie-break (last_run_pk) for same-ts rows | ✓ | — | — |
| **S-U3** | trace_id generation = `${source_table}-${pk}` | ✓ | — | — |
| **S-I1** | source DB query → enqueue (testcontainers: 2 postgres instances) | — | ✓★ | — |
| **S-I2** | idempotency — same row twice → 1 transfer_jobs row | — | ✓★ | — |
| **S-I3** | sniffer crash recovery — kill -9 + restart, no loss | — | ✓★ | — |
| **S-I4** | source DB query timeout → 3 retries, last_run_ts unchanged | — | ✓ | — |
| **S-I5** | tie-break batch correctness — N same-ts rows split across batches | — | ✓★ | — |
| **S-E1** | kind cluster: 1000 source rows → sniffer + worker → 1000 terminal jobs (= C5') | — | — | ✓★ |

★ = critical, ship blocker.

### Critical test cases

#### S0. Polling window overlap correctness (S-I2 + F1 regression)
- Insert source row at ts=T. Run #1 sniffs window [T-30min, T]. 5 min later, run #2 sniffs window [T-25min, T+5min] (5 min overlap).
- Pass: `COUNT(transfer_jobs WHERE trace_id='sourceA-X')` == 1; transfer_events 'enqueue' status appears once.

#### S1. Sniffer crash mid-batch (S-I3)
- 100 source rows. Sniffer processes 50, then `docker-compose kill -9 sniffer`. Restart.
- Pass: final `COUNT(transfer_jobs WHERE trace_id LIKE 'sourceA-%')` == 100. Zero loss, zero duplicates.

#### S2. Tie-break correctness (S-I5)
- 10 source rows with identical `updated_at=T` (pk=1..10). Sniffer batch_size=3 (forces 4 batches).
- Pass: all 10 enqueued exactly once; sniffer_state.last_run_pk = "10".

#### S3. Source DB query timeout isolation (S-I4)
- testcontainers postgres with `pg_sleep(35)` injection; sniffer query timeout = 30s.
- Pass: sniffer times out → 3 retries → all fail → `sniffer_state.last_run_ts` unchanged from previous run.

### Source DB mock strategy

- **Two testcontainers postgres instances:** source DB (mocks NiFi's source) + imgsync DB (transfer_jobs).
- **Source schema:** minimal first-principles subset — `id BIGINT PRIMARY KEY, updated_at TIMESTAMPTZ, file_path TEXT`. Real schema confirmed Week 4 with internal DBA.
- **Polling source contract test set:** same set applies if source type changes (Postgres → MySQL in v2).

### CI integration

- S-U: `make test-unit`
- S-I: `make test-integration` (existing docker-compose + add source DB container)
- S-E1: `make test-e2e-sniffer` (kind cluster, Week 5 cutover gate)

### Budget

- S-U1~U3 (unit): ~30 min CC
- S-I1~I5 (integration): ~0.5 day CC
- S-E1 (E2E + C5'): ~1 day CC
- Total sniffer testing: ~2 days

### Test plan doc impact

Update `nineking-main-eng-review-test-plan-20260427-040000.md`:
- Coverage Map: add rows 21–29 (sniffer scenarios)
- Critical test cases: add C8~C11 (S0/S1/S2/S3)
- Replace C5 with C5' (drop NiFi reconcile, use imgsync self-audit)

---

## Section 7: Open questions, risks, scope cuts

### Open questions (resolve before Week 4 sniffer dev starts)

| ID | Question | Impact | Resolve by |
|---|---|---|---|
| **OQ1** | Source DB schema specifics — `updated_at` column name, timestamp precision (ms/s), pk type (BIGINT/UUID) | sniffer query SQL, trace_id format | Week 4 start, 1 meeting w/ internal DBA |
| **OQ2** | NiFi's polling cadence — 1 min? 5 min? Match it or run faster? | sniffer cron interval, source DB load | Week 4, inspect NiFi config files |
| **OQ3** | Read-only credential availability for source DB | security, sniffer pod secret mgmt | Week 4 start, internal security team |
| **OQ4** | FTP target disk capacity for shadow files (~2× NiFi output) | F4 (disk full) likelihood | Week 4 start; if tight, shorten shadow window or sample shadow (10% of rows) |

### Risks (accepted, mitigations in place)

| ID | Risk | Mitigation |
|---|---|---|
| **R1** | Source DB load 2× during shadow (NiFi + sniffer both polling) | sniffer starts at 1/4 NiFi cadence (read-mostly), ramps to parity once stable |
| **R2** | `.imgsync_shadow_v1` files unused before cutover (disk waste) | shadow-cleanup cron, 7-day retention |
| **R3** | Sniffer derives different dst path than NiFi for same row → divergent outputs | dst-path logic mirrors NiFi 1:1; verified during Week 4 NiFi DSL analysis |
| **R4** | 30-day OOM counter (SC#5) noisy during shadow — can't tell which system crashed | on-call ticket root-cause field must name the system (operational requirement) |

### Out of scope (v1.1+)

- `imgsync replay --trace-id X --dst Y` single-row manual command (F5 mitigation)
- Multi-source-DB sniffer (v1: single source DB only)
- Horizontal scaling for sniffer (v1: single sniffer pod, no advisory lock on sniffer_state)
- Non-Postgres source DB (MySQL, Oracle) — v1 Postgres only

---

## Design summary

Brainstorming added one new component to imgsync v1:

1. **Architecture (Section 1):** `imgsync sniffer` subcommand, single binary, separate pod (replicas=1).
2. **trace_id (Section 2):** `${source_table}-${pk}` deterministic, NiFi-independent.
3. **State (Section 3):** `sniffer_state(source_id, last_run_ts, last_run_pk)` — single-row table, tie-break makes batches safe.
4. **Reconcile (Section 4):** no NiFi compare. `transfer_events` is the source of truth. `.imgsync_shadow_v1` suffix is operational safety only.
5. **Cutover (Section 5):** 4 phases (shadow → 24h+24h → single redeploy → 7-day soak → NiFi retired). Cutover criteria C1~C4 are imgsync-self-only.
6. **Testing (Section 6):** S0~S3 critical (polling overlap, crash recovery, tie-break, query timeout). C5 → C5'.
7. **Open questions:** OQ1~OQ4 — Week 4 start gate.

### Design doc rev 5 changes (apply after this spec is approved)

- New Sniffer section (Architecture + state + cutover phases)
- Schema: add `sniffer_state` table + Helm migration step
- The Assignment: Week 4 (sniffer body), Week 5 (cutover gate kind test)
- SC#1 replaced: NiFi compare → imgsync C1~C4 self-audit
- Cross-reference Test Plan sniffer section
