# RFC-259 screen drivers

Controller-side wrappers that produced the RFC-259 measurements in
[docs/benchmarks/rfc-259](../../../../docs/benchmarks/rfc-259/README.md). Each campaign
script runs `tools/clientbench/scripts/servercost.py` or `wirediff.py` (older
copies, `servercost-cyc.py` and `wirediff.py`, are kept here as they ran) against
a local three-process FoundationDB cluster, in alternating arm order, and appends
to one JSONL file. They assume the working directory `${FDB_PERF_HOME}/cpu-work`
with its `bin/` arm binaries and cluster file, and the server PIDs of the running
cluster; see [docs/perf/README.md](../../../../docs/perf/README.md#paths-in-imported-documents).

| Script | Committed result |
|---|---|
| `wirediff-campaign.sh` | `results-wire-master-vs-cgo.jsonl` |
| `writegap-cyc.sh` | `results-closed-loop-cycles.jsonl` |
| `grv-observe-campaign.sh`, `grv-observe-64.sh` | `results-grv-observe.jsonl`, `results-grv-observe-64.jsonl` |
| `grv-write-confirm.sh`, `current-gc-confirm.sh` | `results-grv-write-confirm.jsonl`, `results-current-gc-confirm.jsonl` |
| `grv-dispatch-screen.sh`, `grv-send-noack-screen.sh` | `results-grv-dispatch-screen.jsonl`, `results-grv-send-noack-screen.jsonl` |
| `write-coalesce-screen.sh`, `coalesce-opt-campaign.sh`, `coalesce-final-campaign.sh` | `results-write-coalesce-prototype.jsonl`, `results-write-coalesce-prereview.jsonl`, `results-write-coalesce.jsonl` |
| `header-reserve-screen.sh`, `lazy-timeout-screen.sh`, `native-yield-screen.sh` | `results-header-reserve-screen.jsonl`, `results-lazy-timeout-screen.jsonl`, `results-native-yield-screen.jsonl` |
| `paced-campaign.sh`, `paced2-campaign.sh` | `results-paced-load-sleep.jsonl`, `results-paced-load.jsonl` |
| `wdelay-ab.sh`, `turn2-campaign.sh` | `results-writer-turn-prototype.jsonl`, `results-writer-turn.jsonl` |

The other scripts are exploratory screens whose output was not committed
(`broad-campaign.sh`, `commit-noack-*.sh`, `conflict-campaign.sh`,
`grv-sample-screen.sh`, `native-campaign.sh`, `native-grv-compact-*.sh`,
`read-coalesce-screen.sh`, `read-send-noack-screen.sh`, `turn-ab.sh`), one-off
probes (`run.sh`, `sc.sh`, `sc2.sh`, `srv.sh`, `roundtrace.sh`, `waves.sh`,
`split*.sh`) and their summarizers (`*.py`). The prototype patches they measured
are summarized in the result write-ups; the committed one is
[prototype.patch](../../../../docs/benchmarks/rfc-259/prototype.patch).
