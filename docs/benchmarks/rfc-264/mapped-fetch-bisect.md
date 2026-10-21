# RFC-264: mapped-fetch bisect results

## Finding and scope

The historical local comparison between Go
`267a41434a81723b07e7150869d702636a1b91a9` and
`fa8725f1dcf6ff3ac66825ab6e3e819efbc0abcc` measured USE_REMOTE_FETCH ns/op
increases of **1.177× reverse** and **1.147× forward**. The absolute remote
increase was localized to #954 (`590aa97e3..203560a9e`). The later increase in
remote/ordinary ratio at #968 (`0b0d1e71c..ae14b2208`) was principally an
improvement in ordinary SCAN_AND_FETCH, not a new absolute remote slowdown.

This is source-interval localization, **not a causal decomposition** of timer,
dispatch, scheduling or coalescing costs, and not a recoverable optimization
budget. [RFC-264 is Rejected](../../../rfcs/264-send-wait-cost-and-ordering.md);
the owner retains #954's 10/20 µs default and accepts its latency-versus-CPU/
allocation trade-off. No further prototype or measurement follows from this
report.

The workload was `BenchmarkPlanSkip/cell=rows10`, plan-level Skip 3, small
records, on a local single-storage-server, single-redundancy memory-engine
cluster. Each operation created a transaction, opened the store, executed the
plan and drained ten returned rows. Remote retained Skip above the mapped
cursor, rather than pushing positive Skip into mapped selector resolution.
This local comparison is not a result for a multi-node deployment.

## Compared inputs

All nine arms used **Go 1.26.9**, default Bazel configuration and the same
relevant planskip/benchcluster harness from
`d94b4c61377d173b7c6884fde426a227dab0f0bb`. Source and binary identities are
historical; they do not describe current master or a later toolchain.

| Arm | Full source commit | Binary SHA-256 |
|---|---|---|
| old | `267a41434a81723b07e7150869d702636a1b91a9` | `69931dd8640781beb13b254b88869cccb85f7854b7b4d0132ae28d8a9a44a6e6` |
| pre-#954 | `590aa97e326b612cf7768c3c14578d3470281443` | `ca2d70e7180fd14dbd6657827fa2fcaacc13c27b9d498012189e47d902fafe85` |
| #954 | `203560a9e21e57a3c561736423e5a27f0d443781` | `043050d0e0ca2bc3eb2b8cd70206dc47291812be7b41a8989682c9064d897954` |
| #956 | `7d0f13860df22c1d9817d0d0cf73c64edaba204a` | `6b46f64e74719bcea5d4d393cf2758d16aa74882288424184ea7a53c1feaf30a` |
| pre-#965 | `34a94d93ffb5baf99498122c274a1c553eac1a6a` | `c77e9963e549dc6d76483d75c262494c4566d4936bf02f7a0a0e46710642b85c` |
| #965 | `eecd839d99b2e7854e68b432552e1d6a4c333ae8` | `e8f4ce2fc47cb37d8a7b865af7689859278786768c1966e536d783a62219a56a` |
| #967 | `0b0d1e71c67f3d1e92c26d62b8ed5af78056373c` | `349e5422c3903b275908f454c4ec66fab90437e6e16aa5837b550d70aa8b8fcf` |
| #968 | `ae14b220883c4985031d5659f8b34cd7ecece8a9` | `558b904eff25921001235f1c06bbf9254ec2936dbc6cd8ad2c40d6c15add75fb` |
| new / #969 | `fa8725f1dcf6ff3ac66825ab6e3e819efbc0abcc` | `b2020a2044a5feba3e674d5e12a26ed76ff5b114be1bd38aa74491f5d53c29a4` |

## Method

The fixed population completed **144/144 sequential processes**: nine arms ×
four blocks × two directions × two methods. Each process ran three 1-second
`testing.B` repeats, for **432 measured repeats**, without profiling. Clients
used `GOMAXPROCS=8`, `GOGC=100`, `GOMEMLIMIT=off`, no `GODEBUG` override and a
CPU partition disjoint from the servers. A quiet start gate excluded detected
build/container activity; all scheduled observations were retained. Cleanup was
verified.

