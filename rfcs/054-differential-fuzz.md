# RFC-054: Differential fuzzer vs libfdb_c — random op sequences (C2 follow-up)

**Status:** Implemented (FDB-C-dev ACK + Torvalds ACK)
**Item:** RFC-010 C2 follow-up (extends RFC-053)
**Goal:** Push the libfdb_c differential from hand-picked single-op batteries toward
*exhaustive* coverage: generate **random sequences** of operations, apply the same
sequence through both the pure-Go client and `libfdb_c`, and assert byte-identical
persisted state. Sequences exercise interactions that single-op tests miss —
read-your-writes accumulation, repeated atomics on one key, clear-then-set,
overwrite ordering.

## Problem

RFC-053 landed L2 (write battery) + L3 (read parity) and `interop_test.go` has 15
hand-written differential tests — all **single, hand-picked** shapes. Nothing
generates *sequences*, so divergences that only appear under op interaction (e.g. a
ClearRange that clamps then a Set into the cleared region; an atomic applied N times
accumulating in the RYW cache; a Clear of a key written earlier in the same txn)
are unprobed. The C binding is the reference; a fuzzer that drives both and compares
is the closest we get to "absolute proof of behavioral identity" over the op space.

## Design

A Go fuzz target `FuzzDifferential` in `pkg/fdbgo/bench` (reuses the existing
dual-client fixture: one container, `goClient` + `cgoClient`, network-thread
singleton). Runs its seed corpus under `just test`; fuzzes longer on demand
(`-test.fuzz=FuzzDifferential`).

1. **Decode** the fuzz bytes into a deterministic op sequence: a list of
   *transactions*, each a list of ops drawn from a small alphabet —
   `Set(k,v)`, `Clear(k)`, `ClearRange(b,e)`, and the atomics
   (`Add/And/Or/Xor/Max/Min/ByteMin/ByteMax/AppendIfFits`). Keys/values are drawn
   from a **tiny domain** (a handful of short keys, small operands **including the
   empty operand** — `doMin` returns the operand when `!otherOperand.size()`,
   `Atomic.h:184`) so collisions, overwrites, and atomic accumulation are frequent —
   that is where interaction bugs live. **The decode is a fixed left-to-right walk
   of the byte stream — never map iteration — so the op order is fully deterministic
   and reproducible from the seed** (Torvalds).

   **Fixed harness invariant — API version 730.** Both clients run at API version
   730 (the bench `TestMain`), so the `apiVersionAtLeast(510)` `Min→MinV2` /
   `And→AndV2` op-code upgrade (`NativeAPI.actor.cpp:5990-5994`) applies identically
   on both sides. The fuzzer relies on this — it is not incidental (FDB-C-dev).
2. **Apply** each transaction through **both** clients to its own prefix
   (`goPfx`/`cPfx`), committing per transaction (so RYW accumulation within a txn is
   exercised, and committed state carries across txns).
3. **Compare:** after the sequence, read the full KV set under each prefix, strip
   the prefix, and assert the residual (key,value) lists are **byte-identical**.
   Reads are pinned to a **fresh shared read version** (RFC-053 L3 lesson: the two
   clients' independent GRV caches otherwise diverge). On mismatch, fail with the
   decoded sequence so the seed reproduces it.

### Excluded (with reason — no silent gaps)

- **SetVersionstampedKey/Value:** the 10-byte stamp is the commit version, which
  differs per client/txn → not byte-comparable without masking, and the key
  variant isn't even known until commit. Covered by RFC-053's dedicated SVV test;
  excluded from the byte-compare fuzzer rather than fudged.
- **Oversized keys/values:** the C binding *aborts* the process on these
  (`CATCH_AND_DIE`), so they can't run in-process. The thresholds are pinned by
  RFC-053's L1 + boundary differential. The fuzzer keeps operands within limits.
- **Deliberate conflicts / control-plane:** versions, reply tokens, conflict
  ordering — per the C2 data-plane rule.

## Performance

Test-only. Each fuzz exec is a bounded op sequence against one shared container.
Slower than a pure-CPU fuzzer (FDB round-trips), but the seed corpus is small and
runs in CI; long fuzzing is manual/opt-in. No production code.

## RYW coalescing — the key intra-txn axis (FDB-C-dev Gap B)

C++ RYW `coalesce` folds, *within one txn*, an atomic applied after a `SetValue`
into a literal `SetValue` (`WriteMap.cpp:366`), and atomic-onto-atomic of the same
type stays that type (`:368`). Byte-identity then holds **only if Go's RYW evaluates
the atomic against the RYW-pending Set, not the storage value**. The fuzzer's
per-txn multi-op grouping exercises this; the seed corpus pins it explicitly. If Go
diverges here, the corpus seed fails — i.e. the fuzzer *finds* the bug rather than
masking it.

## Test plan

- `FuzzDifferential` seed corpus with crafted sequences, each targeting an
  interaction axis: **(a)** `Set(k,v)` then `Add(k,1)` in one txn → both commit
  `v+1` (RYW atomic-onto-Set coalescing, `WriteMap.cpp:366`); **(b)** same-key
  atomic accumulation (`Add(k,1)`×N in one txn); **(c)** clear-then-set; **(d)**
  ClearRange-clamp then Set into the cleared region; **(e)** a missing-key `Min`
  (the Min→MinV2 teeth case).
- Demonstrated to have teeth: temporarily skipping the Min→MinV2 upgrade makes the
  fuzzer fail on the missing-key-Min seed.
- Isolation: each fuzz exec clears its prefixes first, and the prefixes carry a
  per-process nonce so parallel fuzz workers (separate processes, shared container)
  never collide; reads pin a fresh shared GRV (the RFC-053 L3 lesson).
- `just test` green (corpus runs); a manual `-test.fuzztime` burst produces 0
  mismatches. This **samples** the op space toward "closest we get to absolute
  proof" — a fuzzer samples, it does not *prove* identity.

**Result:** corpus + `TestDifferential_RYWCoalescing` (6 interaction cases) pass;
teeth verified (reverting Min→MinV2 fails `min_missing_then_add`); a 40s live burst
ran **8068 execs at ~230/sec with 0 mismatches** — 8000+ random op sequences,
byte-identical persisted state across the pure-Go client and libfdb_c.
