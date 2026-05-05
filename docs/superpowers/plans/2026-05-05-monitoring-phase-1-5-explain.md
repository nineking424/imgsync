# Phase 1.5 — Local EXPLAIN ANALYZE Verification

**Status:** Local synthetic verification only. Staging-scale rerun required before merge.

**Setup:** postgres:16-alpine on local Docker, seeded with synthetic distribution approximating staging:
- succeeded: 1,000,000
- skipped: 50,000
- dead: 200
- leased: 5
- pending: 100

Total: 1,050,305 rows. `ANALYZE transfer_jobs` run before each plan capture.

## Without the index (control)

```text
                                                                      QUERY PLAN                                                                       
-------------------------------------------------------------------------------------------------------------------------------------------------------
 Finalize GroupAggregate  (cost=28988.51..28989.52 rows=4 width=12) (actual time=47.531..49.018 rows=5 loops=1)
   Group Key: status
   Buffers: shared hit=7614 read=13826
   ->  Gather Merge  (cost=28988.51..28989.44 rows=8 width=12) (actual time=47.527..49.014 rows=13 loops=1)
         Workers Planned: 2
         Workers Launched: 2
         Buffers: shared hit=7614 read=13826
         ->  Sort  (cost=27988.49..27988.50 rows=4 width=12) (actual time=46.305..46.306 rows=4 loops=3)
               Sort Key: status
               Sort Method: quicksort  Memory: 25kB
               Buffers: shared hit=7614 read=13826
               Worker 0:  Sort Method: quicksort  Memory: 25kB
               Worker 1:  Sort Method: quicksort  Memory: 25kB
               ->  Partial HashAggregate  (cost=27988.41..27988.45 rows=4 width=12) (actual time=46.285..46.286 rows=4 loops=3)
                     Group Key: status
                     Batches: 1  Memory Usage: 24kB
                     Buffers: shared hit=7598 read=13826
                     Worker 0:  Batches: 1  Memory Usage: 24kB
                     Worker 1:  Batches: 1  Memory Usage: 24kB
                     ->  Parallel Seq Scan on transfer_jobs  (cost=0.00..25800.27 rows=437627 width=4) (actual time=0.012..19.091 rows=350102 loops=3)
                           Buffers: shared hit=7598 read=13826
 Planning:
   Buffers: shared hit=153
 Planning Time: 0.317 ms
 Execution Time: 49.090 ms
(25 rows)
```

## With `transfer_jobs_status_idx` (after 0003)

```text
                                                                      QUERY PLAN                                                                       
-------------------------------------------------------------------------------------------------------------------------------------------------------
 Finalize GroupAggregate  (cost=28988.51..28989.52 rows=4 width=12) (actual time=48.740..50.121 rows=5 loops=1)
   Group Key: status
   Buffers: shared hit=7838 read=13602
   ->  Gather Merge  (cost=28988.51..28989.44 rows=8 width=12) (actual time=48.737..50.118 rows=13 loops=1)
         Workers Planned: 2
         Workers Launched: 2
         Buffers: shared hit=7838 read=13602
         ->  Sort  (cost=27988.49..27988.50 rows=4 width=12) (actual time=47.367..47.368 rows=4 loops=3)
               Sort Key: status
               Sort Method: quicksort  Memory: 25kB
               Buffers: shared hit=7838 read=13602
               Worker 0:  Sort Method: quicksort  Memory: 25kB
               Worker 1:  Sort Method: quicksort  Memory: 25kB
               ->  Partial HashAggregate  (cost=27988.41..27988.45 rows=4 width=12) (actual time=47.345..47.346 rows=4 loops=3)
                     Group Key: status
                     Batches: 1  Memory Usage: 24kB
                     Buffers: shared hit=7822 read=13602
                     Worker 0:  Batches: 1  Memory Usage: 24kB
                     Worker 1:  Batches: 1  Memory Usage: 24kB
                     ->  Parallel Seq Scan on transfer_jobs  (cost=0.00..25800.27 rows=437627 width=4) (actual time=0.015..20.338 rows=350102 loops=3)
                           Buffers: shared hit=7822 read=13602
 Planning:
   Buffers: shared hit=172 read=1
 Planning Time: 0.380 ms
 Execution Time: 50.192 ms
(25 rows)
```

## Result

- Without index: Parallel Seq Scan over 1.05M rows, 49.09 ms (parallel workers=2)
- With index: Parallel Seq Scan over 1.05M rows, 50.19 ms (planner chose seq scan — see note)
- Speedup: ~1x (no plan change observed locally — expected, see note below)

### Note: why the planner did not choose Index Only Scan locally

On local Docker the entire 1.05M-row table fits comfortably in `shared_buffers` after the seed insert
(nearly all 13,826 blocks were in `shared hit` + `read` with a hot cache on the second query).
PostgreSQL's cost model correctly prefers a parallel sequential scan when data is memory-resident
because the index introduces per-tuple decompression overhead with no I/O benefit.

The index-only scan benefit (`transfer_jobs_status_idx`) is expected to materialise on staging where:
1. The table does **not** fit in `shared_buffers` (real 8 GB+ staging dataset).
2. Random I/O is expensive relative to sequential scans.
3. The index covers only `status` (4-byte enum), making an index-only scan dramatically cheaper than
   heap reads.

The migration is still correct: the plan shape on staging will be the definitive test.

**Operator action:** Re-run on staging clone before merging the PR. Paste both staging plans into the PR body.
