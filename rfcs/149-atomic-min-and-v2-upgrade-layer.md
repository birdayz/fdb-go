# RFC-149 — Move the Min→MinV2 / And→AndV2 atomic upgrade into `client.Transaction.Atomic`

**Status:** Draft (v2 — FDB-C-dev NAK'd v1's 4-line framing: the API version isn't threaded to the client
and the stacktester discards it, so the literal diff wouldn't compile or close the divergence. v2 §3 adds
the real plumbing — API version into `client.database` with mandatory-set semantics + wiring the
stacktester + keeping the facade V2. See §3.)
**Item:** TODO.md item 2 / RFC-056 continuation item 3 — `/hunt-divergences` standing axis (atomic-op
edges vs libfdb_c). This is the concrete divergence the atomic-op hunt surfaced.
**Reviewers:** **FDB-C-dev** (wire conformance vs release-7.3 — REQUIRED, substitutes for Graefe on
client/wire) + Torvalds + codex + @claude. Route via the `fdb-client-engineer` skill.
**Classification:** wire **data-plane** divergence (atomic-op code on the wire / committed bytes). C++
(libfdb_c 7.3.75) is the spec.

---

## 1. Problem (verified real, reachable)

C++ upgrades the legacy `Min`/`And` atomic op codes to their V2 variants **below the binding API**, in
`ReadYourWritesTransaction::atomicOp` (`fdbclient/ReadYourWrites.actor.cpp:2243-2248`):

```cpp
if (tr.apiVersionAtLeast(510)) {
    if (operationType == MutationRef::Min)      operationType = MutationRef::MinV2;
    else if (operationType == MutationRef::And) operationType = MutationRef::AndV2;
}
```

(Identical block in the lower `Transaction::atomicOp`, `NativeAPI.actor.cpp:5990-5995`.) So **any** caller
of `fdb_transaction_atomic_op(MIN/AND)` — including every reference binding's `tr.min()`/`tr.bit_and()` —
gets `MinV2(18)`/`AndV2(19)` on the wire.

Go puts the upgrade **only in the facade** (`pkg/fdbgo/fdb/transaction.go:252,256,281`:
`And`/`BitAnd`→`MutAndV2`, `Min`→`MutMinV2`). The low-level `client.Transaction.Atomic`
(`pkg/fdbgo/client/transaction.go:1306`) — the true analog of `RYW::atomicOp` — does **no** upgrade; it
just appends `Mutation{Type: op}`.

The cross-binding conformance harness bypasses the facade: `cmd/fdb-stacktester/operations.go:726-727,
736-737` maps `"AND"/"BIT_AND"→client.MutAnd(6)` and `"MIN"→client.MutMin(13)` (the raw codes, correctly
mirroring `fdb_transaction_atomic_op(MIN)`), then dispatches via `client.Atomic(mutType,…)`
(`operations.go:415/424`). **Result: the Go binding tester ships `Min(13)`/`And(6)`; the reference
binding ships `MinV2(18)`/`AndV2(19)`.**

These fold differently **on an absent key** — the exact zero-length/absent bug the V2 codes were created
to fix. Go's own dispatch already treats them as distinct (`ryw.go:1205-1218`), and the real FDB server
does too:
- `doMin(absent, le64(10))` → `0x0000000000000000` (existing treated as `""`, operand high byte forces
  existing-wins, zero-filled) — `ryw.go:1375`.
- `doMinV2(absent, le64(10))` → `le64(10)` (returns operand) — `ryw.go:1401`.
- `doAnd(absent, {0xFF,0xFF})` → `{0x00,0x00}`; `doAndV2(absent, …)` → `{0xFF,0xFF}`.

**Reachability:** real and runnable through `cmd/fdb-stacktester` (the RFC-016 binding tester, whose whole
job is wire-identity with C/Python/Java). The official `bindingtester.py` Go-vs-Python on any seed doing
an absent-key MIN/AND/BIT_AND diverges in final DB state. Production apps using `fdb.Transaction.Min/And`
are **not** affected — the facade masks it by hard-coding V2.

