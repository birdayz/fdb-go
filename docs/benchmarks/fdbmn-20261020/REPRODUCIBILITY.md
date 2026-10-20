# Reproducibility and remaining run-day inputs

## Recovered, verified and usable now

The previous campaign is `${FDB_PERF_HOME}/hcloud-multinode-vpc-20261014/`; its frozen sources/builders are in sibling `hcloud-rebench-20261014/`. The final production tree was `267a41434a81723b07e7150869d702636a1b91a9`; frozen harness was `d94b4c61377d173b7c6884fde426a227dab0f0bb`.

`build-check/README.md` and `build-check/artifact-audit.json` are the detailed verification record:

- All14 executables in the original rebench binary manifest are retained and match their original hashes. Both placementprobe executables also match their separate manifest.
- All four previous production source archives and all five final-tree frozen-harness overlays match their original source receipt hashes. The overlays'44 regular-file members match the d94 Git blobs. Their copies are under `frozen/overlays/` in this campaign.
- The complete frozen clientbench directory is28 files; both new external source exports contain exactly those28 files, byte-identical, with no later source files accidentally left behind.
- **Previous production and current rehearsal master both compile the frozen harness.** Seven Bazel targets built per tree, producing14 executables. Old:102.299s,999 actual sandbox/56 internal processes. Rehearsal master:85.442s,136 actual sandbox/877 disk-cache/56 internal processes. These are build action counts, not tests. Input manifests remained unchanged after the builds.
- All14 rebuilt executables report Go1.26.9. The Bazel SDK compiler hash equals the previous compiler receipt: `e90c4e12df236ea0c2e7536fd68da8a47fc0b585c19588bf6c1a70cfc296a0f9`. Host `go version` is not the build compiler. Both inspected production trees use go1.26.0 plus toolchain go1.26.9; verify the eventual merged tree again.
- The rebuilt old Go client is bit-identical to the archived client (`505d7fe8de93824cf844645dcd58be5e931a05dd0e2866c14f4bb0933d2c479e`). Six other old rebuild outputs are not byte-identical; local native-link/rpath/build environment differs. Do not describe those as exact-binary reproduction or mix old/new build environments in a measured comparison.
- The exact old runtime was recovered from `${FDB_PERF_HOME}/results/fdb-perf-20261009/baseline-complete-v3/artifacts/binaries/`:
  - fdbserver `179df8ec7292ad060fb7bd60a771ff4a8dd3989a8115638356c73feea4fc2c5f`
  - fdbcli `6dfe3328389fe8ff8175ddc080e278c5b07d1d2601f97079cba1194bfb2e41fb`
  - libfdb_c.so `e4ad7fed7b7580bf5f7b38f0a4279d37e698f69d534182e5de0b8beeef6cd1b1`
  All match original runtime receipts. Server/CLI identify7.3.77/source3ea44ce1d9003ad095e408039e1f755c319c4dfb. The unrelated local7.3.77 library has a different hash; a matching version string alone is insufficient.
- With LD_LIBRARY_PATH pointing to that exact runtime, archived CGo linkage resolves. Archived placementprobe-cgo noargs and unowned-store checks both exit2, empty stdout and no receipt; stderr diagnostics retained. They return before opening a cluster, so these were not cluster probes.
- Live preparation inventory listing matches the original17 IDs across10 kinds. Real teardown report-only selector sees zero campaign resources; its receipt has no actions. Offline refusal controls cover every protected resource, accidental campaign relabel and isolated ID/name protection, plus wrong-label/prefix controls. The post-teardown verifier was also tested with fixture-only fake lists: identical inventory passes; extra primaryIP, missing server and failed list all fail loudly (`provenance/verify-inventory-controls.json`).
- An **offline client-plan shape rehearsal** with the real rebuilt binaries/frozen scripts produced28 campaigns×18 runs,0 unsealed (`provenance/client-plan-rehearsal.log`). Its separate `work/client-plan-rehearsal/` uses the rehearsal master SHA and synthetic rendered cluster identity, is explicitly marked NOT-A-CAMPAIGN-SEAL, and was never executed. It proves existing sealer support for the three-arm mapping, not real cluster qualification or a final merged-tree plan.

No repository test lane was run: there are no source edits requiring `just test` here. No test binaries, benchmarks or cloud mutations were executed. Compiling a Bazel `*_test` target was **build only**, not a test pass.

## Tooling comparison

The prepared repository tools were exported read-only from master `ae14b220883c4985031d5659f8b34cd7ecece8a9` to `tooling/tools/benchcampaign/`. `provenance/tool-drift.json` compares matching archived Python files by name and group, with full hashes. For22 matching multinode Python files,19 are identical and only these three **test files** differ:

- test_build_reconcile_input.py
- test_client_campaign_mn.py
- test_reconcile_telemetry.py

Every compared runtime multinode script, compared singlenode Python helper and compared frozen clientbench script is unchanged. This claim is scoped to the files listed in that manifest, not a claim that all repository tools are unchanged.

Production `tools/clientbench` **did change** after d94 (pacing/lifecycle/run code). The full frozen overlay is therefore required; do not build the current clientbench harness HEAD or let later extra files survive beside the old overlay. Rehearsal master M2 benchmark source paths are already unchanged, but the same explicit overlay policy is used.