Cells rotated left by block `b`. At cell position `c`, chronological arms rotated
left by `(2*b + 2*c) mod 9`, reversing when `b+c` was odd. Each arm/cell appeared
once per block. There was no adaptive sampling, outlier deletion or partial-run
comparison. All process exits, binary identities, fixture audits and before/after
fetch-path audits passed. Audits required ten returned rows, Skip 3 and the
expected order/fetch path; remote used one remote opening and fourteen
remote-entry events, while ordinary used index scanning and ten record loads.

The process statistic was **median of three ns/op means**, not per-operation
p50/p99. Absolute displays are geometric means of four process statistics.
Comparisons are geometric means of four paired within-block ratios:

- Adjacent: later/earlier, separately by method and direction.
- Remote/ordinary: USE_REMOTE_FETCH / SCAN_AND_FETCH.
- Difference-in-differences (DiD): `(remote_arm/remote_old) /
  (ordinary_arm/ordinary_old)`.

All bracketed ranges are **four-block min/max, not confidence intervals**.
Traffic, error rates and saturation are not reported in these historical
latency tables; no values are inferred from the timing means.

## Absolute results

In the Latency column, **R** is USE_REMOTE_FETCH ns/op, **O** is SCAN_AND_FETCH
ns/op, **R/O** is their paired ratio and **DiD** is relative to old. Lower ns/op
is faster. Neither R nor O is an operation-latency quantile.

| Direction / source | Latency | Traffic | Errors | Saturation |
|---|---|---|---|---|
| reverse · `267a41434` | R 511,843; O 742,754; R/O 0.689; DiD 1.000 | not reported | not reported | not reported |
| reverse · `590aa97e3` | R 515,410; O 750,173; R/O 0.687; DiD 0.997 | not reported | not reported | not reported |
| reverse · `203560a9e` | R 607,148; O 839,207; R/O 0.723; DiD 1.050 | not reported | not reported | not reported |
| reverse · `7d0f13860` | R 600,792; O 842,497; R/O 0.713; DiD 1.035 | not reported | not reported | not reported |
| reverse · `34a94d93f` | R 602,094; O 841,616; R/O 0.715; DiD 1.038 | not reported | not reported | not reported |
| reverse · `eecd839d9` | R 603,270; O 844,790; R/O 0.714; DiD 1.036 | not reported | not reported | not reported |
| reverse · `0b0d1e71c` | R 598,005; O 843,471; R/O 0.709; DiD 1.029 | not reported | not reported | not reported |
| reverse · `ae14b2208` | R 598,291; O 729,117; R/O 0.821; DiD 1.191 | not reported | not reported | not reported |
| reverse · `fa8725f1d` | R 602,557; O 730,746; R/O 0.825; DiD 1.197 | not reported | not reported | not reported |
| forward · `267a41434` | R 523,087; O 729,563; R/O 0.717; DiD 1.000 | not reported | not reported | not reported |
| forward · `590aa97e3` | R 515,757; O 728,420; R/O 0.708; DiD 0.988 | not reported | not reported | not reported |
| forward · `203560a9e` | R 605,543; O 819,983; R/O 0.738; DiD 1.030 | not reported | not reported | not reported |
| forward · `7d0f13860` | R 601,094; O 814,606; R/O 0.738; DiD 1.029 | not reported | not reported | not reported |
| forward · `34a94d93f` | R 603,174; O 826,111; R/O 0.730; DiD 1.018 | not reported | not reported | not reported |
| forward · `eecd839d9` | R 601,010; O 825,343; R/O 0.728; DiD 1.016 | not reported | not reported | not reported |
| forward · `0b0d1e71c` | R 601,992; O 817,592; R/O 0.736; DiD 1.027 | not reported | not reported | not reported |
| forward · `ae14b2208` | R 607,297; O 705,542; R/O 0.861; DiD 1.201 | not reported | not reported | not reported |
| forward · `fa8725f1d` | R 600,049; O 710,883; R/O 0.844; DiD 1.177 | not reported | not reported | not reported |

## Adjacent source-interval ratios

