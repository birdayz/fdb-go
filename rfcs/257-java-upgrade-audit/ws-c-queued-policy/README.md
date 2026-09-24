# WS-C fresh/resumed queue target policy

Tracking: TODO.md, “WS-C fresh/resumed queue target policy”. Accepted design:
../ws-c-design.md. Not completed WS-C or an implementation gate ACK.

Read Java IndexingBase.handleStateAndDoBuildIndexAsync, markIndexesWriteOnly,
markSingleIndexWriteOnly, pendingWriteQueueRefusalReason and the mutual guard at
the start of setIndexingTypeOrThrow. OnlineIndexer.IndexingPolicy supplies an
empty requested-index set and 100 closeout attempts by default.

Go policy now explicitly requests target names and distinguishes nil/default100
from explicit zero closeout attempts. Fresh preparation chooses queue only for
requested, capable, non-versioned targets at format>=15 outside mutual mode;
others remain ordinary. Continuing builds retain their persisted states, reject
unequal follower states, validate every target stamp, and reconstruct the queued
set. The indexer's session set is published after the preparation transaction
commits, not from an uncommitted attempt. Mutual queued takeover is refused before
fresh-force-overwrite shortcuts, including direct per-index stamp entry.

Seven added real-FDB specs (20 total selected Queued store dispatch specs): four
fresh policy cases (requested capable, no request, format14, mutual), resumed
queue preservation under changed policy, mixed followers in either primary order,
and mutual rejection before overwrite. Existing capability/version/SPFresh tests
remain in the same suite. Format15 setup still uses the existing advanced Build
API; normal Open's ceiling is intentionally unchanged until lifecycle completion.

Live JVM oracle in index_state_conformance.{java,_test.go} invokes Java's actual
OnlineIndexer.buildIndex(false): both mixed primary orders reject with
ValidationException, and mutual queued takeover rejects with RecordCoreException,
with the exact pinned messages. It runs on a unique test-owned non-tenant prefix
because the indexer's runner creates independent contexts; finally clears only
that prefix. This is test fixture cleanup, not operational heartbeat cleanup.

Evidence under /var/tmp/fdb-upgrade-recovery/ws-c/:
- queued-policy.log: 20 / 3495 selected recordlayer specs pass.
- queued-policy-full.log: full93/93, 42 executed/51 cached, 748.006 seconds,
  before the added Java oracle (three Go source hashes checked).
- queued-policy-jvm.log: 1 / 1521 selected spec pass; three QUEUED-RESUME lines.
- queued-policy-race.log: actual rules_go race, both targets uncached; 20 selected
  recordlayer specs and 1 selected conformance spec pass.
- queued-policy-final-full.log: final full suite including oracle and booking.
Five final source hashes are retained in source.sha256.

Still open: drain retry/transaction ownership and commit-bound continuations,
heartbeat/session validation and all-exit cleanup, full lifecycle/format15 opening,
completed implementation gate reviews, WS-D–K and actual CI. No publication or PR
changes performed.
