# Live/diagnostic sealer helper

`seal_live.py` runs **only the existing `seal_multinode.py` planning/sealing functions** on the campaign client. It never invokes a runner, benchmark, cluster command, SSH, or cloud operation. Keep `p0.py` beside it (shared hash/output guards only; importing it does not run P0).

```sh
R=/srv/fdb-bench
python3 -B "$R/execution/placement/seal_live.py" client-diagnostic
# After diagnostic/capacity work and completed P0:
python3 -B "$R/execution/placement/seal_live.py" testingb-diagnostic
python3 -B "$R/execution/placement/seal_live.py" client-final
python3 -B "$R/execution/placement/seal_live.py" testingb-final
```

Options: `--root` defaults `/srv/fdb-bench`; `--p0-root` defaults `ROOT/provenance/p0`; runtime paths default `/usr/bin/fdbcli`, `/usr/sbin/fdbserver`, `/usr/lib/libfdb_c.so` and can be supplied with `--fdbcli`, `--server-binary`, `--native-library`.

| Mode | Exact population | Manifest | Result prefix |
|---|---|---|---|
| `client-diagnostic` | M1w1/grv, final-Go + CGo, 2 blocks, 32 operations, 4 warmup: 4 processes | `R/diagnostic/m1/plans/manifest.json` | `R/diagnostic/m1/results/client/M1w1` (R1's grv output is beneath it) |
| `testingb-diagnostic` | M2skip/rows10/forward, ordinary + remote, 2 blocks, 1 repeat, 1x: 4 processes | `R/diagnostic/m2/plans/manifest.json` | `R/diagnostic/m2/results/M2skip` |
| `client-final` | 28 campaigns × 18 processes; original 3 arms, 6 blocks and worker budgets | `R/plans/client/manifest.json` | `R/results/client/PHASE` |
| `testingb-final` | M2rl: 12 cases/144 processes; M2skip: 4 cases/48; 6 blocks, 3 repeats, 3s | `R/plans/testingb/manifest.json` | `R/results/PHASE` |

Every manifest's directory also gets **`sealed.json`**, containing mode, manifest SHA256, protocol/orders paths and hashes, plan/schedule hashes, result prefixes, and the exact sealer argv/stdout. The helper prints a compact JSON result with the manifest hash. Exit0 is a verified exact seal; exit2 is refusal/failure. Existing diagnostic workspaces, plan output directories, or corresponding result phase outputs are refused. Partial failed output is preserved, not overwritten or resumed.

Diagnostics write their own `protocol.json` and generated `orders.json` under `R/diagnostic/{m1,m2}/`. Their production-revisions object and source receipt remain identical to the real protocol, but campaign IDs/result roots are distinct and marked diagnostic-only. The real `R/protocol.json`, `R/orders.json`, source receipt, and all pinned inputs are checked unchanged after sealing. **`client-diagnostic` never reads P0 files** and can run before the user-key-clearing diagnostic/capacity stage. Do not run that diagnostic after final P0 placement.

All modes verify and explicitly pin exactly the source receipt's **2 source archives + 5 original frozen overlays**, plus **2 cleanup compatibility files when declared** (exactly9 artifacts, not7), all 3 runtime files, cluster/layout, relevant binaries, PP, Python, this helper/shared guard, and every regular source/tool file under multinode, singlenode, and frozen script directories (excluding generated Python bytecode). The three post-P0 modes additionally verify `complete.json`, PP/cluster identity, every `seal-pins.json` path/hash, exact all-store fixture/selected mappings, and placement-command hash; every P0 seal-pins path is passed to the existing sealer. The original runtime/source/phase counts are not weakened.

The helper does **not** run or time diagnostics; parent orchestration supplies R1/R2/R3/R4, deadline/acknowledgement, telemetry, M2 placement command, reconciliation, and timing authorization. M2 diagnostic results are qualification evidence, never campaign results. The original P0 `placement-command.json` can be passed to R2; no DD mode change or cluster write happens here.

## Declared cleanup compatibility

The baseline remains `frozen_harness_revision=d94b4c61377d173b7c6884fde426a227dab0f0bb` and source `harness_commit` remains that same base. When protocol `harness_compatibility` exists, source receipt metadata must be identical and contain exactly: nonempty `description`, full `upstream_commit=066920ebb8d22c5618b31ef0a56fa7674c773cdb`, `client_harness_archive`, `patch_file`, `archive_sha256`, `patch_sha256`. Canonical paths are `R/frozen/cleanup-compat/client-harness.tar.gz` and `R/frozen/cleanup-compat/combined.patch`; both file hashes must match the metadata and the exact9 source-artifact map. Missing, extra, mismatched, source-only, or null declarations are refused. The exact7 base branch is allowed only when neither protocol nor source declares compatibility; a declared compatibility never accepts only7 artifacts.

Client modes pass the **declared patched archive**, not the original overlay, to `--harness-archive`. Generated plans therefore carry its SHA as `harness_id`. The original5 overlays remain pinned unchanged; deployed frozen scripts are separately pinned at their existing paths. Diagnostics preserve the compatibility metadata exactly. `sealed.json` records it and the selected archive path. Both metadata-bearing JSON inputs and both compatibility files enter the final pins. This helper verifies declared identities/hashes; the authorized five-file patch/archive-content/build audit remains separate. M2/P0 code is unchanged.

## Offline verification

`python3 -B verify_seal_offline.py` exercises the exact7 baseline; `python3 -B verify_seal_offline.py --compat` exercises the declared exact9 compatibility and rejection controls. Both use the **real existing sealer** in all four modes using private fake hash-only files under this subtree. `subprocess.run` is forbidden during those checks. It verifies the exact populations, isolated results/protocols, all P0 pins, all four overwrite refusals, source/real-protocol preservation, and manifest hashes. `verification-seal-live-base-v2.log` and `verification-seal-live-compat.log` contain the current pass outputs, including selected-archive `harness_id` assertions and metadata/path/hash/7-vs9 refusal controls. This is external planning validation, not a repository Bazel lane or fresh cluster qualification.
