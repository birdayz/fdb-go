# CI runner infrastructure (OpenTofu)

This directory provisions the self-hosted GitHub Actions runner that runs the heavy CI
gates (1M stress, full `-race`, the 23 `pkg/fdbgo` fuzz targets, the `libfdb_c`
differential, and every FDB-testcontainer suite). It is **infra-as-code**: the entire
runner — OS packages, pinned toolchain, the GitHub runner, the self-healing watchdog, and
the orphan-container sweep — is described by `main.tf` + `cloud-init.yaml`, so anyone with
the inputs below can stand up a byte-for-byte-equivalent runner. See **RFC-108** for the
reproducibility/supply-chain rationale.

> A public, no-Docker **reproducibility floor** (build + vet + the wire-compat unit tests)
> runs on GitHub-hosted `ubuntu-latest` via `.github/workflows/hosted-smoke.yml`, so a fork
> gets a green signal it can reproduce without this box. This runner is the *heavy* gates.

## The fleet, and what `gh-runner-drain-*` is NOT

Two runners, both carrying the single label `hetzner-fdb-vm`:

| name | Terraform resource | notes |
|---|---|---|
| `gh-runner-fdb` | `hcloud_server.runner` | the grandfathered dedicated box |
| `gh-runner-drain-0` | `hcloud_server.runner_pool` (`count = var.runner_count`) | the pool |

The pool ran `gh-runner-drain-0..3` until the fleet was cut to `runner_count = 1`; the
pool resource is unchanged and count-parameterized, so raising the variable and applying
brings the other indices back.

**`drain` is a historical provisioning name, not a lifecycle state.** The pool was stood
up on 2026-08-01 to *drain* a CI backlog (`#566`, "the 2026-08-01 drain-fleet provision")
and the name stuck. These boxes are ordinary durable runners: `runner_mode` defaults to
`classic` — "register-and-listen, one durable registration per box" — and **`bazelscaleset`
is not deployed on any of them** (verified: zero scaler units fleet-wide). There is no
draining, no teardown path, and no autoscaler that could leave phantom capacity behind.

This is written down because the name has already cost one investigation an evening: a
runner named `drain` that is `online` and `busy=false` in the API reads exactly like a box
parked in a draining state that would accept no work. It is not. If you are debugging an
unacquired job, **`drain` is not your lead.**

### Reading a red lane that never ran

A self-hosted job GitHub cannot place is cancelled after a fixed **~10 minutes** with the
annotation *"The job was not acquired by Runner of type self-hosted even after multiple
attempts"* and — the part that matters — **`steps: []`**. Nothing executed. A lane in that
state is not red about what it tests; it never tested anything.

```sh
gh api repos/birdayz/fdb-go/actions/runs/<id>/jobs -q '.jobs[] | "steps=\(.steps|length) \(.conclusion)"'
```

`steps=0` ⇒ classify as placement, not regression, before reading any further. That
10-minute window is **GitHub's and is not configurable**; it bears no relation to any
`timeout-minutes` in our workflows (the `libfdb_c` lane allows 40 minutes and its inner
`go test` 30, against a worst-observed green of 2m38s).

Two causes produce that signature, and they are distinguishable:

- **Upstream dispatch failure.** Check the pool journals for the window
  (`journalctl -u 'actions.runner.*' --since ... --until ...` on each box). If a runner
  was *idle and listening* while the job went unacquired, the placement failure was
  GitHub's — nothing here can prevent it. This is what happened on 2026-08-06.
- **Oversubscription.** If every runner was busy, the job queued out. That one *is* ours,
  and it is now the **expected** state: one push starts **5** self-hosted jobs (`ci.yml` 4 +
  `nightly-libfdbc.yml` 1) against **2** slots, so three of them queue and a slow one can
  age out of the ~10-minute window. This is the accepted cost of the one-box pool.
  `//infra:infra_test`'s `TestPushFanOutVersusTheRunnerPool` no longer fails on it — it
  computes the ratio from source (workflows + `var.runner_count`) and logs it, so
  `--test_output=all` on that target tells you the current fan-out and how far
  `var.runner_count` is from a 1:1 fit. **Check that ratio before reading a zero-step red
  lane as a regression**; above 1, queueing is the boring explanation. What still fails
  hard there is an untrustworthy count: a parse that finds no jobs, or a `strategy.matrix`
  it cannot size statically.

