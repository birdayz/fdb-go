# Fresh 100k ingest profiling arm

This is preparation for **one** guarded 100,000-span diagnostic invocation, not a
20k inserts/s result or authorization to start a server. Root must run the full
fast lane, explicitly approve the tested diagnostic bytes, seal a configuration,
and issue the run instruction. Formal policy/application review is deferred to
the coherent winning candidate before merge; operator approval is not a formal ACK.
Prior NAK reports and findings remain part of the experiment's provenance. There is no sweep, escalation, adoption, cleanup,
Drop, native-filesystem change, installer, SETFLAGS, or durability-knob change.
The old 13-file 10M profile, ready `rw1m0286` and every retained volume stay
untouched. The final PoC ceiling remains 10 million spans; this profile admits
only 100k.

## One explicit arm

`policy.arm(run, batch, workers)` admits only the explicit `prof20k` plus four-digit
namespace; no run is a CLI default. The first intended arm is `prof20k0290`.
Each invocation requires a new output directory and new exact container/volume:

| Field | Value |
| --- | --- |
| owner/profile | `otel283-vxnar` / `fdb4-4cpu-16g-ingest20k-v1` |
| run and session | the same explicit `prof20kNNNN` |
| container | `otel283-fdb4-RUN-vxnar` |
| volume | `otel283-vxnar-fdb4-RUN16-v1` |
| output basename | `ingest20k-RUN` |
| ports | loopback-published 14883–14886 |
| cluster seed | `otel20k:otel20k@127.0.0.1:14883` |
| image | `foundationdb/foundationdb@sha256:559b178a137bbe09272ad6e3752531aa04e2ddacab8ccfa4ffa86467db033bea` |

A second arm requires another invocation and all renewed gates. Existing names or
output paths are rejected, even after a failed load. No restart/resume path is
provided. Other owner-labelled databases and FDB/CH name variants must be cleanly
stopped; protected original names also bind their full IDs. The unrelated stopped
volume-inspection utility is not misclassified as a database.

The resource/workload envelope is fixed: 4 CPU, 16 GiB memory and memory+swap,
bridge networking, four server processes with 4 GiB limits, two storage caches of
2 GiB and other caches 64 MiB. Single redundancy/same host+zone is **not HA**.
The two-active-storage-role/two-Redwood-primary gate is this profile's admission
requirement, not a FoundationDB single-redundancy guarantee. Native 7.3.77 can seed
one storage process and recruit another asynchronously. If the desired layout is
not proven within startup admission, refuse before load and retain actual status;
do not force internal recruitment/replica knobs or weaken inode proofs.
Redwood, FDB checksums/fsync and default queues remain unchanged. Workload is
seed283, one tenant, eight attributes, match-every100, batch save mode, time order,
K100, hash-bytes32, two-million candidates and 256 MiB query byte budget. The first
run is **batch32/workers2**. Sealed configuration may admit batch32 or64 and workers2,
4 or8 (integers only); this is not permission to escalate. Every later pair needs
root's explicit approval after stable lower arms, a new fresh run and all renewed
gates. There is no automatic loop/sweep, and count/time/transaction guards stay fixed.
The selected application's 1500ms transaction, 300KB estimated/500KB actual save,
one-second save and finite fresh-run retry guards remain in force.

## Independent application and operations seals

Application source revision is explicit, not the current operations checkout HEAD.
The first intended instrumented source is
`12b1c7da8882d9cce7d4d95a9d240f623eb0df45`, clean detached source
`/tmp/fdb-hotelbench.VxNArC/ingest20k-app-src-v2`, executable SHA256
`8c4ae526610e4d740067c8661b7f708273cca9e73cc882aa1673f2994ce64b8c`, and source
manifest SHA256
`b07a5d60f0289635e9bb3d41105ced971a209868c7ba43544b0c5ec4b7ae4a6b`.
Root supplies actual absolute binary/manifest paths from their receipts; the driver
never builds the app. A separately reviewed b5 baseline can also be explicitly
bound, without pretending that first-arm performance is a speedup over old 1M.
The existing `--diagnostics` flag captures the new app's client metrics; no invented
profiling flags are used. Original evidence is retained, including failures.

The sealed approval is one strict JSON object containing **every literal** from
`policy.arm(run)`, plus:

