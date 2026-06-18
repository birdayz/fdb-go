# RFC-118: `SimTransport` — faithful frame-level fault injection, closing the C4 read-path test gaps (`pkg/fdbgo`)

**Status:** Implemented (branch `client/close-grv-cache-and-simtransport`; full pre-commit
`just test` + `-race` green; all four gaps revert-proven). RFC review: **FDB C++ maintainer ACK**
(faithfulness model verified against 7.3.75 — inline channel + `canReplyWith` gate + QueueModel
backoff confirmed; no read error reaches the root channel, so the root→inline upgrade loses no
coverage) + **Torvalds ACK** (three impl conditions, pinned in "Implementation contract" below).
Implementation re-review: **FDB C++ maintainer ACK** + **Torvalds ACK** (both on the impl HEAD,
two nits fixed: derive the envelope vtable closures from the reply types' own `VTableClosure`;
read QueueModel fields under `mu`) + **/code-review high** (no correctness findings). **PR gauntlet
(codex + @claude + CI) pending push/PR** — not yet run (the branch is local).

**Closes:** `TODO.md` **D1** (`SimTransport`) and the four **C4 deferred Phase-0 test gaps**
(TODO "C4. Deferred Phase-0 test gaps"):
1. Inline `LoadBalancedReply.error` on `parseGetKeyReply` / `parseGetKeyValuesReply` specifically.
2. `PendingGet.Resolve` flush-error arm.
3. Range wrong-shard across a partial continuation / `more=true` (mid-scan, forward + reverse).
4. `future_version` (1009) / `process_behind` (1037) read-path QueueModel backoff wiring.

**Spec:** C++ `libfdb_c` 7.3.75 (`/home/birdy/projects/foundationdb`, `git describe` → `7.3.75`).
`file:line` cites are at that tag.

**Scope:** **Test infrastructure only.** Every production path these gaps name is *already wired
and correct* (verified `file:line` below) — what is missing is a *faithful* fault-injection
harness that drives each path the way real FDB does, and the regressions that pin it. No production
`pkg/fdbgo` byte changes. Wire-compat impact: **none** (no request/reply/continuation/index bytes
change). The only wire concern is the *faithfulness of the injected reply bytes*, which is exactly
what the FDB-C-dev review must validate.

---

## Problem

The deterministic fault injectors in `client/fault_test.go` (`wrongShardConn`, `dropReplyConn`)
are the client/wire analog of FDB's Sim2/BUGGIFY — but they have two limitations that left four
read-path dimensions unprobed:

1. **They inject errors through the wrong channel.** A read reply carries an error two ways:
   the **root** `ErrorOr<T>` union, and the **inline** `LoadBalancedReply.error` field. Real FDB
   storage servers deliver read `wrong_shard_server` / `future_version` / `process_behind` through
   the **inline** field (`storageserver.actor.cpp` `sendErrorWithPenalty`, below), *never* the
   root. `wrongShardConn` replaces the whole frame body with a **root** `ErrorOr` error
   (`buildFDBErrorResponse(1001)`, `fault_test.go:337`). So the existing `TestWrongShardServer_*`
   tests prove the client handles a *root-channel* 1001 — an unrealistic delivery for a read — and
   leave the **inline** channel (the one production decodes at `readpath.go:341/948/1007` via
   `wire.ReadInlineReplyError`) unexercised end-to-end. RFC-115 §6 fixed the reply *writer* so an
   inline `Optional<Error>` round-trips (`wire/types/inline_error_test.go`), but no *integration*
   test feeds a faithful inline reply through a read retry loop. (**Gap 1.**)

2. **They have no frame targeting and no rewrite.** `wrongShardConn` replaces "the next non-PING
   frame"; it cannot skip to the *N*-th frame, and it cannot rewrite a reply (e.g. set `More=true`
   to force a continuation). That blocks any *mid-scan* fault (**Gap 3**). There is also no
   write-side fault to drive `PendingGet.Resolve`'s `conn.Flush()` error arm (**Gap 2**), and no
   test drives an inline `future_version`/`process_behind` reply to assert the QueueModel records
   the backoff (**Gap 4**).

These are not production bugs — the wiring exists:

- **Gap 1 (decode):** `parseGetKeyReply` (`readpath.go:341`), `parseGetKeyValuesReply`
  (`readpath.go:948`), `parseGetValueReply` (`readpath.go:1007`) each call
  `wire.ReadInlineReplyError(&r, types.GetXxxReplySlotError)`. Decode is correct and unit-pinned
  (`wire/types/{inline_error,erroror}_test.go`); the *integration* dimension is unprobed.