The original external `run-multinode.py` and `analyze_multinode.py` are **not parameterized reusable commands**: they embed the old campaign directory, remote paths, cluster/address assumptions, key path and manifest hashes, and assume the old analysis layout. They remain historical artifacts, not scripts to invoke unchanged. The prepared checklist uses repository R1/R2/R3/R4 scripts directly and records needed runtime rendering. A later external controller driver may mechanically substitute new private paths/identities and render the new sealed commands, but must preserve phase/workload stop semantics and never introduce hcloud mutations outside the approved provisioner/guard.

## What cannot be finalized or reproduced now

1. **RFC-263's merged production SHA does not exist in the prepared inputs.** It remains `TBD_AFTER_RFC_263_MERGE`. Current-master build evidence proves only rehearsal compatibility. The actual merged tree must build the same frozen harness and compiler before final sealing. No harness change is currently needed. If a future API change breaks it, use the minimal compatibility-only reviewed choice in PROTOCOL; do not silently replace the harness.
2. **Original Debian installers were not found in the documented bounded search** (fdb-perf, Downloads, /tmp, /var/cache, maxdepth4, matching fdb/foundation .deb names); this is not a machine-wide absence claim. Exact extracted runtime payloads are available. For ordinary cloud package installation, redownload the original7.3.77 packages and verify pinned SHA256 from previous setup-vm.sh before installation:
   - foundationdb-clients_7.3.77-1_amd64.deb: `b6c263723009bddbc30b6ae8cf9854340bd35cef14152abb0da8fb5af2b91254`
   - foundationdb-server_7.3.77-1_amd64.deb: `789f1ed4cbacc0fd7043da2217c9e57516e4ec94fa7140853ec22a7e715785cc`
   No download/installation was performed in preparation. Full OS package/kernel/image state is a fresh receipt, not recoverable merely from FDB binary hashes.
3. **Fresh hardware allocation, RTT, storage identities and shard placement cannot be reproduced exactly.** Re-run F0 and all P0 controls/attempts on fresh VMs; label actual per-case placement before timing. Never reuse the old cluster file/probe/placement receipt as new-cluster evidence, and never force the old partial-placement fractions after seeing timing.
4. **No final executable plans yet.** Only seed-derived orders can be frozen now. Real binary/cluster/source/fixture/placement inputs, final paths and manifest acknowledgement are needed for client-plans and testingb-plans. Templates under commands/ refuse the TBD SHA. An untimed R1 diagnostic clears user keys; run it before final P0 or restore/requalify placement afterwards.
5. **Original contaminated/unexecuted cases stay such.** Last campaign's multi-worker batchset50 failed the100MB tLog gate and batchset50x1000 remained unexecuted; there is no old clean baseline to recover for those rows. New runs retain identical cases/gates and disclose omissions instead of fabricating historical comparisons.
6. **M1 is intentionally now three-arm.** Six blocks remain, but six permutations once replace the old two-arm permutations repeated three times. The order seeds and balanced-block method remain; literal pairwise order is not identical. This is explicitly preregistered; compare old/new concurrently, not by pairing historical processes.
7. **Safety inputs remain run-day facts:** actual controller /32/public key, explicit provisioning approval, current quota/prices/image ID, unchanged protected inventory or owner-approved reconciliation, and one fixed T0+8h deadline/supervised guard. Dry-run192.0.2.1/32 and example deadline are deliberately non-live inputs.
8. **Implicit primary IPs:** provision.py does not label/name them and teardown_guard covers only four explicit resource kinds. The owner approved this exception without changing provisioning code: create one server per invocation, immediately list/assert every primary IP is assigned to a campaign server ID and auto_delete=true, and record server/IP IDs outside Git before any next server create. A failure stops and is reported by exact ID for owner correction. After teardown require the exact ten-kind inventory. If a primary IP survives, cross-check its ID against the recorded campaign-owned list and report it; never delete it automatically. Owner-only exact-ID remediation is distinct from broadening the guard.

## Artifacts to start with

- `PROTOCOL.md`, `protocol.json`, `orders.json`: preregistration and fixed experimental shape.
- `RUN-DAY.md`: build→qualify→seal→M2→M1→reconcile→teardown→inventory→write-up checklist.
- `provision-dry-run.txt`: actual no-execute all-four topology printed commands; `provision-staged-dry-run.txt` and per-role dry-run scripts prepare the required one-server execution stages.
- `commands/cloud-init.sh`, `commands/render-cluster.sh`: local rendering commands only, not yet run.
- `commands/teardown-{deadline,immediate,report-only}.sh`: exact label/prefix/protection lists; destructive variants prepared, not run.
- `verify_inventory.py`, `commands/verify-inventory.sh`: full read-only inventory check; original inventory snapshots were not overwritten.
- `provenance/`: preparation safety evidence, original protocol/orders copies, current read-only price/tool evidence and baseline hashes.
- `build-check/`: reproducible external source exports, build script/logs/manifests, binaries and refusal-only probe evidence (about7.6GiB). No Git worktree is used for these edits/build exports.
