# Short Get1000 comparison — GRV idle/correctness iteration001

## Result, narrowly scoped

The new pure-Go candidate had lower paired mean latency than both the previous RYW-only Go version and official CGo for this Get1000 workload on this one co-located VM/server configuration. This is a short adaptive measurement, NOT a general all-workload or tail-latency win.

Campaign `quick-grv-idle-get1000-001`:6blocks, each of the6three-arm orders once;18fresh processes;100warmup+300measured operations perprocess;5400measured operations total. Completed in66seconds including cooldown/setup/analysis. Every arm passed exact raw-count/value/fresh-transaction-persistence validation and the prearm quiescence gate; zero measured/warmup errors or callback retries.

| Arm | Pooled mean µs | Pooled p50 µs | Pooled p99 µs | Median run process CPU µs/op | Median run Go heap B/op | Median run Go allocations/op |
|---|---:|---:|---:|---:|---:|---:|
| Previous RYW-only Go | 1654.073 | 1637.278 | 1947.046 | 893.677 | 8145.00 | 88.44 |
| New GRV Go | 474.328 | 456.706 | 961.374 | 532.303 | 7816.48 | 84.99 |
| Official CGo | 665.572 | 666.776 | 882.684 | 516.422 | 1528.00 | 16.00 |

Geometric means of paired RUN-level ratios (numerator/denominator):

| Comparison | Mean latency ratio | p50 ratio | p99 ratio |
|---|---:|---:|---:|
| NewGRV / priorRYW Go | 0.28627 | 0.27664 | 0.35361 |
| NewGRV / CGo | 0.71408 | 0.71605 | 0.91139 |
| PriorRYW Go / CGo | 2.49439 | 2.58838 | 2.57738 |

NewGRV/CGo mean ratios ranged0.61247–0.90692 across all6pairs: about28.6% lower paired mean latency overall, ~1.40x faster. NewGRV/priorRYW mean ratio implies ~3.49x faster for this control. The delta contains several GRV correctness/scheduling changes, not a one-line ablation.

Tail caveat: paired RUN-p99 ratio0.91139 is NOT the ratio of pooled p99s. The candidate's pooled p99 is worse than CGo (961.374 versus882.684µs), and the individual paired run-p99 ratios ranged0.75426–1.10492. No blanket tail-latency win is claimed.

CPU caveat: candidate process CPU remains higher than CGo despite lower wall latency. The paired run CPU/op ratio is1.06395 versusCGo, and0.58882 versuspriorRYW. Median-run CPU numbers in the table are a different descriptive aggregation. Go heap counters exclude libfdb_c's C++ heap; do not present the Go heap table as a total-memory comparison.

## Exact correctness/path checks

Each Go arm completed1800measured transactions with1800successful GRV batches/readversions;0GRVcachehits,0connectionfailures,0commits,0transaction retries. These are read transactions with empty commits. CGo native RPC counters are UNOBSERVED, not zero; every CGo arm loaded the expected libfdb_c SHA256 `e4ad7fed7b7580bf5f7b38f0a4279d37e698f69d534182e5de0b8beeef6cd1b1`. Neither pure-Go executable loaded a native FDB library. See `counter-path-validation.json`.

## Provenance and correctness status at authorization

- Base source: `26abdbaac12be60ea8955027f6047e10474596f1`.
- New whole-client delta SHA256: `32d59b91cdd509ba0cb985f88f52a2d62fdae4ad0020f832183ba79555e024e3`.
- New binary: `5b6e7302f977785317a83e4d9172555c48fe400f9362536e7fe8436d0b9c355b`.
- Prior RYW-only binary: `207d170cfb6a965a81e61a2403e4096a36a93c0eceda61135a4b9414285d8ca8` (unchanged).
- Official CGo binary: `af2e91489057be13a4b74c55c047b06de15d2483a981697523b076648f929dde` (unchanged).
- Frozen v3 harness archive: `41f594c990477bc4b29c565160599a57266cecb23adaae582180f7464fb2d0ea`; unchanged harness/options/runner. Full raw schedule saved before execution; seed391712.
- All737client source entries matched the frozen local tree;739frozen entries including module files checked, and checked again before measurement. Remote standalone harness unit tests passed. FDB7.3.77/API730; Go1.26.9; GOMAXPROCS8; client CPUs8–15, FDB2–7, collector0–1; same2second cooldown+1second group/perCPU gate. No competing builds/tests/profiles ran during measurement.

Root authorized after actual client_test execution passed, including four retry regressions, two real-FDB idle dispatch regressions, three prior GRV regressions, and the fixed commit-uncertainty fixture. Root also reported fast lane99/99pass (54executed,45cached), plus C++ and code-health ACKs. Full91 was STILL RUNNING at authorization and was NOT full-green: the vector timing criterion passed at1.4428s loaded, but a recordlayer20ms polling-pulse fixture race failed; root reported a deterministic fixture replacement drafted pending freeze end. This measurement does not claim the whole local or remote full suite passed. See `authorization.json`.

The earlier `grv-aligned-001` candidate was built but NEVER performance-measured: its full client test hung in commit-uncertainty recovery. Its source/binary/build artifacts are retained separately in this backup rather than silently removed. No old standalone GRV patch was transplanted.

## Limits and next step

Six blocks/1800operations perarm, one virtualized co-located server, fresh process and bounded warmup, adaptive iteration/selection bias: all restrict generalization. No physical SKU/whole-host isolation asserted. Bootstrap output is exploratory only; pooled tails descriptive. No new profiles or additional campaigns were run after this authorized comparison. Await root's next decision before another iteration.