- **Gap 2 (flush arm):** `PendingGet.resolve` (`transaction.go:858-869`): on `p.conn.Flush()`
  error → `handleConnError(p.addr)` + cancel/release + `resolveFull()` (relocate+retry). Live
  code, zero coverage — existing pipelined tests set `flushed:true` to skip it
  (`readpath_unit_test.go:363`).
- **Gap 3 (continuation relocate):** `getRangeImpl` inner loop (`readpath.go:671-767`) narrows the
  shard range after a `more=true` batch (`shardBegin = keyAfter(lastKey)`, `:765`) and, on a
  wrong-shard mid-scan, invalidates the *narrowed* range (`:704`) and resumes from there (`:708`).
  No test reaches the inner loop with `more=true` then faults the continuation frame.
- **Gap 4 (QueueModel backoff):** all three read senders pass
  `isFutureVersionOrProcessBehind(err)` to `endRequestFull` (`readpath.go:323/548/587/885`);
  `loadbalance.go:320` advances `failedUntil`; `chooseServer`/`chooseTopTwo` (`:125/:228`) skip a
  server while `now < failedUntil`. The only tests call `endRequestFull` directly (unit) or set
  `failedUntil` by hand — none drive an injected 1009/1037 *through a read*.

---

## C++ spec — the inline error channel

`fdbserver/storageserver.actor.cpp:1855-1865` — for any `LoadBalancedReply`-derived reply
(`GetValueReply`, `GetKeyReply`, `GetKeyValuesReply` all are), an error is delivered as a
**successful** reply whose inline `error`/`penalty` fields are set:

```cpp
template <class Reply>
typename std::enable_if<isLoadBalancedReply<Reply>::value, void>::type
sendErrorWithPenalty(const ReplyPromise<Reply>& promise, const Error& err, double penalty) {
    if (err.code() == error_code_wrong_shard_server) ++counters.wrongShardServer;
    Reply reply;
    reply.error = err;        // <-- INLINE Optional<Error>, not the root ErrorOr
    reply.penalty = penalty;
    promise.send(reply);      // <-- send(), NOT sendError() — root tag = value/2, data empty
}
```

The non-`LoadBalancedReply` overload (`:1867-1874`) uses `promise.sendError(err)` (the root
channel) — that path is for non-read replies, not the read gaps here. The three read handlers'
catch blocks route through this inline overload: `getValueQ` (`:2536`), `getKeyValuesQ` (`:4710`),
`getKeyQ` (`:6715`), gated by `canReplyWith` (`:137-142`, which whitelists `wrong_shard_server` /
`future_version` / `process_behind`). All three reply types are inline-channel-eligible because
they derive from `LoadBalancedReply` (`StorageServerInterface.h`: `GetValueReply` `:298`,
`GetKeyValuesReply` `:389`, `GetKeyReply` `:566`). The FDB-C-dev review confirmed **every** root
`sendError(wrong_shard_server())` in the storage server is on a NON-read request (checkpoint,
change-feed, waitMetrics, GetShardState) or the `GetKeyValuesStream` path the Go client's `getRange`
does not use (`:6428`) — so no point/range read error is reachable via the root channel, and
upgrading the wrong-shard tests from root- to inline-injection loses no real-FDB coverage.

`fdbrpc/include/fdbrpc/LoadBalance.actor.h:336-351` — the client reads `errCode` from the **inline**
field and threads `futureVersion` + `penalty` into the QueueModel:

```cpp
errCode = loadBalancedReply.get().error.present() ? loadBalancedReply.get().error.get().code()
                                                  : error_code_success;          // :338
bool futureVersion = errCode == error_code_future_version
                  || errCode == error_code_process_behind;                       // :348
modelHolder->release(receivedResponse, futureVersion,
    loadBalancedReply.present() ? loadBalancedReply.get().penalty : -1.0);       // :350-351
```

`QueueModel::endRequest` (`fdbrpc/QueueModel.cpp:36-46`) is where `futureVersion` advances the
per-address backoff: it grows `futureVersionBackoff` and sets `d.failedUntil = now() +
d.futureVersionBackoff`. Go matches this in `loadbalance.go:315-320`. (Gap 4's assertion targets
exactly this state.)