- `schema: "ingest20k_fresh100k_v1"`, `approved: true`;
- `source_revision` and identical `source_approved_revision` (explicit operator approval);
- absolute `binary`, `source_root`, `source_manifest`, `probe_binary`, `out`;
- `binary_sha256`, `input_manifest_sha256`, `probe_sha256`, `probe_source_sha256`;
- `policy_sha256` and identical `policy_approved_sha256` (explicit operator approval);
- `quiet_thresholds_approved: true`, `growth_overshoot_accepted: true`.

No approval file is generated automatically. `policy.runtime_sha()` hashes ordered
`SHA256  basename\n` records for `RUNTIME_FILES`, including C getter source and its
build script. Tests/README are reviewed separately. The app source manifest resolves
relative names against the explicit detached source root. Source manifest entries
and clean source HEAD are checked at admission, before/after workload stages and
after output writers join, not on every monitor tick. App/probe bytes, manifest
bytes, approval/request/configuration and critical operations hashes are checked
periodically by the monitor as well. Both executables must be non-writable and
executable. Application source/binary hashes and explicit operator approval bind
each experimental candidate; that approval does not claim formal application
review. Isolated candidates may be measured before that review; formal application
review remains separate before merge. The baseline revision above retains its
genuine prior formal ACK. The tested operator-approved diagnostic binds operations
bytes with `policy_approved_sha256` exactly equal to `policy_sha256`. Formal policy
and application review remain separate before merge, with actual prior NAK
provenance retained rather than relabelled as an ACK. All runtime guards remain
mandatory; approval does not waive GET, ownership, durability or resource limits.

The operator-only foreground form is:

```text
python3 -B driver.py run --approval SEALED_APPROVAL --execute-approved
```

Use a runtime-tracked supervised job, never `&`, nohup, setsid or daemonization.
Do not execute from a mutable/private draft directory. Mount source directories
must be readable for the image's fdb UID. Choose a canonical fresh output parent
on the **Docker backing filesystem**, outside source/operations trees; RAM-backed
`/tmp` is not a suitable growing output root.

## Read-only inode getter

The pinned image lacks `lsattr` at the six ordinary PATH locations. No package or
new image is installed. The explicit user authorization permits the small static
`inode_flags.c` GET-only probe on this **new** disposable profile. Its source uses
only FS_IOC_GETFLAGS, no setter. It opens a fixed path grammar under `/var/fdb/data`,
rejects symlink parents/final links and wrong file types, checks Btrfs and matching
device/statx mount ID at each descendant, and retains original JSON metadata. Any
unknown/failed GET fails closed. C=1 prints its observed metadata then fails;
compression is allowed. It does not expose internal NODATASUM or establish actual
O_DIRECT behavior, checksum equivalence, crash safety or production suitability.

Root builds it separately using:

```text
bash build-probe.sh NEW_ABSOLUTE_OUTPUT_DIRECTORY
```

The existing parent must be writable. Required host tools: a static-capable C
compiler (`CC`, otherwise `cc`), readlink, sha256sum, readelf, grep and mkdir.
Build records source/compiler/command/exit/binary/static-ELF receipts and **never
runs** the result. Root seals the built executable 0555. The current separately
built artifact is
`/tmp/fdb-hotelbench.VxNArC/ingest20k-probe-build-v2/inode-flags`, SHA256
`55daed4087c104c4a22cdb4bc287947b4b6fd1dc15f5bf6591ba8b901ddc11eb`, source SHA256
`35d06ffd3f61753ff864fc925be6f44e8691f31e1f3bc220c17ba2d3f81bc543`.
This 0555 artifact executed the initial root GET in0296: flags0, NOCOW=false,
UID0, Btrfs; original proof is private
`/var/tmp/fdb-hotelbench.VxNArC-ingest20k/ingest20k-prof20k0296/server.stdout`.
The populated-file GET census was not reached. The old asset is retained.
A production-source change invalidates that build and requires rebuilding.

Only that sealed executable is mounted **read-only** at
`/etc/ingest20k/inode-flags`; configuration is another read-only bind. The new data
volume remains writable for FDB. Before any layout writes, entrypoint verifies
empty root and runs the GET gate. After strict readiness, GET evidence covers root,
directories, all regular data/trace/metadata files, both Redwood primaries and the
matching SQLite/WAL/logqueue UUID set. Host-side pure validation checks UID4059,
flags, path population, device/mount consistency and primary relationships;
`cow.ready.json` binds the original JSONL SHA and probe source/binary hashes.
No flag remediation, retry workaround or use on retained data is automatic.