**Severity: MEDIUM.** A genuine wire-data divergence on a reachable conformance path, currently latent.
RFC-062's atomic differentials all go through the facade (`differential_atomic_test.go:68-101` use
`tx.And`/`tx.Min` → V2), so the raw `client.Atomic(MutMin/MutAnd)` path is **unprobed** — the exact axis
the C client's RYW upgrade covers. It is also an architectural trap: any future internal
`client.Atomic(MutMin)` caller (e.g. a record-layer counter) would silently emit the legacy code.

## 2. Investigation (C++ spec ↔ Go layering)

The rest of the atomic fold path is a byte-faithful 1:1 port of `Atomic.h` (verified per-op: ADD, AND,
OR, XOR, MAX, MIN, BYTE_MIN, BYTE_MAX, APPEND_IF_FITS, COMPARE_AND_CLEAR, MIN_V2/AND_V2,
SET_VERSIONSTAMPED_*; the `valueSizeLimit=100000` matches `CLIENT_KNOBS->VALUE_SIZE_LIMIT`). The **only**
divergence is the layer at which the Min→MinV2 / And→AndV2 op-code upgrade is applied. C++ applies it in
the RYW layer (below the binding API) so it is universal; Go applies it in the facade (above
`client.Transaction`) so it is bypassable.

## 3. Fix (v2 — expanded per FDB-C-dev NAK; the 4-line diff alone does NOT close the divergence)

C++ applies the upgrade in BOTH the RYW layer (`ReadYourWrites.actor.cpp:2243-2248`) AND the lower
`Transaction::atomicOp` (`NativeAPI.actor.cpp:5990-5995`) — identical blocks, harmlessly double-applied
(RYW upgrades first; the native re-check is a no-op). Bindings pass the RAW code via
`fdb_transaction_atomic_op(MIN/AND)`; RYW does the upgrade. So the C++-faithful Go mapping is: the `fdb`
facade = the binding (passes raw or pre-upgraded), and `client.Transaction.Atomic` = `RYW::atomicOp`
(does the upgrade). Four parts:

**3a. The upgrade, at the top of `client.Transaction.Atomic`** (`transaction.go:1306`), 1:1 with C++ —
above the commit-mutation append AND the RYW-map apply, so the upgraded code reaches BOTH the committed
frame and the in-txn RYW read, and applies even when RYW is disabled (matches C++ ordering, verified):
```go
if tx.db.apiVersionAtLeast(510) {
    switch op {
    case MutMin: op = MutMinV2
    case MutAnd: op = MutAndV2
    }
}
```

**3b. Thread the API version into the client — the load-bearing plumbing (without it 3a does not even
compile).** Today `client.Transaction` / `client.database` carry NO API-version field (only the `fdb`
facade holds it as a package global, and `client` cannot import `fdb` — import cycle). C++ stores it on
the `DatabaseContext` (`Transaction::apiVersionAtLeast → trState->cx->apiVersionAtLeast`); mirror that:
- add `apiVersion int` to `client.database` (the `DatabaseContext` analog) + an `apiVersionAtLeast(v) bool`
  helper; `client.Transaction.Atomic` reads it through `tx.db`;
- `client.OpenDatabase` / `OpenDatabaseFromConfig` accept the API version (param or `client.Option`) and
  store it on the `database`;
- **mandatory-set semantics:** C++ cannot open a DB without `fdb_select_api_version`, so "unset" never
  occurs there. Go's client CAN (the stacktester does today). The client must require the version at open
  so the `>=510` gate can NEVER silently no-op; the `fdb` facade (which already holds the selected version
  globally) wires it into its `client.OpenDatabase` call.

**3c. Wire the binding tester's API version — without this the divergence the RFC exists to close STAYS
OPEN.** `cmd/fdb-stacktester/main.go:25-30` parses `apiVersion` then discards it (`_ = apiVersion`) and
calls `client.OpenDatabase(ctx, clusterFile)` with no version. After 3a/3b an unset version → gate false →
the stacktester STILL ships `Min(13)`/`And(6)`. Fix: pass the parsed `apiVersion` into the stacktester's
`client.OpenDatabase`, deleting the `_ = apiVersion` TODO.

