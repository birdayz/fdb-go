# WS-B shared-state coherence correction

Tracking: TODO.md, “WS-B shared-state coherence correction”.
Previous actual delta verdicts: [ws-b-delta-review](../ws-b-delta-review/),
reviewed tree `c59322c012c53500679e7f345360fac6706468b9`.
Three WS-B reviewers returned NAK; the scoped SPFresh reviewer returned ACK.
The Codex launch log records actual gpt-6-astra/xhigh even though the SPFresh
reviewer correctly could not independently introspect its runtime identity.

## Findings and fixes

The previous delta review identified two new compositions missed by the green
suite. Both now have real-FDB red→green tests:

1. Former-index cleanup can write through a new handle before its first state
   getter attaches the context view. `setIndexState` now performs that attachment
   before locking and writing. The v1→v2→v3 regression starts with a nonempty
   store and disabled index, opens v1 to establish the shared view, drops the
   index in v2, then re-adds its name with a fresh subspace and DISABLED policy
   in v3, all in one context. It checks raw bytes, the older handle's view,
   committed DISABLED state, preserved record and cold-open scan rejection.
2. A per-handle lock did not serialize FDB state writes with shared publication.
   The common view mutex now covers both the transaction mutation and the
   shared-map update. The per-handle lock still protects the local header/map.
   Lock order is stateMu→view.mu; attachment completes before taking stateMu.

DFS found the same write/publication ordering in context range clearing. It now
holds the context view registry mutex and all registered view mutexes before
issuing the real transaction clear, then updates matching state entries before
unlocking. Registry serialization prevents concurrent clear operations from
acquiring multiple view locks in competing orders. Ordinary state writes hold
only their own view mutex and do not acquire the registry mutex while holding it.
State read conflicts survive range clearing.

Two deterministic ordering specs cover point state writes and range clearing.
A decorator delegates every operation to real FDB and pauses one completed
mutation. TryLock detects whether publication is protected; on the buggy path
it forces the second writer to finish before the first publishes its obsolete
value. Tests compare actual FDB bytes and both handles after completion. No
fabricated storage results or scheduler sleeps. Channel waits are bounded.
A compiled mutation moving the transaction range clear before locking killed
the range-clear spec, then was restored. The initial test build's []byte versus
fdb.Key type error is retained but not counted as a red test execution.

Tagged Java updateIndexState was reread: transaction mutation and cached state
publication belong inside the state-write section. Context sharing makes that
section common across Go handles, rather than only per handle.

## Verification

Frozen **6781 tracked/untracked nonignored files** were checked unchanged after
both whole-suite runs and four sequential stress runs. Source tree:
`a50bfc93d1d3d43a46949eba84b8a7a8ec05b626` (Git tree, not commit).
The evidence/doc booking follows the verified source freeze.

- **93/93 uncached targets executed and passed**.
- **14/14 instrumented race targets executed and passed**: client, fdb,
  recordlayer, chaos, conformance, cascades/... and executor. Not the PR
  relational race set and not an actual CI run.
- Recordlayer race: **3402/3403 Ginkgo**, **2110 Go RUN/PASS**; conformance
  **1391/1510 Ginkgo**, **70 Go RUN/PASS**; executor **1952 Go RUN/PASS**.
  Existing opt-in/target exclusions unchanged; no new skips.
- Three focused coherence specs pass; the preceding two-spec reproducer failed
  for both reported defects. Range-clear ordering mutation compiled and failed
  its actual one-spec test, not merely the build.
- Pinned formatter, just gazelle, bazelisk mod tidy, git diff --check passed.

## Fresh stress comparison

Both baseline and current were rerun after the coherence fix, sequentially on
the same filesystem, n=2 each. Baseline commit:
`e48f5b4965543cd4d99b5578356059e12d969c7c` (true merge-base); current source tree
`a50bfc93d1d3d43a46949eba84b8a7a8ec05b626`. All four runs have 24 RUN/PASS,
matching query row counts and zero transaction retries. Loads are retained.

| Population | Baseline seconds, n=2 | Current seconds, n=2 | Mean ratio |
|---|---|---|---|
| 100k customers | 6.506822 / 6.626023 | 6.960202 / 6.998901 | 1.0629x |
| 1M orders | 146.224801 / 146.527169 | 151.286793 / 151.849443 | 1.0355x |
| Whole stress test | 173.32 / 173.64 | 178.74 / 179.36 | — |

This supersedes the earlier 1.0195x/1.0548x measurements for decisions about this
source. The earlier numbers remain historical measurements of different trees.
No parity claim, no inference that host load bounds the residual, and no claim
that all residual cost is explained. The prior reviewers explicitly accepted
the preceding performance result as non-blocking; the new result is supplied
for their final confirmation, not silently treated as the old result.

## Gate status

Final confirmation of this correction is required. Prior NAKs are not converted
into ACKs by tests or this document. WS-C–K and upgrade-wide verification/CI
remain unfinished. No commit, push, merge or PR-state changes; published HEAD
remains `71ccd8cf8b3fd0dbafe283e91171818e36af555e`.

## Subsequent correction review

The actual review of tree `6ffe8d3b5b541bdd1c668c6e13e45ebe475292a3`
returned four NAKs for a newly identified initial-attachment gap. The results
above remain historical measurements of source `a50bfc93...`, not current
acceptance. See [initialization/reload/cache correction](../ws-b-attachment-closure/README.md)
for complete verdicts, new regressions and replacement tree-specific measurements.
