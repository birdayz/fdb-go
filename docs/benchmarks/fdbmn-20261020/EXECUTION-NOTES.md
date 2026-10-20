# Execution notes — fdbmn-20261020

## Authorized inputs

Owner GO (root4155/4157): run on current master `fa8725f1dcf6ff3ac66825ab6e3e819efbc0abcc`, not waiting for RFC-263. Historical Go production `267a41434a81723b07e7150869d702636a1b91a9`; Apple binding over exact7.3.77. Original harness base d94b4c613. Fixed order M2rl,M2skip,M1w1,M1w64,M1w8,M1w32. No Git worktree edits.

Eight-hour lifetime started09:55:06Z on2026-10-20; deadline **17:55:06Z**. Guard future `86bf334e-3a9b-4f9f-b43a-5aaac33ec79c:37` was running before any cloud create; state.json records it. Label and prefix are `fdbmn-20261020-NONCE`; private /24 is10.89.20.0/24.

## Created campaign resource IDs

| Kind | ID | Name/role |
|---|---:|---|
| server |<campaign-resource-id>|client|
| server |<campaign-resource-id>|node1|
| server |<campaign-resource-id>|node2|
| server |<campaign-resource-id>|node3|
| network |<campaign-resource-id>|campaign private network|
| firewall |<campaign-resource-id>|campaign firewall|
| SSH key |<campaign-resource-id>|campaign controller public key|
| primary IP |<campaign-resource-id>|assigned to client<campaign-resource-id>|
| primary IP |<campaign-resource-id>|assigned to node1<server-id>|
| primary IP |<campaign-resource-id>|assigned to node2<server-id>|
| primary IP |<campaign-resource-id>|assigned to node3<server-id>|

Every primary IP was checked immediately after its one-server create stage, before the next create: new ID, assignment to the recorded campaign server, auto_delete=true. No primary IP is ever directly deleted by this campaign; survivors go to the owner for exact-ID review. The original17 protected resources remain the immutable teardown comparison set.

## Pre-measurement findings and corrections

1. **Distribution service stop:** Ubuntu24.04's SysV-generated foundationdb service stayed active after `systemctl disable --now` plus mask. The explicit `systemctl stop foundationdb.service` correction stopped its whole service cgroup on all four new hosts; zero distribution fdbserver was verified before installing campaign units. Logs: bootstrap-*.log, bootstrap-resume-*.log, bootstrap-correction.json. No existing machine was contacted.
2. **Cluster connection ID:** renderer accepted `mn20261020_NONCE`, but FDB7.3.77 rejected it with2104 Connection string invalid. Before initial configure, regenerated with alphanumeric `mn20261020NONCE`; retained old render and failed-unit logs. Database then created successfully. No timed data or placement receipts existed at correction time.
3. **Frozen lifecycle incompatibility:** first current-master capacity smoke completed32GRVs with0transaction errors, but the d94 drain guard rejected38 queued cleanups despite38 executed. Source uses runtime.AddCleanup legitimately. This was a harness contract failure, not an observed transaction correctness defect; old Go smoke passed. Original raw diagnostics remain `diagnostic-artifacts/capacity/`.
4. **Declared compatibility amendment, root4166/4167/4170:** exact #948 lifecycle.go+cleanup-chain test from `066920ebb8d22c5618b31ef0a56fa7674c773cdb`, matching Python run.py validator predicate removal, pending-vs-completed fixtures and scope prose only. No workload, measured window, CPU-accounting, numeric analysis, pacing, BUILD, main or output-shape change. All three client arms rebuilt from the identical28-file amended archive.
   - Combined patch SHA256: `31bc349b7a1ef71fc5bbf300ed728fe536e161f5610a1187edc718e63fddb007`.
   - Archive SHA256: `76990b867f337bdad0132cc69aa3f6f9a9bcf0cb0859cc8c7f8514def684f508`.
   - All use originalGo1.26.9/compiler identity. Rebuild ended10:18:21Z and Bazel was shut down; root was notified immediately so the local timing screen could resume.
   - New-master32GRV repeat passed with amended hashes/source/runtime verified; root notified. Full-count capacity checks are separate qualification artifacts, never included in campaign ratios.
5. **External controller review:** tracked bounded remote units and exact cgroup quiescence prevent a lost SSH acknowledgement from leaving an unobserved runner after R3 stops. Launch intent is recorded before dispatch. Node sampler/import/cluster/layout and all client/R4 code are checked against acknowledged seal hashes; start/stop receipt identities/hashes must match. Unknown runner state fails closed. These are external control safeguards, not timed-harness changes.

## Placement continuation and execution budget

