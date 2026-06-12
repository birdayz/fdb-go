# RFC-098: `accessed_unreadable` (1036) — reading a pending versionstamped key throws

**Status:** ACCEPTED — FDB-C++-dev NAK→ACK (the NAK corrected SVK to range-unreadable +
key transformation; two impl cautions folded: offset-suffix preservation, Clear erases
range-unreadability); Torvalds NAK→ACK ×2 (test-flip enumeration; blast-radius section
mandatory — his audit found the GC purge txn scans-then-appends, making scan-before-append
the real SPFresh invariant, verified globally; non-blocking nit: the refresher scan lines
also run inside non-appending write txns).
**Scope:** the deferred full-parity half of the RFC-056 item "versionstamped-pending read
= unreadable" (TODO.md). The RFC-058 op-type write map made this implementable.

## Problem

In C++, a key with a pending `SetVersionstampedKey/Value` is **unreadable**: its value
cannot be known before commit (the 10-byte stamp is assigned by the proxy), so reading
it THROWS `accessed_unreadable` (1036, `error_definitions.h:63`). The Go client
approximates the key as **absent** (Get→nil, GetRange→omit) — deliberate, documented
(RFC-056/058), consistent across base states, but a real behavioral divergence: a C++
app gets a loud error where a Go app silently reads "no such key" for a key it just
wrote. The absent-approximation can mislead application logic (e.g. read-modify-write
on a versionstamped key would treat it as fresh).

## C++ semantics (all verified at 7.3.75)

