# Perf acquisition COMPLETE — 2026-10-11

Do not restart any acquisition or reuse the SSH target. The VM was deleted successfully at **2026-10-11T16:41:58Z**; deletion action **<action-id>**, server<server-id> GET404 confirmed. Its former automatically allocated IPv4 primary-IP resource<primary-ip-id> also returns404. API evidence is in ../deletion/. No credentials were copied into artifacts or onto the VM.

Authoritative completion manifest: ../R17-R18-COMPLETION.json.

## R17

Original tracked task280 completed80segments/640processes,15:18:04–16:34:48UTC (76m44s). No rerun, failed run, interim inference or R16 pooling. All640 pass pinned validation. Allfive registered endpoints pass both99% joint circular moving-block intervals,L8/L16,100000resamples each.

Execution manifest SHA256:2161634738fd23cfd1e63098b5b66275e6cf0b1e0e3bead5017d37d428bdbc0f.

Archive ../fullstack-r17.tar.gz SHA256:4943768b532b2c57797c677f37b9381c10d922fbee81ef7821a3174a29d2e6f0.27153payload file hashes verified onVM and locally; archive SHA verified. Extracted ../fullstack-r17-extracted/fullstack-r17/. Results also in analysis/.

Independent audit in independent-audit/: no production analyzer/statistics code imported;640rawruns,2.4M measured+128k warmup rows,800pairedlogs,5GMs, both100kbootstraps via literal index expansion. All20CIbounds and both index-stream hashes match exactly.

## R18

18timings+2separate200-op traces complete16:40:09UTC; no failures or reruns. Analysis validated all20processes. Exploratory only,noCI/no shipping recommendation. go2/go8 GMs: meanlatency1.034391,p991.157216,windowCPU.927739,drained.918573,E-A.909458.

Execution manifest SHA256:7f3f224e8558ac141c05b661294f3123852356ac08db917ed8a36c02f2422cf3.

Archive ../fullstack-r18.tar.gz SHA256:2a3b3b79f88008d45cb8c06321525405c437caa13d42d7a6995e20904c6c3842.2132payload file hashes verified onVM and locally; archive SHA verified. Extracted ../fullstack-r18-extracted/fullstack-r18/.

Independent audit ../work-fullstack-r18/independent-audit/: no production analyzer/parser imported; all198CPU/latency pairedratios and33GM/ranges match exactly. Independent originalperTIDtrace parse+decimal timestamps+uniqueA–E signatures: everywhole/phase perTID+aggregate selected-syscallcount matches. Phasefutex1870→1139(go8→go2),epoll_pwait788→779,nanosleep1051→864. No causal timing inference or TID-role guess; thread-name observations were empty because bothtracesfinishedwithin1s.

## M1

Cancelled explicitly by perf-lead2 before R18: no cleared(A)SHA due suspected superlinear range-read regression, so no combined NEWhandoff and NO M1VMrun. Localharnessreview fixes anddraftartifacts remain ${FDB_PERF_HOME}/results/m1 for future work. Root's conditional extension was not used. No further VMhold or allocation authorized here.

Scope: one reimaged existing instance/freshcluster, not independent hostreplication. CPU E-A includesstartup/fixture/warmup/validation/CSV/GC/shutdown, not isolated steady-stateCPU. Product source/binaries were frozen; repository correctness/CI/review approval is separate. No repository changes, builds or tests were needed for these completed acquisitions.
