# RFC-031: Statistics E2E Verification

**Status:** Implemented
**Item:** P1.1 — Wire statistics from FDB

## Problem

P1.1 says `StatisticsProvider` returns `1e6` for everything. That description is stale: `fetchTableStatistics()` was implemented in nightshift-100 (612cfc20) and wired into the production planning path at `cascades_generator.go:299`. The full pipeline works:

1. SQL INSERT → `SaveRecord()` → `addRecordCount()` (FDB atomic ADD)
2. SQL SELECT → `planSelectCascades()` → `fetchTableStatistics()` → snapshot read of count index → `MapStatistics` → planner cost model

Unit tests in `plan_harness_test.go` prove `MapStatistics` affects plan selection. But no FDB integration test proves the end-to-end path: insert rows → EXPLAIN → assert plan reflects real row counts.

## Investigation

**Java comparison:** Java does NOT feed FDB record counts into the Cascades planner. Java's `CardinalitiesProperty` derives cardinality purely from schema analysis (unique index fully bound → 1, otherwise unknown). Go's `StatisticsProvider` with real FDB counts is a Go-only improvement.

**Go code audit:**
- `fetchTableStatistics()` at `cascades_generator.go:1049-1101` — opens a snapshot transaction, reads per-type counts via `GetSnapshotRecordCountForRecordType`, returns `MapStatistics`.
- Called at 3 sites: SELECT planning (line 299), subquery planning (line 336), DML planning (line 584).
- Metadata builder at `metadata/builder.go:200-204` always configures `RecordTypeKey()` (multi-table) or `EmptyKey()` (intermingled).
- Count maintenance fires on every insert (`store.go:617`) and delete (`store.go:382`).
- Nil/error returns fall back to `DefaultStatistics{}` (1e6) — best-effort, never blocks planning.

## Bug fixes (Torvalds review)

**Bug 1: Unnecessary transaction commit.** `fetchTableStatistics` used `DB.Run()` which calls `Transact()` — committing an empty read-write transaction on every uncached query. Fixed: added `FDBDatabase.RunRead()` using `ReadTransact()` (no commit, no write conflict ranges), refactored stats reading to use direct snapshot reads via `ReadTransaction.Snapshot().Get()` instead of opening a full store.

**Bug 2: Intermingled fallback fabricates equal distribution.** The `else` branch divided `total / nTypes` for non-RecordTypeKey schemas, feeding incorrect per-type counts to the cost model. Fixed: return nil for non-RecordTypeKey schemas — honest "don't know" falls back to `DefaultStatistics` (1e6 for all types), which is safe.

**Bug 3 (noted, not fixed): No statistics caching.** Every cache-miss SELECT opens a read-only FDB transaction. The plan cache already mitigates most re-reads. Stats-level caching (TTL-based) is a future optimization.

## Fix

1. Add `FDBDatabase.RunRead()` — read-only transaction wrapper
2. Refactor `fetchTableStatistics` to use `RunRead` with direct snapshot reads
3. Drop intermingled equal-distribution fallback
4. Add FDB integration test proving stats-driven plan selection works e2e

## Performance

`fetchTableStatistics` now uses read-only snapshot transactions (no commit overhead). ~1 FDB snapshot read per record type per uncached query. Snapshot reads are non-conflicting.

## Test plan

- FDB integration test: `TestFDB_StatisticsDrivenPlanSelection` — insert data, EXPLAIN, assert plan shape
- Existing unit tests continue passing: `TestPlanHarness_WithStats_*`, `TestEndToEnd_CostExtractionWithStatistics`
