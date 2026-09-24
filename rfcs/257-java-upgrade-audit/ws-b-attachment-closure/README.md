# WS-B initialization, reload and cache coherence correction

Tracking: TODO.md, “WS-B initialization/publication correction”.
Previous review: Git tree `6ffe8d3b5b541bdd1c668c6e13e45ebe475292a3`.
All four actual correction reviews returned **NAK**. Their complete prompts,
outputs and launch logs are retained here as `coherence-*`. The launch logs
record gpt-6-astra / xhigh. The prior point-write and range-clear ordering
findings were accepted as resolved; initial state attachment was not.

## Reproductions and correction

An unchanged-metadata Open loaded its local state without registering the
context view. A subsequent context clear followed by the first getter could
publish the pre-clear DISABLED map. MarkIndexDisabled then incorrectly returned
false without restoring the state key; maintenance could skip an index that a
cold transaction would consider READABLE. A load already in flight could publish
the same obsolete map. Three real-FDB specs reproduced this before the fix
(`attachment-red.log`): opened handle, in-flight Open, and in-flight lazy Build.
The range-result decorator materializes the real FDB snapshot, then pauses its
return. It does not fabricate results. TryLock forces the old implementation's
clear-before-publication ordering without scheduler sleeps.

Open now holds the context registry lock across loading (including cache Get)
and binding. Lazy initialization does the same. The explicit assume-readable
builder fast path is preserved; already-bound handles do not reassign the view.

DFS found and pinned two further defects:

* Explicit ReloadRecordStoreState updated only the handle-local map, not the
  common authority (`reload-red.log`). Reload now holds registry → stateMu →
  view.mu across reading and publication, retaining established read conflicts.
  It publishes to all bound handles and invalidates cached state if the loaded
  index map changed. A deterministic reload-versus-clear case also exercises
  this read/publication boundary.
* Clearing state could leave a warmed shared cache advertising DISABLED after
  commit (`attachment-cache-red.log`). Clears overlapping a registered index
  state subspace mark context state dirty and bump the metadata stamp. Clear
  ranges are retained in the context so a later store load also bypasses a
  pre-clear cache entry and invalidates the shared stamp. The two cache tests
  cover clear before and after Open, prove actual cache admission, save a record
  after the clear, and verify raw state and a populated index scan after commit.
  Ordinary record-range clears are negative controls: no metadata invalidation.

Lock order: registry → per-handle stateMu → common view.mu for reload;
registry → per-handle stateMu for initial Open binding; ordinary setters finish
initialization before taking stateMu → view.mu. Clears hold registry and common
views; no path in this correction acquires registry while holding a view.

Java 4.14.2.0 FDBRecordStore checkVersion installs the loaded state before header
reconciliation; preloadRecordStoreStateAsync publishes after the combined read;
markIndexNotReadable reads actual state before deciding a transition is redundant;
updateIndexState couples state publication with dirty-state/cache invalidation.
The context-wide authority is the approved Go mechanism for keeping multiple
handles coherent, so its read-to-publication boundary must be common as well.

## Verification

Source Git tree **`21c07c49f28fd0be9acf54fa5eee56fb3384f963`** (not a commit).
The **6812 tracked/untracked nonignored files** in `attachment-tree-freeze.json`
were verified unchanged after both full runs and all four stress runs.
Documentation/evidence booking follows that source freeze.

* **93/93 targets executed uncached and passed** (`attachment-full-uncached.log`).
* **14/14 targets executed with `--@rules_go//go/config:race` and passed**:
  client, fdb, recordlayer, chaos, conformance, cascades/... and executor.
  This is not the complete PR relational race set and not actual CI.
* Retained race logs show recordlayer **3409/3410** Ginkgo specs and conformance
  **1391/1510**. Existing opt-in/target exclusions unchanged; no new skips.
* **10 focused specs passed**: the three prior lifecycle tests plus seven new
  initialization/reload/cache cases (`attachment-green-final.log`).
* Three compiled, applied and restored mutations were killed: omit pre-load clear
  history, omit post-clear metadata-stamp invalidation, omit shared reload
  publication. Driver, JSON and full logs retained; build errors are not kills.
* Pinned formatter, just gazelle, bazelisk mod tidy and git diff --check passed.
  An earlier just test ran 42 targets with 51 cached, before the reload/cache
  corrections; the uncached result above is the final-source verification.

## Fresh stress comparison

Baseline commit **`e48f5b4965543cd4d99b5578356059e12d969c7c`**, true merge-base;
current source tree **`21c07c49f28fd0be9acf54fa5eee56fb3384f963`**.
Sequential, same-filesystem, n=2 on each side. Each run has **24 RUN/PASS**;
all **26 parsed row-count output records** agree across the four logs; zero
reported transaction retries. Full logs include load averages.

| Population | Baseline seconds, n=2 | Current seconds, n=2 | Mean ratio |
|---|---|---|---|
| 100k customers | 6.629920 / 6.667944 | 7.017619 / 6.954991 | 1.0507x |
| 1M orders | 147.014639 / 146.930949 | 151.014568 / 151.468437 | 1.0290x |
| Whole stress test | 181.37 / 174.15 | 178.46 / 182.29 | — |

These supersede the preceding tree's 1.0629x/1.0355x for decisions about this
source. They establish neither parity nor a causal explanation of the residual.
ORDER BY PK timings vary in both baseline and current logs; the whole-test
samples are not presented as a stable latency estimate.

## Gate status

Actual final correction confirmations are still required. Tests do not turn the
retained four NAKs into ACKs. WS-C–K and upgrade-wide verification/CI remain
unfinished. HEAD remains `71ccd8cf8b3fd0dbafe283e91171818e36af555e`;
no commit, push, merge or PR-state change authorized or performed.

## Subsequent correction review

All four actual reviews of tree `dd0956b11e901609edcd5f19d36ef64f04482b8a`
returned NAK. Initial binding was accepted; reload locking, clear-only commit,
noncacheable deletion policy and exported snapshot coherence required further
correction. Complete verdicts and replacement tree-specific evidence are in
[clear policy and lock-order correction](../ws-b-clear-policy-closure/README.md).
The measurements and superseded mechanisms above remain historical, not current
acceptance or a description of the later source.
