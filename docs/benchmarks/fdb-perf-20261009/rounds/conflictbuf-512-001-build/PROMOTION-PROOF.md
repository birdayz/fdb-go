# batchset50 conflict-buffer path proof (source, not a measured counter)

Candidate: base `26abdbaac12be60ea8955027f6047e10474596f1` plus complete client patch SHA-256 `b3c09057e81ac448c8b05881e10cd52a34067a27152fcd0a4c367e514e0861c3`; all 740 client/module source files match root's frozen manifest. Harness v5 source archive SHA-256 `738348814d86cc6ec828aa3590625275e9f1b74bed7ae92e06e359c480151cfc`.

## One batch, 50 ordinary conflict-generating Sets

- `tools/clientbench/internal/bench/workload.go:76–89` prepares 50 keys per worker, with the common arm prefix and suffix `batchset50/%04d/%04d` (worker, slot). Worker IDs are at most 1023 under the existing 1024-worker bound; slot IDs are 0..49, so the suffix is exactly 20 bytes.
- `workload.go:223–230` performs all 50 Sets in one callback, with distinct prepared 100-byte values. `:248` sends batchset50 to a write transaction (only read1000 and grv use the read-only path).
- `tools/clientbench/cmd/clientbench/backend_go.go:59–65,76` dispatches to the facade's normal Transact and Set. Only timeout and retry-limit defaults are set at `:34–41`; no write-conflict disabling option is enabled.
- `pkg/fdbgo/fdb/transaction.go:197–198` forwards Set to the raw transaction; `pkg/fdbgo/client/transaction_state_entry.go:17–20` forwards to stateSet under the operation state guard.
- `pkg/fdbgo/client/transaction.go:1413–1425` appends the mutation and invokes addWriteConflictForKeyLocked under conflictMu. The latter allocates **2*len(key)+1** bytes at `:2975–2988`, storing [key,key+zero) endpoints. Distinct keys are not removed or deduplicated before this allocation.

## Capacity arithmetic

Let P be the arm prefix byte length. Every batch key has K=P+20 bytes. All 50 write conflicts therefore request:

`50 * (2*K + 1) = 100*P + 2050` bytes.

The existing config requires `clientbench/` (12 bytes), trailing slash and at most 256 prefix bytes (`tools/clientbench/internal/bench/run.go:44–48`). Hence:

- Minimum accepted prefix: P=12, K=32, each conflict=65 bytes, **3250 bytes total > 512**.
- Default `clientbench/v1/`: P=15, K=35, each conflict=71 bytes, **3550 bytes total > 512**; the eighth Set crosses the threshold (8*71=568).
- Maximum accepted prefix: P=256, K=276, each conflict=553 bytes, **27650 bytes total < 32768**.

The candidate's small pool is 512 bytes and large pool 32768 bytes (`transaction.go:1282–1306`). On exhaustion of a small buffer, conflictBufAlloc selects the pool from cumulative required size (`:2926–2937`). Thus, with a normal arm prefix **P<=235**, the first Set uses the small pool and a later Set necessarily promotes to the 32 KiB pool. At longer accepted prefixes (P>=236), even one conflict exceeds 512, so the first allocation directly selects the large pool instead; this still exercises large-buffer handling but not small-to-large promotion. The eventual campaign must retain P<=235 for every arm and record its exact P/K/threshold arithmetic before release. No run has yet been authorized.

This is a deterministic source/shape argument, not an observed allocator counter. It does not establish candidate correctness or a performance win. Root owns parent-client regression/full-suite validation and review. Fresh-process setup also contains larger write batches outside timing; timed batchset50 uses a fresh transaction for each measured operation, so setup does not change the per-transaction cumulative-size calculation.

## Scope of final-state verification

`workload.go:253–294` checks every worker's original write key and all 50 final batch values in a separate read-only transaction, after the measured interval. Expected batch values account for the final measured operation assigned to each worker, falling back to warmup or seed when necessary. This verifies final persistence, not every intermediate committed value. Both adapters must return successful write commits for measured operations; native internal RPC counters remain unobserved.
