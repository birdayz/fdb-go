# RFC-282 four-machine repeat: measurement extensions

Preparation/tooling only. These files do not authorize provisioning or launch a
campaign. No historical publication, frozen workload, quota, arm order, original
R3/R4 safety gate, placement control or R4 omission rule is changed. The main
campaign is the full prior matrix: seven M1 workloads at workers1/8/32/64 (28cases,504processes), plus
M2rl12cases and M2skip4cases (192processes/576testing.B repeats). Optional linger
is a separate preregistered phase, never a substitute for primary cases.

The old campaign exporters contain historical population assertions and skip
w32. `export_results.py` instead validates the exact44-case protocol and joins
**every** case to full-phase R4 and the canonical frozen adapter's analysis.
It emits clean whole-case latency/throughput/client estimates and separate
observed cgroup-accounting deltas under `fdbmn-rfc282-public-results/v2`, not
actual-window server CPU estimates. Contaminated/unverifiable cases stay
visible with public-safe reason categories and SHA256 identities. Exact diagnostics
stay in the hash-pinned private R4 report; addresses, paths and resource names are
never copied into public coverage. Missing phases fail rather than inherit October's
budget omission. Zero clean cases are coverage, not performance success. All six phases
must be passed to the existing analysis adapter; no omitted-phase override hides
real data. Existing numerical helpers and strict raw-count/CPU/CSV validators are
reused, not forked. `m2_export.py` is the old M2 accounting/clustered-bootstrap
export with only population/path constants parameterized.

## Freeze before measurement

Pin exact pre-RFC282 merge-base and merged master, all binary/compiler/native
library identities, protocol/orders, adapter, exporter/helpers and every input
manifest. Both Go trees and CGo use one toolchain and byte-identical amended
frozen harness. The [measured fence overlay](fences/README.md) is a separate,
hash-pinned amendment on top of d94b4c613+#948; apply every runtime and Go-test
hunk identically to A/B/C, verify complete original/patched source hashes and
archive member populations, then rebuild all arms. The patch's actual Go controls
must execute in the frozen-export validation before qualification; the repository
Python patch-integrity controls alone do not compile or exercise that Go code.

The new protocol must preregister `measurement_extensions` with:

- `throughput_preregistered: true`;
- `server_cpu_source: exclusive-cgroup-v2-usage-usec`;
- `server_cpu_metric: observed-cgroup-accounting-delta`;
- `server_cpu_actual_window_available: false`;
- `process_cpu_interval_ns: 10000000` (100Hz);
- `process_cpu_min_inner_intervals: 10`;
- `process_cpu_max_time_envelope_ratio: 1.2`;
- `process_cpu_clock_reserve_ns: 2000000` (1ms per clock);
- `process_cpu_phase_correlation_range_ns: 1000000`.

These are fixed measurement-policy fields, not runnable provisioning data. No
new protocol or launch configuration is supplied here. Source receipts retain
both amendment hashes and the resulting complete harness archive hash. New
results must not claim the prior archive hash names the fence-amended bytes.

## Observed cgroup accounting: separate from original R4 safety

Keep original R3 **1Hz** sampling and unchanged R4 safety windows/gates. A second
instance of the reviewed `node_sampler.py`, pinned to housekeeping CPUs0–1,
samples at `--interval 0.01 --cgroup-cpu`; it writes exclusive phase directories
under `telemetry-cpu/<phase>/<node>/`. Supply the exact generated FDB unit list
with repeated `--unit` (seven units on node1, five each on node2/3); cgroup mode
refuses implicit/discovered or duplicate units. It adds observation only, no status
polling in per-operation paths. All17 campaign `fdbserver` MainPIDs, their starttimes and
`/proc/PID/stat` user/system ticks are retained. The base1Hz sampler also retains
these fields additively using the reads it already made. Proc diagnostics retain
utime+stime, not child CPU. Neither proc ticks, other host processes, host
`/proc/stat` busy time, guest duplication nor the sampler's own process is added
to the observed server cgroup-accounting deltas.

Both start/stop receipts include real positive `SC_CLK_TCK` for diagnostics,
bootID, exact unit population/PIDs and chrony tracking. The authorized accounting
source is cgroup-v2 `cpu.stat usage_usec` for exactly17 private systemd FDB unit
cgroups. Require canonical `/sys/fs/cgroup/system.slice/fdb-bench-PORT.service`
ControlGroup paths, cgroup-v2 evidence, unique device/inode identity, no subgroups,
`cgroup.procs` exactly MainPID and `cgroup.threads` exactly its FDB thread IDs from
`/proc/MainPID/task`. Membership and directory identity must remain identical in
every sample and start/stop receipt. A helper/foreign task, group recreation,
restart, unsupported cgroups or missing/reset usage counter **blocks this
accounting observation**, not permission to substitute coarse proc ticks.
User/system microsecond components are retained if both available, never
fabricated as zero.

