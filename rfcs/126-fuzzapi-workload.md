# RFC-126 — FuzzApiCorrectness workload: a property-based per-operation error-contract fuzzer

**Status:** Draft
**Item:** TODO.md C3 ("Ride their test designs"), increment 7 (final) — the **FuzzApiCorrectness**
workload (RFC-119 §7 named gap: "property-based multi-txn"). The last remaining C3 gap.
**Spec:** `fdbserver/workloads/FuzzApiCorrectness.actor.cpp` @ 7.3.75 (18 operation contracts).
**Contracts cited:** `ExceptionContract` (`:44-108`), the 18 `TestXxx` structs (`:726-1485`), the
boundary generators `makeKey`/`makeValue`/`makeKeySel`/`makeRangeLimits` (`:567-655`), size limits
(`ClientKnobs.cpp:75-78`, `NativeAPI.actor.cpp:11611-11627`), key boundaries (`SystemData.cpp:31-36`).
**Test-only — expected zero production/wire impact** (the §6 exception: a fuzzer-found contract
divergence is a real Go client bug, fixed in this increment). Differential-vs-libfdb_c is N/A for most
of this surface (libfdb_c's binding `CATCH_AND_DIE`-aborts on the synchronous oversized-key/version
throws, which is exactly why Go validates gracefully at commit — a documented divergence).

---

## 1. Problem — the unprobed dimension: a coverage-guided fuzzer over the *error* contract

FuzzApiCorrectness is FDB's per-operation **error-contract** fuzzer: for each client API call
(`set`/`clear`/`get`/`getKey`/`getRange`×4/`atomicOp`/`watch`/`add{Read,Write}ConflictRange`/...) with
**random boundary-biased arguments** (25% oversized keys, 25% empty, 15% `\xff\xff` special keys,
oversized/empty values, negative/huge limits, invalid mutation op-codes, inverted ranges), it asserts
an `ExceptionContract`: the thrown error code (or non-throw) must match a documented per-op map of
{code → Never | Possible | Always}.

Go already has **extensive** error-contract coverage — but all of it at **fixed points** or on the
**valid-write** axis:
- `bench/differential_fuzz_test.go` (`FuzzDifferential`) fuzzes random *valid-write* op sequences and
  compares **persisted state** byte-for-byte. It **explicitly excludes** the error/boundary space:
  "oversized keys/values (the C binding aborts the process), conflicts (control-plane)" (`:24-25`).
- `bench/differential_errorcode_test.go`, `client/sizelimits_test.go`,
  `bench/differential_getkey_boundary_test.go`, `client/c_binding_port_test.go` pin specific error
  codes (2004/2101/2102/2103/2005, the size ladder, getKey boundaries) at **hand-written fixed
  points**.

**The gap:** no **coverage-guided fuzzer** explores the **error/boundary** space. Fixed tests probe
the thresholds they were written for; a fuzzer explores the whole argument cross-product — the exact
`key == maxKey` boundary, the `>=`-vs-`>` per-op comparator distinction, `key1 == key2` vs `key1 >
key2`, every mutation op-code, versionstamp offsets at `pos+10 == size-4`, negative-vs-INT_MIN limits
— and op/arg combinations no fixed test enumerates. This is the faithful FuzzApiCorrectness port, and
it is the **complement** of `FuzzDifferential` (which owns the valid-write/state axis): this owns the
**error-contract** axis.

## 2. The C++ design (cited, `FuzzApiCorrectness.actor.cpp` @ 7.3.75)

Each `TestXxx` runs one op with random args and checks its `ExceptionContract` two ways
(`ExceptionContract::handleException` `:62-91`, `handleNotThrown` `:94-107`):

1. **On throw:** the code is tolerated iff it is in the **global-ignore set** (`:62-70`:
   `used_during_commit`, `transaction_too_old`, `future_version`, `transaction_cancelled`,
   `key_too_large`, `value_too_large`, `process_behind`, `batch_transaction_throttled`,
   `tag_throttled`, `grv_proxy_memory_limit_exceeded`) **or** present in the op's contract with
   occurance `!= Never`. Otherwise → **SevError + rethrow** (a contract violation).