The other image-tool presence checks succeeded via individual read-only copies
from the exact stopped pinned-image container, including `find`, Bash, timeout,
coreutils/stat/date/du/id/cut/sort/sha256sum/mkdir/cp/chown, pgrep, Tini and FDB
executables. Receipts are private at
`/tmp/fdb-hotelbench.VxNArC/ingest20k-entry-tools.LJCiN6`. These establish presence,
not a live bootstrap/GET success. jq and Python are **host** tools, not image deps.

## Time, ownership and safety

The global heavy-lane path is `${FDB_LANE_LOCK:-${TMPDIR:-/tmp}/fdb-heavy-lane.lock}`,
matching the existing lane wrapper, including empty-variable fallbacks. Its
descriptor is acquired nonblocking **before quiet admission** and retained through
confirmed stop. PID/start-time/device/inode lease proof is recorded and the sealed
FD/current pathname identity is revalidated throughout quiet/supervision, immediately
before create/start and each monitor iteration. The monitor and attached Docker client inherit that descriptor;
the monitor never reacquires flock. Do not wrap this driver in a nested flock.

The absolute session deadline is **1200 seconds including setup**. It is passed
once in the immutable container command. GNU timeout wraps **all entrypoint setup
and the full monitor/server process group**, with TERM five seconds before KILL
and one second rounding slack. A restart cannot renew the deadline and rejects
nonempty data. Host supervisor/monitor also enforce the same deadline and watch
one another's heartbeat. Arbitrary host/kernel/daemon failure is not a promise of
hard-real-time shutdown; unconfirmed stop is FAILED/UNKNOWN, never clean success.

Bootstrap authorizes one canonical `configure new` only after fresh exact-ID,
nonce/image/volume, initial empty-root GET, owner/seed/processId/no-engine-file
layout and source proofs. `bootstrap.authorized.json` binds those raw receipts,
source/seed/probe hashes and exact identity; it claims no preconfigure status or
quorum measurement. Native7.3.77 `configure NEW` uses exclusive initialization
(`GenericManagementAPI.actor.h`: init_id and INITIALIZE_NEW_DATABASE), rejecting
an already-created database. This native operation, not full preconfigure status,
is the authoritative creation guard: the latter can block on transactional status
futures in a never-created database. Prior finding/failed-run history is retained;
this phase-authority change is not a warning waiver or lost-history claim.

The single external configure attempt keeps its20s cap, including ambiguous or
timeout failure: never retry, adopt, FORCE or fall back to reconfiguration. Only
sole `Database created` stdout with empty stderr succeeds; already-created,
configuration-changed, malformed/error or extra warning output refuses the trial.
After creation, bootstrap may wait only for typed clean native7.3.77 recovery
progress or documented DD `initializing` (whose healthy field is omitted).
Original strict health and the two-active-storage/two-primary GET admission gate
remain mandatory before load. Unknown/malformed states, degraded processes, wrong
identity/configuration, every warning including batch probe timeout, command and
resource failures still abort. Pending resets the three-sample streak; drain/load
never use startup classification.

Caps: initial readiness180s, CLI load300s, strict drain300s, source census120s,
independent oracle120s. Each stage needs its complete budget plus120s shutdown
margin to fit the remaining total. No extension, sweep, automatic shortening or
phase reuse. The caps need not all be consumed; an arm that cannot finish is a
failed diagnostic with retained receipts.

Renewed admission is seven samples, each10–15s apart: MemAvailable>=24GiB,
max-minus-min variation<=1GiB, CPU PSI some avg10<=5%, memory some<=0.1% and
full<=0.01%, I/O full<=1%, and unchanged pswpin/pswpout. No empty-swap requirement
or swap/kernel change. Admission may wait at most180s (up to19 samples10s apart)
within the original1200s session, accepting only the latest seven consecutive
samples that pass every unchanged threshold. Only typed, valid busy accounting
is retried; malformed/nonfinite/missing accounting, decreasing counters, bad
intervals, lease loss and deadlines fail immediately. Every `quiet-i.json` remains;
`quiet.accepted.json` binds the accepted window's indices/raw hashes and prior
busy reasons. No stage budget is shortened and no threshold is relaxed.

