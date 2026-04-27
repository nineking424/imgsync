# imgsync — Operator Runbook

This is the on-call cheat sheet. Everything you need to debug a stuck transfer
should be on this page.

## 1. Enqueue a job manually

```bash
kubectl -n <ns> run --rm -it imgsync-cli \
  --image=<repo>/imgsync:<tag> --restart=Never \
  --env=IMGSYNC_DSN=<dsn> -- \
  enqueue \
    --trace-id=<trace> \
    --src=ftp://host/path/to/file \
    --dst=file:///mnt/share/dst/file \
    --src-protocol=ftp \
    --dst-protocol=localfs
```

## 2. Audit a single job (one-line SQL)

```sql
SELECT j.id, j.status, j.attempts, e.status AS event, e.ts, e.detail
  FROM transfer_jobs j LEFT JOIN transfer_events e ON j.id = e.job_id
 WHERE j.trace_id = '<trace>' AND j.dst = '<dst>'
 ORDER BY e.ts;
```

Joining `transfer_jobs.id = transfer_events.job_id` keeps the row set scoped to
one job's events; do not join on `trace_id` alone, which fans out across
re-enqueued (trace_id, dst) pairs.

Status meanings:
- `pending`     — waiting for a worker to lease
- `leased`      — held by a worker, transfer in progress
- `succeeded`   — terminal: success
- `skipped`     — terminal: source absent/unreadable (`ErrSkippable`)
- `dead`        — terminal: permanent error or max_attempts exhausted

Event statuses (in `transfer_events.status`):
- `enqueue`, `lease`, `success`, `skip`, `fail` (transient), `expire`, `dead`

## 3. Find stuck jobs

```sql
-- Anything that's been leased longer than the sweeper threshold
SELECT id, trace_id, locked_by, locked_at, NOW() - locked_at AS held_for
  FROM transfer_jobs
 WHERE status = 'leased'
   AND locked_at < NOW() - INTERVAL '5 minutes'
 ORDER BY locked_at;
```

The sweeper runs every 30s. If the above query returns rows, the sweeper is
not running — check pod logs for the leader pod.

## 4. Scale up / down

```bash
helm upgrade imgsync deploy/helm/imgsync \
  --reuse-values --set replicaCount=8
```

Expected: 8th pod is leasing within 5 minutes (cold) or 1 minute (warm cache).
Check throughput in `/healthz`:

```bash
# The chart's fullname helper collapses to the release name when it contains
# "imgsync"; otherwise the service is "<release>-imgsync". When in doubt:
#   kubectl -n <ns> get svc -l app.kubernetes.io/name=imgsync
kubectl -n <ns> port-forward svc/imgsync 8080:8080
curl localhost:8080/healthz | jq
```

## 5. Rollback a bad release

```bash
helm history -n <ns> imgsync       # find the last good revision
helm rollback -n <ns> imgsync <N>  # rollback to revision N
```

Migrations are forward-only. Rolling back the chart does NOT revert the schema —
the older binary is expected to read the newer schema gracefully (any new column
defaults to NULL/sensible).

If the rollback hangs because the bad release has crashlooping pods:

```bash
kubectl -n <ns> delete pod -l app.kubernetes.io/name=imgsync --force
helm rollback -n <ns> imgsync <N>
```

## 6. Drain before rolling restart (rarely needed)

```bash
# Stop accepting new leases by setting replicaCount=0
helm upgrade imgsync deploy/helm/imgsync --reuse-values --set replicaCount=0

# Wait for in-flight to finish (≤ terminationGracePeriodSeconds = 60s) + sweeper window
sleep 360

# Bring it back up
helm upgrade imgsync deploy/helm/imgsync --reuse-values --set replicaCount=4
```
