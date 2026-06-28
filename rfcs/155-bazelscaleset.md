# RFC-155: bazelscaleset — a bazel-warm, dependency-isolated runner scale set for the single CI box

**Status:** DRAFT (revised per bazel-pro-from-google + Torvalds review) — pending re-review before
implementation. CI/infra item — no query-engine or client/wire behaviour change (no Graefe /
FDB-C++ gate). Supersedes the classic register-and-listen runner in `infra/` for the self-hosted
box; leaves the PR gates (`ci.yml`) and the nightlies unchanged.

**Trigger:** On 2026-06-28 a 7-PR merge flurry wedged the self-hosted runner. Each master push
fires `ci.yml` (concurrency `cancel-in-progress: true`) + `nightly-libfdbc`, so the rapid
push/cancel churn killed the runner's `Runner.Listener` broker connection
(`BrokerServer System.Net.Sockets.SocketException (125): Operation canceled`). The box stayed
`online` to GitHub but stopped pulling the queue — "Listening for Jobs" forever. This is a
well-known **persistent-listener wedge** in `actions/runner` (community #120813, #142620;
actions/runner #3478, #3609); GitHub's own remedy is **ephemeral runners**. We healed it
manually (restart) and shipped a tighter watchdog (see *Interim*), but the band-aid doesn't
remove the bug class.

## Problem

Four things are true at once and the current setup can only satisfy three:

1. **Reliability.** The classic long-lived listener wedges under churn. The box is *expected to
   work*, even if it just grinds slowly through a backlog — a wedge that needs a human (or a
   heuristic watchdog) to notice is not acceptable.
2. **One machine.** There is exactly one self-hosted runner (`gh-runner-fdb`, 4 vCPU / 7.6 GiB,
   provisioned by `infra/` via OpenTofu). Multi-machine autoscaling / ARC-on-Kubernetes is out
   of scope and overkill. The fix must run as a small daemon on this box.
3. **Warm bazel.** Cold bazel per job is expensive here: a cold `--output_base` re-downloads the
   bzlmod graph (rules, `go_deps`, the FDB C++ toolchain), re-runs loading+analysis, restarts the
   JVM server, and recompiles from scratch. The current runner is *persistent* precisely to keep
   this warm. Any ephemeral design must not throw warmth away.
4. **Dependency hygiene.** `github.com/actions/scaleset` pulls `golang-jwt/jwt/v4`, `google/uuid`,
   and `hashicorp/go-retryablehttp` (its library packages — not docker/cobra, which are only in
   its examples). These are mundane libraries; the point isn't that they're dangerous, it's that a
   CI-only tool's deps have no business in the *product's* `go.mod`/`go.sum`/`MODULE.bazel` — they
   bloat the bzlmod `go_deps` graph and widen the product's vuln-scan surface with code that never
   ships. Keep them out.

The classic runner satisfies (2) and (3) but fails (1); naive ephemeral satisfies (1) but loses
(3). This RFC gets all four.

## Goals / Non-goals

**Goals**
- Eliminate the *runner's* persistent-listener wedge class; bound + supervise the supervisor's own
  long-poll so it can't silently take its place.
- Keep bazel warm across ephemeral jobs: `bazel-out` + the local action cache persist on disk per
  slot; the JVM server + analysis cache stay warm while the server survives (durability in §2/§7).
- Single box, no Kubernetes, one supervisor process.
- Strict dependency isolation: scaleset's closure lives in a **separate Go module**.
- Backlog is handled by serialization (or bounded, RAM-aware concurrency) — slow is fine, wedged is not.

**Non-goals**
- Multi-machine / cloud autoscaling, spot fleets, ARC.
- Replacing the nightlies or the per-PR `ci.yml` gates (unchanged).
- A merge queue (separate concern; would also help the flurry case but is heavier).
- Changing what the jobs *do* (same bazel/Docker/testcontainer workloads).

## Background — GitHub Actions *runner scale sets* (`github.com/actions/scaleset`)

Scale sets are GitHub's modern runner model (the engine inside actions-runner-controller),
exposed as a standalone Go library — **no Kubernetes required** (the README is explicit). The
shape we use:

- `CreateRunnerScaleSet` registers a named scale set whose `Labels` are what `runs-on:` targets.
- `MessageSessionClient` opens a **long-poll message session** (≈50 s blocks, `202` when idle,
  explicit `DeleteMessage` ack) — *not* the wedge-prone BrokerServer socket.
- `listener.Run(ctx, scaler)` drives a `Scaler` we implement:
  - `HandleDesiredRunnerCount(count)` → ensure ≤ `maxRunners` runners exist,
  - `HandleJobStarted` / `HandleJobCompleted` → bookkeeping + cleanup.
- For each runner we call `GenerateJitRunnerConfig(&{Name, WorkFolder}, scaleSetID)` and launch the
  stock `actions/runner` with `ACTIONS_RUNNER_INPUT_JITCONFIG=<EncodedJITConfig>`.

**Runners are JIT-ephemeral: one job per runner process, then it exits.** The *runner's*
persistent listener — the thing that wedged — is gone. But the supervisor now owns the only
long-lived loop (the long-poll session), so the *same* failure mode (a half-open poll, "online
but not pulling") moves to it; this is not hand-waved — §4.4/§5 bound each poll and self-heal via
a fresh session, and retarget (not retire) the watchdog at the supervisor. Status: *Public
Preview* — the Go API is stable and we pin it (separate `go.sum`), so client drift is contained;
the residual risk is server-side *protocol* drift, which we hedge by keeping the classic runner
resurrectable (§Migration, §Alternatives). It is GitHub code extracted from ARC.

## Design

### 1. Separate Go module (the hard dependency-isolation requirement)

`bazelscaleset` lives in its **own** Go module with its own `go.mod`/`go.sum`, so scaleset's
closure never enters the FDB module:

```
tools/bazelscaleset/        # nested module — own go.mod (module fdb.dev/tools/bazelscaleset)
  go.mod  go.sum
  main.go  scaler.go  slots.go  README.md
```

- A nested `go.mod` is **automatically excluded** from the parent module's package graph, so the
  root `go.mod`/`go.sum` are untouched and `go build ./...` / `go mod tidy` at the repo root never
  see scaleset.
- It is **excluded from the bazel build**: add `tools/bazelscaleset` to `.bazelignore` so
  `bazel build //...`, gazelle, and `MODULE.bazel`'s `go_deps` never resolve scaleset. The tool is
  **not** part of the product build graph.
- It is built with **plain `go build`** (it is CI/infra tooling, not a bazel target). The binary is
  produced either in a tiny GitHub-hosted CI step or on the box at provision time (pinned by
  version+sha like every other tool in `infra/fetch-verified.sh`).

> Decision (locked): directory is `tools/bazelscaleset/` rather than `cmd/bazelscaleset/`. `cmd/`
> reads as "part of the main module"; a nested module under `cmd/` works but is confusing. Either
> way it is a **separate module** — that is the load-bearing part.

### 2. The bazel-world question: warm vs isolated

At `maxRunners=1` (the locked default, §3) "the pool" is **one** stable work dir that is always
the latest — there is no second slot, so no load-balancing miss onto a cold one. The heart of this
section is therefore simple: *one warm work dir + a shared `repository_cache`, no `disk_cache`,
all on the CI data volume*. The Options framing below is what generalises that to `maxRunners>1`
(the slot pool is the mechanism either way) — the N=1 reader can skim Options B/C.

The tension: ephemeral runners want a *clean room* per job; bazel wants *warmth*. "Warm" is three
distinct caches, with very different **durability** (the thing the first draft overstated):

| Cache | Holds | Keyed by | Persistence |
|---|---|---|---|
| `--output_base` | `bazel-out` + **local action cache** (on disk); JVM server + loading/analysis (Skyframe) cache (**in server memory**) | **workspace path** = `<WorkFolder>/<repo>/<repo>` | disk parts survive across runners *and* reboots; server + analysis survive only while the server process lives |
| `--repository_cache` | downloaded+extracted externals (bzlmod, `go_deps`, the FDB C++ toolchain) | content hash | until pruned |
| `--disk_cache` | content-addressed **action outputs** (a local "remote cache") | action key | — (we do **not** use one) |

**RFC-108 framing (precise).** `.bazelrc` sets no `disk_cache`/`repository_cache` today on purpose
(RFC-108: "every build from source — no external cache trust boundary"). The distinction that
keeps A+C inside that line: a `--disk_cache` is an *imported/persisted artifact CAS* (the trust
boundary RFC-108 rejects); `--repository_cache` is the same `MODULE.bazel.lock`-hash-verified
externals (poisoning it needs local write = same trust domain); a per-slot `--output_base` is
*local live-incremental* build state — it **does** contain a local action cache, so it *is* a
cache, but the box's own just-built outputs, not anything imported. The honest residual cost of a
warm `output_base` reused across hundreds of PRs (vs Option B's fresh room): it is **not** a
from-scratch build and can mask non-hermeticity (undeclared inputs, mtime/symlink edge cases) —
small, but real for a "build from source" project, and the reason §5 keeps prune/expunge levers.

**Option A — warm per-slot work folders (locked core).** A fixed pool of `maxRunners` stable work
folders on the CI volume (`/mnt/ci-data/bazelwork/slot-0…`; see *Filesystem* below). Each JIT
runner gets a slot's stable `WorkFolder`; with the **default** `actions/checkout` the workspace
path `<WorkFolder>/<repo>/<repo>` is stable, so `output_base = output_user_root +
md5(workspace_path)` is stable, so the slot's `bazel-out` + action cache (and, while it lives, its
JVM server + analysis cache) persist across the ephemeral runners cycling through that slot.
Concurrent runners take *different* slots → distinct `output_base`s → per-`output_base` locks, no
cross-slot server-lock contention. `actions/checkout`'s `clean: true` (`git clean -ffdx`) removes
only the convenience symlinks (`bazel-out`, `bazel-bin`) and does **not** follow them into
`output_base` (which lives outside the workspace), so warmth survives the checkout; the sibling
`_work/_actions` and `_work/_tool` caches also persist (intended — cached `checkout`/`setup-*`).
*Assumption:* a workflow that sets a non-default `actions/checkout` `path:` changes the workspace
path → different `output_base` → cold slot.

**Warmth durability — what actually erodes it (and the §7 mitigations).** The disk parts
(`bazel-out`, action cache) are durable; the *server + analysis* are memory-resident and die on:
- **Idle reaping.** `--max_idle_secs` defaults to 3 h and is **unset** in `.bazelrc`. A quiet night
  > 3 h reaps the server → next job pays full loading+analysis + server restart, disk caches
  intact. §7 pins `--max_idle_secs` high so the per-slot server stays resident between jobs — that
  is what makes the *analysis* half of "warm" true.
- **OOM.** This box has a documented OOM history (cloud-init: 10 kills / 42 days, fdbserver 137). A
  resident JVM server (default `-Xmx` ≈ 25 % of 7.6 GiB) next to a 3–4 GiB FDB build is a prime
  victim; a killed server = cold analysis next job. §7 bounds the heap (`--host_jvm_args=-Xmx…`)
  and protects the server via `oom_score_adj` (as we already do for Ryuk).
- **Restart / reboot.** Survives `output_base` disk state, not the server.

So at `maxRunners=1`, *slot-miss* genuinely cannot happen (one slot, always latest — why Option B
is deferred), but cold *server* starts still occur from the above. Option B's `disk_cache` would
also rescue those cold-server cases (cross-restart action reuse) at the trust-boundary cost — the
trade we are deferring, not pretending away.

**Option B — fresh dir per job + shared `--disk_cache`** (deferred). Maximal isolation (fresh
`output_base` every job) + a shared content-addressed action cache. But loading/analysis/server
are **cold every job** (disk_cache caches actions, not analysis) and it **reintroduces a disk
cache** (reconsiders RFC-108). Only worth it once `maxRunners>1` makes slot-miss real; flipping to
it changes only the work-folder + cache flags.

**Option C — shared `--repository_cache`** (folded into A, always-on). One
`--repository_cache=/mnt/ci-data/bazel-repo-cache` for every slot. Hash-verified by
`MODULE.bazel.lock`, so sharing is not a new trust boundary — it just avoids re-downloading the
FDB C++ toolchain etc. for a fresh slot. (Populated via atomic download-to-tmp + rename, so
concurrent multi-slot access is safe; at N=1 it is a single writer anyway.) Note `repository_cache`
saves only the *download* — the FDB-from-source *compile* is a `bazel-out`/Option-A win.

**Filesystem (load-bearing).** Slots **and** the `repository_cache` go on the **CI data volume**
(`/mnt/ci-data`), the same filesystem as `output_base` (`~/.cache/bazel → /mnt/ci-data/bazel`).
Two reasons: (a) the root disk is the scarce 75 GB one RFC-115 already overflowed with the
FDB-from-source build; (b) bazel hardlinks inputs into its sandbox and externals from the repo
cache — *across* filesystems those hardlinks silently fall back to **copies** (slower sandbox
setup, extra I/O). Same-volume keeps hardlinks working and the bulk off the root disk.

**Decision (locked): Option A + C now; B deferred.** Warm per-slot `output_base` (durability tuned
in §7) + shared `repository_cache`, **no** `disk_cache`, all on `/mnt/ci-data`. Scaling = add a
slot (+ RAM, §3).

### 3. Concurrency on one box (RAM-bound)

A bazel build + FDB testcontainers wants ~3–4 GiB; the box has 7.6 GiB. So **`maxRunners` defaults
to 1** — serialize the backlog (matches today; "slow through a backlog is fine"), one warm slot.
`minRunners` (pre-warmed idle runners) defaults to 0 (pure on-demand). The supervisor advertises
`maxCapacity` so GitHub never assigns beyond it.

`maxRunners=2` is **not** a free config flip — it is RAM-gated, i.e. effectively a **hardware**
decision. Two slots mean two resident JVM servers *simultaneously* (the idle slot's server holds
its heap until `--max_idle_secs`), plus up to 2× build + testcontainer RAM if both ever run at
once. On a 7.6 GiB box with a documented OOM history that is the binding constraint, not "active
build RAM" alone. Raising `maxRunners` should come with a bigger `server_type` (or a lower per-job
RAM ceiling), not just a flag edit.

### 4. Runner lifecycle (native JIT, not Docker)

The dockerscaleset example runs each runner in a container; we run it **natively** so jobs keep
using the **host** Docker for FDB testcontainers and share the warm slot:

1. `HandleDesiredRunnerCount` → for each free slot up to target: `GenerateJitRunnerConfig` with
   `WorkFolder = <slot path>`.
2. Launch `run.sh` as a subprocess (the daemon runs as systemd `User=runner`, so no root/uid
   juggling; the stock runner refuses root anyway) with `ACTIONS_RUNNER_INPUT_JITCONFIG=…` and
   `DisableUpdate` (we pin the runner version in `infra/`).
3. The runner takes exactly one job and exits (JIT-ephemeral). A per-runner goroutine `Wait()`s,
   frees the slot, and — if `--sweep-fdb` — kills orphaned FDB testcontainers (a dead test leaks
   them) **without** touching the bazel cache.
4. **Bounded poll + self-heal.** The supervisor wraps each long-poll in a hard timeout
   (`--poll-timeout`, default 2 m, well above the ~50 s idle poll). A half-open poll errors out,
   `listener.Run` returns, the process exits, and `systemd Restart=always` reconnects with a fresh
   session — so the supervisor's own loop cannot silently inherit the runner's old wedge.
5. **Slot-leak guard.** A runner is launched only because a job was assigned+acquired, so one
   should arrive in seconds. If it does not — e.g. the run was *cancelled mid-flight*, the churn
   case that triggered this RFC — `--job-start-timeout` (default 5 m) kills the idle runner and
   reclaims its slot, so a cancelled run can't pin the only slot. (Disabled when `minRunners>0`,
   where idle runners are expected.)
