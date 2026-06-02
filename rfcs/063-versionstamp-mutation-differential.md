# RFC-063: Versionstamp-mutation differential (offset + structure, stamp masked)

**Status:** Draft
**Item:** RFC-010 C3 (fresh differential axes) — "versionstamp-offset validation". The
versionstamp mutation wire format (offset encoding + stamp placement) is the wire hard line and
is currently **excluded** from differential testing.

## Problem

`SetVersionstampedKey`/`SetVersionstampedValue` place a commit-assigned **10-byte versionstamp**
(8-byte transaction version + 2-byte batch, big-endian) into a key or value at an offset encoded
in the mutation operand. The operand is `[data…][offset]` where `offset` is a little-endian
suffix (4-byte for API ≥ 520, 2-byte for < 520) giving the position — within `data` (the operand
minus the offset suffix) — where the 10-byte stamp goes. At commit the server strips the offset
suffix, writes the stamp at `offset`, and converts the op to `SetValue`
(C++ `Atomic.h:249-315`, `transformVersionstampMutation`). Go validates the offset
(`pkg/fdbgo/client/transaction.go:1388-1398`), adjusts it by +8 for a tenant prefix (`pkg/fdbgo/client/commitpath.go:354-363`),
and ships the operand as-is for the server to transform.

The differential fuzz **excludes** these ops (`differential_fuzz_test.go:24`). The existing
cross-engine coverage is thin: `TestInterop_Versionstamp` writes via **Go only** (reads via cgo,
checks the stamp is non-zero — no go-vs-cgo compare), and `TestDifferential_VersionstampedValue`
(`differential_test.go:126`) is a masked go-vs-cgo `SetVersionstampedValue` but **only at offset
0** with one fixed shape. Neither covers `SetVersionstampedKey` at all, non-trivial offsets, the
2-byte tuple user version, `GetVersionstamp`, the error/boundary paths, or multiple stamps per
commit. So the wire-critical offset/structure handling is largely unproven against libfdb_c. This
RFC adds the missing axes (and supersedes the offset-0 Value base case with a non-zero-offset
suite).

The 10-byte stamp differs per commit (it IS the commit version), but everything else — the
prefix, the user data around the stamp, the 2-byte tuple user version, and crucially **where the
stamp lands** — must be byte-identical. Masking the 10-byte stamp makes a clean differential.

## Investigation

- Go facade: `tx.SetVersionstampedKey(key, param)` / `SetVersionstampedValue(key, param)` →
  `Atomic(MutSetVersionstampedKey/Value, …)`; `GetVersionstamp()` returns the committed 10-byte
  stamp (`pkg/fdbgo/client/transaction.go:1138-1146`). cgo has the identical facade. Both at API 730 → 4-byte
  offset suffix.
- Tuple layer: `tuple.PackWithVersionstamp(prefix)` encodes an incomplete versionstamp + the
  offset suffix (`pkg/fdbgo/fdb/tuple/tuple.go:594-641`); the 12-byte tuple versionstamp is 10-byte tx version
  (replaced at commit) + 2-byte **user version** (preserved). Masking `[off, off+10)` leaves the
  user version intact for comparison.
- Read-back: for `SetVersionstampedKey` the materialized **key** holds the stamp (unknown
  pre-read) → range-scan the prefix to find it; for `SetVersionstampedValue` the key is fixed →
  point-read, the stamp is in the **value**.

## Fix

Net-new differential coverage (no production change expected — the path is believed correct;
this proves the offset/structure identity). New file
`pkg/fdbgo/bench/differential_versionstamp_test.go`. A custom runner writes the same template via
both clients (each to its own prefix, separate commits → different stamps), reads back the
materialized key/value, **masks `[offset, offset+10)`**, and byte-compares the masked results
go-vs-cgo (plus the unmasked surrounding bytes). Per CLAUDE.md, a no-pre-existing-bug axis is
teeth-checked by fault injection (mis-place the offset / mask the wrong region) and confirming a
case fails, then reverting.

