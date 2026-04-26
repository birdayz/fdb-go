# RFC 026: FDB gRPC Trajectory — Implications for Our Go Wire Client

## Status: INFORMATIONAL (research notes; no action required today)

## Why this exists

apple/foundationdb merged a non-trivial body of gRPC code starting Dec 2024
(PR [#11782](https://github.com/apple/foundationdb/pull/11782)). This RFC
documents what the gRPC work actually IS, what it ISN'T, and what would
force us to revisit our pure-Go FlowTransport client port.

This was prompted by surface-level signals that looked like FDB might be
migrating its wire protocol — which would invalidate `pkg/fdbgo/client`'s
entire bet. The detailed reading below answers: **no, it isn't, and our
Go client is not at risk in any visible 2025/2026 trajectory.**

## TL;DR

- FDB's data plane (Get / Set / Commit / GetReadVersion / GetRange /
  watches / atomic ops) is **still FlowTransport**, the same wire
  protocol our Go client implements. It is being actively maintained
  and refactored as recently as April 2026.
- The gRPC work that landed in 2024+ is **scoped to the control plane**:
  cluster admin (Exclude / Include / Configure / GetStatus), worker
  lifecycle plumbing, and a file-transfer service for backup/bulk-load.
  It is exposed via a NEW `fdbctl/` library, not the regular client API.
- The 2019 RFC (issue [#2302](https://github.com/apple/foundationdb/issues/2302))
  that imagined a gRPC gateway for transactional ops was officially
  abandoned in 2021 by an FDB maintainer
  ([@sfc-gh-mpilman comment](https://github.com/apple/foundationdb/issues/2302#issuecomment-887334466)),
  with no replacement RFC since. The 2024+ gRPC work is a different,
  smaller-scoped effort.
- Our `pkg/fdbgo/client` Go port targets FlowTransport. Its Java and
  Python interop story is unchanged. There is nothing on FDB's
  published roadmap that would force a Go-side gRPC port.

## Background — the abandoned 2019 RFC

[Issue #2302 "Provide a more standard RPC client interface"](https://github.com/apple/foundationdb/issues/2302)
was opened October 2019. The wiki RFC at
[FoundationDB-RPC-Layer-Requirements](https://github.com/apple/foundationdb/wiki/FoundationDB-RPC-Layer-Requirements)
described a gateway server that would:

- Wrap the C client and expose **transactional operations** (reads,
  range queries, commits) over a stable gRPC API.
- Free non-C-FFI callers from version-handshake complexity.
- Possibly evolve into a separate `fdbserver` role with built-in
  discovery and load balancing.

This RFC explicitly proposed putting the **data plane** behind gRPC. It
was the most ambitious framing.

The decisive update from 2021 (Markus Pilman, Snowflake-FDB engineer):

> **"We don't have any plans on finishing the gRPC service — not because
> we don't like the idea but because we don't have anyone working on it.
> We are planning to implement a proxy-service, but it will still
> require you to use the C client (the main motivation here is
> performance related)."**

He recommended community gRPC bridges
([Lionrock](https://github.com/panghy/lionrock)) for callers wanting that
shape. Issue #2302 has had no follow-up activity since.

**No replacement RFC for client-facing gRPC has appeared.** No
`design/` doc, no `documentation/` markdown, no GitHub Discussion
proposes the data plane on gRPC.

## The 2024+ gRPC work — what actually merged

PR [#11782 "Add gRPC support to FDB"](https://github.com/apple/foundationdb/pull/11782)
landed Dec 11, 2024. The PR description is unambiguous:

> "This PR gRPC server along with Flow with goal of supporting
> new/specific features but **not entirely replacing FlowRPC itself**."

Subsequent commits (Jan 2025 — April 2026) refine the gRPC integration
without touching the data plane. Highlights:

- PR #11892 — gRPC file transfer service (bulk-load / backup blob
  movement between processes).
- PR #11984 — gRPC service registration on worker roles.
- PR #12005, #12023 — TLS for the gRPC server.
- PR #12533, #12555, #12603 — `fdbctl/` library + `ControlService`
  proto with administrative RPCs.

The proto file lives at `fdbctl/protos/control_service.proto`. There is
**no `transaction.proto`, `commit.proto`, `storage.proto`, `read.proto`,
or anything resembling a transactional client API**. The entire
namespace is `fdbctl`.

## Exact RPC surface (as of April 2026)

Verbatim summary of `fdbctl/protos/control_service.proto`:

```proto
service ControlService {
    rpc GetCoordinators(GetCoordinatorsRequest)        returns (GetCoordinatorsReply);
    rpc ChangeCoordinators(ChangeCoordinatorsRequest)  returns (ChangeCoordinatorsReply);
    rpc ConfigureAutoSuggest(ConfigureAutoSuggestRequest) returns (ConfigureAutoSuggestReply);
    rpc Configure(ConfigureRequest)                    returns (ConfigureReply);
    rpc GetStatus(GetStatusRequest)                    returns (GetStatusReply);
    rpc GetWorkers(GetWorkersRequest)                  returns (GetWorkersReply);
    rpc Include(IncludeRequest)                        returns (IncludeReply);
    rpc Exclude(ExcludeRequest)                        returns (ExcludeReply);
    rpc ExcludeStatus(ExcludeStatusRequest)            returns (ExcludeStatusReply);
    rpc Kill(KillRequest)                              returns (KillReply);
    rpc Maintenance(MaintenanceRequest)                returns (MaintenanceReply);
}
```

Every one of these is an `fdbcli` admin command translated to gRPC. The
`Configure` RPC carries cluster-level redundancy / storage-engine /
role-count knobs; `GetStatus` returns the same JSON document
`fdbcli status json` already produces; `Exclude` / `Include` / `Kill`
manage worker lifecycle; `Maintenance` toggles a zone-pin for hardware
swaps.

There is nothing here a typical application would call.

## What's NOT here

- No `Transaction` message or `Commit` RPC.
- No streaming for range reads.
- No cluster-file-resolution or coordinator-discovery handshake (those
  remain a client-library concern; the gRPC server uses them as a
  CLIENT internally, behind the proxy).
- No watch / event subscription.
- No client-version handshake. The proto uses bare protobuf3 — no
  embedded version negotiation, no compatibility flags.
- No "proxy-mode" entry point — the 2021 abandoned plan was a proxy
  gateway; this is purely admin-side.

## Concurrent activity on FlowTransport

While gRPC was being added (2024-2026), FlowTransport itself has been
under active maintenance:

- April 2026 PR #12971: "Convert several FlowTransport.actor.cpp
  actors to standard coroutines."
- April 2026 PR #13032: same conversion work, follow-up.
- Various related cleanups across `fdbrpc/`.

Apple is investing in FlowTransport's maintainability — directly
opposite of "we're sunsetting it."

## What this means for `pkg/fdbgo/client`

Our Go FlowTransport port:

- Implements the FDB wire protocol (`ConnectPacket` handshake, token-
  routed RPCs, server-side serialization compatible with C++ Flow's
  `ObjectSerializer`).
- Targets the data plane the C client uses. Apps using our client
  speak the same wire protocol as apps using `libfdb_c`.

The 2024+ gRPC work does NOT touch any of this:

- The gRPC server runs ALONGSIDE the FDB processes. It is an OPTIONAL
  side-channel for operators / control-plane integrations.
- The gRPC server is itself a CLIENT of FlowRPC — internally it speaks
  FlowTransport to the cluster, then translates results to gRPC for
  the operator.
- A Go application using `pkg/fdbgo/client` keeps speaking
  FlowTransport directly to the cluster. No path through the gRPC
  server.

**Concrete: nothing in `pkg/fdbgo/client/` needs to change for the
2024+ gRPC work to land. Our binding tester (which compares us
against the C client's wire output) stays the canonical proof of
correctness.**

## Watch criteria — when to revisit

We should re-open this RFC if any of these signals appear in
apple/foundationdb:

1. **A `transaction.proto` or `client_service.proto` lands in
   `fdbrpc/protos/` or `fdbserver/protos/`.** Today the only proto is
   `fdbctl/protos/control_service.proto`.
2. **A PR titled along the lines of "Add transactional RPC service" or
   "Migrate client connections to gRPC".** No such PR exists today.
3. **Issue #2302 reactivates with a concrete plan**, or a new issue
   supersedes it with a roadmap.
4. **The C client (`libfdb_c`) gains a gRPC-only configuration
   option** (e.g. `fdb_select_api_version()` accepts a gRPC URL). This
   would signal client-facing gRPC adoption.
5. **FlowTransport is publicly deprecated** in a release-notes entry
   or a `documentation/` markdown.

None of these signals exist as of 2026-04-26. Should they appear, the
follow-up work is bounded and visible:

- A second wire transport in `pkg/fdbgo/client/` that speaks gRPC
  alongside the existing FlowTransport implementation.
- A version-negotiation step at connection time so the client picks
  the right transport per cluster version.
- The `pkg/fdbgo/wire/types/` generator (`cmd/fdb-schema-extract`)
  remains useful since the on-the-wire data shapes are unchanged —
  only the framing/RPC layer changes.

## Open question (for posterity)

The 2019 RFC's strongest argument was **client-version-handshake
complexity**: every C-binding consumer had to implement the
`select_api_version` dance to keep wire compatibility. We dodged that
in the Go port because we control both ends — our wire types are
generated from C++ headers via `cmd/fdb-schema-extract`, so we
upgrade with FDB.

If FDB ever DOES ship client-facing gRPC, our port has two choices:

1. **Stay on FlowTransport** — same as `libfdb_c` users do today; no
   advantage lost.
2. **Add a gRPC client** — only worth it if gRPC offers features
   FlowTransport can't (multi-cluster fan-out, federation, etc.). We
   cross that bridge if we get there; today there's nothing forcing
   the decision.

## References

- Issue #2302 — https://github.com/apple/foundationdb/issues/2302
- Wiki RFC (2019) — https://github.com/apple/foundationdb/wiki/FoundationDB-RPC-Layer-Requirements
- Forum thread (2019) — https://forums.foundationdb.org/t/rpc-layer-requirements-and-design/1817
- PR #11782 (gRPC support landed) — https://github.com/apple/foundationdb/pull/11782
- PR #11892 (file transfer) — https://github.com/apple/foundationdb/pull/11892
- PR #12603 (Exclude in gRPC) — https://github.com/apple/foundationdb/pull/12603
- PR #12971, #13032 (FlowTransport coroutine conversion, April 2026)
- Lionrock — https://github.com/panghy/lionrock — community gRPC bridge
- The `control_service.proto` file — `fdbctl/protos/control_service.proto`
  on `apple/foundationdb` `main`.
