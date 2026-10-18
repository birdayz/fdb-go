# Commit allocations: owned conflict vector and compact GRV state

2026-10-18. Two allocation cuts on the `set100` commit path, measured together:

- **Conflict ranges** are sorted and merged in the commit's own vector, with typed
  sorting instead of reflection and no second clone (#951; ReadYourWrites.actor.cpp
  segments and NativeAPI.actor.cpp self-conflict injection unchanged).
- **The native commit's read version** keeps the shared GRV future and a compact request
  snapshot instead of a private copy of the whole `Transaction` and an incarnation.

Separately, each cut moved client CPU by less than its noise (`native-grv-compact`: 1.010
closed, 0.987 paced, against `7d0f13860`; #951's own screen: "essentially unchanged").

## Setup

As in the [writer-turn report](writer-turn-20261018.md): local three-process cluster,
clientbench on CPUs 0–7 (`GOMAXPROCS=8`, `GOGC=100`), drivers on CPUs 12–15, no other
work during measurements. Binaries: master `7d0f13860` (Go `b9fe8be6…d82c`) and both
commits on it (Go `e5556313…1b09`); this branch is the same client code rebased onto
`c27afbfd2`, which changes only a conformance test.

## Results

[24 configurations](results-commit-alloc-screen.jsonl): six alternating pairs per row,
`servercost.py --no-syscall-count`. Geometric means of paired ratios with Student-t 95%
intervals on their logarithms; medians per operation.

| Row | Client CPU | Allocations | GC cycles per run | Server cycles |
| --- | --- | --- | --- | --- |
| `set100`, 32 workers, closed | 0.935 (0.902–0.971); 39.9 → 36.6 µs | 0.883; 69.2 → 61.1 | 0.827 | 0.994 (0.918–1.075) |
| `set100`, 2,000/s, 64 workers | 0.881 (0.692–1.120); 108.9 → 103.8 µs | 0.884; 72.6 → 64.6 | 0.812 | 0.877 (0.669–1.150) |

- Closed-loop client CPU falls 6.5%, with 12% fewer allocations and 17% fewer GC cycles.
- The paced row's wide intervals come from block 3, where the branch arm ran disturbed:
  58.3 µs client CPU, 1,938 operations/s and 64.6% of starts over 1 ms late (master in
  that block: 7.5%). Without that block the paced row is client CPU 0.966
  (0.929–1.006), server cycles 0.971 (0.887–1.063), allocations 0.889, GC cycles 0.805.
- Server cycles show no established change.

## Closed-loop round splits

A [wire capture](results-commit-alloc-split.jsonl) of eight alternating closed-loop
runs (32 workers) records, per run, the first commit batch that did not hold a whole
round. In lockstep a GC pause can split a round, and the split persists. Fewer GC
cycles did not make whole rounds last measurably longer: first split at batch 6, 37, 7
and 163 on master and 35, 21, 32 and 129 with both cuts; transactions in whole rounds
0.04–0.47 against 0.01–0.22.