1. **Source + stickiness** (`WriteMap.cpp:96-97`, in `mutate`):
   `is_unreadable = it.is_unreadable() || op == SetVersionstampedValue || op ==
   SetVersionstampedKey` — an entry becomes unreadable when a versionstamped op lands
   on it, and unreadability is STICKY across subsequent mutations on that entry (a
   later plain `Set` on the same key keeps it unreadable for the transaction's
   lifetime). `clear()` paths construct fresh entries (cleared ranges are readable —
   you know they're empty); boundary entries preserve the flag
   (`WriteMap.cpp:171-241`).
2. **Throw sites** (`RYWIterator.cpp:45-46` `type()`, `:75-76` `kv()`): any read that
   lands the RYW iterator on an unreadable segment throws 1036 — Get, GetRange
   (touching the segment), GetKey (resolution walking onto it).
3. **Read-path matrix** (`ReadYourWrites.actor.cpp:397-406` dispatch):
   - RYW disabled (`READ_YOUR_WRITES_DISABLE`) → `readThrough`: storage only, no
     write map, **no throw**.
   - snapshot read with snapshot-RYW **disabled** (`snapshotRywEnabled <= 0`,
     RFC-061 counter) → `readWithConflictRangeSnapshot`: SnapshotCache iterator, no
     write map, **no throw**.
   - everything else — regular reads AND snapshot reads with snapshot-RYW enabled
     (the default) → `readWithConflictRangeRYW` → RYWIterator → **throws**.
4. **SVK is RANGE-unreadable, not a point entry (review correction — this is the
   load-bearing subtlety).** `SetVersionstampedKey` (`ReadYourWrites.actor.cpp:
   2263-2295`):
   (a) marks the **entire candidate stamp range** unreadable via
   `writes.addUnmodifiedAndUnreadableRange(range)` (`:2271`; impl
   `WriteMap.cpp:205-242` — UNMODIFIED + unreadable range entries), where the range
   is `getVersionstampKeyRange` (`Atomic.h:268-287`: the placeholder key with the
   min-version stamp … with `\xff`×10);
   (b) **transforms the key** — the placeholder filled with the min-bound stamp
   derived from the cached read version (`:2276`, `Atomic.h:289`) — and only then
   `writes.mutate`s at the TRANSFORMED key (`:2295`).
   Consequences: a Get/GetRange of a **different** key inside the candidate range
   throws 1036; and a bypassed read of the unreadable-but-UNMODIFIED part of the
   range reads **through to storage** (there is no local entry there).
5. **`BYPASS_UNREADABLE` option** (`ReadYourWrites.actor.cpp:2611-2613`; applied per
   read at `:98` via `bypassUnreadableProtection`): for **SVV**, a bypassed read
   returns the entry's current value with the 10-byte placeholder bytes as written
   (C++ unit: `RYWIterator.cpp:433-449`); for the **SVK** unmodified-unreadable
   range, bypass reads through to storage (consequence of fact 4). Option is
   transaction-scoped, settable any time. Go's facade ALREADY ships
   `SetBypassUnreadable()` as a silent no-op (`pkg/fdbgo/fdb/options.go:149`) —
   once reads throw, that stub is a landmine; implementing it here is debt
   repayment, not gold-plating.
6. **Error class:** 1036 is NOT retryable (`fdb_c.cpp:84-93` — in neither
   MAYBE_COMMITTED nor RETRYABLE_NOT_COMMITTED; not in `onError`'s arms) — it
   surfaces to the caller as a programming-model error.
7. **Stickiness detail (review-verified):** the exact-key "SetValue replaces the
   stack" branch is gated `!it.is_unreadable()` (`WriteMap.cpp:125-126`); on an
   unreadable entry a later plain Set is PUSHED (`:147-148`) and the entry stays
   unreadable (`:141`). `clear()` inserts readable cleared begin entries (`:195`)
   and preserves the flag only at the end boundary (`:202`).
8. **GetRange reach (review-verified):** `limits.isReached()` breaks at
   `ReadYourWrites.actor.cpp:685` BEFORE the throwing `type()` at `:692` — a
   limited scan that stops before the unreadable segment does not throw.

## Go state today

`rywEntry` (RFC-058) already tracks the needed fact: `resolveAtomics` returns
`unresolved == true` when the chain contains a versionstamped op (`ryw.go:979-989` —
the comments literally call the key "UNREADABLE"), terminal and dominant over cleared.
The two read chains (`ryw.go:303-313` Get-merge, `:663-668` GetRange-merge) and
`ryw_getkey.go` currently map `unresolved` → absent. `atomic()` refuses to eager-fold
a versionstamp into a plain entry, preserving the signal. What's missing is only the
SURFACING: an error instead of absence, the stickiness on subsequent plain Sets, the
option, and the path matrix.

## Fix

1. **Stickiness:** `rywEntry` mutation paths mirror `WriteMap.cpp:97/:125/:141`:
   once a versionstamped op lands on a key, a subsequent plain `Set`/atomic on
   that key keeps the entry unreadable (today Go's `set()` at `ryw.go:182`
   wholesale-replaces the entry and loses the signal — verified; the matrix's
   stamp-then-plain-set row pins the fix). `Clear` produces a readable cleared
   entry (matches C++ and today's Go).
2. **SVK needs RANGE-unreadable state (C++ fact 4):** a per-key check in the merge
   chains cannot reproduce C++. The Go cache gains unreadable-RANGE tracking
   mirroring `addUnmodifiedAndUnreadableRange`: on SVK, (a) mark
   `getVersionstampKeyRange(placeholderKey)` unreadable-unmodified, (b) transform
   the key with the min-bound stamp from the read version exactly as
   `Atomic.h:289` does — **preserving the 4-byte offset suffix handling** (the
   transform fills the placeholder; the suffix was already consumed by the
   operand parse — review caution: don't strip/duplicate it when storing), (c)
   store the pending entry at the TRANSFORMED key. Reads (point and range)
   consult the unreadable ranges in addition to per-entry `unresolved`.
   **A later `Clear` overlapping the stamp range erases range-unreadability for
   the cleared span** (C++ gets this free from the shared PTree — Go's range set
   must subtract cleared spans; dedicated test row).
3. **Surfacing:** the Get / GetRange / GetKey merge chains return
   `&wire.FDBError{Code: ErrAccessedUnreadable}` (new constant, 1036) when the read
   REACHES an `unresolved` entry or an unreadable range — replacing the
   absent-approximation — on exactly the C++ matrix paths: regular reads and
   snapshot reads with snapshot-RYW enabled. RYW-disabled reads and
   snapshot-RYW-disabled snapshot reads keep storage semantics (no write-map
   consultation — already true in Go).
4. **`BypassUnreadable` option:** `Transaction.SetBypassUnreadable(bool)`; the
   existing facade stub (`fdb/options.go:149`) wires through instead of lying.
   Bypassed reads: SVV entries return the operand with placeholder bytes as
   written; SVK's unmodified-unreadable range reads through to storage (C++ facts
   4-5).
5. **GetRange:** throws when the unreadable segment is REACHED in iteration order
   (C++ fact 8) — a limited scan that stops before it does not throw. Forward and
   reverse.

## Wire-compat statement

No wire bytes change. This is read-path error semantics: keys/values written are
identical; commits are identical. The only change is WHAT a read of a
versionstamped-pending key returns pre-commit (error instead of absent), aligning
with C++/Java apps sharing the cluster.

## Blast radius (Go consumers of the old absent-approximation — audited)

- **`GetMetaDataVersionStamp`** (`pkg/recordlayer/database.go:826-835`)
  snapshot-reads the key `SetMetaDataVersionStamp` (`:814`) stamp-writes — and it
  ALREADY catches 1036 (ported from Java, where the throw is real). That handler is
  dead code today; this RFC makes it live. No change needed.
- **The record layer is immune by design:** versionstamp mutations are QUEUED
  (`versionMutations` + `localVersionCache`) and flushed to the FDB transaction
  only in `flushVersionMutations()` immediately before commit
  (`database.go:639-650`) — the Java-faithful pattern that exists precisely
  because of unreadability. During the read phase nothing versionstamped is
  pending on the transaction.
- **SPFresh audited — the safety property is ORDERING, not absence:** the GC
  purge transaction does BOTH — `spfresh_gc.go:112` → `spfreshRehome` →
  `fullReload` (`:226`) → changelog scan (`spfresh_cache.go:114`), then appends
  at `spfresh_gc.go:124`. That is safe because the scan PRECEDES the append:
  1036 fires only when a read reaches an ALREADY-pending stamped write. The
  invariant SPFresh transactions must hold is "changelog scans before changelog
  appends within one transaction" — a post-append scan would throw. The cache
  refreshers (`spfresh_cache.go:114/:193`) are read-only transactions; the GC
  trim clears ranges (cleared = readable). The only direct
  `tx.SetVersionstamped*` writers in pkg/recordlayer are `database.go:814`
  (covered above) and `spfresh_storage.go:459` (this bullet).

## Test plan

- Port the C++ matrix as a table test over {regular, snapshot+rywOn,
  snapshot+rywOff, rywDisabled, bypass} × {SVV, SVK} × base states
  {storage-absent, storage-present, locally-cleared, plain-set-then-stamp,
  stamp-then-plain-set (stickiness)} — plus the SVK-specific rows: a read of a
  DIFFERENT key inside the candidate stamp range throws; bypass on that range
  reads through to storage; the pending entry lives at the transformed
  (min-stamp) key.
- **Flip the three pinned approximation tests IN PLACE** (same names, same base
  states, expectations changed from absent/nil to 1036) so the diff shows the
  semantic change loudly: `TestRYW_VersionstampedAbsentNoPhantom`,
  `TestRYW_VersionstampedOverClearedOrPlainNoPhantom`,
  `TestRYW_VersionstampUnreadableIsSticky` (`pkg/fdbgo/client/ryw_test.go:1289/
  :1330/:1394`). They pinned real shipped behavior; this is deliberate,
  spec-driven evolution — not deletion.
- Differential vs libfdb_c (`pkg/fdbgo/bench`): same op sequences through both
  clients; both must return error CODE 1036 on the same reads, and identical bytes
  under bypass. The differential being red on the old behavior IS the proof of
  divergence; green after.
- GetKey: selector resolution walking onto an unreadable segment throws (and the
  offset-walk phantom-slot semantics from RFC-058 stay intact for bypass mode).
- GetRange reach semantics: limited scan stopping before the unreadable key does
  not throw; scan reaching it does. Forward + reverse.
- Revert-proof: the differential + matrix tests red on the absent-approximation.

## Implementation addendum (post-differential findings)

Two facts surfaced by the libfdb_c differential after the RFC was ACK'd; both
verified at 7.3.75 and ported. FDB-C++ + Torvalds re-review covers them.

### GetPipelined has its own write-map consult — gated

`Transaction.GetPipelined` (the facade `Get` fast path) consults the RYW write
map inline (transaction.go) instead of routing through `rywCache.get()`. The
original patch gated only `rywCache.get()`, so through the FACADE a key inside
a pending SVK candidate range read straight through to storage (returned 0
instead of 1036 — caught by `TestDifferential_Unreadable/svk_other_key_in_range`),
and a sticky-unreadable folded entry (stamp-then-plain-Set) returned the folded
value as a cache hit. Fixed: the same unreadable gate (entry.unreadable ‖
isUnreadableLocked, skipped under bypass) runs before any cache hit or server
send. Unresolved-chain entries already took `ErrNeedFullRYW` into
`rywCache.get()`, which owns bypass resolution. Pinned by matrix rows
`pipelined_get_svk_range_1036` / `pipelined_get_sticky_entry_1036`.

### A failed read poisons the transaction's commit (ryw->reading)

C++ tracks every read future in `ryw->reading`, an `AndFuture`
(ReadYourWrites.actor.cpp — reading.add at :1691 get, :1707 getKey, :1767
getRange, :1849 getAddressesForKey, :1290 watch setup). `commit()` waits on it
before ANY commit work (:1358-1359), and an errored future is never removed
(`AndFuture::add` keeps errored futures, `isReady` only pops successful ones —
flow/genericactors.actor.h:1912-1942; `waitForAll` = `quorum(n=size)`, whose
`oneError` fires immediately). Net semantics: **a read that failed — even one
whose error the caller caught and swallowed — fails a later Commit with that
same error, until reset** (`resetRyow()` does `reading = AndFuture()`, :2715).
Empirically confirmed: libfdb_c commits fail 1036 after a swallowed 1036
GetKey/GetRange (differential `getkey` / `getrange_reach` poisoning asserts).

Go had no equivalent — a swallowed read error left commit clean (silent
divergence, found only because the RFC-098 differential commits after probing).
Ported as `Transaction.readErr` (first tracked read error, mutex-guarded since
pipelined futures resolve on other goroutines):

- **Recorded** at the C++ reading.add-equivalent tails: `Get`,
  `Snapshot.Get/GetKey/GetRange/GetRangeReverse`, `GetKey`, `getRangeDir`,
  `GetPipelined`'s 1036 gate + `PendingGet.Resolve` final outcome (pipelined
  send + resolve together model ONE C++ read future; transient locate/send
  failures that get re-driven are not terminal outcomes and are not recorded —
  the re-drive's result is), `GetAddressesForKey`, `WatchSetup`'s value read.
  GRV failures are recorded too (C++ acquires the read version inside the
  tracked future).
- **Not recorded**, matching C++: eager validation errors
  (key_outside_legal_range etc. — returned before a read future exists),
  `GetEstimatedRangeSizeBytes`/`GetRangeSplitPoints` (C++ uses waitOrError, no
  reading.add), and context cancellation (no C++ analogue — C++ cancellation is
  whole-transaction via resetPromise; recording it would poison commits
  libfdb_c would allow).
- **Checked** in `Commit` after `checkTimeout` (a fired timebomb sits in
  resetPromise, surfaced before the actor's wait(reading)) and before all
  commit-time mutation validation and the read-only fast path.
- **Cleared** in `reset()` and `postCommitReset()`.

Blast radius note: code that swallows a read error and then commits now fails
that commit — exactly as it would on libfdb_c. The known in-repo pattern
(record layer `GetMetaDataVersionStamp` catching 1036) only issues the read
when the dirty flag is unset, i.e. when no same-txn stamp write happened — so
it cannot self-poison; the catch handles cross-path races where Java/libfdb_c
would poison identically.

Pinned by matrix rows `commit_poisoned_by_swallowed_read_error` (poison +
Reset clears) and `commit_poison_precedes_validation` (1036 outranks the
commit-deferred 2102), plus the differential poisoning asserts on both clients.

### The unreadable-cap scan must not touch sortedKeys (quadratic blowup)

`unreadableScanCapLocked` runs on EVERY getRange (it computes the window cap
before the fast-path branch). Its first version called `ensureSortedLocked`,
which rebuilds the sorted write-key index O(N log N) after every write
invalidation — so a transaction interleaving writes with range reads (the
record layer's standard batch shape) went quadratic: the recordlayer suite
blew its 900s bazel budget in the pre-commit run (50k interleaved set+scan
iterations: 3m30s quadratic vs 0.03s fixed).

Fix: a dedicated sorted `unreadableKeys` index on the rywCache, maintained
incrementally at the flag transitions (atomic() inserts on the false→true
transition; clear()/clearRange() remove when deleting an unreadable entry —
the only flag-off paths; set() preserves the flag so no index change). The
scan short-circuits when both `unreadableKeys` and `unreadableRanges` are
empty (the overwhelmingly common case) and otherwise binary-searches the
tiny index — no sortedKeys rebuild on any read path. C++ pays the analogous
cost inside its PTree (following_keys_unreadable bits ride the existing tree
nodes); the dedicated index is the map-of-writes equivalent.

Pinned by `TestRYW_UnreadableCapScanNotQuadratic` (50k interleaved ops under
a 30s bound — red at 3m30s on the quadratic version, revert-proven) and
`TestRYW_UnreadableKeysIndex` (insert/sticky-preserve/clear/clearRange
transitions drive the cap scan).

### Blast radius follow-up: tests may not swallow a failing read and commit

`store_open_retryable_error_test.go` ("keeps the fdb.Error type…") drove
exactly one store-open attempt by setting a poisoned read version EVERY
attempt and swallowing the resulting 1009 (`return nil, nil`) — relying on
the old gap where a swallowed read error left commit clean. With reading
poisoning the commit now fails 1009, which is retryable, so the Run loop
retried the still-poisoning closure forever: the recordlayer suite hung at
this spec until its 900s bazel budget (the second, independent cause of the
pre-commit timeout alongside the quadratic cap scan). libfdb_c loops
identically on this shape — the test's assumption was the divergence. Fixed
by capturing the open error on attempt 1 and RETURNING it (clean attempt 2
proves the wrapped error stayed retryable — a strictly stronger pin of the
original regression than the errors.As assertion alone).

### FDB-C++ implementation-review catch: selector walks need unreadableRanges boundaries

`boundCandidatesLocked` (the merged-segment boundary source for getKey
resolution) drew bounds from write keys, cleared ranges, and snapshot-cache
entries — but not from `unreadableRanges`. The SVK candidate range's HEAD
`[begin, entry)` (begin = key@stamp(minVersion); the pending entry sits 4
suffix bytes above it) contains no write-map key, so without a boundary at
`begin` that head is swallowed into the unknown segment starting BELOW the
range — and a reverse selector anchored inside the head escapes downward to a
storage key where libfdb_c throws 1036 (C++ gets the boundary for free:
addUnmodifiedAndUnreadableRange inserts explicit range nodes, WriteMap.cpp:
205-242, and RYWIterator type() throws at :45-46). Red repro:
`lastLessThan(begin+ε)` resolved to the storage key below the range.

Fix: contribute the `unreadableRanges` lo-1..lo+1 window's begin/end to the
boundary candidates (same sorted/coalesced window argument as `cleared`).
Pinned by matrix rows `getkey_from_inside_svk_range_head_1036` (red→green),
`getkey_crosses_svk_range_1036` / `getkey_crosses_emptied_svk_range_1036`
(entry/cleared-edge adjacent shapes, green before and after — they document
why the simpler shapes did NOT reproduce), and the differential row
`getkey_from_inside_svk_range_head` (go==cgo).