2. **On non-throw:** for every contract entry with occurance `Always`, if the op did **not** throw it →
   **SevError** ("should have thrown but didn't").

`requiredIf(C)=C?Always:Never`; `possibleButRequiredIf(C)=C?Always:Possible`;
`possibleIf(C)=C?Possible:Never` (`:58-60`).

**Boundary-biased generators (`:567-655`).** `makeKey` (`:567-591`): 25% empty, 25% oversized
(`[10001,20000]`), 50% in-range (`[1,10000]`); then 20% of the non-empty cases get a `\xff\xff`
prefix → ~15% special keys. `makeValue` (`:593-613`): 25% empty, 25% oversized (`[100001,200000]`),
50% in-range. `makeKeySel` (`:615-626`): `(makeKey(), coinflip orEqual, offset)` with offset 40% zero
/ 30% negative / 30% positive. `makeRangeLimits`/inline limit: 20% zero / ~40% negative / ~40%
positive. Constants: `KEY_SIZE_LIMIT=10000`, `SYSTEM_KEY_SIZE_LIMIT=30000`, `VALUE_SIZE_LIMIT=100000`,
`TRANSACTION_SIZE_LIMIT=1e7`, tenant `PREFIX_SIZE=8`. `getMaxWriteKeySize(key, raw)` =
`key.startsWith("\xff") ? 30000 : 10000 + (raw ? 8 : 0)` (`NativeAPI.actor.cpp:11611`).
`getMaxKey(tr)` = `"\xff\xff"` if `useSystemKeys && !tenant`, else `"\xff"` (`:292-298`).

**The per-op contracts** (full tables in the extracted spec; the codes Go implements are §3). The
load-bearing subtlety: `key_outside_legal_range` is `Always if key >= getMaxKey` for
**get/atomicOp/set/clear(single)/watch** but `Always if key > getMaxKey` for
**getKey/getRange/{read,write}ConflictRange** — the `>=`-vs-`>` distinction is deliberate and per-op.

## 3. Go's contract — what's faithful, what diverges

The Go client implements a **subset** of the contract codes; the rest are special-key-space / tenant /
option features Go does not (a documented TODO gap). The fuzzer asserts the **Go contract** =
the C++ contract restricted to implemented codes, with divergences encoded explicitly.

**Faithful (assert against the C++ condition):**

| Code | Ops | Go site |
|------|-----|---------|
| `key_outside_legal_range` (2004) | Get/GetKey/GetRange/Set/Clear/AtomicOp/Watch/ConflictRange — **with the per-op `>=` vs `>`** | `transaction.go:445/470/486/507/666/699` (reads), commit-build `:1490` (clear range end) |
| `inverted_range` (2005) | AddRead/WriteConflictRange, GetRange (KeyRange), Clear(range) | `transaction.go:2484/2499` (conflict-range); clear/getRange begin>end |
| `key_too_large` (2102) | Set/AtomicOp/Watch (two-ceiling ladder) | `transaction.go:1522`, `sizelimits.go:getMaxWriteKeySize` |
| `value_too_large` (2103) | Set/AtomicOp | `transaction.go:1526` |
| `client_invalid_operation` (2000) | AtomicOp versionstamp offset | `transaction.go:1546/1551` (`validateVersionstampOffset`) |

Note `key_too_large`/`value_too_large` are in the **global-ignore-on-throw** set, so on throw they are
always tolerated; their `Always` is enforced only by the non-throw check. Go's deferred-to-commit
validation means the *commit* surfaces 2102/2103 — the fuzzer drives a commit to enforce the
must-throw side, matching `TestDifferential_VersionstampValidationOrder`'s model.

