# RFC-119 — Cycle workload: a pure-client serializability oracle

**Status:** Draft
**Item:** TODO.md "Native fdbgo client" → **C3. Ride their test designs** (now `# NEXT` item 1).
First increment of C3: port FDB's `Cycle` workload. Follow-ups (named in §7) port the other four.
**Spec:** `fdbserver/workloads/Cycle.actor.cpp` @ tag **7.3.75** (`/tmp/fdbsrc`).

---

## 1. Problem

C3 says: FDB's `fdbserver/workloads/*.actor.cpp` are unrunnable for us (Sim2-only), but each
scenario + invariant is language-agnostic — port the adversarial *designs* to drive the pure-Go
client against testcontainers. The named workloads are Cycle, AtomicOps, ConflictRange,
Serializability, FuzzApiCorrectness.

A coverage audit of what already exists in this repo (so we don't reinvent):

| Workload | Existing Go coverage | Gap |
|----------|----------------------|-----|
| **Cycle** | **none** | **entirely missing** |
| AtomicOps | `client/concurrent_stress_test.go` (`TestConcurrentAtomicAdd`: N workers × M `Add` → sum invariant), `client/ryw_adversarial_test.go` (`TestRYWAtomic_AllTypes`: RYW-vs-committed for ~20 op shapes) | distributed idempotency-under-retry stress |
| ConflictRange | `client/atomic_conflictrange_test.go` (write-conflict-range generation per op), `client/retry_adversarial_test.go` (self-conflicting predicate on commit_unknown) | concurrent read/write race-detection scenario |
| Serializability | `pkg/recordlayer/chaos` `RunConcurrent` (model-shadow verify under faults) — **record-layer-coupled**, not a pure-client KV oracle | a pure-client isolation oracle |
| FuzzApiCorrectness | `bench/differential_fuzz_test.go` (random op seq vs libfdb_c, byte-identical persisted state), `client/ryw_fuzz_test.go` (`FuzzRYWCache`) | property-based multi-txn |

**The single highest-leverage gap is Cycle**, and it is special: Cycle is *the* canonical FDB
serializability oracle. It is purely client-level (plain KV, no record-layer schema), so it
exercises exactly our launch target — `db.Transact` + `Get`/`Clear`/`Set`/`commit` + conflict
detection + `onError` retry — and a single deterministic check proves **serializable isolation
held across all concurrent transactions**. Porting Cycle closes the Cycle gap *and* delivers the
missing pure-client Serializability oracle in one workload.

This first increment ports Cycle. It is **test-only** — zero production-code change, zero wire
impact — so the wire-compat surface is nil; the load-bearing question for review is *fidelity of
the ported invariant to the C++ check*, not byte compatibility.

## 2. The C++ design (cited, `Cycle.actor.cpp` @ 7.3.75)

**Ring layout.** `nodeCount` keys; `operator()(n)` writes `key(n) → value((n+1) % nodeCount)`
(`Cycle.actor.cpp:134`). So values form a single directed Hamiltonian cycle
`0 → 1 → 2 → … → N-1 → 0`. Each value is the integer index of the *successor* node.
`key(n)=doubleToTestKey(n/nodeCount, prefix)` (`:125`), `value(n)=doubleToTestKey(n, prefix)`
(`:126`), `fromValue(v)=testKeyToDouble(v,prefix)` decodes back to the int (`:132`).
`doubleToTestKey(p)=format("%016llx", bits(p))` — a 16-hex-char string of the double's IEEE-754
bits (`tester.actor.cpp:82`). For `p ∈ [0,1)` and positive ints, those hex strings sort
order-preserving, so the range read returns nodes in index order.

**Client transaction** (`cycleClient`, `:153-211`). Each txn:
1. pick random `r ∈ [0, nodeCount)` (`:160`);
2. read the 3-edge chain: `r2 = fromValue(get(key(r)))`, `r3 = fromValue(get(key(r2)))`,
   `r4 = fromValue(get(key(r3)))` (`:172-183`). I.e. `r → r2 → r3 → r4`. A missing read is a
   `CycleBadRead` SevError (`:173-182`);
3. `clear(keyRange(r), AddConflictRange::True)` — a single-key-range clear immediately overwritten
   by the following set; "Shouldn't have an effect, but will break with wrong ordering" (`:187-189`);
4. the swap (`:190-192`): `set(key(r), value(r3))`, `set(key(r2), value(r4))`,
   `set(key(r3), value(r2))`. This transposes r2 and r3 in the ring:
   `r → r2 → r3 → r4` becomes `r → r3 → r2 → r4`. **The swap preserves the single-cycle
   property** — it is the move whose serial composition keeps exactly one Hamiltonian cycle, and
   which under *non*-serializable interleaving corrupts the ring (splits it / orphans a node);
5. `commit()`; on `transaction_too_old`/`not_committed` bump a counter and `onError(e)` retry
   (`:200-207`).

**The check** (`cycleCheckData`, `:230-293`). One client reads the whole range at a single read
version (`:316-319`) and walks the ring from index 0, following `i = fromValue(data[i].value)`
exactly `nodeCount` times. It fails (SevError `TestFailure`) iff:
- `data.size() != nodeCount` — "Node count changed" (`:231`);
- it returns to 0 before `nodeCount` steps — "Cycle got shorter" (`c && !i`, `:250`);
- `data[i].key != key(i)` — "Key changed" (`:259`) (range dense & ordered);
- a value decodes outside `[0, nodeCount)` — "Invalid value" (`:269`);
- after `nodeCount` steps `i != 0` — "Cycle got longer" (`:277`).

Passing ⇔ the data is exactly one Hamiltonian cycle over all N nodes. That is the serializability
proof: every committed swap preserved the invariant, so isolation was serializable.

## 3. Proposed Go change (test-only)

New file `pkg/fdbgo/client/cycle_workload_test.go` in the `client` package (alongside the existing
`isolation_test.go` / `concurrent_stress_test.go` / `ryw_adversarial_test.go` adversarial tests —
reuses the `openTestDB` shared-container fixture; **not** `pkg/recordlayer/chaos`, which shadows the
*record store* and is record-layer-coupled — Cycle is plain KV).

A small in-file harness (no new package — YAGNI until a second pure-client workload lands):

```go
type cycleWorkload struct {
    nodeCount int
    prefix    []byte
}
func (w *cycleWorkload) key(n int) []byte    // prefix + %016x(n)  — dense, sorts by index
func (w *cycleWorkload) value(n int) []byte  // decimal int bytes
func (w *cycleWorkload) fromValue(v []byte) (int, error)
func (w *cycleWorkload) setup(ctx, db) error // write key(n) → value((n+1)%N) for all n
func (w *cycleWorkload) clientTxn(ctx, db, rng) error // one swap txn via db.Transact (retry loop)
func (w *cycleWorkload) check(ctx, db) error // single-version range read + Hamiltonian-cycle walk
```

**Encoding divergence, flagged for the C++ reviewer.** The keys/values live under the test's own
unique prefix and are **never shared with Java/C** — they are not wire-compat data. So Go uses a
clean order-preserving encoding (`%016x` of the node index for keys → dense + index-sorted;
decimal for values) instead of porting `doubleToTestKey`'s `bits(double)` hex. The *invariant*
(`check`) is ported 1:1 from `cycleCheckData`. If the reviewer prefers a byte-faithful
`doubleToTestKey` port for exactness, it's a cheap swap — calling it out rather than silently
diverging (CLAUDE.md: diverge only when test-internal + cleaner, and document it).

`clientTxn` uses `db.Transact(ctx, fn)` — the production retry loop already does the C++
`onError(e)` handling (classify → backoff → retry on `transaction_too_old`/`not_committed`), so the
Go client loop IS the port of `:200-207`. The `clear(keyRange(r), AddConflictRange::True)` maps to
`tx.ClearRange(key(r), key(r)+" end")` (single-key-spanning range; the AddConflictRange::True is
the FDB default for ClearRange, so no extra call).

## 4. Executable spec — exactly what the test proves

1. **`TestCycle_SerializableUnderConcurrency`** (real FDB, testcontainers): setup a ring of
   N=1000 nodes; run A=16 goroutines each doing T swap-txns concurrently (real FDB conflict
   detection is the chaos source, exactly as the C++ workload relies on the real cluster);
   `check` passes — the ring is still exactly one Hamiltonian cycle of length N. Run the check
   10× (determinism). Assert a non-trivial number of swaps actually committed (anti-vacuity: the
   workload must have *done* work, not no-op).
2. **Revert-proof (the teeth):** a sub-test that deliberately applies a **non-atomic** swap (two
   separate committed txns instead of one — i.e. break isolation by splitting the swap across
   commits, allowing an interleave) drives `check` **red** with "Cycle got shorter/longer". This
   proves the check actually detects a broken ring, not just that the happy path passes.
   (Equivalently: a unit test that hands `check`'s walk a hand-corrupted ring — split into two
   cycles, an orphan, a changed key — and asserts each named failure mode fires. Pure, fast,
   deterministic, no FDB.)
3. **`check` unit test** (no FDB): table of corrupt rings (size-off, short-cycle, long-cycle,
   key-changed, value-out-of-range) → each maps to the matching `cycleCheckData` failure.

The teeth here are item 2/3: the value of a serializability oracle is entirely in *whether the
check catches a broken ring*. A green happy-path alone is a fake checkbox.

## 5. Wire-compat impact

**None.** Test-only; no production code touched; no bytes written that any other client reads
(own test prefix). The differential-vs-libfdb_c gates do not apply (nothing to diff). The review
question is invariant fidelity, not wire bytes.

## 6. Why this is the right first increment (not all 5 at once)

Project rhythm is one logical change per PR. Cycle is self-contained, the only 0%-covered
workload, and uniquely doubles as the pure-client serializability oracle the audit found missing.
Bundling AtomicOps/ConflictRange/FuzzApi (which already have substantial coverage) into this PR
would be busywork that dilutes review focus. Each remaining workload is its own increment.

## 7. Follow-ups (the rest of C3, each its own PR)

- **Cycle under injected faults** — drive the same workload through `SimTransport` (RFC-118) /
  the chaos `ChaosTransactor`, injecting `commit_unknown_result` / wrong-shard / reply-drop, and
  assert the ring *still* checks out after retries (the C++ runs Cycle under Sim2 machine faults;
  this is the faithful analog). Deferred to keep PR1 to the real-concurrency spine.
- **Serializability.actor.cpp** — explicit multi-txn isolation sequences (if Cycle's coverage
  proves insufficient).
- **ConflictRange / AtomicOps / FuzzApiCorrectness** — port the *gaps* the audit identified, not
  re-cover what `atomic_conflictrange_test` / `concurrent_stress_test` / `differential_fuzz`
  already pin.

## 8. Risks

- **Flake risk.** Under heavy parallel-container load a stale pinned read version in `check` can
  hit `transaction_too_old` (1007). Mitigation: `check` reads via `db.Transact` (retry loop), and
  the swap txns likewise — no hand-pinned version that can age out mid-walk. No timing-dependent
  assertion (per the skill's #288 lesson: deterministic > flaky).
- **Over-conflict / starvation.** With N too small vs A, every swap conflicts and the workload
  livelocks on retries. Mitigation: N=1000 ≫ A=16 (matches the C++ `nodeCount ≫ actorCount`
  ratio), bounded txn count, context timeout.
