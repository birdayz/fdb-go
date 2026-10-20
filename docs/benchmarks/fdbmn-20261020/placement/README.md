# P0 execution package

Generated for the dedicated `fdbmn-20261020-NONCE` campaign, production `fa8725f1dcf6ff3ac66825ab6e3e819efbc0abcc`, previous `267a41434a81723b07e7150869d702636a1b91a9`, frozen harness `d94b4c61377d173b7c6884fde426a227dab0f0bb`. No provisioning, SSH, cloud API, or measurement orchestration is included. Generation/verification used only local files, archive receipts, and fake operations.

## On the campaign client, after F0

Install **both** `p0.py` and `placement_checked.py` together at their final absolute paths before invoking. Those paths become sealing inputs; do not move them afterwards. The default root is `/srv/fdb-bench`, with the requested binaries, generated `cluster-ssd/{fdb.cluster,layout.json}`, `protocol.json`, and `tooling/tools/benchcampaign/{multinode,singlenode}`.

Use four **literal hashes from the independently reviewed F0 receipts**, not freshly computed command substitutions as a substitute for F0 validation. Replace all bracketed values below. The output parent must exist, but OUTPUT must not exist. Run in the supervised foreground with enough lifetime for up to three 120-second settling intervals plus seeding/ballast/probes. The default per-attempt settle timeout is 1200 seconds; a settle timeout stops P0, not a placement retry.

```sh
umask 077
python3 -B /srv/fdb-bench/execution/placement/p0.py \
  --execute --root /srv/fdb-bench \
  --output /srv/fdb-bench/provenance/p0 \
  --expect-cluster-sha256 <F0-cluster-file-sha256> \
  --expect-placementprobe-sha256 <F0-placementprobe-cgo-sha256> \
  --expect-fetch-sha256 <F0-fetch-master-sha256> \
  --expect-planskip-sha256 <F0-planskip-master-sha256>
```

Exit **0** means every original receipt has integrity and all cells are process-disjoint. Exit **3** is valid **integral partial placement from attempt 3**, not a failed qualification; preserve and report its fractions/labels separately. Exit **1** is an operational/integrity defect: stop and investigate; never continue to timing. Exit **2** is refusal before execution. Check `complete.json`, not merely absence of stderr. No retry, overwrite, resumption, fallback timing, or best-attempt selection is implemented.

P0 changes only DD mode and the fixed owned fixture/ballast/scratch ranges. It has **no DD-on exit trap**. It leaves DD off after selection, through M2 timing. The parent must run any R1 diagnostic that clears user keys **before** this P0. Do not run an R1 clear or reenable DD between final selected probes and M2. R2 diagnostics must preserve the selected state.

## Preserved evidence and selection

- `qualification/`: exit-2 refusal logs; original scratch small/split writes and canonical probes; each split mutation independently cleared/reseeded with an integral baseline; exact ID7/suffix physical delta and intended integrity rejection; scratch clears with positive read-version/zero-key verification. Offline original/key/provenance/positive-owner controls run on every integral qualification probe.
- `attempt-{1,2,3}/`: all 3 stores are seeded/ballasted with DD on **before one shared settle/freeze**. Ballast is 256/512/1024 MiB per store. Then all ballast ranges are cleared/verified, followed by canonical probes/receipts/complete original offline controls for all stores. Every poll and every attempt is retained. First all-store/all-cell pass wins; otherwise attempt 3 wins. Integrity errors abort instead of retrying.
- `selected/`: a second canonical all-store sweep, compared against the selected attempt, checks unchanged cluster/keys/boundaries/process teams/keyServers digest/DD lock owner+write token. Shared lock epoch and unchanged role/recovery identity are checked across both sweeps. All labels come from this final stable frozen state.
- Every controlled command has exclusive stdout/stderr and `command.json` with literal cluster-file/PP hashes, argv, exit status, timestamps, and pinned-input hashes checked before/after execution. All outputs are mode0600, directories0700, outside Git and without symlink path components.
- `fixture-receipts.json`: absolute `benchfetch/{small,split,planskip}` → selected attempt's exact benchmark-owned JSON fixture receipts.
- `selected-receipts.json`: absolute store → final stable receipt paths; use these for `--placement-receipt` sealing arguments.
- `placement-command.json`: R2 JSON argv template invoking the external PP-hash adapter, then the unchanged frozen `placement_command.py`. The template contains the **literal** expected cluster-file and PP hashes. The adapter preserves the original tool's receipts and adds `OUTPUT.command.json` containing those literals for both actual probe/status commands. Fresh per-case/when paths are enforced by the original tool.
- `seal-pins.json`: additional exact paths/hashes for the generated template/map, selected fixture/placement receipts, the resolved Python interpreter, and both external scripts. **Add every key here as `--pin` when sealing**, alongside the prepared runtime/source/layout/tool pins. The prepared shell script's `provenance/fixture-receipts.json` and `p0/fixture-*.json` assumptions must be adjusted to these actual paths (or the map copied exclusively and the emitted template amended/rehashed before sealing). Do not omit chosen fixture files because they are nested under `attempt-k/`.

The original preset includes rows100 placement cells although this campaign does not time them. They remain in canonical qualification and the all-cells pass rule, matching the original tool/preset. Machine-disjointness is descriptive only; acceptance is storage-process/address disjointness.

## Fixture-only seeding, no timing results

Fetch selects `^BenchmarkIndexFetch$/^payload={small,split}$/^P0_FIXTURE_ONLY_NO_CELL$`; planskip selects `^BenchmarkPlanSkip$/^P0_FIXTURE_ONLY_NO_CELL$`. The existing frozen parent functions clear/reseed/audit their own store before enumerating children. The intentionally nonexistent child prevents every timed leaf from running. Logs must contain **exactly one exact frozen fixture audit**, a binary PASS, no before/after audit, and no ns/op/B/op/allocs/op result. A failure of that contract stops P0. No benchmark result is collected or counted.

Both fetch fixtures seed ID0..999, price=id, quantity=id%7+1; split uses the frozen deterministic 100000-byte padding/two chunks. **Frozen planskip seeds quantity=1**, not id%7+1: this differs from the new protocol's shorthand describing all M2 fixtures together. This package preserves the exact frozen source/fixture rather than changing the harness. `invocation.json` records the distinction. The prior archive seeding logs had timed 1x SCAN_AND_FETCH leaves; those are used only as fixture-audit oracles, not repeated as measurements here.

## Offline verification

```sh
python3 -B execution/placement/verify_offline.py \
  --campaign ${FDB_PERF_HOME}/campaigns/fdbmn-20261020 \
  --archive ${FDB_PERF_HOME}/hcloud-multinode-vpc-20261014
```

These are standalone external Python checks, not repository tests and not Bazel fast/full-lane verification. They parse all scripts, recompute the complete original offline controls against all 9 archived real placement probes, check all 3 archived on-cluster mutation deltas/intended errors, and exercise sequencing/selection/output guards/R2 delegation with fake operations. They never execute placementprobe, fdbcli, benchmarks, SSH, or cloud commands. Fresh on-cluster qualification still runs in P0; archive greens do not substitute for it.