- The first P0 settle helper incorrectly treated transient initialization queues/missing moving-data fields as an immediate fatal error. Root4200 authorized an exact polling correction and same-attempt continuation within the original1200-second deadline. That continuation achieved123.849s healthy zero movement before the original deadline, froze DD, then an added non-protocol full-ballast census encountered1007 on the split store. No ballast clear had occurred. All original outputs and the failed read were retained.
- Root4218 authorized abandoning that added census, not weakening canonical integrity or mixing versions. The reviewed frozen continuation validates the completed on-time settle, DD-off epoch, source/history/roles and no-clear boundary, obtains an empty-scratch provenance snapshot only, then resumes the original ballast-clear/empty-verify/strict canonical sequence. It does not replay settle, reseed attempt1, or retry the failed census. Exact p0 SHA256 `f40d150614377f9a896b95cc0973cbad8d5c8bf17bb22a3e5565f0bec2939c84`; resume SHA256 `a40c682317556bbf42936d5ee9f0c7464bd92eadeec94f3f530355e835f295af`. Independent ACK plus11 recovery/12 existing checks preceded launch11:21:49Z. A nominal25-poll assumption was corrected before use: actual elapsed>=120s and original-deadline proofs remain required; status/hash command time means a valid proof need not contain25 polls.
- Integral partial attempts1/2 proceed through the original authorized first-all-pass-else-attempt3 rule; no best-cell or best-attempt selection. New output/sealing root is `/srv/fdb-bench/provenance/p0-continuation-2`.
- Root4187's budget rule dropped **M1w32** after setup passed11:15Z. It is unexecuted-for-budget, not a performance result; capacity qualification is never substituted. Nominal protocol/orders stay intact. Remaining order is M2rl→M2skip→M1w1→M1w64→M1w8. Drop w8 next only if needed and notify the owner first.
- P0 completed11:30:49Z rc3 integral partial with selected attempt3. Small and planskip cells all process-disjoint; split rows1000 has fraction0.82, remaining split cells1.0. All machine-disjoint fractions0.0 are descriptive. DD remains off and labels/fractions are retained, not upgraded to cross-machine claims.
- M2 diagnostic1 correctly failed frozen audit validation: the external diagnostic plan requested1 repeat/1x, while the unchanged frozen parser requires at least6 before/after audit records (3 pairs). Its sole ordinary process emitted PASS and a valid pair; no remote arm or measured campaign ran. Its contaminated R4 verdict remains evidence. Root4236 authorized only a fresh diagnostic/m2-v2 with3 repeats/same1x using the existing sealer functions. Original sealer argv were replayed with fresh workspace paths; no helper extension, frozen runner/parser/validator, live protocol or final3repeat/3s budget changed. Seal SHA256 `00509d22d86b0307a00e824386a8ac42763a91fe7e56db8e47535996e63b8802`.
- M2 diagnostic2 correctly refused before any process because the original P0 placement work-dir reused diagnostic1's case/before namespace. Root4245 authorized only a derived template changing `--work-dir` to `{output}.work`, pinned via unchanged existing sealing functions. Diagnostic3 used3 repeats/same1x and completed R4-clean1/1; the two failed diagnostics remain separate evidence. Final measured M2 uses that same output-specific work directory, explicit pinned template `/srv/fdb-bench/provenance/p0-continuation-2/placement-command-output-work.json`. No source/runner/parser/placement-probe/fixture/identity/gate change.
- Final readiness11:41:42Z: client manifest SHA256 `0a329eac7a1d366bf53cf5385056cb50f486bdf424b9354b96bd8ada4461c334`; M2 manifest SHA256 `80f0bc49b0a7b0a04bf97feefe6d61d1baebf87d785989dc255bf8ab1ecceee5`. Exact plans/seals/protocol/source receipts archived in `provenance/final-sealed-plans.tar.gz` (SHA256 `8f2ce78526dd6431885a48a0e187211a5b31cb3938dfa076bd76b3102577a06d`). Nominal populations remain28 client campaigns×18 processes and M2rl12×144/M2skip4×48. First measured phase M2rl launched11:41Z; all preceding data are qualification-only.
- Operational collection/teardown/inventory completion is17:30Z; provider guard remains17:55:06Z. The effective runner lifetime17:20Z retains900s measurement reserve, plus600s phase collection and900s final operational reserve. Five remaining full phase caps+collection+final reserve total19050s, so worst-case readiness must be no later than12:12:30Z. No measurement has run during qualification/P0.

## Measured phase outcomes

- **M2rl:** ended12:33Z; controller rc3, R4 complete12 cases:9clean/3contaminated/0unverifiable. Three complete-case omissions, without retries or partial ratios: split-limit1-reverse, split-rows10-forward, split-rows1000-forward. All failed the unchanged pre-arm client maximum-per-CPU busy threshold0.15 (observed0.151515/0.158416/0.151515); aggregate client busy3.39/4.03/3.78%, steal0. All other quiescence groups passed. This is environmental exclusion, not a demonstrated production correctness error. Runner quiescent; R3/results/R4 collected. Durable detail: `provenance/M2rl-omission-causes.json`.
- **M2skip:** launched12:33Z, ended12:47Z with the same final manifest and explicit output-specific placement template. R4 complete4 cases:3clean/1contaminated/0unverifiable, controller rc3. Whole-case skip3-rows10-forward omitted without retry for unchanged pre-arm client CPU gates (aggregate6.78%>5%, max CPU23.23%>15%, steal0). Runner quiescent; R3/results/R4 collected. No source, protocol, gate, or pacing change between phases. Approved DD-on/healthy-zero settling boundary follows; no further M2 after this boundary.

