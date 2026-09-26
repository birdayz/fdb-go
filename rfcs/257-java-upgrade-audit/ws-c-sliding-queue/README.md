# WS-C sliding pending replay interoperability

Tracking: TODO.md, “WS-C sliding pending replay interoperability”. This advances
../ws-c-maintainer-queue/README.md; it is not completed WS-C or an implementation ACK.

Two retained live-JVM specs in conformance/sliding_window_index_conformance_test.go
exercise six replay steps each, with Java-produced bytes replayed in Go and
Go-produced bytes replayed in Java. Every step compares complete Any bytes,
checks graph membership through BOTH engines, and pins raw count/boundary tuples.
The window is unpartitioned ASC, size two; this is not DESC or partition coverage.

Sequence: better insertion evicts the boundary; duplicate insertion is idempotent;
deleting that insertion re-elects a persisted overflow record; an equal-price
larger-PK entry stays in overflow; deleting a member promotes a missing source
entry (Java advances count/boundary without inserting a graph node); deleting that
missing entry promotes a persisted overflow record whose vector changed since
initial insertion. The final Java graph vector bytes and Go search distance pin
use of the CURRENT source vector for indirect promotion, not a historical one.
The queued directly inserted records have no persisted source record.

Source authority: SlidingWindowIndexMaintainer.serializePendingWriteQueue,
updateFromQueue, updateWindowWhileWriteOnly, handleInsert, handleDelete and
reElectFromOverflow in Java 4.14.2.0. VectorIndexMaintainer.toIndexEntry returns
vector bytes in the value tuple, not a distance: the first extension of the oracle
incorrectly cast these bytes to Number and failed both selected specs with a
ClassCastException. The oracle was corrected to assert actual vector bytes;
the retained final cases exercise that result field. No production defect or
assertion relaxation was involved. Initial red log retained below.

Evidence under /var/tmp/fdb-upgrade-recovery/ws-c/:
- sliding-queue-oracle-red.log: incorrect JVM result-type assumption, two failures.
- sliding-queue-jvm.log: 2 / 1519 selected specs pass; twelve SLIDING-QUEUE lines.
- sliding-queue-race.log: same two final specs pass with actual rules_go race mode.
- sliding-queue-final-full.log: final full-suite verification (including this booking).
Two source hashes are retained in source.sha256 for post-run verification.

Queued state, store dispatch/readability, eligibility/policy, session/drain/cleanup,
completed implementation reviews and WS-D–K remain open. No publication or
operational changes performed.