Scheduled lanes are deliberately outside that budget (`nightly-fuzz.yml` alone is 5 jobs):
they are not racing a merge, and a nightly overlapping a push is a queueing cost rather
than a gate. If nightlies start being cancelled unacquired, revisit that.

## Prerequisites

- `tofu` (or `terraform`) ≥ 1.7, the `hcloud` + `minio` providers (`tofu init`).
- Environment / variables:
  - `HCLOUD_TOKEN` — Hetzner Cloud API token (provider auth).
  - `MINIO_USER` / `MINIO_PASSWORD` — Hetzner Object Storage S3 credentials (for report upload).
  - `-var github_runner_token=<TOKEN>` — a **short-lived (~1 h)** runner registration token:
    `gh api -X POST repos/birdayz/fdb-go/actions/runners/registration-token --jq .token`.
    It is consumed once by `config.sh` at first boot and is useless afterward; nothing secret
    is baked into a persisted image layer. This one token registers **every** box: the pool
    falls back to it (`local.pool_registration_token`), and `runner_registration_token` is
    only for the case where the pool must register on a *different* token from the
    grandfathered box. Both defaulting to `""` independently is how a fresh apply once
    rendered `config.sh --token ` on all four pool boxes; `//infra:infra_test` now pins it.

## Stand up / update the runner

```sh
cd infra
tofu init        # once
tofu apply       # provisions (or updates) gh-runner-fdb; prints server_ip / ssh_command
```

`tofu output ssh_command` gives `ssh root@<ip>`. `tofu output report_url` is the published
test-report URL.

## What is pinned (and why it's reproducible)

Every artifact the runner downloads is pinned to a **version AND a SHA-256**, verified by
`fetch-verified.sh` before use — a corrupted/tampered/moved artifact aborts the provision
instead of installing silently. The pins live in **one place**: the `locals.versions` block
in `main.tf` (runner, bazelisk, just, mc, FDB client). Bump a tool by editing its
`{version, sha256}` pair together — never one without the other.

The **build itself** does not depend on this image: `.bazelversion` (Bazel 9.0.1) and
`MODULE.bazel.lock` (all bzlmod deps, by hash) are committed in the repo, and bazelisk on
the box is just a launcher that reads them. `.bazelrc` configures **no remote/disk cache**,
so every build is from source — no external cache trust boundary.

Residual drift (documented, bounded): the Hetzner `ubuntu-24.04` base image is a rolling
label (point-release may differ between provisions) and apt packages beyond `docker.io`
ride the Ubuntu mirror. Every *test-relevant tool* is pinned on top, so this is bounded to
the apt baseline. `fdb_version` is pinned to **7.3.77** to match `MODULE.bazel` + the
testcontainers default (so the host `libfdb_c` the differential harness links is the same
FDB the tests run against).

## Self-healing (cloud-init)

- **`runner-watchdog`** (every 10 min): restarts `WATCH_UNIT` when it is **not active**, and
  nothing else. It reads `/etc/runner-watchdog.conf`, which the mode branch writes — no
  config, no action (hardcoding a unit name once burned `NRestarts` of 1143/94/4115/2141 on
  a binary that was never installed). It skips a **masked** unit, because a mask is a
  deliberate "never run this here" and a watchdog that restarts through one defeats the
  enforcement.
- **`orphan-fdb-sweep`** (every 5 min): kills FDB testcontainers running > 30 min that were
  started BEFORE the running job's `Runner.Worker` (orphans whose parent test died), and pins
  Ryuk's OOM score so the kernel reaps it last. The start-time comparison is load-bearing and
  its own section below: without it this swept the LIVE container of every long-running test.
