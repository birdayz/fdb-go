# Rev5 tail diagnosis: GC promotion gate proven; historical residual

**Later gate status:** the [fixed confirmation](rev6-confirm-results.md) cleared
the 8w p999 blocker by the owner's preregistered rule, with power/epoch/order caveats.
The historical populations and observed mechanism below are not reinterpreted.

**Not merge approval.** Revision 6 selects the detector-only identity protocol and retains every-rearm recovery. The prototype removes the measured R5 arm-path GC-promotion waits and improves p99 against R5 in every paired run, but its 8-worker p99 and p999 remain adverse against the contemporaneous original baseline in every pair. That residual is unresolved; R4 itself is adverse at 8w p99/p999 in five of six contemporaneous pairs, so there is a base-design contribution rather than a detector-only explanation. No 8w execution trace was collected for this population; all traces below are 32w. At this historical scratch freeze, no production Go implementation had changed and the branch still contained the Phase 1 checkpoint. The later accepted-design/production correctness checkpoint is recorded in [rev6-production-evidence.md](rev6-production-evidence.md); it does not approve these residual tails or replace broader CPU qualification.

## Populations and discipline

Raw destination: `/tmp/rfc263-rev5-tail.I5smMO`. All write-ups/scripts are in-tree under `docs/benchmarks/rfc-263/` and `tools/benchcampaign/campaigns/rfc-263/`. Raw CSV, summaries, profiles, traces, source archives and compiled binaries remain outside the tree, indexed by SHA256.

1. **Contemporaneous three-arm screen:** six balanced permutations of baseline/R4/R5 at 32 workers, 500k measured + 2k warmup, then the same six at 8 workers, 20k + 2k; 36 valid runs, 2026-10-20 12:36:58–12:43:30 UTC.
2. **Separate diagnostics:** baseline/R4/R5 block, mutex and execution-trace runs at 32 workers/500k; 9 valid runs, 12:46:40–12:49:18 UTC. Never pooled with timing.
3. **Detector-only causal A/B with contemporaneous control:** six balanced three-arm blocks R5/identity/baseline at 32 and 8 workers, same budgets; 36 valid runs, 12:56:44–13:03:12 UTC.
4. **Separate identity trace:** 32 workers/500k, 13:05:38–13:05:55 UTC. This is attribution, not qualification timing.

Each complete batch held the owner's `fast.lock`; it was released between batches. No builds/tests/profiles/CPU-heavy analysis overlapped timing. Every run retains uptime before/after, UTC bounds, exact argv/environment, executable/source/harness hashes, load/idle, complete samples and success/error checks. Affinity 2,3,14,15; GOMAXPROCS 4, GOGC 100, GOMEMLIMIT off, empty GODEBUG; unchanged pure-Go harness and existing real tmpfs FDB cluster. Host background services were present, not claimed absent. The first 36-run screen includes an 8w R4 idle sample 0.8839; it is preserved, not silently excluded.

Quantiles are lower order statistics `floor((n−1)p)` per run. All full samples and complete 1 µs-bin count histograms are retained; no overflow bucket or dropped tail. Tables are medians of per-run metrics, not pooled transaction quantiles.

## Fresh baseline/R4/R5 screen

All latency values are µs; CPU is process user+system µs/transaction in the measured window.

| Workers / arm | CPU | Mean | p50 | p90 | p99 | p999 |
|---|---:|---:|---:|---:|---:|---:|
|32 baseline|60.492906|779.109|695.112|1053.847|2332.559|4761.425|
|32 R4|59.810470|772.728|689.492|1041.648|2338.971|4791.065|
|32 R5|59.856449|796.986|701.444|1051.823|2847.875|5879.498|
|8 baseline|95.709675|483.452|455.814|603.394|1003.796|1466.032|
|8 R4|95.269900|487.208|458.383|610.131|1023.364|1566.328|
|8 R5|94.364150|492.743|461.264|604.400|1122.582|1616.037|

At 32 workers R5 p99 and p999 are adverse against baseline in **all six pairs**; R5 p99 is also adverse against R4 in all six. R4 against baseline is mixed. At 8 workers R5 p99 against baseline is adverse in five of six pairs; do not hide the favorable pair or host-load variation. Historical comparisons are separately documented in `rev5-historical-tail.md` and are not contemporaneous controls.

