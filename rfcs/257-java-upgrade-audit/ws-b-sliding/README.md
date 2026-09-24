# WS-B sliding-window insertion progress

WS-B design received Graefe/Torvalds/independent-storage ACKs at SHA256
699f084274f4acf98ebf40c9329a577bac952657e4c57f527f34ce5cb1b17390;
reports/prompts are retained in ../ws-b-design-review/. These are DESIGN ACKs,
not implementation acceptance. Workstream completion review remains outstanding.

Read complete Java SlidingWindowIndexMaintainer at pinned4.14.2.0 and Go class.
Implemented serializable tracked-entry-first insertion, exact missing-boundary
corruption, callback-separated bookkeeping for ordinary/write-only delegates,
removal of preemptive delete, and SW_REINSERT_ALREADY_TRACKED. Stored tuple layout
unchanged; existing size-one overflow correction retained. Queue/state4/format15
are not enabled.

33 new real-FDB specs failed before implementation; the full68 focused sliding
specs pass after implementation (Ginkgo filtering deselects other specs; those
are not runtime skips). Matrix covers both directions, ordinary/write-only
replay, four window sizes spanning partial/full/overflow, and partitions.
Each replay checks complete entry keys/values, count/boundary, graph membership,
operation counters, and a changed vector payload to prove the delegate actually
refreshes rather than only incrementing a timer. Corruption regression pins both
entry points. Existing tests remain, with obsolete preemptive-delete comments
corrected. Scoped Go-source sweep found the new metric at three lines and no
old metric/preemptive-delete explanation; historical audit descriptions retained.

Four applied/compiled/killed/restored mutations: tracked-entry gate, overflow
refresh gate, delegate refresh callback, missing-boundary check. Earlier formatting
and nilness build refusals receive no mutation credit; final mutant3 logs are the
credited population. Sources restored byte-for-byte in finally blocks.

just gazelle and bazelisk mod tidy completed; just test PASS,93 targets (39 executed,
54 cached),725.694s. This is NOT an uncached full-safety-net claim. Raw full output
/var/tmp/fdb-upgrade-recovery/ws-b/sliding-just-test.log. Final workstream uncached,
race, Java interoperability and review gates remain open. No commit/push/merge.
