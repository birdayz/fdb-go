# fdbmn-20261020

Multi-node baseline of current master (`fa8725f1d`) on four dedicated 16-vCPU
cloud hosts on a private VPC (1 client host, 3 FoundationDB 7.3.77 server hosts),
repeating the [`hcloud-multinode-vpc-20261014`](../hcloud-multinode-vpc-20261014/README.md)
phases M1 (pure-Go vs CGo at 1/8/32/64 workers) and M2 (`USE_REMOTE_FETCH` vs
`SCAN_AND_FETCH`) under a preregistered protocol.

**Status: executed and torn down on 2026-10-20.** Results are in
[RESULTS.md](RESULTS.md) and local diagnostic profiles in
[profiles-local.md](profiles-local.md). The 32-worker M1 phase was dropped for
budget before measurement. The protocol, procedure and decision documents in this
directory are as they stood after execution.

## Known corrections

The protocol is not edited for these, because its bytes are a hashed input (the
in-tree `protocol.json` differs from the sealed file only by the sanitization
recorded in `docs/perf/IMPORT-PROVENANCE.tsv`, which also holds the sealed
file's sha256). Reviews found:

- `protocol.json:603` (`server_pre_arm`) explains the 60 s storage durability-lag
  gate with "(FDB 7.3.77 TARGET_DURABILITY_LAG_VERSIONS = 5 s normal storage write
  batching cycle)". That is wrong. In FoundationDB 7.3.77 the knob is a
  ratekeeper limit, passed into its normal and batch limits
  (`fdbserver/Ratekeeper.actor.cpp:719-735`), not the storage batching interval.
  `fdbclient/ServerKnobs.cpp:905` sets it to `350e6` versions, 350 s at
  `VERSIONS_PER_SECOND` = `1e6` (`:36`); the batch variant
  `TARGET_DURABILITY_LAG_VERSIONS_BATCH` is `150e6` (`:907`). The storage commit
  interval is `STORAGE_COMMIT_INTERVAL` = `0.5` s (`:1010`). The 60 s gate itself
  is protocol policy and is unaffected.
- `protocol.json:571` (`versioning_finding`) states that every data-distribution
  transaction that changes the shard map calls `checkMoveKeysLock`. In 7.3.77 that
  holds only for the default `SHARD_ENCODE_LOCATION_METADATA` = `false`
  (`fdbclient/ServerKnobs.cpp:190`). With that optional feature on,
  `cleanUpSingleShardDataMove` (`fdbserver/MoveKeys.actor.cpp:630-702`, which
  asserts the knob at `:636`) runs its own transaction: it rewrites `keyServers`
  (`:689-690`) and commits (`:702`) without `checkMoveKeysLock`. `startMoveShards`
  calls it (`:1749-1752`) and then retries, so the caller's lock check at `:1626`
  does not cover it; the path is selected only when the feature is enabled
  (`rawStartMovement`, `:3102-3126`). This is a correction to the protocol's
  claim about 7.3.77, not a claim about the measurements.

## Contents

| Path | Content |
|---|---|
| [PROTOCOL.md](PROTOCOL.md), [protocol.json](protocol.json), [orders.json](orders.json) | Preregistered protocol, phases, seeds and counterbalanced orders. |
| [REPRODUCIBILITY.md](REPRODUCIBILITY.md) | Source trees, frozen harness `d94b4c613` and runtime identities. |
| [RUN-DAY.md](RUN-DAY.md) | Run-day checklist written during preparation. |
| [EXECUTION-NOTES.md](EXECUTION-NOTES.md) | Execution log: pre-measurement findings and corrections, per-phase outcomes, decisions. |
| [decisions/](decisions/) | Post-run decision records: no A1 re-attempt (`no-A1-decision.json`), the pre-arm CPU burst attribution, and the operator-side polling deviation from M1w8 on (`polling-ops60-*.json`). |
| [build-check/](build-check/) | Build verification; the `#948` lifecycle-drain deviation patch applied to the frozen harness (`cleanup-compat-*.patch`, amendment record with per-file sha256). |
| [placement/](placement/) | Shard-placement P0 freeze, resume and live-seal procedures, and their sha256 lists. |
| [ANALYSIS-ADAPTER.md](ANALYSIS-ADAPTER.md) | Offline analysis adapter and its controls. |
| [MANIFEST.sha256](MANIFEST.sha256) | sha256 of the preparation documents and command files. |

## Frozen tools

The campaign's tools are in
[`tools/benchcampaign/campaigns/fdbmn-20261020`](../../../tools/benchcampaign/campaigns/fdbmn-20261020/).
The campaign executes its own pinned copies; the in-tree copies are records.

| Tool | Pinned sha256 | In-tree copy |
|---|---|---|
| `execution/collect_final.py` (collector) | `8ebe4b4224c3ecf858414203cad8ff7b2256b6b55224b9d2a78fedfa99631217` | byte-identical |
| `execution/analyze_campaign.py` (analysis adapter) | `700fd3c18a319ea6738c00652846313797a4f4bfbcf50e4b3aee37e720a70289` | two default paths written as `${FDB_PERF_HOME}` |
| `execution/test_analyze_campaign.py` | `1d622f864dfa0fb3785af9f08d6e609bee4cdd059edc0b34b449dc8cad5fb967` | byte-identical |

`verify_frozen.sh` checks all three; for the adapter it restores the original
paths from `FDB_PERF_HOME` before hashing. The multi-node and single-node tooling
the campaign deployed is identical to `tools/benchcampaign/{multinode,singlenode}`
at `fa8725f1d`. Later in-tree changes to those tools (strict JSON parsing of hcloud
lists, `inventory.py`, `--protect-file`, test skips replaced by fixture assertions)
postdate the campaign, so the in-tree tools now differ from what was deployed.

From M1w8 on, the phase controller ran as `execution/run_phase_ops60.py` with
`remote_ops60.py`: the operator-side polling deviation in
[decisions/](decisions/), which reuses one SSH connection and polls every 60 s
instead of 10 s; the protocol and the frozen harness are unchanged.
`execution/enable_dd_for_m1.py` is the M2→M1 data-distribution boundary step.
`//tools/benchcampaign/campaigns/fdbmn-20261020:fdbmn_20261020_test` runs the
adapter's and phase controller's offline controls.

Cloud resource IDs, the resource-label suffix and the names and address ranges
of unrelated resources the campaign protected are removed; see
[docs/perf/README.md](../../perf/README.md#paths-in-imported-documents).