So a faithful injected read error is: the reply type's `MarshalFDB()` with `HasError=true`,
`Error.ErrorCode=<code>`, `Penalty=<p>`, and empty data — byte-identical in shape to what
`sendErrorWithPenalty` puts on the wire. The Go reply structs already model exactly these fields
(`getkeyvaluesreply_generated.go:39-46`: `Penalty, HasError, Error, Data, Version, More, …`), and
RFC-115 §6 made the writer emit the inline `Optional<Error>` union faithfully.

---

## Proposed change — `SimTransport`

One reusable proxy-frame loop, parametrized by a per-frame callback. It is the shared core that
`wrongShardConn` and `dropReplyConn` already *are* (read ConnectPacket → forward frames → skip
PINGs → optionally mutate), lifted into a single primitive so the four gaps reuse it instead of
growing a fifth and sixth bespoke conn. No new abstraction beyond "the existing loop + a callback."

New file `client/simtransport_test.go` (test-only):

```go
// simConn proxies server→client frames through an io.Pipe (the proven
// wrongShardConn pattern) and runs `intercept` on each non-PING reply frame in
// order. PINGs always pass through untouched (keep-alive). The callback owns all
// targeting (it closes over a frame counter) and all mutation (replace / rewrite
// / drop / pass-through) — there is no rule-struct, just a func.
type simConn struct {
    net.Conn
    pr        *io.PipeReader
    intercept func(idx int, token transport.UID, body []byte) (newBody []byte, drop bool)
}
```

- `wrongShardDialer` and `dropReplyDialer` become **thin constructors** over `simConn` (an
  intercept that replaces / drops the next non-PING frame). **Zero caller churn** in the 6
  `dropReply` call sites or the `wrongShard` tests; the duplicate proxy loop in `fault_test.go` is
  deleted. ("No parallel pipelines" — one loop.)
- **Faithful inline-error builder** (closes Gaps 1, 3, 4):
  `inlineErrorReply(replyType, code uint16, penalty float64) []byte` returning
  `(&types.GetXxxReply{HasError: true, Error: types.Error{ErrorCode: code}, Penalty: penalty}).MarshalFDB()`.
- **`More`-flip rewriter** (Gap 3): decode the real `GetKeyValuesReply`, set `More=true`,
  re-marshal — `Data` round-trips as `[]byte`, so the data plane is unchanged; this faithfully
  simulates a server that chose to return a partial batch.
- **Gap 2 — closed-conn flush error (no new conn type needed).** A `net.Conn` write-fault races
  the writeLoop's unconditional auto-flush (`transport/conn.go:550-552` clears `hasDirty`, after
  which `Conn.Flush()` short-circuits to `nil` at `:477`), so it cannot deterministically reach the
  arm. Instead, hand-construct a `PendingGet` (the established pattern of the existing
  `TestPipelinedGet_Resolve_{TransportError,Timeout}Retries`) with a **real conn that has been
  `Close()`d** (joins the loops, `:Close`) then `SendFrameDeferred`d (sets `hasDirty=true`
  unconditionally at `:464`, before its own ctx-done return). `Resolve()`'s `Flush()` then sees
  `hasDirty=true` + cancelled ctx + exited writeLoop → returns `errConnClosed` (`:485/:495`)
  **deterministically** — a faithful Flush error (a conn torn down between the deferred send and the
  flush, which the connection monitor does in production). The arm fires; `resolveFull → getValue`
  re-dials (the pool self-heals on a closed entry, `database.go:375-378`) and returns the value.

The wrong-shard injection in the existing `TestWrongShardServer_*` tests is **upgraded from the
root channel to the faithful inline channel** — this both fixes their faithfulness and is the
integration half of Gap 1 for `getValue` (Gap 1 proper adds `getKey`/`getKeyValues`).

Anti-self-confirming (CLAUDE.md / `fault_test.go` P6): every injected code is the **canonical
literal** (1001 / 1009 / 1037), never a code-under-test constant.

---

## Executable spec (the regressions)

All against a real testcontainer FDB, `t.Parallel()`, unique key prefixes, container timeouts.
Each is **revert-proven**: back out the production arm (or the faithful-channel switch) → red.

1. **Gap 1 — inline error on `getKey`/`getKeyValues`/`getValue`.** Seed; warm cache + RV; arm
   `simConn` to replace the next `GetKey`/`GetKeyValues`/`GetValue` reply with
   `inlineErrorReply(…, 1001, p)`; assert the read **retries** (cache invalidated) and returns the
   correct value/key/range. Revert-prove by deleting the `ReadInlineReplyError` call in the parser
   → the inline 1001 is missed → no retry → wrong/garbage result.
