# RFC-111: Cluster-file re-watch — follow coordinator-set changes (port `libfdb_c` monitor-and-rewrite)

**Status:** Accepted (v3 — Torvalds ACK on v2; addresses FDB-C++ maintainer's v2 NAK: the
`ParseClusterString` acceptance-set gate, §8)
**Item:** `rfcs/prod-readiness-go-client.md` punch-list **P0.1** ("Cluster-file re-watch") — the
single most consequential production gap in §3 of that assessment.
**Spec:** FoundationDB C++ `libfdb_c` at tag **7.3.75** (`/tmp/fdbsrc`). C++ is the spec.
**Wire-compat impact:** The on-disk cluster file is **cross-tool shared state** — a C++/Java client
reading the file the Go client rewrote MUST parse the identical coordinator set. The persisted file
content is matched byte-for-byte against C++ `ClusterConnectionFile::persist` *and* the connection
string is matched byte-for-byte against C++ `ClusterConnectionString::toString` (golden vectors,
including hostname + `:tls` coordinators).

---

## Problem

`db.clusterFile` is captured once at open and is **immutable after creation**
(`client/database.go:149`). Every coordinator contact reads the same frozen list:
`tryAllCoordinators` (`database.go:484/491/493/500/504/512`) and `buildOpenDatabaseCoordRequest`
(`coordinator.go:17`). The topology monitor (`topology.go`) re-polls those *same* coordinators every
5s to refresh the **proxy** list, but it has **no mechanism to learn that the coordinator set itself
changed**.

When an operator runs `coordinators auto` / `coordinators change` (or replaces a failed coordinator),
the coordinator set rotates. The Go client keeps hammering the stale addresses forever; once the old
coordinators are decommissioned, **every transaction blocks until the caller's `ctx` cancels — an
unrecoverable strand**. `libfdb_c` survives this; we don't.

## C++ spec — two change-propagation paths

`libfdb_c` learns about coordinator-set changes two ways, both in
`fdbclient/MonitorLeader.actor.cpp` `monitorProxiesOneGeneration` (the proxy/`ClientDBInfo` monitor
that is the analog of our `topologyMonitor`):

### Path A — protocol *forward* (primary path; old coordinators still reachable)

When the set changes, the **old** coordinators register a forwarding leader. The
`OpenDatabaseCoordRequest` reply (`ClientDBInfo`) carries a **`forward`** field (`Optional<Value>`,
the serialized new `ClusterConnectionString` — the server sets `info.forward = req.conn.toString()`,
`Coordination.actor.cpp:400/406/645`):

- `CommitProxyInterface.h:132` — `Optional<Value> forward;`, serialized at logical position 3
  (`CommitProxyInterface.h:151-161` → Go `ClientDBInfoSlotForward = 3`, value at slot 4).
- `MonitorLeader.actor.cpp:939-948` — when `rep.get().read().forward.present()`, the client builds an
  intermediate record from the forwarded connection string, **asserts it has ≥1 coordinator**
  (`:946` `ASSERT(...getNumberOfCoordinators() > 0)`), and **returns the generation** (restart against
  the new coordinators). The reply's proxies are *ignored* on a forward (the `forward` branch returns
  before `clientInfo->setUnconditional`, `:964-966`).
- `MonitorLeader.actor.cpp:949-959` — once connected to the new coordinators, if the in-memory
  connection string differs from the on-disk file (`connRecord != info.intermediateConnRecord`), it
  **persists** the new string: `connRecord->setAndPersistConnectionString(...)`.

The leader-election monitor (`monitorLeaderOneGeneration`, `:608-627`) has the same forward handling,
but it is **N/A for the Go client**: the Go client never uses the `getLeader`/nominee RPC — it goes
straight to `OpenDatabaseCoordRequest` (`coordinator.go`). Only the proxy-monitor path applies.

### Path B — re-read the on-disk file (fallback; *all* current coordinators unreachable)

