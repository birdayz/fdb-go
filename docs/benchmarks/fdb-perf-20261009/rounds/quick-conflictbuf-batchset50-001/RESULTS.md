# batchset50: small-buffer promotion control — short paired comparison

Campaign `quick-conflictbuf-batchset50-001`, seed 391717, root release #350. Separately frozen **v5-batchset50**, identically used by old Go, new Go and official CGo; never substituted for original v3 workload results. Six counterbalanced blocks containing all six three-arm orders exactly once; 100 measured +40 warmup operations per fresh process, workers=1. All18arms completed: **1800 measured commits /90000 measured Sets**, 600 commits/30000 Sets per arm, plus720 total warmup commits/36000 Sets and untimed fixtures. Runner plus analysis/validation took approximately65seconds (2026-10-10 03:10:42–03:11:47 UTC).

## Result and limits

New/old paired mean ratio **0.99173**, with only3/6 mean pairs lower and range **0.87943–1.06063**. No material mean regression was observed in this short control, but **this is not a noninferiority proof**. Median-oriented and CPU results are slightly higher: paired p50 **1.01646**, process CPU **1.01371**. Paired Go-byte allocation is effectively unchanged (**1.00078**), while allocation count rises **1.00429** (higher in all6pairs).

Against CGo, new mean **1.02143** and process CPU **1.39256**: **do not claim a native performance win for this workload**. The mean near parity does not erase the CPU gap or mixed tails.

Ratios below are geometric means of six **paired run-level** ratios, per whole50-Set committed transaction, not per individual Set. This short adaptive one-VM result is exploratory; bootstrap outputs are retained but not confirmatory. No universal tail/capacity claim.

| Comparison | Mean latency | p50 latency | p99 latency | Process CPU | Go allocated bytes | Go allocation count |
|---|---:|---:|---:|---:|---:|---:|
| New / old | 0.991733 | 1.016458 | 0.959422 | 1.013711 | 1.000780 | 1.004289 |
| New / CGo | 1.021427 | 1.004915 | 1.218463 | 1.392561 | 70.962020 | 7.910934 |
| Old / CGo | 1.029941 | 0.988644 | 1.269996 | 1.373727 | 70.906737 | 7.877147 |

New/old p99 pair range0.493345–1.484139, only3/6lower. New/CGo paired p99 is worse in5/6pairs despite a slightly lower pooled p99; that discrepancy is why pooled tails must not substitute for paired-run comparisons. With100observations per run, p99 is an especially noisy near-extreme order statistic.

### Pooled descriptive latency and run-resource summaries

| Arm | Pooled mean / p50 / p99 (µs/transaction) | Median process CPU (µs/transaction) | Median Go allocation (B/transaction) | Median Go allocations/transaction | Total measured Go GC cycles (six runs) | Total measured Go GC pause (ms) |
|---|---|---:|---:|---:|---:|---:|
| Old grv-idle | 2658.743 /2448.143 /6775.213 | 1186.985 | 113822.36 | 464.835 | 38 | 5.404 |
| New conflictbuf512 | 2632.611 /2497.323 /4839.756 | 1193.675 | 114339.40 | 466.870 | 36 | 6.270 |
| Official CGo | 2578.551 /2472.673 /4903.746 | 851.015 | 1616.16 | 59.000 | 0 | 0 |

Go GC cycles per run: old[6,6,7,6,7,6], new[6,6,6,6,7,5], CGo[0,0,0,0,0,0]. CGo allocation/GC figures cover only its Go runtime, **excluding the native C++ heap**; the large Go-byte ratios are not total native allocation or memory ratios. Process CPU includes user+system time. RSS high-water is retained in raw summaries, not treated as live or isolated measured-window heap.

## Workload and actual promotion shape

