# WS-C final delta confirmation scope

Initial implementation candidate `2a4d33046f8640673643ea989709665f425cbbc5`
received four NAKs; original verdicts remain in `../ws-c-implementation-review/`.
Accepted WS-B base is tree `ba06f34ff8998f61040a491e4f3a3e8b8de7ccf9`.
The new exact tree is supplied in each prompt. These are TREE objects: use
`git diff OLD NEW`, not merge-base syntax. Read the full source delta and its
interactions with the accepted WS-C design and original implementation; this is
a delta confirmation, not a sampled review or a new review per fix commit.

Five fixes require confirmation:

1. Serializable all-target heartbeat admission precedes destructive metadata
   reconciliation and preparation. Legacy/malformed and incompatible active
   sessions preserve all store bytes on refusal, even if a caller commits it.
2. Independent terminal cleanup owns transaction/retry/commit lifetime and the
   remaining deadline. IMPORTANT DFS correction: cancelling a dispatched Go
   future may be ineffective. The final implementation polls IsReady with an
   owned ticker/context wait, and calls Get only after readiness. It neither
   blocks on Get nor spawns an abandoned Get goroutine. A regression wraps real
   dispatched FDB commits with readiness withheld and cancellation ineffective;
   reverting to blocking Get plus cancellation fails it. Backend-dispatched
   cleanup can finish later, clearing only the old session's UUID keys.
3. Java-compatible slash-plus-message-suffix validation at consumed nested Any
   boundaries: vector/sliding/delegate insert/delete/delete-where. Replay
   negatives retain entries/counters/index data; positive prefixes apply.
4. Mutual renewal writes only the own UUID after admission commits; rejected
   admission never publishes its token. Unadmitted/direct calls remain strict.
   Two overlapping disjoint real-FDB batches commit on their first attempts.
5. Drain/merge callbacks refresh and validate every still-owned target across
   sequential follow-up; already-published scannable targets are retired, not
   recreated. Deterministic-clock real-FDB tests exceed the lease and attempt
   permitted takeovers of both queued and ordinary followers. Blocking and
   disablement fail before committing current-target work.

Read `../ws-c-followup-liveness/README.md` and its linked per-finding evidence.
Latest final-source evidence in `/var/tmp/fdb-upgrade-recovery/ws-c/`:
`followup-uncached-full.log`: 93/93 executed and passed uncached;
`followup-final-race.log`: 183/3604 specs plus ordinary Go tests/seeds under
actual race instrumentation; `followup-final-jvm-race.log`: 9/1523 JVM specs,
including 12 asserted nested-Any results. The 35-file freeze manifest is NOT
changed-file population. All source hashes remained unchanged after these runs.

Boundaries: format maximum15, default14; approved raw/nullable-array/scalar/sort
extensions preserved; C++7.3.77. SPFresh queue-ineligible and algorithms/LIRE
unchanged; HNSW synchronous. Its merge callback tests establish wiring, NOT
future deferred child-transaction backend proof. GuardiANN is WS-D; absent
Lucene (not Go TEXT) remains upgrade-wide work. No overall migration completion,
CI-green, or merge-readiness claim.

Read only: no edits, tests, mutations, staging, commits, publication, CI triggers,
PR changes or operational cleanup. Actual gpt-6-astra/xhigh reviews required;
incomplete review is not ACK. Return ACK/NAK with exact reviewed tree and
file:line evidence, including any interaction finding. Do not inherit an ACK
from green tests or paper-only preservation.