`MonitorLeader.actor.cpp:883-900`: on each iteration the client checks
`connRecord->upToDate(storedConnectionString)` — does the on-disk file still match the in-memory
string? (`ClusterConnectionFile.actor.cpp:71-84`: re-reads the file and `toString()`-compares.)

- up-to-date → fine.
- **not** up-to-date **and** `allConnectionsFailed` **and** `storedConnectionString.getNumberOfCoordinators() > 0`
  → another process rotated the set and rewrote the file while we couldn't reach anyone: **adopt the
  file's connection string** (`:898`) and restart the generation.
- not up-to-date but coordinators *are* reachable → just flag `incorrect_cluster_file_contents`
  (informational, `:901-913`); the reachable coordinators / forward stay authoritative. The file is
  corrected later via the Path-A persist, not by adopting the file.

`allConnectionsFailed` is set only after a full round-robin sweep with no success (`:975-978`) — i.e.
exactly our `tryAllCoordinators` returning an error.

### Persisted file format (`ClusterConnectionFile::persist`, `ClusterConnectionFile.actor.cpp:148-182`)

```
atomicReplace(filename,
  "# DO NOT EDIT!\n# This file is auto-generated, it is not to be edited by hand\n"
  + cs.toString() + "\n");
```

`cs.toString()` (`MonitorLeader.actor.cpp:438-453`): `key@` then **all `coords` (IP NetworkAddresses)
in order, then all `hostnames` in order**, comma-joined, each rendered with its own `:tls` suffix when
TLS. `key` is `description:id`. `atomicReplace` is a temp-write + durable rename (no torn file). On a
write error C++ **catches and returns false** (`UnableToChangeConnectionFile`, `:173-178`) — it does
**not** fail the connection. `ClusterConnectionMemoryRecord` (`ClusterConnectionMemoryRecord.actor.cpp:26`)
is the no-file variant: memory-only.

---

## Proposed Go change (v2)

### 1. `connRecord` — port `IClusterConnectionRecord`, single mutation path, best-effort persist

Replace the immutable `database.clusterFile` field with a mutex-guarded record that localizes the
mutable connection string + the file-vs-memory distinction (C++ `ClusterConnectionFile` /
`ClusterConnectionMemoryRecord`). An `atomic.Pointer` is **insufficient**: Path B is a read-the-file →
compare → conditionally-swap that must be one critical section (an atomic pointer can't make the file
I/O atomic with the swap).

```go
type connRecord struct {
    mu       sync.Mutex
    cf       *ClusterFile  // current in-memory connection string (coordinators + cluster key)
    filename string        // "" => memory-only (no persistence), like ClusterConnectionMemoryRecord
    logger   *slog.Logger  // persist failures are logged + swallowed here, never returned as fatal
}

// get snapshots the active connection string under the lock.
func (r *connRecord) get() *ClusterFile

// setAndPersist swaps the in-memory string UNCONDITIONALLY, then best-effort persists to disk
// (Path A / forward adopt). The on-disk write is skipped when the file already equals cf (idempotent,
// matching C++ persist's up-to-date short-circuit). A persist error (EROFS/EPERM/...) is logged and
// swallowed — adoption already took effect; the connection is never failed by a file-write error
// (C++ ClusterConnectionFile.actor.cpp:173-178). No-op write when filename=="".
func (r *connRecord) setAndPersist(cf *ClusterFile)

// adoptStoredIfChanged is Path B as ONE critical section: re-read the on-disk file, compare to the
// in-memory string (a.String()==b.String()), and if it changed AND has >=1 coordinator, swap
// in-memory (the file is already authoritative — no re-persist). Returns whether it changed. Parse
// errors are logged and treated as "no change" (C++ upToDate swallows file errors, returns false).
func (r *connRecord) adoptStoredIfChanged() (changed bool)
```

There is **no** `set()` — exactly one in-memory-only mutation (Path B, inside the locked
`adoptStoredIfChanged`) and one swap+persist (Path A). **Single-writer in practice:** `bootstrap` runs
fully *before* `topologyMonitor` is started (`database.go:637` then `:649`), and thereafter only the
single topology-monitor goroutine mutates the record — one writer at a time. The mutex guards readers
(the coordinator fan-out) against that writer and makes Path B's read-compare-swap atomic.