6. **Restart reconciliation.** On startup the supervisor kills stray `run.sh` / `Runner.Listener`
   / `Runner.Worker` from a crashed prior incarnation before accepting work, so it never launches
   a new runner into a slot a stray job is still writing. Warm bazel *servers* are left running (a
   fresh runner reconnects; killing the runner's bazel *client* via its process group already
   releases the `output_base` lock — we do **not** `bazel shutdown`, which would throw away warmth).
7. **Idempotent registration.** A scale-set name is unique per group; a stale set left by a crashed
   run (same name) is deleted before re-creating, so a `Restart=always` daemon can't crash-loop on
   "name already exists".

### 5. Cleanup, disk, self-healing

- **Orphan FDB sweep — per-job *and* periodic.** The supervisor sweeps orphaned
  `foundationdb/foundationdb` containers when the box goes idle (per-runner, never touching the
  bazel cache). This does **not** replace the cloud-init `orphan-fdb-sweep.timer`: that timer fires
  unconditionally every 5 min and catches orphans regardless of *how* the parent died — including a
  supervisor that itself died mid-job, which the per-job sweep would miss. Keep it as
  belt-and-suspenders (it's 30 lines and each FDB orphan leaks ~700 MB RSS).
- **Disk watermark — warmth-preserving levers, in order.** `bazel-out` + the repo cache grow. A
  bare `bazel clean` is the **wrong** routine lever (it deletes `bazel-out` + the local action
  cache → cold actions next build); `--expunge` deletes the whole `output_base` (fully cold slot).
  So: (a) size the 100 GB CI volume so the watermark rarely trips; (b) the **first** lever for disk
  pressure is age-pruning the `repository_cache` (`find -atime`; bazel has no built-in GC for it)
  and dropping stale external-repo versions — none of which touch the warm server/analysis/actions;
  (c) `bazel clean --expunge` **per slot** is the *nuclear, last-resort* lever behind a high
  watermark (it is per-`output_base`, so it resets one slot cleanly without touching others);
  (d) never reach for a bare `bazel clean`.
- **Self-healing.** No *runner* listener to wedge; the supervisor's long-poll is bounded (§4.4), so
  a stuck poll self-restarts via systemd with a fresh session. The heuristic `runner-watchdog` is
  **not retired but retargeted**: its job changes from the classic-runner wedge arms to a simple
  liveness check on `bazelscaleset.service` (process up + poll making progress) — the external net
  for a *wholesale* supervisor hang the in-process poll timeout can't catch. A GitHub API outage
  degrades gracefully: `go-retryablehttp` (built into the scaleset client) backs off and retries;
  no jobs run, nothing wedges, work resumes when the API returns.

### 6. Auth / secrets

GitHub App (preferred — `ClientID` / `InstallationID` / `PrivateKey`, scoped + rotatable) or a PAT
fallback. Secrets arrive via env only (`…_APP_PRIVATE_KEY` / `…_TOKEN`) so they never hit the
process table; non-secret config via flags/env. The App needs the self-hosted-runner admin scope
on the repo to register the scale set + mint JIT configs.

### 7. Build & deploy (infra)

- **Binary build: GitHub-hosted CI step, pinned by sha.** The box has *no* system Go toolchain
  (bazel manages Go; cloud-init installs no Go SDK), so building on the box is a non-starter. Build
  `bazelscaleset` from the nested module in a small GitHub-hosted CI step and fetch it on the box
  via `fetch-verified.sh` (version+sha pin, consistent with every other tool in `infra/`). No
  box-side build, no throw-away bazel workspace.
- **`infra/cloud-init.yaml`:** add a `bazelscaleset.service` (systemd, `User=runner`,
  `Restart=always`, `EnvironmentFile` `0600` for the App private key). Create the warm-slot +
  `repository_cache` dirs **on `/mnt/ci-data`** owned by `runner`. **Retarget** (don't delete) the
  `runner-watchdog` at `bazelscaleset.service`; **keep** the periodic `orphan-fdb-sweep.timer`.
  Keep the classic `config.sh`-register provisioning **behind a flag** (break-glass, see Migration)
  rather than deleting it, until scale sets reach GA.
- **`.bazelrc` (warmth durability, §2):** set `--max_idle_secs` high (keep the per-slot server
  resident between jobs) and bound the server heap with `--host_jvm_args=-Xmx…`; have cloud-init
  set the bazel server's `oom_score_adj` (as it already does for Ryuk). These are what actually
  make the "warm analysis" claim true on this OOM-prone box.
- **`infra/main.tf`:** swap the `github_runner_token` registration var for the App credentials.

## Migration plan

A `tofu apply` that changes `cloud-init.yaml`/`user_data` can force Hetzner to **replace** the box
— which destroys the warm cache this RFC exists to preserve and forces re-registration. (The
`/mnt/ci-data` volume survives a replace; the root disk and the bazel server do not.) So the
migration is applied **out-of-band on the live box**, with cloud-init updated afterward for *future*
fresh provisions only:

1. Land this RFC (review) + the nested module + the binary (pinned).
2. Drop the supervisor onto the **live** box out-of-band (unit + binary + slot dirs via ssh, not a
   recreate), and stand the scale set up **alongside** the classic runner under a *distinct* label.
   Run a few real PRs against it. (Warm-build comparison is only meaningful on the **2nd+** run,
   once the slot has warmed — the first run on a fresh slot is cold by definition.)
3. Flip `runs-on` (or reuse the `gh-runner-fdb` label) to the scale set. **Stop** the classic
   `actions.runner.*` service but keep its provisioning **resurrectable behind a flag**
   (break-glass while scale sets are Public Preview); retarget the watchdog at the supervisor.
4. Fold the changes into `cloud-init.yaml` so a future *fresh* provision reproduces the box —
   accepting that such a provision starts cold (warmth is a runtime property, not a provisioning
   one; the `/mnt/ci-data` caches that survive a replace give it a head start).
5. Keep the tightened watchdog as the interim net until step 3 lands (already deployed).

## Alternatives considered

- **Tightened heuristic watchdog only** (already deployed as interim): heals the wedge in ~6 min;
  still a band-aid — the wedge keeps happening. Good bridge, not the destination.
- **Classic `actions/runner --ephemeral` + a token-refresh service:** also removes the persistent
  listener, but it's the *old* register/listen stack with a known wedge surface and needs us to
  build token-refresh anyway; scale sets are GitHub's forward path and give the better poll
  protocol + `maxCapacity` for free.
- **actions-runner-controller (ARC):** the robust autoscaler, but **requires Kubernetes** on a
  one-box setup — disproportionate.
- **myoung34/docker-github-actions-runner:** ephemeral wrapper, but Docker-in-Docker collides with
  our host-Docker FDB testcontainers + cold per-container bazel.
- **Reduce CI on master push:** rejected by the maintainer — the backlog is legitimate; fix the
  runner, don't shrink coverage.

## Decisions

These were open questions in the draft; the maintainer has now locked them.

1. **Cache strategy: A + C now, B deferred.** Warm per-slot `output_base` + shared
   `repository_cache` (both on `/mnt/ci-data`), **no** `disk_cache`. The disk parts of `output_base`
   are durable; the server + analysis are memory-resident and need `--max_idle_secs` + OOM
   protection to stay warm (§2/§7). B (fresh-room + `disk_cache`) is deferred — at `maxRunners=1`
   slot-miss can't occur, and B reintroduces the RFC-108 trust boundary; it would also rescue
   cold-*server* starts, the trade we knowingly defer. See §2.
2. **Directory/module: `tools/bazelscaleset/`** as a **separate Go module** (its own
   `go.mod`/`go.sum`). `cmd/` was rejected — it reads as "part of the main module". The separate
   module is the load-bearing dependency-isolation property.
3. **Concurrency: `maxRunners=1`, `minRunners=0`** (7.6 GiB box — serialize the backlog; slow is
   fine, wedged is not). `maxRunners=2` is **RAM/hardware-gated, not a config flip**: two resident
   JVM servers + up to 2× build RAM on a box with a documented OOM history (§3).
4. **Build path: plain `go build` from the nested module, in a GitHub-hosted CI step, pinned by
   sha** (the box has no Go toolchain). `.bazelignore`'d so scaleset never enters the bazel graph /
   `MODULE.bazel`. No throw-away bazel workspace.
5. **Auth: GitHub App preferred, PAT fallback.** The App credential
   (`ClientID`/`InstallationID`/`PrivateKey`, self-hosted-runner admin scope on `birdayz/fdb-go`)
   does not exist yet and must be created by the maintainer before the smoke test; PAT can bootstrap.
6. **Reproducibility: within RFC-108, with one honest caveat.** A shared `repository_cache` is the
   same hash-verified externals (`MODULE.bazel.lock`); a per-slot `output_base` is local
   live-incremental state (incl. a local action cache — a cache, but not an *imported* CAS). Only
   `disk_cache` crosses the boundary, and we don't use one. The price of a warm `output_base` is
   that it isn't a from-scratch build and can mask non-hermeticity — mitigated by the §5
   prune/expunge levers, not eliminated.
