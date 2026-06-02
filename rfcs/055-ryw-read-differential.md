# RFC-055: RYW-read differential vs libfdb_c (C2 follow-up)

**Status:** Implemented (scope narrowed — see "Findings & scope")
**Item:** RFC-010 C2 follow-up (extends RFC-053/054)

## Findings & scope (what landed vs RFC-056)

The RYW-read differential immediately surfaced a **cluster** of RYW-resolution
divergences from libfdb_c. What this PR lands vs defers:

**Landed (verified):**
- **Get + GetRange RYW differential** (`differential_ryw_test.go`): one uncommitted
  txn per client at a shared read version, identical seeded storage + pending ops,
  byte-compare the RYW-merged `Get`/`GetRange`. Deterministic cases pass.
- **Fix: getRange dropped empty-value pending keys** (`ryw.go`). The merge appended
  pending writes only `if entry.value != nil`, but `value == nil` means
  "Set-to-empty / atomic-resolved-to-empty" — a PRESENT key (cleared keys are
  removed from the write map, never tombstoned). So an empty-value pending key (e.g.
  from `Xor(k,"")`, or a resolved atomic) vanished from a merged GetRange. Now always
  appended. Pinned by the `xor_empty_operand` case.

**Deferred to RFC-056 (RYW-correctness audit) — real divergences, documented not
papered:**
- **GetKey ignores RYW.** `Transaction.GetKey` resolves the selector against storage
  only (readpath.go `getKey`), never merging pending writes — unlike C++
  `RYW::getKey`/`resolveKeySelectorFromCache`. A pending Clear/Set does not shift the
  resolution. The correct fix is a faithful port of that algorithm (`removeOrEqual` +
  offset walk over the merged write-map with readToBegin/readThroughEnd); a shortcut
  via merged GetRange does NOT capture FDB's offset/orEqual semantics (verified: it
  diverged on `{key, orEqual, offset>1}`). Snapshot.GetKey is correct (bypasses RYW).
- **applyAtomic on present empty values.** e.g. `Xor(d,"")` then `Min(d,"0")`: Go
  resolves to the operand (`0x30`, MinV2-missing semantics) while libfdb_c does
  little-endian min with zero-extended empty (`0x00`). The RYW `applyAtomic` empty/
  missing handling diverges for several ops.

These are a connected RYW read-resolution audit (a `FuzzRYWRead` fuzzer that drives
them is held for RFC-056, where it validates the fixes). Rushing a partial port of
the selector/atomic resolution would ship NEW divergences, so it is deferred whole.
**Goal:** Close the one remaining differential gap on the data plane:
**read-your-writes (RYW) reads** — a `Get`/`GetKey`/`GetRange` issued *within* an
uncommitted transaction, whose result the client resolves by merging the txn's
*pending* mutations with the storage snapshot. Assert that the pure-Go client and
`libfdb_c` resolve these reads byte-identically.

## Problem

RFC-053/054 compare the **committed** state. That exercises RYW *write* resolution
(the commit-time coalesce — atomic-onto-Set folds, etc.) because the committed
bytes *are* the coalesced result. It does **not** exercise RYW *reads*: when an
app, mid-transaction, reads a key it just Set / Cleared / Add'd, the client must
serve the *pending* value (merged with storage) — a **distinct code path** from
commit-coalesce. The Go RYW cache (pending write map + clear-range map + atomic
accumulation + range-merge) is exactly where transliteration-from-C++ bugs hide,
and nothing differentially tests the *read* side of it. A divergence here is a
real behavioral difference (a Go app reading its own pending writes gets a
different answer than a C app) even when committed bytes agree.

## Design

Extend the RFC-054 fuzzer (same `pkg/fdbgo/bench` dual-client fixture) with an
RYW-read mode and add a focused deterministic test.

**Determinism setup (so storage is identical for both clients):**
1. Clear both per-test prefixes (`goPfx`/`cPfx`).
2. Optionally pre-seed identical committed data into both prefixes (one committed
   txn each), then capture **one shared read version `V`** (RFC-053 L3 lesson).
3. Open ONE uncommitted transaction per client, `SetReadVersion(V)`, and apply the
   **same** random op sequence (writes only — Set/Clear/ClearRange/atomics) as
   *pending* mutations.
4. After each prefix of the sequence, issue RYW reads through both txns and
   compare: `Get(k)` for each key in the domain, `GetRange(prefix)` (the merged
   pending+storage view), and `GetKey(selector)`. Strip the prefix; assert
   byte-identical values / merged KV sets / resolved keys.
   **GetKey selectors MUST be clamped to the seeded prefix range** (Torvalds): a
   selector that walks off the end of the prefix resolves into the
   concurrently-mutated shared keyspace and would false-positive. Probe keys stay
   within `[prefix, prefix+\xff)` and the selector result is only compared when both
   land inside the prefix (else it's a no-op, not a divergence).
5. Never commit (RYW reads are pre-commit). **Both txns are explicitly `Cancel()`ed**
   (Torvalds): the cgo binding's C handle needs explicit cleanup, not GC.

Because storage@`V` is identical (seeded the same) and the pending ops are
identical, any difference is a pure RYW-resolution divergence — the target.

**`FuzzRYWRead`** drives steps 3-4 from fuzz bytes (reusing RFC-054's deterministic
op decoder). **`TestDifferential_RYWReads`** pins deterministic cases: read a
pending Set; read a key cleared after a Set; read across a pending ClearRange
(hole); read a key with accumulated atomics; GetRange spanning pending + seeded
storage; GetKey selector landing on a pending vs cleared key. Plus the three
highest-divergence-risk paths FDB-C-dev called out, seeded explicitly:
- **(a)** read INTO a pending clear-range hole that crosses the seeded-prefix
  boundary (clear `[a,c)` over seeded `b`; `GetRange` must omit `b`, `GetKey` must
  skip it) — `WriteMap` clear-range merge in the read path.
- **(b)** `GetKey` with a non-trivial **offset >1** over pending writes
  (`resolveKey` offset arithmetic is the subtlest read path) — clamped to the
  prefix per the rule above.
- **(c)** an atomic accumulated onto a key **cleared earlier in the same txn** (the
  dependent-op-onto-cleared-range branch: it coalesces over an empty `SetValue`
  base, NOT unreadable) — distinct from atomic-onto-storage.

Seeding care (FDB-C-dev): for `CompareAndClear` reads, seed the base value so both
clients see the same compare outcome (a matching CAC makes `kv()` return nullptr →
the key vanishes from the merged view); for atomic-on-cleared-range, both clients
must agree the result is readable.

### Excluded (unchanged from RFC-054, same reasons)

Versionstamp (stamp unknown pre-commit *and* not byte-comparable), oversized (C
aborts), conflicts/control-plane. Snapshot reads (`Snapshot().Get`) bypass RYW by
design — out of scope for the *RYW* differential (covered separately if needed).

## Performance

Test-only; reuses one container. RYW reads are client-local (no commit), so each
exec is cheaper than RFC-054's commit-per-txn — more execs/sec.

## Test plan

- `TestDifferential_RYWReads` deterministic cases pass.
- Teeth: a deliberately broken RYW read-resolution (e.g. ignoring a pending Clear)
  makes a case fail.
- `FuzzRYWRead`: corpus green in CI; a manual burst produces 0 mismatches.
- Honest framing: this **samples** the RYW-read space — closest we get, not a proof.