- **`bazel-cache-prune`** (hourly): trims `/mnt/ci-data/bazel-disk-cache` to a 60%
  volume-usage target, oldest-mtime-first (Bazel ≥ 7 touches entries on cache hit, so
  mtime order is LRU order). Runs the stale-output-base sweep *first*, because those
  directories free space the cache trim would otherwise evict warm entries to reclaim.

  Why not the obvious alternatives:
  - **Age-based pruning** (`-mtime +N`) deletes *nothing* precisely when it's needed:
    heavy CI fills the 98 GB volume from empty in under 2 days, so at ENOSPC no file
    passes any age cutoff. Two runners went ENOSPC exactly this way (2026-08, 69 GB +
    20 GB caches, every file younger than the 7-day cutoff).
  - **Bazel's in-server GC** (`--experimental_disk_cache_gc_max_size`, kept in
    `/etc/bazel.bazelrc` as a second line of defense) only runs when the server is
    *idle* — a box saturated with back-to-back jobs never triggers it.
  - **Daily cadence**: at the observed fill rate, 60% → 100% takes ~19 h, and
    `OnCalendar=daily` + 1 h jitter can gap up to 25 h between runs — ENOSPC in between.
    Hourly with a 10 min jitter closes that; at/below target the script is a
    single `df` and exits.

  Safety against live Bazel traffic: `tmp/` and `gc/` are excluded from eviction
  (in-flight uploads land in `tmp/` before rename; Bazel's own
  `DiskCacheGarbageCollector` excludes both), `-mmin +15` keeps young entries out of
  the candidate scan, and each candidate's mtime is rechecked immediately before the
  unlink — `sort` buffers the whole scan, so an entry hit (= touched, Bazel ≥ 7)
  between scan and deletion would otherwise still be on the list. The recheck
  compares whole seconds deliberately: `find`'s `%T@` and `stat`'s fractional `%.Y`
  print different precisions, and a strict string compare would skip every file and
  turn the prune into a silent no-op. A *completed* entry unlinked while a reader
  has it open is harmless — the open FD survives the unlink; Bazel treats the next
  lookup as a cache miss.

  `tmp/` itself is not immortal: interrupted uploads (Bazel killed, host reboot)
  leave UUID-named partials there that nothing else reaps — startup only recreates
  the directory. The prune deletes `tmp/` files older than a day, far beyond any
  live upload's lifetime, before the usage check so a tmp-flooded volume still
  recovers.

### The wedged-listener gap (open, deliberately)

The watchdog recovers a **dead** listener. It does not recover a listener that is *active
but wedged* — process up, connection dead, claiming nothing. Classic mode therefore sets
`WATCH_UNIT` and **no** `WATCH_HEARTBEAT`; only `bazelscaleset` sets one, because that
supervisor is ours and stamps a heartbeat on every successful poll.

This is a decline, not an oversight, and the reason is that every available substitute
restarts runners **mid-job** — which has already taken live CI down twice:

- The stock `actions/runner` publishes no heartbeat and cannot be made to.
- Staleness of the listener's own `_diag/Runner_*.log` is the only local signal, and it
  does not separate the two states. Measured on `gh-runner-drain-0`: an **idle healthy**
  listener writes nothing between credential refreshes (~30 min observed, an undocumented
  implementation detail, so a safe threshold has to be far above it), while a job in
  progress writes to `Worker_*.log` and leaves `Runner_*.log` untouched for its whole
  duration — the nightly batch lane runs 2h+. Any threshold high enough to avoid false
  positives on an idle box is also inside the silent window of a long job.
- A queue-aware check (queued runs fleet-wide + this listener idle for N minutes) *would*
  separate them, but it needs a GitHub token with repo scope sitting on every box. Not
  worth it for this failure mode.

`-var runner_ephemeral=true` closes the gap from the other end (a fresh process per job, so
no wedge survives a job) at the cost of a cold cache; see below for why it is not the
default yet. `//infra:infra_test` pins the absence of a heartbeat arm in classic mode, with
the failure message stating what a future one must land alongside: a job-in-progress guard.

## The boot path (why Docker is masked, and what unmasks it)

`cloud-init.yaml` points to this section. The whole sequence exists because three things
that look independent are not: cloud-init's stage order, `nofail` fstab semantics, and the
fact that the runner registers itself whether or not the box can run containers.

1. **`packages:` starts `docker.io` before `runcmd` links the volume.** An unmasked daemon
   creates its data-root as a mode-0711 dir on the **root disk**; the volume link then
   cannot nest inside it (100 GB idle while caches fill 75 GB), and the traverse-only
   ancestor makes the runner refuse to start with *"Fail to create and validate runner's
   work directory"*. `bootcmd` is the only hook before package install, so the mask lives
   there.