**To-verify — a likely real divergence the fuzzer will surface (the increment's value):**
- **`range_limits_invalid` on a negative row limit.** The C++ contract is `Always if limit < 0`
  (int-limit `TestGetRange0/2`). Go currently treats a negative limit as **unlimited**
  (`readpath.go:650`, `ryw.go:572`: "0 or negative = no limit"; `range_result.go:effectiveLimit` only
  special-cases 0), so it does **not** throw `range_limits_invalid`. If libfdb_c rejects a negative
  row limit (modern explicit-`reverse` API), this is a real validation gap → **fix Go in this
  increment** (add the `limit < 0 → range_limits_invalid` check on the public range path) and pin it.
  The fuzzer is precisely the probe that decides this; the RFC flags it up front rather than
  discovering it mid-implementation.

**Out of scope (Go's typed API is structurally safer than the C contract surface):**
- **`invalid_mutation_type`.** Go's public atomic API is the typed `Add`/`And`/`Or`/… facade — an
  invalid mutation op-code is **not expressible** at the call site (compile-time safe); there is no
  raw-`atomicOp(code)` entry point for an app to misuse. The C++ contract's random-op-code dimension
  (`:1120-1142`) has no app-facing analog in Go, so it is not part of the Go contract.

**Documented divergences (encode Go's behavior, with a cited note):**
- **Special-key-space (`\xff\xff/...`)** — Go has no special-key module; every `\xff\xff` key is
  `>= maxReadKey` so Go returns `key_outside_legal_range` (2004) where C++ routes to special keys /
  `special_keys_*`. So the Go contract for a special key is **2004** (reads) / the write path's 2004,
  not the C++ `special_keys_*`/success. (Known gap — TODO "special-key-space unimplemented".)
- **SetVersion(v<=0)** — C++ contract is `version_invalid Always if v<=0`; Go's `SetReadVersion` is
  **graceful** (accepts, defers to `transaction_too_old`), an intentional divergence (CLAUDE.md "never
  panic in library code"; the C++ `setVersion` `CATCH_AND_DIE`-aborts). Encode: SetReadVersion never
  throws `version_invalid` synchronously.
- **No tenant, no options** — the Go port runs the **no-tenant** path (`getMaxKey = "\xff"` without
  `READ_SYSTEM_KEYS`, `"\xff\xff"` with), so the `tenant_not_found`/`illegal_tenant_access`/tenant
  `invalid_option` entries are out of scope (their conditions are all false on the no-tenant path).
  `TestSetOption`/`TestOnError`/`TestGetAddressesForKey` are out of scope (option-space fuzzing,
  onError contract, and `getAddressesForKey` are separate surfaces — noted as follow-ups §7).

## 4. Proposed Go change (test-only) — `pkg/fdbgo/client/fuzzapi_contract_test.go`

A native Go fuzz target `FuzzApiContract` plus a deterministic seed-corpus battery, porting
FuzzApiCorrectness's `ExceptionContract` oracle for the in-scope ops (§3).

### 4.1 Structure

- **Generators** (1:1 with C++ `:567-655`): `makeKey(rng)`, `makeValue(rng)`, `makeKeySel(rng)`,
  `makeLimit(rng)` with the exact size distributions (25/25/50, 15% `\xff\xff`, offset/limit splits).
  Driven by the fuzzer's byte stream (deterministic decode, like `decodeFuzzOps`).
- **Contract table**: a Go map per in-scope op (`opSet`, `opClear`, `opClearRange`, `opGet`,
  `opGetKey`, `opGetRangeSel`, `opGetRangeKR`, `opAtomic`, `opWatch`, `opAddReadCR`, `opAddWriteCR`,
  `opSetVersion`) → a function `(args) → map[int]occurance` computing each code's occurance from the
  args (the C++ conditions, with the per-op `>=`/`>` and the Go divergences §3).
- **`runOp`**: decode one op + its args from the fuzz bytes; run it on a real Go transaction
  (testcontainer); for ops validated at commit (Set/Atomic/Clear), drive a `Commit` to surface the
  deferred 2102/2103/2004; capture the error code (or nil).
- **`checkContract`**: the `handleException`/`handleNotThrown` logic — on throw, the code must be in
  the global-ignore set or the op contract (`!= Never`); on non-throw, no `Always` entry may be unmet.
  A violation → `t.Fatalf` (the SevError analog) with the op + args + got/expected.

### 4.2 The oracle (what `t.Fatalf`s)

- **Threw an uncontracted code** (not global-ignore, not in the op's map, or mapped `Never`) → bug:
  Go rejected with a code the contract forbids.
- **Did not throw a `Always` code** → bug: Go accepted an input the contract requires it to reject
  (e.g. an oversized key that committed, an inverted conflict range that didn't error, a key past
  `maxKey` that read successfully). **This is the high-value direction** — a missing/weakened
  validation is a silent wire divergence (an app relying on the error gets none).

### 4.3 Determinism / flake-freedom

Native Go fuzzing (`FuzzApiContract`) for exploration + a deterministic `TestApiContract_Battery`
of seed cases pinning each op's boundary (`key==maxKey`, `key1==key2`, exact size ladder, op-code
edges, versionstamp `pos+10==size-4`). `t.Parallel`, unique key prefix per run. No timing assertions —
the contract verdict is a pure function of the (op, args) and Go's synchronous/commit-time validation.
The global-ignore set absorbs the legitimately-transient codes (1007/1009/1037/...), so a loaded
cluster can't flake the oracle. Container timeout per CLAUDE.md.

## 5. Executable spec (what the test proves)

1. **No weakened validation (must-throw):** across the boundary-biased argument space, every input the
   contract marks `Always` for an in-scope code is rejected by Go with that code — no oversized
   key/value commits, no inverted conflict range / clear / getRange succeeds, no key past `maxKey`
   reads/writes, no bad versionstamp offset commits, and (after the §3 fix) no negative row limit is
   silently treated as unlimited. The per-op `>=`-vs-`>` `maxKey` comparator is exercised at the exact
   boundary. **Expected first finding:** `range_limits_invalid` on a negative limit (§3) — fixed +
   pinned here.
2. **No spurious/wrong-code rejection (must-not-throw-wrong):** Go never rejects with a code the op's
   contract forbids (`Never` / absent / outside the global-ignore set).
3. **Documented divergences pinned:** special keys → 2004; `SetReadVersion(v<=0)` → no
   `version_invalid` (graceful). Regressions guard that these stay as designed.
4. **Coverage-guided:** `FuzzApiContract` runs clean for ≥1M execs (0 contract violations) on the
   in-scope surface; the seed battery pins the boundaries deterministically.

## 6. Wire-compat impact

**None expected — test-only.** No production path changes.

**The exception:** a fuzzer-found contract violation (spec #1 or #2) is a real client bug — Go
validates an input differently from the documented C++ contract (a wrong error code, a missing
rejection, or a spurious one). Per the skill (C++ is the spec) and CLAUDE.md (fix bugs as you find
them, DFS), it is **fixed in this increment**: root-cause against the cited C++ condition, fix Go's
validation, pin with the seed battery + a focused regression. A validation fix changes the error a
client sees (behavior-visible) → full FDB-C-dev review as its own commit within the PR.

## 7. Follow-ups (out of scope for this increment)

- **Special-key-space** (`special_keys_*` contracts) — blocked on the unimplemented special-key
  module (separate TODO gap); the fuzzer pins Go's documented 2004 for now.
- **Tenant contracts** (`tenant_not_found`/`illegal_tenant_access`/tenant `invalid_option`) — the
  no-tenant path is in scope; the tenant path is a separate axis.
- **`TestSetOption`/`TestOnError`/`TestGetAddressesForKey`** — option-space fuzzing, the onError
  contract, and `getAddressesForKey` are distinct surfaces; separate increments if pursued.
- This is the **final C3 increment**: with it, Cycle / AtomicOps / ConflictRange / FuzzApiCorrectness
  (the error-contract axis) and Serializability (via Cycle) are all covered.