- **M1w1:** DD-on boundary completed12:50Z with121.474s healthy zero, stable roles and pins. Phase ended13:02Z rc3: R4 complete7 cases,4clean (grv/get100/get1000/range100),1contaminated (set100),2unverifiable (batchset50/batchset50x1000 not reached). Unchanged pre-arm client max-CPU busy17.17%>15% rejected the first set100 arm before its process ran (aggregate4.01%, steal0); existing phase driver stopped at that failure. Entire three write comparisons remain N/A; no retry, capacity substitution, or partial-arm salvage. Runner quiescent; R3/results/R4 pulled. M1w64 launched13:02Z unchanged, then M1w8.

- **M1w64:** ended13:13Z rc3: R4 complete7 cases,5clean (grv/get100/get1000/range100/**set100**),1contaminated (batchset50),1unverifiable (batchset50x1000 not reached). The first new-Go batchset50 arm completed32000/32000 commits with0 errors/retries, persisted validation OK and complete drains, then the unchanged post-arm server gate rejected tLog queue426,278,385 bytes>=100MB. Entire batchset50 comparison N/A; no retry/single-arm throughput claim. Batchset50x1000 remains unreachable under the stopping policy. Runner quiescent; R3/results/R4 pulled. Final measured M1w8 launched13:13Z.

## No A1 re-attempt; polling attribution and operational deviation

Root4333 explicitly declined A1: **no re-attempt in this campaign**. M1w1 set100 is contaminated because its first arm never started after a pre-arm CPU rejection; batchset50 and batchset50x1000 are unreachable/unverifiable because the preregistered conservative stop-on-runner-failure policy stopped the phase. This write-case loss is a **protocol consequence**, not missing data to be recovered using capacity qualification. Originals remain; A1 executions=0. Durable decision: `provenance/no-A1-decision.json`. The owner is putting w1 write cases plus a pre-arm burst fix on the next campaign backlog.

Existing telemetry shows5/5 failed pre-arm windows proxy-overlap a controller SSH/systemctl status poll. Bursts are user+system time on benchmark CPUs8/11/14/11/10, not steal; pulls happen after phase exit. Nearby process snapshots show new systemd --user/(sd-pam) managers. These support SSH-login/PAM churn, but failed gates occur before process snapshots and quiescence JSON lacks start/end clocks, so PID-level cause is not proven. No host probe was added during measurement.

Root4334 authorizes a **pure operator-controller deviation only at the w64→w8 boundary**: reuse a local multiplexed SSH connection and reduce status polling from10s to60s. The local external controller/transport are not sealed manifest inputs; no ten-second polling interval is declared in the frozen protocol. New copies `execution/run_phase_ops60.py` and `remote_ops60.py` preserve the original running w64 source untouched. New SSH options are ControlMaster=auto, ControlPersist=120s, and a campaign-private0700 ControlPath; no host configuration changes. Seal/workload commands, timing, gates, budgets, R3/R4 and teardown protections remain unchanged. Activation was recorded at **13:13:19.616081Z**, only after w64 was quiescent, error-free in orchestration and fully collected/reconciled. Exact ACKed hashes: controller `ff09735342dd74a7e68ff9fe5113ebf37b8bafefdcdb619f9123cd280d3452d3`, transport `8fda5f40a931e92962887941de89534cf633e9aa4ef878418d1bf9c50117462b`. Thirteen existing fake controller controls plus delayed-runner/SSH safety/options controls passed without host contact. Plan, ACK and activation receipts are `provenance/polling-ops60-{plan,source-ack,activation}.json`. W8 alone uses the copies; w64 never did.

## Upstream follow-up (owner to track)

Current master's `tools/clientbench/scripts/run.py` still rejects cleanup-counter growth even though #948's Go drain legitimately accepts completely executed, stable cleanup chains. The matching campaign-only validator correction is in the declared patch and regression fixtures. **This is a latent upstream campaign-script bug**; the owner said they will put it in TODO. No repository source was edited by this campaign. The renderer's over-permissive underscore check and the SysV stop behavior are also recorded above rather than hidden by a retry.

## Evidence locations

- state.json, campaign-owned-primary-ips.json; per-stage provision/IP checks in provenance/.
- build-check/{EXECUTION-BUILDS,CLEANUP-COMPAT-BUILDS}.md and machine-readable JSON/hash manifests.
- frozen/overlays/ retains all original inputs; frozen/cleanup-compat/ retains the exact declared addition.
- provenance/f0-* and status-f0.json: exact runtime hashes, sibling maps, private routes, affinities and100-packet ping receipts. Fresh cluster check-roles passed17processes/9storage/3coordinators; client has no fdbserver.
- diagnostic-artifacts/, diagnostic-controller/, phase-controller/, telemetry/, reconcile/ and results-raw/ hold later stage evidence. Only complete clean R4 cases may enter the final write-up.