2. **The mask must be lifted every boot, not once.** `bootcmd` is `PER_ALWAYS` and `runcmd`
   is `PER_INSTANCE`: mask-in-`bootcmd` + unmask-in-`runcmd` left every boot after the
   first with no container runtime on a box that still registered a runner and claimed
   testcontainers jobs. Making `bootcmd` conditional does not work either — it runs
   `Before=sysinit.target`, so no mount it inspects has happened yet and it reports
   "absent" on every boot. `ci-docker-gate.service` owns the decision instead.
3. **`After=local-fs.target` does not order the gate after the CI volume.** The fstab entry
   is `nofail`, and `nofail` is exactly the option that *removes* the generated mount
   unit's `Before=local-fs.target` — `systemd.mount(5)`: a local mount gains that
   dependency *"unless one or more mount options among **nofail**, `x-systemd.wanted-by=`,
   and `x-systemd.required-by=` is set"*. The mount is still `Wants=`-pulled by
   `local-fs.target`, just unordered against it, so the gate could run mid-mount, see a
   dangling `/mnt/ci-data`, mask Docker and never re-decide (it is a `oneshot`; nothing
   runs it again). The fix is in the fstab options `link-ci-volume.sh` writes:
   `x-systemd.before=ci-docker-gate.service`, the mechanism `systemd.mount(5)` documents
   for precisely this shape. Ordering-only on purpose: a mount that *genuinely* fails must
   still let the gate run and fail closed, which `RequiresMountsFor=`/`x-systemd.requires=`
   would prevent. (`RequiresMountsFor=/mnt/ci-data` is also a no-op here — that path is a
   symlink on the root fs, not a mount point.)
4. **The gate fails closed, and takes the listener with it.** No volume ⇒ Docker stays
   masked *and* the gate exits non-zero *and* stops `WATCH_UNIT`. `svc.sh`'s unit depends
   on nothing but `network-online.target`, so a drop-in written at provision time adds
   `Requires=`/`After=ci-docker-gate.service`. Without that the box stays in the pool and
   fails every Docker-dependent job it claims, one after another — worse for the fleet than
   one box visibly missing. `Requires=` also means the watchdog's `systemctl restart`
   re-runs the gate rather than routing around it.

`infra/link_ci_volume_test.sh` executes the shipped `link-ci-volume.sh` and
`ci-docker-gate.sh` against fixtures and asserts each of these, including the fstab option
and the gate's exit status.

Two things a fixture cannot show, both measured on a real box (`cpx32`/fsn1, provisioned
from this template, three reboots):

- **The ordering option has to be applied to whatever entry is already there.** Hetzner's
  attach-time automount writes the volume's fstab line *itself*, byte-for-byte in the same
  `by-id` format `persist()` writes, so the "does this entry exist" guard skips and an
  option appended only on the write path lands on almost no box. On the first real boot the
  entry came back `discard,nofail,defaults` — no ordering at all — while the fixture suite
  was green. `persist()` now adds the option in place. Before: the generated mount unit's
  `Before=` was `umount.target`. After: `ci-docker-gate.service umount.target`.
- **A box with no volume takes ~90 s longer to decide.** The gate is now ordered after a
  mount whose backing device never appears, so it waits out systemd's device timeout
  (measured: boot 11:33:56, gate 11:35:26) and only then masks Docker and fails. That delay
  is the ordering working — deciding *earlier* is exactly the bug — and it is bounded by
  `x-systemd.device-timeout` (90 s default), not unbounded. The listener is held by
  `Requires=` for the whole window, so nothing claims a job in it. Recovery is automatic:
  re-attach the volume, reboot, and the gate passes, Docker comes back with its data-root
  on the volume, and the listener starts.

> **The live fleet does not have any of this yet.** Every live box predates
> `ci-docker-gate.service` entirely (checked: no gate unit, no fstab ordering, no drop-in),
> so they still have the reboot-with-no-Docker failure this section describes. `user_data`
> is pinned by `ignore_changes`, so the fix reaches a box only via
> `tofu apply -replace=hcloud_server.runner_pool[N]`, one at a time, on an idle box.

## The 32 KiB `user_data` budget