1. **`TestDifferential_VersionstampedKey`** — stamp at offset 0 (key == stamp), stamp after a
   prefix, stamp mid-key (prefix + stamp + suffix). Masked key + the (fixed) value compared.
2. **`TestDifferential_VSValueOffsets`** — `SetVersionstampedValue` at NON-zero offsets (after a
   header, mid-value, binary surrounds), extending the existing offset-0
   `TestDifferential_VersionstampedValue` (`differential_test.go:126`, left in place). Fixed key;
   masked value compared.
3. **`TestDifferential_VersionstampTuplePack`** — build the key with `tuple.PackWithVersionstamp`
   (go) / `cgotuple.PackWithVersionstamp` (cgo) including a non-trivial **user version**, write
   via `SetVersionstampedKey`, read back, mask the 10-byte stamp, compare — proves the tuple
   offset encoding AND that the 2-byte user version is preserved (not masked) identically.
4. **`TestDifferential_GetVersionstamp`** — both clients commit a versionstamped op and call
   `GetVersionstamp()`; assert both return a 10-byte value (structurally valid; not byte-equal
   across distinct commits) and that it equals the stamp materialized into the key/value (mask
   nothing — read the stamp region and compare to `GetVersionstamp`).
5. **`TestDifferential_VersionstampErrors`** — the offset-validation **boundary**: tight-valid
   `offset+10 == body` (commits cleanly, code 0); off-by-one `offset+10 == body+1` (reject);
   negative offset; `offset+10 > body`; operand too small for the 4-byte suffix; empty body. Both
   clients must agree (reject → 2000 at Commit, the client-side guard before the server's
   commit-time silent skip; valid → code 0), asserted go==cgo (FDB-C++ dev).
6. **`TestDifferential_VersionstampMultiOp`** — two `SetVersionstampedKey` ops to distinct keys in
   ONE txn. The differential **corrected a reviewer assumption**: the 10-byte stamp identifies the
   TRANSACTION (commit version + batch order — `CommitProxyServer.actor.cpp` passes the same
   `transactionNumberInBatch` to every mutation of a txn), NOT the operation, so both ops get the
   **IDENTICAL** stamp (the user differentiates via the tuple user version). The test masked-compares
   go-vs-cgo AND asserts both ops share the stamp on each client with the clients agreeing (a client
   that wrongly advanced a per-op id would pass the masked compare but fail this).

**Masking soundness (Torvalds):** the mask offset is derived from the **shared template** (the
case's logical-data length), not from the per-client materialized output, so a Go offset bug
can't relocate its own mask. Before the masked compare, each case asserts (a) total length
byte-equal to the template-derived expectation, (b) the materialized key still starts with its
isolation prefix, and (c) the masked region was non-zero (the stamp materialized). A one-byte-off
stamp shifts the surrounding bytes into/out of the masked window, so the surround compare goes red.

**Tenant +8 offset adjustment** (`pkg/fdbgo/client/commitpath.go:354-363`) is the highest-value remaining axis but
needs tenant infrastructure the bench harness lacks (no tenant setup; cluster tenant_mode
unverified). Filed as a concrete TODO follow-up: open a tenant on both clients, write a
versionstamped key in the tenant txn, read back within the tenant, mask, compare — verifying the
offset is adjusted by the tenant prefix length identically.

## Performance

Test-only. No production change unless a divergence is found.

## Test plan

The tests above ARE the plan. Teeth verified live: loosening `validateVersionstampOffset`
(`offset+10 > bodyLen` → `bodyLen+1`) reddens `offbyone_reject` (go=0 accepts, cgo=2000), the
rest stay green; reverted. Run under `bazelisk test //pkg/fdbgo/bench:bench_test`. The tenant +8
offset adjustment is the one wire path left untested here — filed as the concrete TODO follow-up
above (needs tenant harness setup).