The Latency column gives later/earlier ns/op ratios for remote (R) and ordinary
(O), in reverse and forward directions. Ratios above one mean slower.

| Source interval | Latency | Traffic | Errors | Saturation |
|---|---|---|---|---|
| old → `590aa97e3` | reverse R 1.007, O 1.010; forward R 0.986, O 0.998 | not reported | not reported | not reported |
| `590aa97e3` → `203560a9e` | reverse R 1.178, O 1.119; forward R 1.174, O 1.126 | not reported | not reported | not reported |
| `203560a9e` → `7d0f13860` | reverse R 0.990, O 1.004; forward R 0.993, O 0.993 | not reported | not reported | not reported |
| `7d0f13860` → `34a94d93f` | reverse R 1.002, O 0.999; forward R 1.003, O 1.014 | not reported | not reported | not reported |
| `34a94d93f` → `eecd839d9` | reverse R 1.002, O 1.004; forward R 0.996, O 0.999 | not reported | not reported | not reported |
| `eecd839d9` → `0b0d1e71c` | reverse R 0.991, O 0.998; forward R 1.002, O 0.991 | not reported | not reported | not reported |
| `0b0d1e71c` → `ae14b2208` | reverse R 1.000, O 0.864; forward R 1.009, O 0.863 | not reported | not reported | not reported |
| `ae14b2208` → new | reverse R 1.007, O 1.002; forward R 0.988, O 1.008 | not reported | not reported | not reported |

At **#954**, remote increased **1.178 [1.169, 1.192] reverse** and
**1.174 [1.161, 1.190] forward**. Ordinary increased
**1.119 [1.090, 1.162] reverse** and **1.126 [1.112, 1.146] forward**.
The paired remote/ordinary changes were **1.053 [1.026, 1.072]** and
**1.043 [1.028, 1.053]**.

At **#968**, ordinary improved **0.864 [0.853, 0.872] reverse** and
**0.863 [0.856, 0.872] forward**, while remote was approximately flat:
**1.000 [0.994, 1.007]** and **1.009 [0.999, 1.018]**. The paired
remote/ordinary changes were **1.157 [1.144, 1.167]** and
**1.169 [1.146, 1.189]**. Near-one remote estimates are not equivalence results.
Every range here is **block min/max, not a CI**.

## Source interpretation and limits

#954 introduced C++-parity positive connection coalescing, together with changes
to timer/direct-write/dispatch machinery. C++ 7.3.77
`fdbrpc/FlowTransport.actor.cpp:708–710` waits for the jittered
`max(MIN_COALESCE_DELAY, MAX_COALESCE_DELAY - (now - lastWriteTime))` before a
writer cycle. `flow/Knobs.cpp:211–212` sets **10/20 µs**; its jitter makes the
nominal positive argument approximately **9–22 µs per cycle, not per RPC**.
Followers can share a cycle, and partial-write tails do not start a new interval
(`FlowTransport.actor.cpp:714–732`). Serial dependent stages entering fresh
cycles cannot share a batching interval before they have been issued.

This source context does not separate the delay's contribution from Go timer,
scheduler, batching or dispatch costs. It does not prove a unique mechanism,
attribute the later denominator improvement to a specific function, or justify
disabling the default. The [local P0/P2t screen](local-quick-screen.md) is a
separate later measurement on a different base and compiler; neither candidate
qualified for promotion.

## Reproduction and evidence integrity

The [portable plan and drivers](../../../tools/benchcampaign/campaigns/rfc-264/mapped-bisect/README.md)
retain the fixed method. Raw repeats, process statistics, complete block ratios,
audits, source/build receipts and binaries remain external. Content SHA-256
references are:

- Input seal: `ed398d1dec3b8a2862ece063a880f6cf42d49b6d383d47c4b4db6914259f8afc`.
- Summary: `c64701dd1c14117829b827e1dd80ea25ce9f6812ee37dd57f83c70ffe813c88e`.
- External artifact manifest: `d0ad9a9bf014c58b469a1494d5555e725c42cab58761a0dff82520d60b01989f`,
  covering **473 regular artifact files**; Python bytecode caches and the
  manifest itself are excluded. Raw data and binaries are not committed.
