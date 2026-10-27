# RFC-282 four-machine repeat: measurement extensions

Preparation/tooling only. These files do not authorize provisioning or launch a
campaign. No historical publication, frozen workload, quota, arm order, CPU gate,
placement control or R4 omission rule is changed. The main campaign is the full
prior matrix: seven M1 workloads at workers1/8/32/64 (28cases,504processes), plus
M2rl12cases and M2skip4cases (192processes/576testing.B repeats). Optional linger
is a separate preregistered phase, never a substitute for primary cases.

The old campaign exporters contain historical population assertions and skip
w32. `export_results.py` instead validates the exact44-case protocol and joins
**every** case to full-phase R4 and the canonical frozen adapter's analysis.
It emits only clean whole-case estimates; contaminated/unverifiable cases stay
visible with reasons. Missing phases fail rather than inherit October's budget
omission. Zero clean cases are coverage, not performance success. All six phases
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
- `process_cpu_interval_ns: 10000000` (100Hz);
- `process_cpu_min_inner_intervals: 10`;
- `process_cpu_max_time_envelope_ratio: 1.2`;
- `process_cpu_clock_reserve_ns: 2000000` (1ms per clock);
- `process_cpu_phase_correlation_range_ns: 1000000`.

These are fixed measurement-policy fields, not runnable provisioning data. No
new protocol or launch configuration is supplied here. Source receipts retain
both amendment hashes and the resulting complete harness archive hash. New
results must not claim the prior archive hash names the fence-amended bytes.

## Process CPU: separate original R4 safety and fine-grained accounting

Keep original R3 **1Hz** sampling and unchanged R4 safety windows/gates. A second
instance of the reviewed `node_sampler.py`, pinned to housekeeping CPUs0–1,
samples at `--interval 0.01 --cgroup-cpu`; it writes exclusive phase directories
under `telemetry-cpu/<phase>/<node>/`. Supply the exact generated FDB unit list
with repeated `--unit` (seven units on node1, five each on node2/3); cgroup mode
refuses implicit/discovered or duplicate units. It adds observation only, no status
polling in per-operation paths. All17 campaign `fdbserver` MainPIDs, their starttimes and
`/proc/PID/stat` user/system ticks are retained. The base1Hz sampler also retains
these fields additively using the reads it already made. Only utime+stime count;
child CPU, other host processes, host `/proc/stat` busy time, guest duplication
and the sampler's own process are **not** added to server-process CPU.

Both start/stop receipts include real positive `SC_CLK_TCK` for diagnostics,
bootID, exact unit population/PIDs and chrony tracking. The preferred measurement
source is cgroup-v2 `cpu.stat usage_usec` for exactly17 private systemd FDB unit
cgroups. Require canonical `/sys/fs/cgroup/system.slice/fdb-bench-PORT.service`
ControlGroup paths, cgroup-v2 evidence, unique device/inode identity, no subgroups,
`cgroup.procs` exactly MainPID and `cgroup.threads` exactly its FDB thread IDs from
`/proc/MainPID/task`. Membership and directory identity must remain identical in
every sample and start/stop receipt. A helper/foreign task, group recreation,
restart, unsupported cgroups or missing/reset usage counter **blocks this CPU
claim**, not permission to substitute coarse proc ticks. User/system microsecond
components are retained if both available, never fabricated as zero.

The sidecar's stop receipt hashes all sample bytes. Every source/receipt/sample/
parser must be pinned in the campaign input manifest. Main PID/starttime/boot/HZ
change, missing process/counter, malformed sample, negative execution delta,
gap>20ms, adjacent clock step>1ms, phase-wide range of
`realtime_ns-monotonic_ns`>1ms or overlapping sample read brackets disqualifies CPU
for the phase. This phase-wide check also rejects accumulated sub-millisecond
steps; individually small adjacent changes are not proof of clock stability. No missing counter becomes zero.
Original R4 failures continue to exclude the whole case from all comparisons.
A CPU-specific failure is visible as `CPU-unqualified`; it does not rewrite R4.

M1's fence overlay serializes **the exact original start/end time.Time values**
that define monotonic `measured_wall_ns`. UTC/UnixNS and monotonic differences
must agree within1us (a clock-step/correlation qualification check, not a new
throughput denominator); measured UTC fences must lie inside R4's lifecycle run.
All quotas, fixture/warmup exclusions and worker finishing tails stay unchanged.
M2 testing.B has no such fences: report its server and GNU-time client CPU over
explicitly **lifecycle** windows including fixtures/calibration/audits/shutdown.
Never label M2 CPU steady-state CPU/op or its ns/op mean as p50/p99.

The accounting clock bound is the sum of client/server chrony root distances
`abs(offset)+dispersion+delay/2`, taking the maximum over the phase receipts and
sidecar receipts, plus an explicit2ms correlation reserve (1ms per clock), then
1ns timestamp quantum for exact M1 fences or1ms lifecycle timestamp allowance
for M2. The server phase-wide correlation guard and client's measured UTC versus
monotonic wall check remain required. The envelope assumes at most the reserved
1ms additional error per clock beyond phase chrony bounds; it is not a claim to
observe arbitrary unsampled client clock changes or repair clock steps. Original R4 still uses its larger
`+1.001s` safety reserve. Do **not** shrink R4. CPU accounting does not add that
one-second sampler-granularity reserve again: it explicitly bounds each actual
read interval `[realtime_before_i,realtime_after_(i+1)]`.

For each exclusive unit/node, CPU seconds are cumulative `usage_usec` differences
/1e6. Proc user/system ticks are diagnostics only and NEVER added to these totals
or used as fallback. Report actual sampled-bracket CPU seconds, bracket wall
seconds and cores, plus lower/upper measured-window CPU bounds. Lower includes only
intervals wholly inside the clock-uncertainty inner window; upper includes every
interval touching the outer window. Both account for endpoint quantization
(one microsecond per exclusive cgroup endpoint difference). No uniform within-interval interpolation,
background subtraction or guessed server completion fence is used. Sum disjoint
process populations across nodes once; preserve per-node values. The upper/lower
numbers are accounting envelopes, not confidence intervals or proofs all
asynchronous server work for an operation falls inside its client window.

For M1, require at least10 complete inner sample intervals and sampled outer
bracket wall<=1.2×measured wall. A subsecond window with inadequate resolution
is **CPU-unqualified**, never interpolated or made longer after results. Whole
case CPU output is withheld when any compared arm/block fails. Latency and
throughput remain visible if original R4 permits; CPU coverage explicitly marks
the missing measurement. Do not compute a six-block CPU ratio from only surviving
processes. A longer-quota CPU-specific ancillary phase needs a separate protocol;
it must not change this repeat's quotas or consume primary completion reserves.

## Controller lifecycle

`controller.py` subclasses the historical ops60 controller rather than changing
its sealed R1/R2 commands, R3 gate or R4 reconciliation. It requires an explicit
external `--campaign-dir`; SSH retains created-ID/protected-inventory checks,
pinned host keys, ControlMaster and60s runner polling. Pin the new controller,
server CPU module and both imported ops60 modules in the acknowledged manifest.
The original controller hash and the extension's separate pin receipt together
name the executed source; neither is a substitute for the other.

Preflight rejects an unsealed cgroup source/policy, missing helper pins, reused
CPU unit/cgroup or existing CPU output directory. All three bounded sidecar
intents are persisted before first dispatch. Each sidecar has its own exact
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
unit CPU-accounting positive/idle control and verify `usage_usec` units/monotonic
accounting against independently observed user+system activity; retain off/on
sampler overhead and membership proofs before arm timing. Retain control commands/pins/raw counters
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