The sidecar's stop receipt hashes all sample bytes. Every source/receipt/sample/
parser must be pinned in the campaign input manifest. Main PID/starttime/boot/HZ
change, missing process/counter, malformed sample, negative accounted-counter delta,
gap>20ms, adjacent clock step>1ms, phase-wide range of
`realtime_ns-monotonic_ns`>1ms or overlapping sample read brackets disqualifies
accounting observations for the phase. This phase-wide check also rejects
accumulated sub-millisecond steps; individually small adjacent changes are not
proof of clock stability. No missing counter becomes zero.
Original R4 failures continue to exclude the whole case from all comparisons.
A CPU-specific failure is visible as `CPU-unavailable`; it does not rewrite R4.

M1's fence overlay serializes **the exact original start/end time.Time values**
that define monotonic `measured_wall_ns`. UTC/UnixNS and monotonic differences
must agree within1us (a temporal clock-step/correlation check, not a CPU-accuracy
bound or a new throughput denominator); measured UTC fences must lie inside R4's
lifecycle run. All quotas, fixture/warmup exclusions and worker finishing tails
stay unchanged. These fences select accounting observations; they do not turn
accounted counters into actual-window execution measurements.
M2 testing.B has no such fences: select server accounting samples around explicitly
**lifecycle** windows including fixtures/calibration/audits/shutdown. GNU-time
client CPU retains its existing lifecycle scope. Never label M2 CPU steady-state
CPU/op or its ns/op mean as p50/p99.

The temporal sample-selection tolerance is the sum of client/server chrony root
distances `abs(offset)+dispersion+delay/2`, taking the maximum over the phase and
sidecar receipts, plus the existing2ms correlation reserve (1ms per clock), then
1ns timestamp quantum for exact M1 fences or1ms lifecycle timestamp allowance
for M2. The server phase-wide correlation guard and client's measured UTC versus
monotonic wall check remain required. These checks do not observe arbitrary
unsampled client clock changes or repair clock steps. Original R4 still uses its
larger `+1.001s` safety reserve. Do **not** shrink R4. The separate accounting
selection retains each node's actual counter-read brackets; its inner/outer
windows describe temporal eligibility only, not physical CPU bounds.

`cpu.stat usage_usec` exposes scheduler-accounted cumulative runtime. Reading or
flushing it does not force pending runtime of running threads to be accounted.
The accounting debt at each endpoint is unquantified: a delta observed between
two reads can include runtime originating before the bracket and can omit runtime
executed inside it. Microsecond formatting is not a freshness or execution-error
bound. **Actual-window server CPU is unavailable.** No kernel/perf source,
`/proc/stat` forcing or process-clock substitution is introduced.

For each exclusive unit, retain the difference between the selected cumulative
`usage_usec` observations. Sum disjoint unit populations within each node and
across the fleet exactly once. Report `observed_accounted_cpu_usec` and
`observed_accounted_cpu_seconds` (microseconds /1e6), retaining per-node values
and each node's actual selected sample brackets. Fleet totals sum those observed
node deltas; differing node brackets are not one common execution window. Proc
user/system ticks remain raw diagnostics only, NEVER additions or fallback.
Do not attribute these deltas to execution inside a client or sampled window,
publish actual-CPU lower/upper bounds, CPU/op, cores or server-CPU ratio confidence
intervals, interpolate, subtract an idle/baseline delta, or invent a server
completion fence. Client latency/throughput and existing client CPU accounting
remain separate metrics with their existing scopes.

For M1, still require at least10 complete inner sample intervals and sampled outer
bracket wall<=1.2×measured wall. These are **temporal-selection guards only**, not
physical CPU bounds or proof of counter freshness. An inadequately covered window
is **CPU-unavailable**, never interpolated or made longer after results. Missing,
malformed or unverifiable accounting evidence in any compared arm/block makes the
**whole case** CPU-unavailable; do not publish partial-arm or surviving-block CPU
comparisons. Latency/throughput remain visible if original R4 permits. Any separate
ancillary phase needs its own protocol and cannot consume primary reserves.

Public output uses `server_cgroup_accounting`. Available observation rows have
`status: observed`, per-node and fleet `observed_accounted_cpu_usec` /
`observed_accounted_cpu_seconds`, and `actual_window_cpu_available: false`.
Unavailable cases retain explicit coverage/reasons rather than zero deltas or
partial arms. The protocol must seal both
`server_cpu_metric: observed-cgroup-accounting-delta` and
`server_cpu_actual_window_available: false`
alongside every existing source/policy field; missing or conflicting semantics
must refuse rather than inherit the superseded bounds policy. Exact field names,
units, closed accounting schemas and a synthetic section fixture are pinned in
[the v2 JSON contract](JSON-CONTRACT.md).

