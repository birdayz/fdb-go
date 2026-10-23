---
lens: spfresh
applies:
  - "pkg/recordlayer/vectorindex/spfresh_*"
  - "pkg/recordlayer/vectorindex/vector_index_*.go"
  - "pkg/recordlayer/vectorindex/hnsw_vector.go"
  - "pkg/recordlayer/vectorindex/hnsw_options.go"
  - "pkg/recordlayer/vectorindex/VECTOR_BENCHMARK_RESULTS.md"
  - "pkg/rabitq/**"
references:
  - "SPANN paper: .claude/skills/spfresh-reviewer/spann-paper.pdf (arXiv:2111.08566); SPFresh paper: https://arxiv.org/pdf/2410.14452"
  - "rfcs/094-spfresh-vector-index.md: the FDB adaptation and its declared divergences"
  - "pkg/recordlayer/vectorindex/VECTOR_BENCHMARK_RESULTS.md: current recall and latency numbers"
---
The SPFresh vector index follows the SPANN and SPFresh papers, adapted to FoundationDB in
RFC-094. A divergence from the papers that RFC-094 does not justify is a defect. Cite the
paper section and our file:line.

### spfresh/invariants — Posting-list invariants hold
Severity: high
Check: split and merge thresholds, posting size bounds and the hard envelope hold on every
path, including chunked drains across transactions, crashes between steps and concurrent
rebalancing; split is the only enlarging operation; no vector is lost or duplicated across
split, reassign and merge.
Verify: give the sequence of operations that breaks the invariant.
Not a finding: a transient violation inside one transaction that the commit resolves.

### spfresh/lire — LIRE protocol fidelity
Severity: high
Check: reassignment is limited to the neighbour posting area as SPFresh §3 defines it;
in-place append, split and merge follow the paper's protocol or RFC-094's declared FDB
adaptation; replication (closure, r, α) and query-aware pruning match SPANN §3 or the RFC.
Verify: cite the paper section and the code that differs.
Not a finding: a difference RFC-094 declares with a reason that still holds.

### spfresh/claims — Recall and latency claims are measured
Severity: medium
Check: a change that claims or could change recall or latency comes with a measurement
against VECTOR_BENCHMARK_RESULTS.md, or does not claim it; default changes are justified by
numbers.
Verify: name the claim and the missing measurement.
Not a finding: code changes with no effect on search or rebalancing.

### spfresh/routing — Two-level routing keeps the papers' latency model
Severity: medium
Check: SPANN navigates centroids with an in-memory graph; ours linear-scans the cached L1
cells and an LRU of per-cell L2 refreshed from the changelog. A change to routing, the L1/L2
caches or their refresh states the topology size at which the scan or the cache-miss rate
breaks the latency model, or stays within the sizes VECTOR_BENCHMARK_RESULTS.md measures.
Verify: name the topology size or the cache path and the measurement that is missing.
Not a finding: routing code whose cost does not grow with the topology.

### spfresh/stability — Recall stays flat over an update stream
Severity: high
Check: SPFresh §5.2: recall over an update stream stays flat. The churn soak and foreground
fill must show it for a change to append, split, reassign, merge or their scheduling. A
topology that quiesces with oversized or orphaned postings is a LIRE violation, not a tuning
gap.
Verify: point to the soak or fill result, or to the path that leaves a posting oversized or
orphaned at quiescence.
Not a finding: a temporary recall dip during a rebalance that recovers at quiescence.