2. **Gap 2 — `PendingGet.Resolve` flush-error.** Pipeline a `GetPipelined`/deferred `GetValue` on a
   `writeFaultConn` armed one-shot; `Resolve()` → `conn.Flush()` errors → `handleConnError` +
   `resolveFull()` re-dials a clean conn → returns the correct value. Assert: correct value, and
   (via QueueModel/metrics) the faulted address was marked bad. Revert-prove by removing the
   `if err := p.conn.Flush(); err != nil` arm → the flush error surfaces / the request is lost.
3. **Gap 3 — range wrong-shard mid-scan (`more=true`), forward + reverse.** Seed ≥3 keys in one
   shard; warm cache + RV; arm `simConn` with a two-step intercept: frame 1 → flip `More=true`
   (rewriter); frame 2 (the continuation) → replace with `inlineErrorReply(GetKeyValuesReply,
   1001, p)`; thereafter pass through. Assert `GetRange` returns the **full** set, **no duplicate
   and no dropped key**, for both `reverse=false` and `reverse=true`. Revert-prove by widening the
   invalidation back to the whole remaining range (pre-`:704`) — a dup/drop appears.
4. **Gap 4 — 1009/1037 → QueueModel.** Inject `inlineErrorReply(…, 1009, p)` (and 1037) once on a
   read to address `A`; assert `tx.db.queueModel`'s `failedUntil[A]` advanced past `now` and
   `futureVersionBackoff` grew, for `getValue`/`getKey`/`getKeyValues`. Revert-prove by passing
   `false` instead of `isFutureVersionOrProcessBehind(err)` at the `endRequestFull` call →
   `failedUntil` does not advance. (Single-SS testcontainer can't observe re-selection, so the
   assertion is on QueueModel state — the cause — not server choice — the downstream effect; a
   pure `chooseTopTwo`-skip unit test already covers selection, `loadbalance_test.go:53`.)

---

## Why not …

- **… write >80 KB to force a real server `more=true` for Gap 3?** `replyByteLimit = 80000`
  (`readpath.go:26`) is a const; a data-volume scan is heavy and the server's split point is
  knob-dependent → flaky. The `More`-flip rewriter is deterministic and equally faithful (a partial
  batch is a legitimate server behavior).
- **… keep `wrongShardConn`/`dropReplyConn` alongside `simConn`?** That is two proxy loops — the
  "parallel pipelines" smell. They collapse to one intercept callback each.
- **… a fake conn for Gap 2?** `PendingGet.conn` is a concrete `*transport.Conn` (not an
  interface), so the Flush error must be driven through a real conn. A `Close()`d conn gives a
  deterministic `errConnClosed` from `Flush()`; a socket-write-fault wrapper would race the
  writeLoop's auto-flush and is unnecessary (the arm handles every flush error identically).

---

## Implementation contract (Torvalds ACK conditions — non-negotiable)

1. **The per-frame `idx` counts non-PING reply frames only.** PINGs always pass through and never
   advance the counter — documented in the `intercept` callback's doc comment. A stray PING must not
   shift the index, or index-based targeting flakes.
2. **Gap 2 uses a closed real conn, not a write-fault wrapper** (resolves Torvalds' one-shot
   concern by construction — there is no armed wrapper to misfire on the retry). The conn is
   `Close()`d (loops joined) *before* it is handed to the hand-constructed `PendingGet`, so the
   only flush outcome is the deterministic `errConnClosed`; the `resolveFull` retry dials a fresh
   conn (pool self-heal, `database.go:375-378`).
3. **The Gap-3 `More`-flip round-trip is pinned before it is relied on.** A
   decode→flip→re-marshal→decode equality assertion on the `GetKeyValuesReply` (Data + all fields
   except `More` unchanged, `More` now true) lands in the Gap-3 test setup *before* the rewriter
   drives the client — so a re-marshal that corrupts `Data` reddens at the assertion, not as a
   confusing dup/drop downstream.

## Risks

- Re-marshaling a `GetKeyValuesReply` for the `More`-flip must round-trip `Data` byte-faithfully.
  Pin with a decode→flip→re-marshal→decode equality assertion in the Gap-3 test setup before it is
  relied on.
- Frame targeting relies on warming cache + RV so only the target read replies flow during the
  armed window (the established `TestWrongShardServer_*` discipline). The relocate after a fault
  issues locate frames on a *different* conn, after the intercept has disarmed.