`cloud-init.yaml` is rendered into `hcloud_server.user_data`, which Hetzner caps at
**32768 bytes**. The file is ~95% comments and *comments are payload*, so a few paragraphs
of otherwise-good rationale can make the fleet unprovisionable — that has already cost one
provisioning attempt. `//infra:infra_test` renders the template and fails the build before
`tofu apply` can be rejected, so **the budget is enforced, not remembered**.

Consequence for reviewers: prose in `cloud-init.yaml` must be about the line it sits on.
Incident narrative goes here instead. Two things worth knowing about the budget:

- A real `bazelscaleset_app_private_key` is interpolated **unconditionally** (the mode
  switch is a shell `if`, not a template directive), and a ~1.7 KB PEM base64s to ~2.3 KB
  of payload — more than the current headroom. Reviving the scale set means moving that
  key out of `user_data` first.
- The `worker` runner_mode branch was removed: no variable value could select it
  (`runner_mode` documents `classic` and `scaleset` only), and it cost ~1.4 KB of budget.
  What it did: strip `bazelscaleset.service` and the watchdog off a box repurposed as a
  remote slot. Without that, the watchdog restarts a supervisor binary that was never
  installed, forever — measured `NRestarts` across the pool: 1143, 94, 4115 and 2141,
  against a unit reporting `disabled` the whole time, which is what made it easy to miss.

## The orphan FDB sweep removed live test containers for five weeks

`orphan-fdb-sweep.timer` runs `orphan-fdb-sweep.sh` every five minutes, and its FDB arm used
to `docker rm -fv` any `foundationdb/foundationdb*` container older than 1800 seconds, under
the comment *"no per-test container lives that long, so these are orphans"*. Three nightly
lanes do exactly that: rowdiff, stress and factory each hold ONE container for hours. So the
sweep was not sweeping orphans — it was force-removing the live container of every
long-running test on this fleet, at 1800s plus up to the timer's five-minute phase.

That is the measured band. The lanes died 30m48s, 33m13s and 33m47s into their runs, against
a booked eight-night range of 31m31s..34m00s (mean 1959s, sigma ~55s). It also explains every
observation that two carefully-refuted memory hypotheses could not:

| observation | why |
|---|---|
| container found GONE, not exited | `docker rm -fv` removes it |
| `OOMKilled=false`, nothing in the kernel log | nothing was OOM-killed |
| a fresh dial BLACKHOLES rather than being refused | no container left to refuse |
| tracks elapsed time, not work done | the trigger is literally an age |
| 2.8% spread across five hosts | every box runs the same timer |
| never reproduced on the dev box | the dev box has no such timer |

The last row was read for weeks as *"the dev box has more RAM"*. It was *"the dev box has no
sweeper"*.

Corroborated by a boundary nobody had looked for: the rowdiff nightly was **green for its
first eleven nights** (2026-07-20..07-31) and has failed **every night since 2026-08-01** —
the day this pool was stood up. `cloud-init` is `PER_INSTANCE`, so a box provisioned before
the sweep's match-by-image-name fix (`6819cfbcb`, 2026-06-01) kept writing the version that
matched nothing and swept nothing; the working version only reached boxes provisioned after
it.

**The fix** compares each container's start time against the running `Runner.Worker`'s. A
container newer than the worker belongs to the job in flight, so its age says nothing about
whether it is abandoned; one OLDER than the worker is an orphan from a previous job and stays
eligible.

The first version of this fix was a blanket "skip everything while a job runs", which is the
rule the retired scaler's own sweeper had (*"runs only when no runner is active on that box, so
a dead test's leaked FDB container is an orphan and removing it cannot disturb a concurrent
job"*) — two sweepers with one job, one guarded and one not, and the guarded one is the one that
does not run. A review lap pointed out that a blanket skip trades one leak for another: a
cancelled job's ~700 MB orphan survives the whole of the next job, and back-to-back work need
never expose the idle tick that would sweep it. The timer samples every five minutes; a nightly
lane runs for four hours. Comparing start times keeps both properties.