Each measured operation creates a fresh write transaction, performs exactly50distinct point Sets of100bytes each (5000value bytes total), then commits once. All values are pregenerated outside timing, identify phase/worker/operation/slot, differ across slots and change each iteration. It overwrites50preseeded keys, not an append workload. No timed Gets/ranges/read-only transaction are used. All50final persisted values per worker are checked byte-exactly in a separate read-only transaction after timing; this checks final state, not a read-back of every intermediate commit. Every intermediate operation still requires successful real-backend commit completion.

All18actual manifest prefixes were distinct and **exactly68bytes**; each batch key is **88bytes**. The candidate Set path reserves `2*len(key)+1` conflict bytes, therefore **177bytes/Set**, **8850bytes/transaction**. The first Set fits512, while the **third Set crosses512** and promotes to the32KiB pool. This is a source/shape proof using checked real command/key sizes, **not an instrumented runtime promotion counter**. See PROMOTION-PROOF.md, batch-prefix-proof.json and counter-resource-validation.json. Prefix lengths are equal across every old/new/CGo arm, avoiding key-length confounding.

## Correctness/path/isolation controls

- Each arm:600measured operations and exactly600callback attempts, **zero errors/retries**;240warmup operations and attempts, also zero errors/retries.
- All phase/count/index/value, seed and persisted checks passed; all quiescence gates passed. The unit shape/persistence-corruption checks were validated before release and are preserved in the build archive.
- Each Go arm: **600GRVs,600successful GRV replies,600commit starts,600commit completions**; zero GRV cache hits, transaction retries, connection failures and maybe-delivered GRVs.
- Native internal counters remain **unobserved**, not zero; mapped library SHA verified in every native process. Pure-Go manifests show no mapped native client.
- Timeout30000ms,retrylimit10,allow-writes,profile none for all arms. Same server/config/library and source/binary pins throughout. No build/test/profile or unrelated remote command during timing. Runner/collector0–1,FDB2–7,clients8–15,GOMAXPROCS8; original2s cooldown,1s quiescence and deliberate1Hz collector. Docker/containerd inactive. One co-located VM/server; no physical-host isolation claim.

## Immutable identities

Base `26abdbaac12be60ea8955027f6047e10474596f1`; Go1.26.9; server/native7.3.77; API730. Common v5 harness SHA-256 **`738348814d86cc6ec828aa3590625275e9f1b74bed7ae92e06e359c480151cfc`**. Both backend adapters are byte-identical to original v3; only this separately versioned workload/preparation/verification extension is new.

- Old client patch `32d59b91cdd509ba0cb985f88f52a2d62fdae4ad0020f832183ba79555e024e3`; `batchset50-v5-grv-idle-001-go`: `c0b08946eb3f6e8ec448d6aecfc24722a9a523170f46d51fe9d80e9c6cef7fc2`.
- New client patch `b3c09057e81ac448c8b05881e10cd52a34067a27152fcd0a4c367e514e0861c3`; `batchset50-v5-conflictbuf-512-001-go`: `5214910d5c4084d5c95d8e919f1867b279b164968d54afbd6a65c52a6fb853c6`.
- `batchset50-v5-cgo`: `8433071f23b26d8163ffed3555a2c32ce2fece22133dd281c679f929a144eee6`. Built in the new-source tree but CGO selects the official native adapter, not the pure-Go client; literal source identity reflects that build tree.
- Native library `e4ad7fed7b7580bf5f7b38f0a4279d37e698f69d534182e5de0b8beeef6cd1b1`.

## Correctness release status — append-only update

Root #350 reported **full131 GREEN151/151**, **95actual+56cached**, **1865.484seconds**, all **6814frozen hashes** verified, with hn-client/hn-codehealth/astra-grv ACK and no Medium findings. This is root's parent just/Bazel result, not a remote rerun by the measurement agent. Subsequent local oversized-unit-case/test-name edits do not alter the measured runtime/binary; exact frozen identities remain archived. Earlier archives keep their earlier truthful pending/failed status, untouched.

Both authorized short campaigns are complete. No additional profile or workload has been run or authorized. Await further root direction after verified backups.
