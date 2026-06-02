# RFC-064: Explicit conflict-range API differential vs libfdb_c

**Status:** Draft
**Item:** RFC-010 C3 (fresh differential axes). The explicit conflict-range API
(`AddReadConflictRange`/`AddReadConflictKey`/`AddWriteConflictRange`/`AddWriteConflictKey`) feeds
the resolver and decides transaction isolation — a missed conflict is a silent correctness bug, a
spurious one a perf bug. It has **no** differential coverage (only unit tests + single-client
`correctness_test.go`).

## Problem & investigation (empirically probed)

The four methods build conflict ranges sent to the resolver (`transaction.go:1678-1708`): the
`...Key` variants form `[key, key\x00)`; the `...Range` variants take `[begin, end)` and reject
`begin > end` with `inverted_range` (2005). A throwaway probe against libfdb_c established the
ground truth (the C++ NativeAPI source is NOT the spec here — its release build has no
inverted-range check, but the **C binding** that cgo uses returns 2005, matching Go):

| case | go | cgo |
|---|---|---|
| `AddReadConflictRange` inverted (begin>end) | 2005 | 2005 |
| `AddReadConflictRange` empty (begin==end) | 0 | 0 |
| `AddWriteConflictRange` inverted | 2005 | 2005 |
| oversized (11 KB) key in range | 0 | 0 |
| read-conflict, probe write OUTSIDE the range | no conflict | no conflict |
| read-conflict, probe write INSIDE the range | 1020 | 1020 |

So there is **no divergence** — Go matches libfdb_c on the edges and the conflict outcome. (An
earlier dirty probe showed a spurious `OUTSIDE go=1020`; it was a shared-namespace/version-timing
confound — a cleanly-pinned probe shows `OUTSIDE go=cgo=0`. The `INSIDE` case briefly showed
`go=1007` (transaction_too_old) where cgo got 1020: a version-pinning artifact — B's fresh GRV
ratcheted `minAcceptableReadVersion` past A's pinned read version — exactly the RFC-058 issue,
fixed by pinning B to the setup version too.) This RFC pins that matching behavior with a
regression differential.

## Fix

Net-new differential coverage (no production change expected). New file
`pkg/fdbgo/bench/differential_conflict_range_test.go`. Reuses the **RFC-058 pinning discipline**
(both A and B `SetReadVersion(vSetup)` so the conflict outcome is a deterministic function of the
scenario, not GRV timing; transient codes like 1007 → retry the whole scenario).

1. **`TestDifferential_ConflictRangeEdges`** — `AddReadConflictRange`/`AddWriteConflictRange`
   inverted (begin>end → 2005), empty (begin==end → accept), normal (accept), and an oversized
   (>10 KB) key in the range; assert the immediate error code go==cgo.
2. **`TestDifferential_ReadConflictRange`** — A `SetReadVersion(vSetup)` + `AddReadConflictRange`
   (or `AddReadConflictKey`) + `Set(sentinel)`; B (pinned to vSetup) writes a probe INSIDE vs
   OUTSIDE the range, commits; A commits → 1020 iff inside. Assert the outcome (conflicted /
   not) go==cgo, for the Range and the Key variant, probe inside / outside / on each boundary
   (`r0` inclusive begin, `r9` exclusive end).
3. **`TestDifferential_WriteConflictRange`** — A `AddWriteConflictRange` (or `AddWriteConflictKey`)
   + commit (succeeds, marks the range written at A's commit version); a reader R (pinned to
   vSetup, before A's commit) `AddReadConflictKey(probe)` + commit → 1020 iff probe ∈ A's
   write-conflict range. Assert R's outcome go==cgo, inside/outside/boundary.
4. **`TestDifferential_SnapshotReadNoConflict`** (FDB-C++ dev) — a SNAPSHOT read adds NO
   read-conflict (C++ gates every `conflictRange.send` on `!snapshot`), so a concurrent write to
   the read key does NOT conflict the reader; a regular read DOES. Both outcomes go==cgo.
5. **`TestDifferential_SelfWriteReadConflict`** (FDB-C++ dev) — a read-conflict on a key the txn
   also writes still conflicts with a concurrent write (the self-write does not suppress it);
   go==cgo.

The oversized-key case appears only in the edge test (immediate code, both accept): a committed
truncation divergence (C++ truncates a conflict-range key to `maxKeySize+1`, Go does not) is
**unobservable** — it would only manifest for a write to a key between `maxKeySize+1` and the full
length, and such writes are rejected `key_too_large` before reaching the resolver. So there is no
reachable wire divergence to pin beyond the immediate accept-code match.

## Performance

Test-only. No production change unless a divergence is found.

## Test plan

The tests above ARE the plan. The edge cases are deterministic (immediate error code). The
commit-conflict cases use the RFC-058 both-pinned pattern + retry-on-transient to be flake-free
(the project forbids flaky tests). Teeth: the boundary cases (`r0` must conflict, `r9` must not)
are the discriminator — a half-open-vs-closed-range bug would flip one. Run under `bazelisk test
//pkg/fdbgo/bench:bench_test`.