**3d. Keep the facade hard-coding V2.** `fdb/transaction.go:252,256,281` keep passing `MutAndV2`/`MutMinV2`
— idempotent w.r.t. 3a (V2 is neither Min nor And → unchanged) and safe regardless of threading. Do NOT
simplify the facade to pass raw Min/And in this RFC: if the version weren't fully threaded to every facade
open path, production `tr.Min` would regress to legacy codes (a new divergence in the opposite direction,
hitting every app). That simplification is a separate follow-up that must first pin "production `tr.Min`
still emits V2" across `OpenDatabase`/`OpenDatabaseFromConfig`/`WrapDatabase`.

## 4. Wire / behaviour impact

Wire data-plane: `client.Atomic(MutMin/MutAnd, …)` now emits `MinV2(18)`/`AndV2(19)` (matching libfdb_c)
instead of `Min(13)`/`And(6)`. No effect on present-key folds (V2 differs from legacy only on absent
keys). No effect on the facade path (already V2). Closes the binding-tester divergence.

## 5. Test plan (red→green differential)

New differential in `pkg/fdbgo/bench` (the cgo-vs-go harness), exercising the **low-level** path the
existing RFC-062 tests skip:
- In one txn: `goTx.Atomic(client.MutMin, absentKey, le64(10))` vs cgo `tr.Min(absentKey, le64(10))`;
  assert both the in-txn RYW read **and** the committed read-back. Before fix: go=`00…00`, cgo=
  `0a000000 00000000` → **RED**. After fix: both = operand → **GREEN**.
- Mirror for `MutAnd` vs cgo `BitAnd`: `{0xFF,0xFF}` operand on absent key → go `{00,00}` vs cgo
  `{FF,FF}` before; both `{FF,FF}` after.
- **Stacktester emits V2 (the closure proof for 3c):** a test that the stacktester path — opened with API
  version ≥ 510 — actually puts `MinV2(18)`/`AndV2(19)` on the wire for `MIN`/`AND`/`BIT_AND` (assert the
  emitted `MutationType`, or run `bindingtester.py` Go-vs-Python on an absent-key MIN/AND and confirm
  final DB-state identity). Without this the gate could silently no-op and CI stays green over the latent
  divergence.
- **Mandatory-version pin (3b):** opening a `client` database with the version unset is rejected (or the
  gate provably can't be reached with an unset version), so a future caller can't reintroduce the
  silent-no-op.
- **Production facade unchanged:** a pin that `fdb.Transaction.Min`/`And`/`BitAnd` still emit V2 (3d), so
  the kept facade hard-coding isn't accidentally dropped.

## 6. Gate & risk

FDB-C-dev (client/wire spec owner) + Torvalds + codex + @claude. Risk: **MEDIUM** (revised up from the v1
"4-line" framing). The op-code remap itself is trivial and idempotent, but the load-bearing work is the
API-version plumbing (3b) + the stacktester wiring (3c): the gate silently no-ops if the version doesn't
reach the client `database`, which is exactly how the divergence persists today. The mandatory-set
semantics + the stacktester-emits-V2 test (§5) are what make the closure real rather than assumed. Keep
the facade V2 (3d) to avoid an opposite-direction production regression.

## 7. Scope

In: the upgrade in `client.Transaction.Atomic` (3a); threading the API version into `client.database` +
`OpenDatabase`/`OpenDatabaseFromConfig` with mandatory-set semantics (3b); wiring the stacktester's
parsed version (3c); keeping the facade V2 (3d); the low-level differential + the stacktester/mandatory
pins. Out: simplifying the facade to pass raw codes (separate follow-up, needs the production-V2 pin
first); any change to the fold functions (verified byte-identical); the broader atomic-op axis (clean per
the hunt — this was the single divergence found).

---

### Hunt status (for the standing `/hunt-divergences` log)

Atomic-op axis vs `Atomic.h`: **one divergence found** (this RFC); all fold functions
(ADD/AND/OR/XOR/MAX/MIN/BYTE_MIN/BYTE_MAX/APPEND_IF_FITS/COMPARE_AND_CLEAR/MIN_V2/AND_V2/versionstamp)
verified byte-identical to C++. Recommended next hunt axes once this lands: option-semantics matrix
(DB-default `defaultFor` wiring) and versionstamp-offset encoding edges (RFC-063 is still Draft).