## Mechanism pinned in the execution trace

Pinned Go 1.26.9 `runtime_makeStrongFromWeak` checks a global GC-completion gate before reading the handle, and can park with reason **GC weak to strong wait** (`runtime/mheap.go:2418–2428,2480–2499`). The gate spans the ragged barrier/additional marking retries and is not limited to the reported STW pause (`mgc.go:1036–1068,1153–1156`). R5 introduced this conversion under the transaction lock on ordinary existing-node arms.

The separate R5 trace contains **5,671 completed parks**, all with `armTimeoutLocked:217 → turnoverToken.finish → publishCommit → Transact → measured runPhase worker` in their stacks. Cumulative parked time is **6.845114217 goroutine-seconds**, maximum **5.698880 ms**; subsequent runnable delay totals **1.095356662 goroutine-seconds**, maximum **0.898624 ms**. These are accumulated goroutine delays, not process CPU or elapsed wall time. R4, baseline and the identity trace each contain **zero** such parks.

All 5,671 waits begin inside recorded GC marking. Every complete park-plus-runnable interval maps **unambiguously and necessarily** inside the same worker's measured CSV sample. Epoch bounds come from traced worker creation/start-channel release and first-running events versus the CSV first-start offsets, not a guessed clock offset. Bounds including ±64 ns trace-clock margin are R5 1,948 ns, R4 1,451 ns, baseline 2,074 ns, identity 4,830 ns. All 32 workers and all complete samples are checked.

Of R5's slowest 1% (5,000 samples), 4,830 overlap GC marking and 2,893 contain one of the waits. Of its slowest 0.1% (500), all 500 overlap GC and 203 contain a wait. **All four arms' slowest 500 overlap GC marking**, including identity without any weak-promotion wait. Therefore GC overlap alone is not a unique diagnosis, and the remaining tail is not attributed to weak lookup, sweep, cleanup or lock contention without further evidence. Detailed bounded alignment, example intervals, stacks and raw hashes are in `rev5-trace-attribution.md` and the external `trace-analysis/` artifacts.

Block profiles are dominated by ordinary select/wait-group waits: total cumulative delay baseline 365.25 s, R4 363.32 s, R5 354.31 s, roughly 96% `runtime.selectgo`. Mutex totals are baseline 1.373 s, R4 1.58 s, R5 1.233 s, mainly query/transport/queue-model paths. These separate diagnostic populations do not explain the R5-specific weak park: its direct `goparkunlock` does not use ordinary block-profile accounting. A clean block/mutex result must not erase the execution-trace evidence. Full raw/top profiles are retained, not only these summaries.

## Detector-only intervention

The prototype changes only the weak import, `n.tx.Value()==nil` to **`weak.Make(tx)!=n.tx`**, and its explanatory comment. Retirement, immutable node/cleanup identity, shard selection, deadline/cause/arm state, ticket publication and drainer are unchanged. The constructor is intentionally unchanged for causal isolation, so resurrection performs a second rare Make lookup without another handle allocation.

Go 1.26.9 finalization sweep unlinks the old weak special (`mgcsweep.go:560–584`) and clears its retained handle box (`mheap.go:2788–2799`). `getWeakHandle` synchronizes with sweep and searches the current span-specials list (`:2610–2627`); later Make either creates a new association (`:2527–2551`) or returns a newer canonical association (`:2577–2598`), never the unlinked old one. The old box remains rooted through immutable `n.tx`, preventing address-reuse equality. This also follows the public weak identity contract (`weak/pointer.go:26–37`).

Registration does **not** take the strong-conversion GC gate or shade the referent. It is not free: every ordinary check disables preemption, may sweep/yield, acquires the span-specials lock and searches its list. No assertion of contention-free or lower intrinsic CPU cost follows from zero allocations.

Before timing, all **20 top-level probes + 2 subtests** actually pass in race and opt builds. The no-renewal mutation compiles and fails all four real-finalizer scenarios at `RESURRECTION_NODE_NOT_RENEWED`; exact restoration reruns green. First creation remains 9 allocations; normal live rearm 0; fresh-association helper 5 − 1 carrier = 4; node/shard/root/ready 64/112/320/24 bytes, cold construction 49. Scratch semantic/quality reviewers ACK the detector, not performance or production shipping.

### Causal A/B results

