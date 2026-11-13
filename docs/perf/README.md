# Performance notes

Design notes, draft RFCs and the benchmark backlog behind the performance work.
Measured results live under [`docs/benchmarks/`](../benchmarks/README.md), one
folder per campaign; campaign tooling under
[`tools/benchcampaign/`](../../tools/benchcampaign/README.md). The CI remote-execution
design note is [docs/ci/rbe-design.md](../ci/rbe-design.md).

These are historical working documents. Each states the tree it was written
against; none is a claim about current master.

| Document | Content |
|---|---|
| [next-bench-backlog.md](next-bench-backlog.md) | Workloads requested for the next campaign (mass multi-tenant). |
| [notes/flatmap-n1-A-client-design.md](notes/flatmap-n1-A-client-design.md) | Eager first range batch and the commit `reading` barrier, against C++ 7.3.77. |
| [notes/flatmap-pipelining-design.md](notes/flatmap-pipelining-design.md) | FlatMap / nested-loop inner pipelining, Java vs Go. |
| [drafts/client-read-context.md](drafts/client-read-context.md) | Draft RFC: allocation-free read cancellation context. Rejected by the owner. |
| [drafts/leader-follower-read.md](drafts/leader-follower-read.md) | Draft RFC: reply delivery without a reader handoff (post-launch). |
| [patches/write-heavy-bench.patch](patches/write-heavy-bench.patch) | Proposed write-heavy range benchmark, not merged. |

## Known corrections

The documents above are kept byte-for-byte as written. Reviews found:

- [drafts/client-read-context.md](drafts/client-read-context.md):96-97: the
  cancellation citation should be `flow/include/flow/ThreadHelper.actor.h:443-454,816-831`.
- [drafts/leader-follower-read.md](drafts/leader-follower-read.md):27-38 (rejected
  draft) overstates unconditional wakeups; Net2 sleeps and wakes conditionally
  (`flow/Net2.actor.cpp:1550-1565,1871-1877`).
- [notes/flatmap-n1-A-client-design.md](notes/flatmap-n1-A-client-design.md):15,43-44:
  the early Advance-based batching description is superseded by §C (lines 49-63).

## Paths in imported documents

Documents and scripts imported from the controller's working directories keep
their original paths with these placeholders:

| Placeholder | Meaning |
|---|---|
| `${FDB_PERF_HOME}` | The controller directory holding campaign state, raw results and archives (outside Git). |
| `${FDB_PERF_STATE}` | The controller directory holding per-campaign SSH configuration (outside Git). |
| `${FDB_GO_WORKTREES}` | The directory holding the repository's worktrees. |
| `<vm-public-ip>`, `<server-id>`, `<action-id>`, `<campaign-resource-id>` | Cloud addresses and resource IDs, removed. |
| `<existing-…>` | Names, IDs and address ranges of unrelated cloud resources a campaign had to protect, removed. |
| `NONCE` | The random suffix of a campaign's resource label. |

The 2026-10-09 campaign directory, whose name carried a time suffix also used
for cloud resource names, is written `fdb-perf-20261009`. [IMPORT-PROVENANCE.tsv](IMPORT-PROVENANCE.tsv) lists every
imported file with its source, source and in-tree sha256 at import time, and the
substitutions applied; `byte-identical` marks an unchanged copy. A file edited after
its import no longer matches its in-tree sha256.