### 2. Thread the snapshot through the ENTIRE coordinator-contact chain

One refresh round must use one consistent connection string — the dialed coordinator AND the
`ClusterKey` (`Description:ID`) must come from the same snapshot. Thread it all the way down:

`refreshTopology`/`bootstrap` take `snap := db.connRecord.get()` →
`tryAllCoordinators(ctx, snap)` → `tryOneCoordinator(ctx, snap, addr)` →
`openDatabaseCoord(ctx, conn, snap, addr)` → `buildOpenDatabaseCoordRequest(snap, replyToken)`.

No coordinator-contact code reads `db.clusterFile`/`db.connRecord` independently after the top-level
snapshot (fixes the `coordinator.go:17` unlocked read + the dial-target/cluster-key split).

### 3. Parse `forward` from the coordinator reply

`DBInfo` gains `Forward string`. `parseClientDBInfoFromReader` (`coordinator.go:90`) reads it exactly
as the generated `types.ClientDBInfo.UnmarshalFromReader` does (slot-3 presence tag + slot-4 value):

```go
if r.FieldPresent(types.ClientDBInfoSlotForward) && r.ReadUint8(types.ClientDBInfoSlotForward) > 0 {
    info.Forward = string(r.ReadBytes(types.ClientDBInfoSlotForward + 1))
}
```

A forward reply's proxies are ignored (matches C++ — the forward returns before
`setUnconditional`).

### 4. `ClusterFile.String()` — byte-faithful port of `ClusterConnectionString::toString`

