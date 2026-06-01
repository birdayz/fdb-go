# RFC-053: Differential testing vs the official C binding (libfdb_c) — C2

**Status:** ACK'd (FDB-C-dev ACK + Torvalds ACK, both after one NAK round — fixes below); implementing
**Item:** RFC-010 C2 (differential vs `libfdb_c`)
**Goal:** Mechanical, high-confidence proof that the pure-Go FDB client (`pkg/fdbgo`)
is behaviorally identical to the official C binding (`libfdb_c`, via
`github.com/apple/foundationdb/bindings/go`) **at the client contract surface**:
the client's serialized wire output (the `CommitTransactionRequest` mutation and
conflict-range vectors) is byte-identical to the C++ reference; persisted state is
byte-identical; reads are semantically identical; range reads are
chunking-invariant.

**What this is NOT.** This does **not** "inherit" FDB's simulation coverage. A
differential harness against a testcontainer has no fault injection, no clock
skew, no network partitions, no buggify. Behavioral-parity-of-the-encoder is one
axis; fault-resilience is a different axis (chaos tests, the other RFC-010 items).
The earlier "absolute proof" framing was overpromised and is dropped (Torvalds #4).

## Problem

The C binding is the client FDB simulation-tests on every CI run; matching it is
the closest we get to inheriting that *correctness definition* (RFC-010 P5). But
we have **no test that runs the same operations through both clients and compares
what each actually emits**. Today's tests exercise our client alone; the Java
conformance suite is record-layer (Java↔Go), not client↔C.

## Investigation

- `github.com/apple/foundationdb/bindings/go` is already in `go.mod`; `libfdb_c.so`
  is on the test image. Both clients can open the **same** testcontainer cluster
  in one process.
- **The only client-determined wire output is the `CommitTransactionRequest`
  mutation + conflict-range vectors** (`NativeAPI.actor.cpp:5961-6008`). The server
  stores `param1→param2` verbatim and computes atomic results itself. Therefore:
- **Comparing the persisted *result* of an atomic does NOT prove the client
  encoded the mutation identically — it proves the server did the math**
  (Torvalds #1). ADD/OR/MIN read-back is server-validated theater *for encoding
  identity*. The authoritative client-identity signal is the **serialized mutation
  vector** (op-code + operands + conflict ranges), captured before it hits the
  wire.
- **Already-correct-but-unpinned in the Go client** (found while writing this RFC):
  - `Min`→`MinV2` (18) and `BitAnd`→`AndV2` (19) op-code upgrade at API≥510
    (`NativeAPI.actor.cpp:5990-5995`) is done in the facade
    (`fdb/transaction.go:261,280`) — but **no test asserts the emitted op-code**, so
    a refactor could silently revert it. This is the dimensional gap C2 exists to
    close.
  - `validateVersionstampOffset` (`client/transaction.go:807`) gates the 4-byte LE
    offset suffix — but parity of the *threshold/encoding* vs C++ is unpinned.
- **Two category errors in the first draft (both reviewers):**
  - **There is no record-splitting at the `fdb_c` layer.** The C client *rejects*
    `value.size() > VALUE_SIZE_LIMIT` with `value_too_large()`
    (`NativeAPI.actor.cpp:5965-5966`). The 100KB-chunk / `pk+\x00` / `+1…` suffix
    scheme is **record-layer**, not client. The correct C2 differential is
    **size-limit rejection parity**, not "identical chunk suffixes."
  - **There is no continuation token at the raw client.** Range reads return
    `(KV[], more)`; the caller re-issues with `firstGreaterThan(lastKey)`.
    "Mutually-resumable continuation, byte-equal format" is record-layer. The
    client-level differential is **GetRange chunking-invariance**.
- **Control plane (exclude):** reply-promise UIDs, read/commit versions,
  trace/span IDs, GRV batching, mutation/conflict *ordering* on the wire, range
  *chunk* boundaries (`more`/limits).

## Design

A new in-process differential harness (`pkg/fdbgo/differential`, real FDB via
testcontainers, `t.Parallel()`). **Three layers**, weakest-to-strongest signal
made explicit so nothing is theater:

### L1 — Wire-encoding golden vectors (the client-identity core)

A **white-box test in `package client`** (where the marshal lives) drives ops,
calls `buildCommitTransactionRequest`, and **taps the bytes that
`MarshalFDBPooled` actually emits** — NOT the `Mutation` structs the test built.
Tapping the structs would be asserting the test's own input back (Torvalds caveat
#1: that is the very circularity); the proof must be on the *serialized* output.
Assert **byte-exact** against golden vectors derived from the **C++ reference**
(`NativeAPI.actor.cpp`) — the neutral oracle, independent of either runtime client
(closes Torvalds #2: the spec is the oracle, not a Go-family client reading its own
writes). **Each golden byte vector carries the `NativeAPI.actor.cpp`/`Atomic.h`
line ref it was derived from, in a comment beside it**, so the derivation is
auditable and a misread can't silently become "truth" (Torvalds caveat #2). Pins:

- op-codes incl. **Min→MinV2 / And→AndV2** at API≥510 (`:5990-5995`);
- operand `param1`/`param2` byte encoding;
- **SetVersionstampedKey/Value**: trailing **4-byte little-endian** offset suffix
  — decoded by `parseVersionstampOffset` (`Atomic.h:258-264`), stripped via
  `substr(0,size-4)` + 10-byte stamp placed at the offset
  (`transformVersionstampMutation`, `Atomic.h:300-314`); `validateVersionstampOffset`
  threshold parity;
- **conflict-range presence per op**: `Set` adds a write-conflict-range;
  `SetVersionstampedKey` does **not** (`:6005`);
- **value_too_large / key-size limits**: both clients reject at the *same*
  threshold (`VALUE_SIZE_LIMIT`, `:5965/5987`).

### L2 — End-to-end persisted differential (the actual libfdb_c binding)

Run each logical op through **both** clients (to its own prefix `goPfx`/`cPfx`)
against one cluster; read the **raw** persisted KVs back, strip the prefix,
compare:

- **Set / Clear / ClearRange / versionstamp-placement** — stored bytes *are* the
  client's output ⇒ **byte-identical** is a true proof.
- **Atomics** — the persisted result is server-computed; byte-comparing it
  validates the *encode+server* path end-to-end and catches gross op-code
  divergence (a missing MinV2 upgrade flips missing-key semantics → a *different*
  stored result), but **L1 is the authoritative encode-identity proof**. Stated
  explicitly so this layer is not mistaken for encoding proof (Torvalds #1).
- **Neutral read.** Read both prefixes with a neutral reader **and** cross-read (C
  reads Go's prefix, Go reads C's) and assert agreement — the server is the
  neutral store; the bytes returned are server-held bytes.
- **error-class parity**: `value_too_large`, conflicts — compare categories.

### L3 — GetRange chunking-invariance + key-selector parity

Raw range reads return `(KV[], more)` — no token. Assert the **merged KV set +
order is identical** across both clients **regardless of where `more`/limits split
the range** (vary `limit`/streaming-mode); assert `GetKey` (key-selector
resolution) parity. Not "byte-equal continuation format" (record-layer).

### Harness shape & layout

L1 is a fast unit test (no container, no CGo) in `package client` white-box. L2/L3
live in a new `pkg/fdbgo/differential` package and drive **both public facades**
(`pkg/fdbgo/fdb` + `apple/.../bindings/go`) against one container — a reusable
table `differentialOp{name, goFn, cFn, verify}`, new shapes one-liners, CI-gated
behind the FDB-container tag.

**Facade op-code upgrade (Min→MinV2) is pinned behaviorally, not via a seam.** The
upgrade decision lives in the `fdb` facade; cross-package white-box can't reach
`client`'s unexported op-code. L1 proves op-codes 18/19 *serialize* correctly; L2
proves `Min()` *chooses* 18 — a `Min` on a **missing** key diverges in persisted
result if the client failed to upgrade (MinV2 changed missing-key semantics; that
is *why* V2 exists). Together they fully pin it without polluting the production
API with a test accessor.

### Network-thread singleton (Torvalds #3 — spec'd, not hand-waved)

The C binding starts **one** network thread per process, set-once and
never-restartable. A package-level fixture guards it with `sync.Once`:
`fdb.MustAPIVersion(N)` + `fdb.StartNetwork()` run exactly once on first use;
every parallel test shares that one network thread. Per-test key prefixes provide
data isolation; no test stops/restarts the network. Documented as a hard
constraint in the fixture.

## Performance

Test-only; no production code. One container + two clients per test; bounded
batteries; runs under the existing `--local_test_jobs` cap.

## Test plan

Phased (each phase lands green; each divergence → **fix the Go client, pin it**,
per the corpus discipline):

1. **L1 golden vectors** first — catches/pins the encoding-identity facts
   (MinV2/AndV2 op-codes, versionstamp 4-byte LE suffix, per-op conflict ranges,
   value_too_large threshold). Highest value, most likely to surface a real Go
   divergence (FDB-C-dev §6).
2. **L2 end-to-end** — Set/Clear/ClearRange/versionstamp byte-identical persisted
   state; atomic end-to-end equivalence; error-class parity.
3. **L3 range invariance** — chunking-invariant merged reads + `GetKey` parity.

`just test` green; new target tagged for the FDB env.

## Divergences found & fixed

Tracked here as C2 surfaces them (the corpus discipline: find → fix → pin).

1. **`SetVersionstampedKey` added a spurious write-conflict range** (found during
   L1 investigation, fixed in this branch). `client.Atomic` added a
   write-conflict range for *every* atomic op; C++ RYW `atomicOp` forces
   `AddConflictRange::False` for `SetVersionstampedKey`
   (`ReadYourWrites.actor.cpp:2268`) because its key carries an incomplete
   versionstamp — a conflict range over the placeholder bytes is meaningless and
   would spuriously conflict two txns stamping the same logical key. Fix: skip the
   range for `SetVersionstampedKey` while still consuming the
   `NEXT_WRITE_NO_WRITE_CONFLICT_RANGE` flag (C++ `getAndReset`, `:2220`). Pinned
   by `TestAtomic_SetVersionstampedKey_NoWriteConflictRange` /
   `…_ConsumesNextWriteNoConflictFlag` (taps the marshaled `CommitTransactionRequest`,
   verified to fail on the pre-fix code).

## Open questions / stretch

- **MITM wire-capture of libfdb_c.** L1 proves the Go client emits the C++-spec
  bytes; we trust `libfdb_c ≡ spec` (it is the reference impl), so encoding
  identity is closed *definitionally*. A Go FDB-protocol proxy in front of the
  single-process container could capture libfdb_c's *actual* emitted
  `CommitTransactionRequest` and diff it against Go's — confirming the
  trust-the-reference assumption empirically. High effort (address advertisement,
  ConnectPacket handshake), low marginal value over L1-vs-spec. Deferred; noted so
  the option is on record.
