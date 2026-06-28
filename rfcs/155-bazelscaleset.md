# RFC-155: bazelscaleset — a bazel-warm, dependency-isolated runner scale set for the single CI box

**Status:** DRAFT — proposed, needs review before implementation. CI/infra item — no
query-engine or client/wire behaviour change (no Graefe / FDB-C++ gate). Supersedes the
classic register-and-listen runner in `infra/` for the self-hosted box; leaves the PR gates
(`ci.yml`) and the nightlies unchanged.

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
   its examples). These must **not** leak into the FDB module's `go.mod`/`go.sum`/`MODULE.bazel`.
   The CI tool and the product must not share a dependency closure.

The classic runner satisfies (2) and (3) but fails (1); naive ephemeral satisfies (1) but loses
(3). This RFC gets all four.

## Goals / Non-goals

**Goals**
- Eliminate the persistent-listener wedge *class*, not heal it after the fact.
- Keep bazel warm across ephemeral jobs (warm server + analysis + action cache + `bazel-out`).
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

**Runners are JIT-ephemeral: one job per runner process, then it exits.** There is no long-lived
listener to wedge — the bug class is gone structurally. Status: *Public Preview* (API stable,
examples may shift); it is GitHub code extracted from ARC.

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

### 2. The bazel-world question: warm vs isolated (the open design space)

The tension: ephemeral runners want a *clean room* per job; bazel wants *warmth*. What "warm"
means here, precisely — three distinct caches:

| Cache | Holds | Keyed by | Cost if cold |
|---|---|---|---|
| `--output_base` | JVM server, loading+analysis cache, **local action cache**, `bazel-out` | **workspace path** | server restart + full re-analysis + recompile |
| `--repository_cache` | downloaded+extracted externals (bzlmod, `go_deps`, http_archives) | content hash | re-download the whole graph (FDB C++, rules, …) |
| `--disk_cache` | content-addressed **action outputs** (a local "remote cache") | action key | recompute every action |