Continuous guards: host>=8GiB, cgroup<90% of16GiB, no OOM/restart/PID replacement,
exact new instance/owner/profile/session/nonce/image/mount/ports/resources and
the periodic immutable-input seals described above. Queue>512MiB **AND** lag>60s for six consecutive nominal
10s samples aborts. Readiness/drain need three original strictly healthy samples,
>=5s apart, queue<16MiB and lag<10s. **No batch-warning exception or warning-permission argument exists**. Status health is never rewritten;
admin probes are not workload latency/SLO evidence.

Conservative space guards are inherited: free floor
`ceil4096(200GiB + 6% of actual shared filesystem total)`, plus64GiB reservation
before creation; stop at observed total new allocation64GiB or host receipts1GiB.
Native ratekeeper5% and team-health6% are unchanged, not multiplied by four.
Observe volume data/logs/traces, conservative full container-root allocation and
all private host receipts. Separate du calls avoid parent-child deduplication.
These are observations, **not hard quotas or exact Btrfs exclusive physical use**;
metadata/shared/compressed extents, other host work and sampling/stop delay can
cause uncertainty/overshoot. Keep unassigned margin. A disk/RAM snapshot is not
admission permission and retained data is already included in free space.

All commands/raw responses/exit codes/hashes and start/end clocks are private.
A failed command, receipt write, malformed/missing accounting, monitor/heartbeat
failure or unexpected health stops only the original new ID+labels/nonce.
Emergency shutdown bypasses failed resource/logging guards but not ownership.
Unknown daemon/stop verification retains the lease and retries for operator
intervention. It never stops/replaces a protected old instance or removes data.

## Measurements and completion

Original status JSON and its latency summary are retained with raw SHA bindings.
Periodic bounded cgroup-v2 `cpu.stat`/`io.stat` reads record membership and file
paths, timestamps and hashes. Their scope is the **whole container**, including
bootstrap, DD, monitoring and workload—not source-only I/O, NAND writes or write
amplification. Missing files/unknown cgroup membership abort, never become zeros.
The external client's CPU is **not** measured by these counters; no perf wrapper
was added to complicate supervision. App phases/client metrics come first.

A successful load alone is insufficient: final strict drain and source audit must
prove100k records,800k equality postings,100k each native span/time index, zero
primary equality keys and1,100,002 owned keys for the pinned layout. The independent
`all` suite retains1600 samples:16 exact classes ×100, ordered iteration IDs,
digests and zero failures. These checks retain the application query guards.
`verification.complete.json` binds source/census/oracle/drain/GET commitments.
After workload, monitor and attachment writers join, source/evidence/certificates
and validation-time receipt hashes are revalidated. `final-inventory.json` is a
nonempty path/size/SHA256 inventory of every prior retained output, including
initial Docker admission, raw commands/status, attached streams and ingest ACK.
`complete.json` binds both verification and inventory hashes and appears only
after these checks, successful monitor exit and confirmed **exit0/noOOM/noRestart**
stop. Any nonzero stop command or lost stop receipt is a sticky failure even if a
later independent check confirms containment. Emergency stop still confirms an exited137
or OOM instance and releases the lease rather than waiting for an exit code to
change. `FAILED_STOP.json` is written only when cleanup reaches the final nonclean
stop gate without a pending error. Earlier guard/monitor failures instead retain
`FAILED.json`, command/monitor exits and stop events when receipt writes succeed;
absence of `FAILED_STOP.json` is not proof of a clean stop. GNU timeout/Tini's actual normal
shutdown behavior must pass this gate; it is not assumed. Server attachment exit is
recorded separately, not invented.
Neither file establishes20k/s, a production limit, HA or an SLO.

Raw directories are0700/files0600 and can contain private client metadata. No
raw status, environment or full inspect is echoed publicly; native traces stay
in the retained private new volume. There is no export framework or publication
step in this profile.

## Validation ownership

Root owns builds, `just test`, Gazelle, commit and formal harness review. The normal
Bazel fast lane declares an offline shell test with explicit discovered>0/executed
count/no-skips gates, plus C pure-predicate and injected-syscall targets. Tests use
fixtures/mocks, never a live database/probe. No direct package tests or helper
execution is a substitute for the whole fast lane. A syntax check is not a pass,
formal ACK, live GET/bootstrap success or run permission.
