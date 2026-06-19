# RFC-126 — FuzzApiCorrectness audit: close two client-side input-validation divergences

**Status:** Accepted
**Item:** TODO.md C3 ("Ride their test designs"), increment 7 (final) — **FuzzApiCorrectness**

> **Reviews.** Original fuzzer draft: Torvalds **NAK** (padding over an already-pinned surface) → fuzzer
> dropped; the `ExceptionContract` is used as an *audit checklist*, which surfaced two real divergences.
> Reframed RFC: FDB-C-dev **ACK** (Divergence A `< -1` boundary airtight vs `FDBTypes.h:754` +
> `fdb_c.cpp:983`; Divergence B 2004 + the read/write split faithful vs `ReadYourWrites.actor.cpp:1954`/
> `:2466`) and Torvalds **ACK** ("ship it" — reframe kills the padding, both divergences empirically real,
> the §3.2 read/write asymmetry (read `maxReadKey`+MVK-exception / write `maxWriteKey` no-exception) is
> correctly split, §6 scope + "C3 complete" honest). Both caught the same §3.2 flattening bug (fixed
> before ACK). Empirically confirmed vs libfdb_c (cgo, api 730): the §2 probe table.
(RFC-119 §7 named gap: "property-based multi-txn"). The last C3 gap.
**Spec:** `fdbserver/workloads/FuzzApiCorrectness.actor.cpp` @ 7.3.75 (the per-operation
`ExceptionContract`), as an **audit checklist** for Go's client-side input validation — not a workload
to re-run.
**Behavior-visible client change** (Go now rejects two input classes it silently accepted) → full
FDB-C-dev review; pinned by red→green differentials vs libfdb_c.

---

## 1. What the review changed (fuzzer → targeted fixes)

The first draft proposed porting FuzzApiCorrectness as a coverage-guided **fuzzer** over the
error-contract space. **Torvalds NAK'd that as padding, correctly:** Go's error contract is already
pinned across `differential_errorcode_test.go` (2004/2101/2102/2103), `sizelimits_test.go` (the
key/value size ladder), `differential_conflict_range_test.go` + `database_test.go` +
`c_binding_port_test.go` (inverted_range 2005, ClearRange inverted), `differential_getkey_boundary_test.go`
+ `fault_test.go` + `ryw_getkey_test.go` (key_outside_legal_range boundaries), and the versionstamp-offset
order (`TestDifferential_VersionstampValidationOrder`). A new fuzzer over an already-pinned surface is a
vanity metric, and its highest-value codes (`key_too_large`/`value_too_large`) sit in the workload's
**global-ignore-on-throw** set (`FuzzApiCorrectness.actor.cpp:62-70`), so the throw side would be a
no-op anyway.

**But using the `ExceptionContract` as an audit checklist against Go's actual validation surfaced two
genuine divergences** — inputs Go silently *accepts* where libfdb_c (and Java) *reject*. Both are
empirically confirmed against libfdb_c (the differential probe below), and both are real wire-contract
divergences: an app that shares a cluster across a Go and a C/Java client gets an error from one and
silent success from the other.

That is the honest, non-padding increment: **fix the two divergences, pin each with a red→green
differential.** No fuzzer.

## 2. The two divergences (empirically confirmed vs libfdb_c, api 730)

Differential probe — same op through the pure-Go client and `libfdb_c` (cgo) on one cluster:

| Operation | Go | libfdb_c |
|-----------|-----|----------|
| `getRange(.., limit=0)` | accept (unlimited) | accept (unlimited) |
| `getRange(.., limit=-1)` | accept (unlimited) | accept (unlimited) |
| **`getRange(.., limit=-2)`** | **accept (unlimited)** | **2012 `range_limits_invalid`** |
| **`getRange(.., limit=-100)`** | **accept (unlimited)** | **2012 `range_limits_invalid`** |
| **`addReadConflictRange(a, "\xff\xff\xff")`** | **accept** | **2004 `key_outside_legal_range`** |
| **`addWriteConflictRange(a, "\xff\xff\xff")`** | **accept** | **2004 `key_outside_legal_range`** |

### 2.1 Divergence A — `getRange` row limit `< -1` not rejected

**C++/libfdb_c:** at api version > 13, `fdb_c.cpp:984` (`validate_and_update_parameters`) does **not**
remap a negative limit to a reverse scan (the negative-sign convention is gated `<= 13`); a non-`-1`
negative `limit` passes through to `GetRangeLimits(rows=limit)`. `getRangeInternal`
(`NativeAPI.actor.cpp:5814`) / `ReadYourWrites::getRange` (`ReadYourWrites.actor.cpp:1749`) then throw
`range_limits_invalid` because `GetRangeLimits::isValid()` (`FDBTypes.h:753`) is
`rows >= 0 || rows == ROW_LIMIT_UNLIMITED(-1)` — false for `rows <= -2`. So the valid set is
`{-1 (unlimited), 0, 1, 2, ...}`; `limit <= -2` is invalid. (Note the boundary is `< -1`, **not**
`< 0`: `-1` is the unlimited sentinel and is valid in both clients — confirmed by the probe.)