| Workers / arm | CPU | Mean | p50 | p90 | p99 | p999 |
|---|---:|---:|---:|---:|---:|---:|
|32 baseline|59.788444|773.107|688.724|1042.375|2363.998|4730.526|
|32 R5|58.716112|773.733|681.320|1014.602|2736.654|5852.787|
|32 identity|59.442367|769.914|690.418|1032.567|2299.065|4812.491|
|8 baseline|95.928575|473.077|448.495|587.664|920.138|1316.689|
|8 R5|92.924075|482.227|453.740|589.852|1061.406|1430.370|
|8 identity|94.447775|475.946|449.687|588.725|947.921|1486.020|

Identity versus R5 paired p99 changes:
- 32w: **−7.35%, −17.69%, −16.56%, −22.95%, −16.35%, −12.93%**.
- 8w: **−6.31%, −2.69%, −11.16%, −16.92%, −10.24%, −12.26%**.

At 32w p999 also improves in all six identity-versus-R5 pairs. Together with the trace and detector-only intervention, this pins the new R5 arm-path GC promotion gate as a cause of its p99 regression. It does **not** identify every slow sample or justify calling the prototype a complete fix.

**Unresolved residuals / cost:** identity against baseline at 8w has p99 adverse in **all six** pairs (+2.19%, +9.64%, +0.37%, +1.41%, +2.85%, +3.19%), and p999 adverse in all six (+13.88%, +11.21%, +16.60%, +1.07%, +0.47%, +41.38%). At 32w p99 is favorable in five pairs and adverse in one; p999 is mixed. Median identity CPU is **+1.24% at 32w / +1.64% at 8w versus R5**, although −0.58%/−1.54% versus contemporaneous baseline. This is a measured program-level trade-off, not exclusive attribution of all added CPU to the specials lock. No CPU/tail approval for other workloads is claimed.

## Identities and evidence

| Arm | Full source identity | Executable SHA256 |
|---|---|---|
|baseline|`fa8725f1dcf6ff3ac66825ab6e3e819efbc0abcc`|`2dd943fdc9d58ff136449a181189ba5843465e86e6ae696d616a9b1bac4a439e`|
|R4|`61d2597e0231dfbedcd898a27c078c3412efaf6a`|`2f0993bd332a9d7085b1c1053659b68867286de95ad1cbe2bf91fe327f088e95`|
|R5|`3f800737de95d937cd1f12babc60b5588814d781`|`d2b9f82132acd6d4c10d3a694003c0c00094181b90f648d70f38f5b7f18d965e`|
|identity|`bd0408b1e0c3828e5d587c51f4161b82033586c2`|`9067e97f4f9d15535964ca7dc37f7b808189bc65d2c639e09a296ba9f0db1a3e`|

Identity full source archive SHA256: `04258b4d4980f3c773e8d383da8140e47d78f46ee0baa68c3a6b6ba881c9fb59`. `identity-detector-only.patch` proves the intervention delta; the complete disposable patch is in-tree as `rev5-identity-prototype.patch` (zero-context apply). Alignment summary SHA256: `0c14e0d53841ef4b0d1ede8f27fe3e2512307c33df41e7bb0ad171ec5f965cd9`. Key external raw SHA256 values:

| Artifact | SHA256 |
|---|---|
| `timing-tail-rows.json` | `db7b334ac31d45ff99e8bf776f7e8ec1bc22c85edbe4bafb020c76a34a2ac90a` |
| `timing-tail-analysis.json` | `1c02ff61d969116f6da120886c94cc100f3723ea9af04ae16cf1df10635d3304` |
| `identity-ab-tail-rows.json` | `30d7b224aaf938998055fe8d33df30a8960806e8625759bd0be12b54a6840e59` |
| `identity-ab-tail-analysis.json` | `b53b897851df3a1c2501b88136c8d6a34621da5eda9b5b3c548bcfe0cbf9ffcc` |
| `identity-preservation-verification.json` | `a30596d36023093ebc0774cd0125dc04485607ad13f61e1b1eb90bc5b4975207` |

Recursive manifests are retained alongside profiles and trace extraction inputs. Invalid attempts are preserved: wrong-main-cwd identity input verification (correct scratch verification later passes), and the initial trace carry-forward RangeActive parser rejection. Neither enters qualification timing.
