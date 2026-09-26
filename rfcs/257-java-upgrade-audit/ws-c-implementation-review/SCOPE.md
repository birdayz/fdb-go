# WS-C completed implementation review scope

Review the complete WS-C implementation against the immutable accepted design
`../ws-c-design.md` and Java 4.14.2.0, not just the most recent increment.
The exact base/final tree IDs will be supplied in each review prompt.

Implemented surfaces: UUID heartbeats and legacy-key refusal; context-aware split
writer; generic pending queue/envelopes/cursor; queue-capable HNSW and sliding
replay; shared maintenance gates; raw and buffered queue emptiness; state-4
producer dispatch and overflow/clear/delete callbacks; fresh/resumed target
policy; single-owner transactional drain; session/stamp/block validation;
record read conflicts and claimed-range rollback; mutual/preset lifecycle;
readable closeout retries; deferred-maintenance control and merger driver;
normal format-15 opening with default creation retained at 14.

Read evidence READMEs in the sibling ws-c-* directories. They scope measurements
and distinguish earlier red/green increments from final-tree evidence. Latest
full local run: format15-full.log, 93/93, 42 executed / 51 cached. Latest scoped
race: 147 recordlayer specs plus format unit tests, one live JVM format15 spec.
Earlier race runs cover queue cursor/split/write-gate/interop dimensions. Full
logs live in `/var/tmp/fdb-upgrade-recovery/ws-c/` as referenced by each README.

Boundaries explicitly retained by the accepted design: SPFresh queue support is
false and its algorithms/lifecycle are unchanged. HNSW merge has no deferred work.
GuardiANN backend integration is WS-D; Lucene is an absent backend, NOT Go TEXT,
and remains an upgrade-wide open requirement. Generic driver tests do not prove
those backends consume callbacks in actual child transactions. Approved scalar,
raw-array, nullable-array, sorting extensions remain. C++ stays 7.3.77.

No overall migration completion, CI-green, merge-ready, or implementation ACK is
claimed before actual reviews. No commit/push/PR changes are authorized. Review
read-only; do not modify files, stage, commit, publish, or trigger CI. Report
NAK with actionable file/line findings if this scope is not acceptance-ready.