**Go:** `readpath.go:650` and `ryw.go:572` map `remaining <= 0 → math.MaxInt` ("0 or negative = no
limit"), and `range_result.go:effectiveLimit` only special-cases `0`. So **every** negative limit,
including `<= -2`, is silently treated as unlimited — a missing client-side validation.

(This is **not** the deliberate `LocalityGetBoundaryKeys` negative-limit-as-unlimited choice pinned at
`fdb_test.go:1157-1172` — that is a *different* API, `LocalityGetBoundaryKeys`, not the general
`getRange` row limit. The probe shows libfdb_c rejects the `getRange` case, so Go's silent-accept is a
real divergence, not an intentional design.)

### 2.2 Divergence B — conflict-range key past `maxReadKey` not rejected

**C++/libfdb_c:** `ReadYourWritesTransaction::addReadConflictRange` (`ReadYourWrites.actor.cpp:1949-1955`)
and `addWriteConflictRange` (`:2461`) throw `key_outside_legal_range` when
`(keys.begin > getMaxReadKey() || keys.end > getMaxReadKey())` and the range is not exactly the
`metadataVersionKey` range (`keys.begin != metadataVersionKey || keys.end != metadataVersionKeyEnd`),
at `apiVersionAtLeast(300)` (always true for a modern client). The `inverted_range` from the
`KeyRangeRef(key1,key2)` construction (caller side, `key1 > key2`) is thrown *first*; this check is on
the already-constructed (`begin <= end`) range.

**Go:** `AddReadConflictRange`/`AddWriteConflictRange` (`transaction.go:2484/2499`) check **only**
`begin > end → inverted_range (2005)`; there is **no** `maxReadKey` check, so a conflict range whose
endpoint is past `\xff\xff` is silently accepted where libfdb_c rejects.

## 3. Proposed Go change

### 3.1 Divergence A — reject `limit < -1` with `range_limits_invalid` (2012)

Add a constant `ErrRangeLimitsInvalid = 2012` (`transaction.go`) and a `2012: "range_limits_invalid"`
wire description. Validate at the three public range-read entries — the RYW-layer analog of
`ReadYourWrites::getRange` — **before** the unlimited-mapping: `getRangeDir` (`transaction.go:1144`),
`Snapshot.GetRange` (`:482`), `Snapshot.GetRangeReverse` (`:502`):

```go
if limit < -1 {                       // -1 == ROW_LIMIT_UNLIMITED is valid; 0/positive valid
    return nil, false, &wire.FDBError{Code: ErrRangeLimitsInvalid} // 2012
}
```

The facade's `effectiveLimit(0) = MaxInt32` (≥0, valid) and `effectiveLimit(-1) = -1` (valid) flow
through unchanged; only a user `limit <= -2` is rejected — matching libfdb_c exactly. No internal
caller passes a literal `< -1` (verified), so the guard is invisible to existing range reads. (Snapshot
+ non-snapshot both covered, matching C++ which validates in the shared RYW layer.)

*(FDB-C-dev review condition — the GetRangeLimits **byte**-limit negative case: N/A for Go. Go's range
API (`RangeOptions{Limit int, Mode StreamingMode}`, internal `getRange(.., limit int, ..)`) exposes
only a **row** limit; there is no app-facing byte-limit parameter to validate, so the `bytes` arm of
`isValid()` has no Go surface. Only the row-limit divergence is reachable, and it is what we fix.)*

### 3.2 Divergence B — conflict-range out-of-range check (read ≠ write — FDB-C-dev catch)

The two methods are **not** symmetric in C++ (the probe missed it because read/write maxKey coincide
when no system-key options are set). Add each guard **after** the existing `begin > end → inverted_range`
check (so inverted wins when both apply, matching C++'s construct-then-check order):

- **`AddReadConflictRange`** — `getMaxReadKey()`, **with** the `metadataVersionKey`-range exception
  (`ReadYourWrites.actor.cpp:1954-1957`). Define `metadataVersionKeyEndBytes = "\xff/metadataVersion\x00"`
  (cf. `SystemData.cpp:1386`):

  ```go
  maxKey := tx.maxReadKey()
  if (bytes.Compare(begin, maxKey) > 0 || bytes.Compare(end, maxKey) > 0) &&
      !(bytes.Equal(begin, metadataVersionKeyBytes) && bytes.Equal(end, metadataVersionKeyEndBytes)) {
      return &wire.FDBError{Code: 2004} // key_outside_legal_range
  }
  ```

- **`AddWriteConflictRange`** — `getMaxWriteKey()` (`tx.maxWriteKey()`, `transaction.go:1082`), **no**
  metadataVersion exception (`ReadYourWrites.actor.cpp:2466-2468` throws unconditionally on out-of-range):

  ```go
  maxKey := tx.maxWriteKey()
  if bytes.Compare(begin, maxKey) > 0 || bytes.Compare(end, maxKey) > 0 {
      return &wire.FDBError{Code: 2004} // key_outside_legal_range
  }
  ```

The differential (§4.2) must exercise this read/write asymmetry under a system-key option (where
`maxReadKey` and `maxWriteKey` diverge), not only the default where they coincide.

### 3.3 Divergence B sibling — `getRangeSplitPoints` out-of-range check (FDB-C-dev impl review)

The impl review caught a third sibling of the same read-path class: C++ `RYW::getRangeSplitPoints`
(`ReadYourWrites.actor.cpp:1875-1877`) rejects `begin > getMaxReadKey() || end > getMaxReadKey()` with
`key_outside_legal_range`, but Go's `getRangeSplitPointsImpl` (`metrics.go`) had no such check — so a
split-points request past `maxReadKey` was silently accepted (and worse, *hung* trying to split into the
system keyspace, where libfdb_c rejects fast). Add the same `tx.maxReadKey()` guard after the poison
check, before the locate loop. Pinned by `TestDifferential_RangeSplitPointsMaxKey`.

**`inverted_range` (2005) first on the metric ops (codex catch).** libfdb_c constructs a `KeyRangeRef`
from the C args *before* entering the RYW op, and the `KeyRangeRef` ctor throws `inverted_range` on
`begin > end` ahead of the used_during_commit / maxKey checks. So an inverted split-points range — even
one *also* past `maxReadKey` — is **2005, not 2004**. Go's API takes raw `begin/end` with no constructing
range, so an inverted check goes **first** in both `getRangeSplitPointsImpl` *and*
`getEstimatedRangeSizeBytesImpl` (the latter is the same construction class; it correctly has **no**
maxKey check — C++ `:1853` validates none — but the inverted check still applies). Both pinned (inverted
in-range → 2005; inverted+>maxKey → 2005 wins; estimatedSize inverted → 2005).

**`transaction_timed_out` (1031) before maxKey (codex catch #2).** C++ checks `resetPromise.isSet()` —
which carries the `SetTimeout` error — *before* the maxKey check (`ReadYourWrites.actor.cpp:1872` before
`:1875`). The metric ops bypass `ensureReadVersion` (where Go's `checkTimeout` normally runs), so a
synchronous `tx.checkTimeout()` is added before the maxKey guard in both impls — but **after** the
`rywPoisonErr` gate, so the deferred `client_invalid_operation (2000)` out-ranks the timeout, matching
`ensureReadVersion`'s established RFC-059 order (poison before checkTimeout) (codex catch #3). Final
early-return order: **inverted (2005) → cancelled (1025) → poison/client_invalid_operation (2000) →
timed_out (1031) → maxKey (2004)**. Pinned deterministically by `TestMetricOps_EarlyReturnPrecedence`
(forces the timeout + poison states; each higher-precedence code wins).

## 4. Executable spec (what the tests prove)

1. **Differential A** (`bench/`, productionized from the probe): `getRange(limit)` through Go and
   libfdb_c agree on the error code for `limit ∈ {0, -1, -2, -100, INT_MIN, 5}` — both accept `{0,-1,
   positive}`, both reject `{-2,-100,INT_MIN}` with **2012**. Red before the fix (Go returns 0), green
   after.
2. **Differential B**: `add{Read,Write}ConflictRange` with an endpoint `> maxReadKey` through both
   clients agree on **2004**; an in-range range and the exact `metadataVersion` range are accepted by
   both. Red before, green after.
3. **Focused Go regressions**: `limit==-1`/`0` still unlimited (no over-rejection); the conflict-range
   `metadataVersion` exception; inverted-still-wins-over-maxKey ordering. Revert-prove each (back out
   the guard → red).

## 5. Wire-compat impact

**Behavior-visible, and the change is the *correct* direction:** Go now rejects two input classes
(`getRange` row `limit <= -2`; conflict range past `maxReadKey`) that libfdb_c and Java already reject,
so this **removes** a divergence rather than adding one — a Go app and a C/Java app on the same cluster
now get identical errors for the same misuse. No persisted-byte change. Because it changes the error an
app observes, it carries full FDB-C-dev review.

## 6. Scope / what is NOT done (and why that's correct)

- **No fuzzer** — the error-contract is pinned at fixed points + differentials (§1); a coverage-guided
  fuzzer over it is padding (Torvalds). The two fixes are the genuine gap.
- **Already-pinned contracts** (no action): key/value `too_large` (2102/2103, `sizelimits_test` +
  `differential_errorcode`), `inverted_range` (2005, `database_test` + `differential_conflict_range` +
  `c_binding_port`), read/write `key_outside_legal_range` on get/set/clear (`differential_getkey_boundary`
  + `fault_test`), versionstamp offset (2000, `TestDifferential_VersionstampValidationOrder`).
- **Documented divergences (unchanged, out of scope):** special-key-space (`\xff\xff` → 2004, no
  special-key module — separate TODO); `invalid_mutation_type` (Go's typed `Add`/`And`/… facade can't
  express a bad op-code); `SetReadVersion(v<=0)` graceful (no `version_invalid`, vs C++ `CATCH_AND_DIE`).
- **C3 conclusion:** with these two fixes, the FuzzApiCorrectness error-contract is fully covered for
  Go's implemented surface (the rest already pinned). Cycle / AtomicOps / ConflictRange /
  FuzzApiCorrectness + Serializability (via Cycle) all covered → **C3 complete.**
