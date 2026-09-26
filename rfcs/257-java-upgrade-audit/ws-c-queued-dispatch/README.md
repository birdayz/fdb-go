# WS-C queued store dispatch and checked state setters

Tracking: TODO.md, “WS-C queued store dispatch and checked state setters”. Design
and deviation authority: ../ws-c-design.md. Not completed WS-C or a gate ACK.

State 4 decodes as WRITE_ONLY_WITH_QUEUE. Write-only family predicates include it;
explicit ordinary/queued predicates remain distinct. Audited production state
consumers under pkg: generic/rank/bitmap maintenance, build-state progress,
GetWriteOnlyIndexes, metadata rebuild dispatch, and explicit ordinary setters.
New checked MarkIndexWriteOnlyWithQueue/ClearAndMarkIndexWriteOnlyWithQueue refuse
format <15, unsupported maintainers and version columns before mutations. They
share existing transition locks and readable->write-only built coverage logic.

Single/batch writer dispatch serializes computed entries under its existing shared
maintenance read gate. DELETE_WHERE preserves preflight and queues each computed
index prefix. Builders/per-entry replay call maintainers directly. Context queue
options default to max100000 and disable-on-overflow=true (Java property values,
not Java's stale default-false prose). Overflow disables through a named commit
check after releasing writer locks; false propagates queue-full. Names are scoped
by store prefix and index, not just index name. Successful disable increments
Java's overflow-disabled counter. DeleteAllRecords cancels build-space buffered
mutations; DeleteStore cancels pending queue callback-family registrations.
Enqueue reads incarnation under the already-held state lock, avoiding recursive
RLock acquisition when a transition is waiting.

IMPORTANT: maximum-supported format remains 14, as does default creation. The
format15 fixtures explicitly persist headers and use the existing advanced Build
API; they do not claim ordinary Open supports 15 before the full lifecycle exists.

Thirteen real-FDB specs in new gazelle-enrolled index_queued_dispatch_test.go:
- Six deterministic capacity-read barriers, before buffering, for single/batch/
  DELETE_WHERE x one/two handles. TryLock proves shared exclusion; checked
  publication then observes the enqueued mutation and refuses.
- Mixed ordinary/queued indexes: single+batch+DELETE_WHERE, persisted ordered
  replay and checked readable publication; graph not maintained until replay.
- Format refusal and unsupported clear-and-mark preserve state/data.
- Version-column and SPFresh checked requests refuse without state mutation.
- Both overflow policies: abort or deferred disable; records, queue and timer
  assertions distinguish outcomes.
- DeleteAllRecords removes buffered entries; DeleteStore cancels an already
  registered overflow callback and the committed store stays empty.

Live JVM oracle in index_state_conformance.{java,_test.go} pins the accepted
safety deviation: Java queues an unsupported VALUE index at format14, while Go
refuses without mutations. Observed QUEUED-SETTER evidence line and exact states.

Evidence under /var/tmp/fdb-upgrade-recovery/ws-c/:
- queued-dispatch.log: 13 / 3488 selected recordlayer specs pass.
- queued-setter-jvm.log: 1 / 1520 selected conformance spec pass, evidence emitted.
- queued-dispatch-race.log: both targets uncached under actual rules_go race;
  13 recordlayer specs and 1 conformance spec pass.
- queued-dispatch-full.log: earlier five-spec dispatch tree, full93/93 with
  42 executed/51 cached, 746.608 seconds (not the final barrier/oracle tree).
- queued-dispatch-final-full.log: final full suite including booking.
Fifteen final source hashes are retained in source.sha256 for post-run checking.

Still open: fresh/resumed indexer target eligibility and mixed-follower validation,
mutual rejection, drain transaction/retry/continuation ownership, heartbeat/session
checks, all-exit cleanup, complete lifecycle coverage and final implementation
reviews. Ordinary format15 opening must wait for those. WS-D–K and actual CI
remain. No publication, PR changes or operational cleanup performed.