`.bazelrc` today sets **no remote/disk cache** on purpose (RFC-108: "every build from source — no
external cache trust boundary"). A `--disk_cache` is a *poisonable artifact cache*; a stable
`--output_base` is just **normal incremental bazel** (the box's own just-built outputs inside a
live server) — not the trust boundary RFC-108 rejected. That distinction drives the options.

**Option A — warm per-slot work folders (recommended core).** Keep a fixed pool of `maxRunners`
stable work folders (`/srv/bazelwork/slot-0…`). Each JIT runner is handed a slot's stable
`WorkFolder`, so the checkout path is stable → the slot's `output_base` is stable → its **JVM
server, analysis cache, action cache, and `bazel-out` persist across the ephemeral runners that
cycle through that slot.** "Each runner gets its own bazel world" = *its slot*. Concurrent runners
take *different* slots → independent `output_base`s → no server-lock contention, no cross-job
contamination. Sequential runners on a slot reuse the warm world. **No disk_cache → RFC-108's
"from source" stance is preserved.** `actions/checkout` already does `clean: true`, so a reused
work dir is not a contamination risk; bazel invalidates analysis on real source changes.

**Option B — fresh dir per job + shared `--disk_cache`.** Maximal isolation (a brand-new
`output_base` every job) with a shared content-addressed action cache for cross-job reuse. But:
loading+analysis+server are **cold every job** (disk_cache caches *actions*, not analysis), and it
**reintroduces a disk cache** → reconsiders RFC-108. This is the "shared disk" you floated; it's
viable if we value fresh-room isolation over warm analysis and accept the reproducibility shift.

**Option C — shared `--repository_cache` (orthogonal, always-on).** Point every slot/job at one
`--repository_cache=/srv/bazel-repo-cache`. The externals are hash-verified by `MODULE.bazel.lock`
already, so sharing them is *not* a new trust boundary — it just avoids re-downloading the FDB C++
toolchain etc. when a fresh slot is first used. Low risk, high value. Fold into A or B.

**Decision (locked): Option A + C now; B deferred.** Warm per-slot `output_base` **plus** a shared
`repository_cache`, **no** `disk_cache`. Fully warm (server+analysis+actions+`bazel-out` per slot),
"own bazel world" per slot, shared externals so a cold slot isn't a cold *download*, and RFC-108's
no-artifact-cache reproducibility property intact. Scaling = add a slot (+ RAM).

B is the future floor for the **slot-miss** problem (a fresh slot with a cold `output_base`), but at
`maxRunners=1` there is exactly **one** slot and it is always the latest — slot-miss cannot happen,
so B buys nothing now and would reintroduce a `disk_cache` (the RFC-108 trust boundary). Revisit B
only if `maxRunners` grows past 1; flipping to it changes only the work-folder + cache flags.

### 3. Concurrency on one box (RAM-bound)

A bazel build + FDB testcontainers wants ~3–4 GiB; the box has 7.6 GiB. So **`maxRunners` defaults
to 1** — serialize the backlog (matches today, and "slow through a backlog is fine"), one warm
slot, no RAM blowup. `maxRunners=2` is a config flip *if* RAM grows; each slot stays independently
warm. `minRunners` (pre-warmed idle runners) defaults to 0 (pure on-demand) but can be 1 to shave
the first-job latency. The supervisor advertises `maxCapacity` so GitHub never assigns beyond it.

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

### 5. Cleanup, disk, self-healing

- **Orphan FDB sweep** per job (replaces the cloud-init `orphan-fdb-sweep` timer; same intent).
- **Disk watermark:** `bazel-out` + repo cache grow. A periodic check `bazel clean` (or
  `--expunge` per slot) above a disk threshold; bounded, and far rarer than today's cold builds.
- **Self-healing is mostly free now:** no persistent listener to wedge. If the *poll session*
  drops, the daemon reconnects; if the daemon dies, `systemd Restart=always` brings it back with a
  fresh session. The heuristic `runner-watchdog` (silence/load/queued arms) is **retired** — its
  whole reason for existing was the wedge this design removes. (We keep a trivial systemd liveness
  restart only.)

### 6. Auth / secrets

GitHub App (preferred — `ClientID` / `InstallationID` / `PrivateKey`, scoped + rotatable) or a PAT
fallback. Secrets arrive via env only (`…_APP_PRIVATE_KEY` / `…_TOKEN`) so they never hit the
process table; non-secret config via flags/env. The App needs the self-hosted-runner admin scope
on the repo to register the scale set + mint JIT configs.

### 7. Build & deploy (infra)

- `infra/cloud-init.yaml`: drop the `config.sh` register step + `runner-watchdog`; add a
  `bazelscaleset.service` (systemd, `User=runner`, `Restart=always`, env-file for the App secret).
- `infra/main.tf`: swap the `github_runner_token` registration var for the App credentials; create
  the warm-slot + repo-cache dirs.
- Pin the `bazelscaleset` binary (built from the nested module) by version+sha in
  `fetch-verified.sh`, consistent with the reproducibility rationale.

## Migration plan

1. Land this RFC (review) + the nested module + the binary.
2. Stand up the scale set **alongside** the classic runner under a *distinct* label, run a few real
   PRs against it, compare warm-build times slot-vs-classic.
3. Flip `runs-on` (or reuse the `gh-runner-fdb` label) to the scale set; retire the classic
   `actions.runner.*` service + the watchdog via `tofu apply`.
4. Keep the tightened watchdog as the *interim* safety net until step 3 lands (already deployed).

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
   `repository_cache`, **no** `disk_cache`. B (fresh-room-per-job + shared `disk_cache`) is the
   future floor for the slot-miss problem only; at `maxRunners=1` slot-miss cannot occur, so B is
   skipped to avoid reintroducing a `disk_cache` and the RFC-108 trust-boundary shift. See §2.
2. **Directory/module: `tools/bazelscaleset/`** as a **separate Go module** (its own
   `go.mod`/`go.sum`). `cmd/` was rejected — it reads as "part of the main module". The separate
   module is the load-bearing dependency-isolation property.
3. **Concurrency: `maxRunners=1`** (7.6 GiB box — serialize the backlog; slow is fine, wedged is
   not). `minRunners=0` (pure on-demand). Both are config flips if RAM grows; each slot stays
   independently warm.
4. **Build path: plain `go build`** from the nested module, `.bazelignore`'d so scaleset never
   enters the bazel graph / `MODULE.bazel`. No throw-away bazel workspace for the tool.
5. **Auth: GitHub App preferred, PAT fallback.** The App credential
   (`ClientID`/`InstallationID`/`PrivateKey`, self-hosted-runner admin scope on `birdayz/fdb-go`)
   does not exist yet and must be created by the maintainer before the smoke test; PAT can bootstrap.
6. **Reproducibility: confirmed within RFC-108.** A shared `repository_cache` is the same
   hash-verified externals (`MODULE.bazel.lock`), not a new trust boundary; a stable per-slot
   `output_base` is incremental local bazel (the box's own just-built outputs in a live server), not
   an artifact cache — both clearly within the "build from source" line. Only `disk_cache` would
   cross it, and we do not use one.