## Controller lifecycle

`controller.py` subclasses the historical ops60 controller rather than changing
its sealed R1/R2 commands, R3 gate or R4 reconciliation. It requires an explicit
external `--campaign-dir`; SSH retains created-ID/protected-inventory checks,
pinned host keys, ControlMaster and60s runner polling. Pin the new controller,
server CPU module and both imported ops60 modules in the acknowledged manifest.
The original controller hash and the extension's separate pin receipt together
name the executed source; neither is a substitute for the other.

Preflight rejects an unsealed cgroup source/observed-metric policy, missing
helper pins, reused CPU unit/cgroup or existing CPU output directory. All three
bounded sidecar intents are persisted before first dispatch. Each sidecar has its own exact
systemd unit, CPUAffinity0–1, control-group kill mode and RuntimeMaxSec equal to
phase cap+900s; all three start receipts must match the original R3 MainPIDs and
be active before any measured runner starts. Recheck lifetime after readiness.
A lost launch acknowledgement follows the inherited stop-and-prove boundary,
not a guess that nothing started.

Collection preserves the original R3 intent population, then restores the full
durable list and stops/proves each CPU unit quiescent. Copy/hash raw CPU evidence
both to controller and client's final `telemetry-cpu/<phase>/<node>` paths before
R4/export. Bad stop receipts still retain adverse raw artifacts. Unknown unit
quiescence or collection failure remains a controller failure; no next phase or
clean export is authorized by a disconnected SSH session. The existing hard
resource teardown guard is independent and must still be armed before creation.
Use this staged controller with the same phase/manifest/protocol/orders/results
flags as the original, plus `--campaign-dir`; diagnostic and measured outputs
remain separate. This file supplies no real campaign state or launch values.

## Qualification, runtime budget and publication

Before arm timing on dedicated hosts, capture idle sampler-off/on controls with
identical layout, CPUs and clocks. Verify100Hz gap/read latency, housekeeping CPU,
all17 exclusive cgroup/PID/thread populations, no FDB-core interference/recovery/
role change and all original quiescence/steal/health gates. Run a controlled FDB
unit accounting positive/idle control to check `usage_usec` units, monotonic
counter behavior and qualitative response to independently observed user+system
activity. This is not a freshness, actual-window attribution or execution-bound
proof; idle observations are not subtracted. Retain off/on sampler overhead and
membership proofs before arm timing. Retain control commands/pins/raw counters
and sampler overhead. Failure is a qualification blocker: stop/report/teardown,
not permission to relax gates, lengthen primary quotas or tune a favourable rate.
Parser/unit controls run locally first; actual hardware overhead qualification
belongs to F0/P0 after explicit launch authorization, never assumed from mocks.

The authorized planning ceiling is four ccx43 hosts, total lifetime16h from first
provision, fixed guard armed first and no extension. Refreshed fsn1 gross server
rateEUR0.526337/host-hour implies64started host-hours=EUR33.685568 gross server
compute plus IPv4. Fresh inventory protections and exact owner-provided merged
SHA/launch confirmation remain mandatory before cloud mutation. Optional phases
run only if separately preregistered and enough reserve remains after primary.

Canonical numerical analysis remains the pinned existing campaign adapter, with
explicit new `--campaign`, `--protocol`, all six `--phase` values and fresh
`--output`. It checks frozen sources/builds/raw audits/controller/R4 before any
estimate. Stage this exporter and historical pure helpers under the same campaign
`tooling/tools/benchcampaign/campaigns/` layout. The CLI refuses helpers executed
outside the manifest-pinned campaign root. Invoke the staged `export_results.py`
with `--campaign ROOT --inputs-manifest MANIFEST --analysis analysis/NAME
--redactions provenance/REDACTIONS.json --output-dir FRESH`; input paths are
relative to ROOT, not the private archive parent. Every read SHA-verifies. The
redaction gate rejects private IDs/names/IP literals/paths/credentials. Publish
only checked aggregate `results.json` under a **new** docs/benchmarks campaign;
old reports and their byte-exact exports are untouched. Register new docs in
`scripts/sync-website-benchmarks.py`, generate/check website mirrors and charts,
and run the established Hugo/site checks before publication.

Validation: repository `just test` through the shared lane lock; final clean-tree
receipt through `tools/lanes/run-verified-lane.sh`. No direct go test or selected
Bazel target is a replacement. Report executed/cached target populations. This
README is methodology/preparation, not a test/build/qualification/run receipt.