`pgrep -x`, matching the process NAME, and **never** `-f`, which matches any command line
*containing* the string. That is not a stylistic preference and the hazard is not theoretical:
while this guard was being tested, `pgrep -f 'Runner[.]Worker'` on the dev box reported a
worker present with no runner anywhere on it — the match was an unrelated agent whose own
`grep -E 'Runner\.(Listener|Worker)'` command line carried the text. A guard that fires
spuriously does not fail safe here; it silently retires the sweep and the disk fills.

`-x` is correct for this fleet because cloud-init installs the official
`actions-runner-linux-x64` tarball, which ships `Runner.Worker` as an **apphost** binary
launched directly, so `comm` is `Runner.Worker` (13 chars, under the 15-char truncation) and
the sweep runs as root, which can see it. A framework-dependent packaging would run it as
`dotnet Runner.Worker.dll` and `-x` would miss — `tools/bazelscaleset`'s cmdline matcher
carries that shape as a defensive case, and an intermediate version of this guard used `-f` on
the strength of it. That was reading a test table as a description of the fleet. If the runner
packaging ever changes, this guard goes silent in the direction that returns the bug.

The worker's start time comes from `ps -o etimes=`, **not** `stat -c %Y /proc/<pid>`. That
looks like a process start time and is not: it is when the procfs inode was allocated — the
first lookup after a cache miss — so it is biased late and unboundedly so. Measured on a dev
box: `/proc/1` read 3 seconds late, a fresh process 1–2. Two ways that restores the original
incident. If the first lookup is the sweep's own tick, the worker reads as minutes younger
than it is and the job's own container becomes "older than the worker". And a dentry evicted
under memory pressure and re-looked-up at hour 3 of a four-hour lane moves the worker's
apparent start to hour 3, at which point the live container is swept.

An unreadable age fails **closed** — every container is protected rather than none.

All five arms are driven, and committed rather than driven by hand:
`infra/orphan_fdb_sweep_test.sh` extracts the shipped script out of `cloud-init.yaml` (so it
cannot drift from what the boxes run) and executes it against stubbed `docker`, `ps` and
`pgrep`, needing neither a daemon nor a runner. `//infra:infra_test` runs it.

| arm | container | worker | outcome |
|---|---|---|---|
| A | older than the 1800s threshold, started AFTER the worker | running | survives |
| B | started BEFORE the worker | running | **removed** |
| C | old | none | removed |
| D | young | none | survives |
| E | old | running, age unreadable | survives (fail closed) |

Arm A carries the detail that makes it worth having: the container is over the age threshold,
so the sweep WOULD remove it without the guard. A fixture under the threshold passes either
way and proves nothing — which is what the first draft of that case did, and it failed.

Note what that does and does not establish: the arms prove the guard's LOGIC with stubs. That a
real worker's `comm` is `Runner.Worker` rests on the tarball's packaging, not on
an observation from one of these boxes, and neither has the timer been caught firing —
`journalctl -u orphan-fdb-sweep.service` around one of the recorded death timestamps would turn
the inference into an observation.

**Deployment, and it is the remaining owner action:** `cloud-init` is `PER_INSTANCE`, so this
fixes boxes provisioned from here on. The live fleet keeps the old script until re-provisioned
or until the file is edited in place.

**Budget note:** this guard fits with about one line of `user_data` budget to spare. No figure
is given here on purpose: it has been wrong twice, once because a later trim in the same round
moved it and once because the guard itself grew. `//infra:infra_test` computes it on every run
and prints it under `--test_output=all` (a passing Go test's `t.Logf` is otherwise hidden);
read it there. What does not change is the shape of the constraint — anything you add to
`cloud-init.yaml` has to come out of a budget with roughly one line left in it, so the next
person to touch that file is trimming, not adding.

## Ephemeral runner (opt-in)

`-var runner_ephemeral=true` configures the runner with `--ephemeral` (one job per process,
fresh state each time — no bazel server or wedged listener can survive a cancelled job into
the next). Default is `false` (persistent + watchdog, which keeps the warm bazel/Docker
cache). **Caveat:** continuous ephemeral operation also needs a token-refresh service (a PAT
to mint a fresh registration token after each job, since the runner deregisters on exit);
that automation is a follow-up — the flag itself just sets the mode.

## Note on state

`terraform.tfstate*` is local state (gitignored). Do not commit it — it can contain the
registration token and resource ids.