`Description:ID@` then **IP coordinators first (in original order), then hostname coordinators (in
original order)**, comma-joined, each with `:tls` appended when `UseTLS`. Coordinators are classified
IP-vs-hostname the way C++ does (`Hostname::isHostname`): a coordinator whose host part parses as an IP
literal is a coord, otherwise a hostname (IPv6 brackets handled). This reproduces C++ `toString`
byte-for-byte for the inputs we accept (uniform-TLS — `ParseClusterString` already rejects mixed-TLS
strings, `database.go:107-109`, so a mixed-TLS forward simply fails to parse and is not adopted; see
§6). Equality is defined as `a.String() == b.String()` — the serialization is the single source of
truth (matches C++ `upToDate`'s `toString()` compare), no separate field comparator that can drift.

### 5. Wire it into bootstrap + topology refresh, with a bounded forward chain

- **`refreshTopology`** (`topology.go:70`):
  - success with `Forward != ""` → `followForward(snap, fwd)`.
  - error (all coordinators failed) → `db.connRecord.adoptStoredIfChanged()`; if it changed,
    `kickTopology()` (Path B).
- **`bootstrap`** (`database.go:455`): same forward-follow inside the connect loop.
- **`followForward(old, fwd)`**: parse `fwd`; **reject** (log, no adopt, no persist) if it fails to
  parse OR has 0 coordinators (port C++ `ASSERT getNumberOfCoordinators() > 0`, `:946`); if it equals
  the current set (`old.String()==new.String()`, a degenerate self-forward) → ignore; otherwise
  `setAndPersist(new)` + `kickTopology()`.
- **Bounded forward chain (Go-only divergence — see Non-goals).** C++ follows forwards with no hop
  bound, relying on actor fair-scheduling; a Go tight loop would hot-spin on a pathological A→B→A
  forward cycle (the success-forward path re-polls immediately). We add a single `db.forwardHops int`
  field, **written only by the active follow path** — `bootstrap` first, then *exclusively* the
  single topology-monitor goroutine (the two never run concurrently, §1) — **reset to 0 immediately
  after a non-forward `applyDBInfo` returns true**, incremented on each forward-follow; past
  `maxForwardHops = 10` we stop following and let the steady 5s tick / bootstrap backoff retry,
  bounding the spin (a legitimate long rotation chain still progresses because the reset fires on each
  successful non-forward connect). The self-forward guard above is a cheap fast-path; the hop counter
  is the real bound.

### 6. `atomicReplace` (port `flow` `atomicReplace`)

Temp-write in the target's directory → `fsync` → `os.Rename` → `fsync` the directory. Content
byte-identical to C++: the two `#` header lines + `cs.toString()` + `\n`. Any I/O error (incl. an
unwritable directory) is returned to `setAndPersist`, which logs + swallows it (best-effort).

### 7. Remove the dead `ClusterFile.InternalKey` field

`InternalKey` (`database.go:27`) is **write-only everywhere** — set by 7 test/bench files but **never
read** anywhere in `pkg/fdbgo` (25 references across 8 files, zero reads; `buildOpenDatabaseCoordRequest`
uses `Description:ID`, `coordinator.go:42`). Leaving a second, ignored source of the cluster key next
to `String()`'s `Description:ID` reconstruction invites a future divergence. Delete the field + all 24
assignment sites.

### 8. Tighten `ParseClusterString` to C++'s acceptance set (gate for "reject, never corrupt")

The whole "never write lossy bytes to the shared file" guarantee rests on `ParseClusterString`
rejecting anything it can't faithfully re-emit. But the current per-token validation
(`net.SplitHostPort`, `database.go:91`) is **laxer than C++**, which accepts a token iff
`Hostname::isHostname(tok) || NetworkAddress::parse(tok)` (`MonitorLeader.actor.cpp:106-118`). C++'s
`isHostname` (`flow/Hostname.actor.cpp:31-44`) is `!ipv4Validation && validation`:

```
validation     = ^([\w\-]+\.?)+:([\d]+){1,}(:tls)?$
ipv4Validation = ^([\d]{1,3}\.?){4,}:([\d]+){1,}(:tls)?$
```

So C++ **rejects** tokens the Go parser currently accepts: `foo:abc` (non-numeric port — fails both
regexes and `NetworkAddress::parse`), `:1234` (empty host), `host name:4500` (space),
`1.2.3.4.5:4500` (`ipv4Validation` matches so `isHostname` is false, and `NetworkAddress::parse`'s
`sscanf("%d.%d.%d.%d:%d%n")` leaves 5 octets so `count != len` → reject). With the new write path
(§6), such a token in a forward/file would be persisted back to the shared file in a form C++/Java
cannot parse — defeating §4's protection. **Fix:** a token is valid iff
`isHostnameToken(tok) || isNetworkAddressToken(tok)`:

- `isHostnameToken` = `!ipv4Validation.MatchString(t) && validation.MatchString(t)` (the two regexes
  above; Go `\w` == ECMAScript `\w`).
- `isNetworkAddressToken` = strip `:tls`, `net.SplitHostPort`, `net.ParseIP(host) != nil` (handles
  IPv4 + bracketed IPv6), and the port is all digits.

This same classification drives `String()`'s coord-vs-hostname ordering (§4): IP token ⇒ coord,
hostname token ⇒ hostname. **One deliberate, documented tightening over C++:** C++'s
`NetworkAddress::parse` is *lax* — its `sscanf`/`std::stoi` **accept-and-truncate** out-of-range octets
and trailing-junk ports (`999.999.999.999:4500` → C++ accepts, silently → `234.234.234.231`;
`[::1]:45x` → port 45). `net.ParseIP` + numeric-port instead *reject* these. This is the **safe**
divergence direction: Go-accept is a strict subset of C++-accept (a 5M-token differential found zero
Go-accepts-but-C++-rejects), so the write path can never persist a token C++ can't parse; and the
over-rejections are **unreachable on any real path** because every forward/file string is produced by
some `toString()`, which always emits normalized octets 0-255. Documented in DIVERGENCES.md alongside
the bounded-forward-chain divergence — *not* claimed as "matches C++". Tightening cannot break a real
(C++-valid, `toString`-normalized) cluster file. Pinned by parse tests.

## Executable spec (what the tests prove)

1. **Forward parse** (unit): crafted `ErrorOr<ClientDBInfo>` with `HasForward` → `parseCoordinatorResponse`
   returns `info.Forward == "<connstr>"`. Revert-proven.
2. **`String()` == C++ `toString` (golden)** (unit): literal expected strings for all-IP, all-hostname,
   **mixed IP+hostname (asserts IPs-before-hostnames reordering)**, plaintext, and `:tls`. This — not a
   Go-internal self-round-trip — proves cross-tool readability. Plus a `ParseClusterString(cf.String())`
   fuzz round-trip for stability.
3. **`setAndPersist` format + best-effort** (unit): adopting rewrites the file to the exact C++ byte
   layout (header + `key@coords\n`), `ParseClusterFile` reads back the new coordinators; a persist to a
   read-only dir logs + swallows and **still swaps in memory** (revert-proven: the in-memory set changes
   regardless of the write outcome).
4. **`adoptStoredIfChanged`** (unit): external file rewrite → swaps in-memory + returns true; identical
   file → false, no swap; empty/garbage file → false (no adopt). Single-lock atomicity.
5. **Empty/garbage forward rejected** (unit): `followForward` with a 0-coordinator or unparseable
   forward does NOT adopt or persist (file untouched), falls through to backoff.
6. **End-to-end forward follow (deterministic fault injection)**: a fake coordinator dialer
   (`fault_test.go` style) answers the first `OpenDatabaseCoordRequest` with a `forward` reply pointing
   at a second fake coordinator, which answers with normal proxies. Assert the client adopts the new set
   (and persists it) and ends up with the second coordinator's topology — the strand is fixed.
   Revert-proven (disable forward-follow → client stays stranded on the dead first coordinator).
7. **Forward-hop bound** (unit/deterministic): a coordinator that forwards in an A→B→A cycle does not
   hot-spin — the hop counter caps the chain and the client backs off.
8. **`ParseClusterString` acceptance set** (unit): two groups. (a) *Matches C++ reject* — `foo:abc`,
   `:1234`, `host name:4500`, `1.2.3.4.5:4500`. (b) *Go-stricter, safe tightening* (C++ accepts +
   truncates these; Go rejects; unreachable on real `toString`-normalized inputs) — `999.999.999.999:4500`,
   `256.1.1.1:4500`. Accepts valid IPv4, bracketed IPv6, hostname, and `:tls` forms. The test labels
   the two groups distinctly so a future reader doesn't "fix" Go to match C++'s lax truncation.

## Non-goals / deliberate divergences (documented in DIVERGENCES.md)

- **No inotify/filesystem watcher.** C++ polls `upToDate` per monitor iteration; our 5s poll + the
  all-failed re-read is the faithful analog (not a correctness gap).
- **Bounded forward chain is a Go-only extension.** C++ has no hop bound (actor scheduling paces it);
  Go's tight loop needs one to avoid hot-spinning a pathological forward cycle. Documented as a
  deliberate divergence, pinned by test 7.
- **Mixed-TLS connection strings are not adopted.** `ParseClusterString` already rejects them
  (`database.go:107-109`); a mixed-TLS forward/file fails to parse → logged, not followed. We never
  write lossy/incorrect bytes to the shared file. (Uniform TLS — the real-cluster case — round-trips
  faithfully.) `db.tlsConfig` is resolved at open and reused; no TLS↔plaintext flip mid-life.
  **DIVERGENCES.md note:** a mixed-TLS forward (e.g. a cluster mid-migration between TLS and plaintext
  coordinators) ⇒ Go *declines to follow* and stays on steady retry, where C++ would follow it.
  Acceptable: mixed-TLS is transient/rare, and writing C++-unparseable bytes to the shared file is
  worse than declining.
- **Leader-election monitor forward path** (`monitorLeaderOneGeneration`) is N/A — the Go client uses
  only `OpenDatabaseCoordRequest`.
