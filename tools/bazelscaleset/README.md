# bazelscaleset

A small supervisor for a **GitHub Actions runner scale set** spanning a self-hosted main
box and, optionally, remote pool boxes reached over SSH. It replaces the wedge-prone
classic register-and-listen runner with **JIT-ephemeral** runners (one job per process,
then exit) that are pinned to **warm bazel work-slots**, so there is no long-lived
listener to wedge and bazel stays warm across jobs.

Design and rationale: [`rfcs/155-bazelscaleset.md`](../../rfcs/155-bazelscaleset.md).

## Why a separate Go module

`github.com/actions/scaleset` pulls `golang-jwt/jwt`, `google/uuid`, and
`hashicorp/go-retryablehttp`. This tool is its **own** Go module (`fdb.dev/tools/bazelscaleset`)
so that closure **never** enters the FDB module's `go.mod`/`go.sum`/`MODULE.bazel`. The directory
is listed in the repo-root `.bazelignore`, so bazel, gazelle, and `MODULE.bazel`'s `go_deps`
never see it. It is built with plain `go build`, not bazel.

```sh
cd tools/bazelscaleset
go mod tidy        # populates go.sum from the module cache
go vet ./...
go build -ldflags "-X main.version=$(git rev-parse --short HEAD)" -o bazelscaleset .
```

## How it works

1. On start it ensures a runner scale set (`--name`, `--labels`) exists against `--url` —
   reusing it by name if already present (patching drifted config in place), creating it only
   if missing — then opens a fresh long-poll message session and runs the listener. The scale
   set is never deleted by the supervisor: GitHub tracks in-flight job assignment against its
   stable ID, so deleting and recreating it (the previous behaviour, on every restart) orphans
   any job already assigned/queued against the old ID. See `ensureRunnerScaleSet` in
   `scaleset.go`.
2. When GitHub reports assigned jobs, the listener calls the scaler, which launches the stock
   `actions/runner` (`run.sh`) as a subprocess with `ACTIONS_RUNNER_INPUT_JITCONFIG`. Each
   runner is handed a **stable per-slot `WorkFolder`**, so its bazel `output_base` (server +
   analysis + action cache + `bazel-out`) persists across the ephemeral runners that cycle
   through that slot.
3. The runner takes exactly one job and exits. A per-runner goroutine reaps it, frees the slot,
   and — when the box goes idle — sweeps orphaned `foundationdb/foundationdb` containers (a dead
   test can leak them) **without** touching the bazel cache.
4. On `SIGTERM`/`SIGINT` it signals every in-flight runner's process group, waits up to
   `--grace-period`, force-kills stragglers, then closes the message session (the scale set
   itself is left registered so the next start reuses it — see above).

At `--max-runners=1` (the default for a 7.6 GiB box) the backlog is serialized through one
always-warm slot. Raise it (and add RAM) to run more slots concurrently; each stays
independently warm.

## Multi-host topology (`--remote-hosts`)

A scale set allows **one message session**, so there is exactly ONE listener: this
supervisor, on the main box. `--remote-hosts user@h1,user@h2,...` fans slots out to pool
boxes:

- **Slot 0 is always local** and unchanged (cloned runner dir, warm work slot).
- **Slot i>0 executes on host i-1 over SSH.** The JIT config is minted exactly as for a
  local slot (with the host's `--remote-work-base`/slot-0 as WorkFolder) and delivered on
  **stdin** into a remote wrapper — never argv, which is world-readable in the remote
  process table. The wrapper starts the box's stock runner (`--remote-runner-dir`, the
  cloud-init-extracted install) under `setsid` in a **detached session** whose own
  `echo $$` writes a pidfile; the runner therefore **survives the supervisor's ssh
  connection dropping** and finishes its one job unattended.
- **Liveness is polled** (a small ssh probe against the pidfile, cmdline-guarded against
  pid reuse); when the runner exits the slot frees, and the **orphan-FDB sweep runs on
  that host** (Docker state is per-box, so the sweep is per-box too, gated on no runner
  being active there). Kill/shutdown escalation (TERM → grace → KILL) goes over ssh as a
  process-group kill of the detached session.
- SSH is the **system ssh binary** (`BatchMode=yes`, `ConnectTimeout`,
  `StrictHostKeyChecking=accept-new`, identity from `--remote-ssh-key`) — auditable, and
  host-key/agent policy stays in ssh_config. Host entries must be plain `user@host`
  (whitespace or a leading `-` is rejected, so a host can never smuggle an ssh option).
- With remote hosts set, `--max-runners` defaults to **1 + len(hosts)** and may not
  exceed it.

Failure modes:

- **Host down at launch**: the slot is marked unhealthy for a cooldown (1 min) and does
  not count toward capacity; the acquisition falls through to the remaining slots and the
  job otherwise stays acquired for the next poll cycle. The never-connected JIT
  registration is removed server-side. A dead pool box never crashes the supervisor
  (an error out of the scaler would exit `listener.Run`).
- **Host lost mid-job**: sustained probe failures (~5 min) reclaim the slot; the JIT
  config is single-use, so a partitioned-but-alive runner finishing later cannot
  double-run anything.
- **Supervisor crash**: on restart, remote pidfiles are reconciled per host (kill the
  recorded session's process group, cmdline-guarded) exactly like local slots.

## Zombie-job watchdog (`--job-terminal-grace`)

GitHub can mark a job terminal (failed/cancelled) server-side while the local
Runner.Worker keeps running — measured live: a run marked failed at 05:54Z whose worker
squatted the only slot until it was manually killed ~4.5 h later. Two signals mark a
runner's job terminal: the session's `JobCompleted` message, and (for the case where that
message never arrives) a periodic probe of the JIT runner's server-side record, which
GitHub deletes once the job is terminal. Once terminal for longer than
`--job-terminal-grace` (default 10 m) with the runner process still alive, the runner's
process group is killed, the job id logged, and the slot reclaimed. `0` disables.

## Reliability

The classic runner wedged because its long-lived listener could go half-open — alive at TCP,
dead at the app layer — and sit "online but not pulling jobs" forever. Going JIT-ephemeral
removes the runner's listener, but the supervisor now owns the only long-lived loop (the
scaleset long-poll), so the same failure mode is handled head-on:

- **Bounded long-poll** (`--poll-timeout`): every poll has a hard ceiling. A half-open poll
  errors out, `listener.Run` returns, the supervisor exits, and `systemd Restart=always` brings
  it back with a fresh session. Each successful poll also stamps `--heartbeat-file`; an external
  systemd watchdog (see `infra/`), **retargeted** from the old classic-runner watchdog (not
  retired), restarts the service if that stamp goes stale — catching a wholesale hang the
  in-process timeout can't.
- **Slot-leak guard** (`--job-start-timeout`): a runner launched for a job that gets cancelled
  before it connects (the churn case that triggered this) is killed and its slot reclaimed, so a
  cancelled run can't pin the only slot.
- **Restart reconciliation**: each runner records its PGID in a per-slot pid file; on startup the
  supervisor SIGKILLs the whole **process group** of any slot whose pid file survived a crash
  (run.sh + Runner.Listener + Runner.Worker + the job's bazel client), so a stray job can't keep
  writing a slot the pool treats as free. It is scoped to **our** slot pid files (never touches a
  classic/other runner on the host) and leaves warm bazel servers running (a fresh runner
  reconnects; killing the bazel client already frees the `output_base` lock).
- **Idempotent, non-destructive registration**: a scale set is a durable resource, looked up by
  name and reused by ID on every start (crash, clean shutdown, or watchdog restart alike); a
  config drift (e.g. `--labels` changed across a redeploy) is patched in place via
  `UpdateRunnerScaleSet`, never by deleting and recreating. Deleting would sever the ID GitHub
  routes in-flight job assignments against — the root cause of a real production incident where
  a systemd-watchdog restart under heavy CI load orphaned a multi-PR backlog that sat `queued`
  forever.

## Configuration

Every flag also reads a `BAZELSCALESET_<UPPER_SNAKE>` env var. **Secrets are env-only** (never
flags, so they never reach the process table):

| Secret env var | Purpose |
|---|---|
| `BAZELSCALESET_APP_PRIVATE_KEY` | GitHub App private key (PEM) |
| `BAZELSCALESET_TOKEN` | Personal access token (PAT fallback) |

| Flag | Env | Default | Notes |
|---|---|---|---|
| `--url` | `BAZELSCALESET_URL` | — | **required**, e.g. `https://github.com/birdayz/fdb-go` |
| `--name` | `BAZELSCALESET_NAME` | — | **required**, scale set name (also the default label) |
| `--labels` | `BAZELSCALESET_LABELS` | `--name` | comma-separated `runs-on` labels |
| `--runner-group` | `BAZELSCALESET_RUNNER_GROUP` | `default` | runner group name |
| `--max-runners` | `BAZELSCALESET_MAX_RUNNERS` | `1` (or `1+len(remote-hosts)`) | concurrent runners = slots; slots get per-slot runner roots. With `--remote-hosts` it may not exceed 1 local + one per host |
| `--min-runners` | `BAZELSCALESET_MIN_RUNNERS` | `0` | pre-warmed idle runners |
| `--runner-dir` | `BAZELSCALESET_RUNNER_DIR` | `/home/runner/actions-runner` | dir with `run.sh` |
| `--work-base` | `BAZELSCALESET_WORK_BASE` | `/mnt/ci-data/bazelwork` | base dir for warm slots — keep on the CI data volume, same filesystem as bazel's `output_base`, **not** the root disk |
| `--sweep-fdb` | `BAZELSCALESET_SWEEP_FDB` | `true` | sweep orphaned FDB containers when idle |
| `--grace-period` | `BAZELSCALESET_GRACE_PERIOD` | `60s` | shutdown grace before SIGKILL |
| `--poll-timeout` | `BAZELSCALESET_POLL_TIMEOUT` | `2m` | hard ceiling on a single long-poll; on timeout the supervisor exits for systemd to restart with a fresh session (must be ≥ 60s) |
| `--job-start-timeout` | `BAZELSCALESET_JOB_START_TIMEOUT` | `5m` | kill a launched runner that never starts a job and reclaim its slot (on-demand only; `0` disables) |
| `--job-terminal-grace` | `BAZELSCALESET_JOB_TERMINAL_GRACE` | `10m` | kill a runner whose job has been terminal on the GitHub side this long while its process is still alive, and reclaim the slot (`0` disables) |
| `--remote-hosts` | `BAZELSCALESET_REMOTE_HOSTS` | _(unset)_ | comma-separated `user@host` pool boxes; slot 0 stays local, slot i runs on host i-1 over SSH |
| `--remote-ssh-key` | `BAZELSCALESET_REMOTE_SSH_KEY` | `/etc/bazelscaleset/remote_id` | SSH identity file for `--remote-hosts` |
| `--remote-runner-dir` | `BAZELSCALESET_REMOTE_RUNNER_DIR` | `/home/runner/actions-runner` | extracted actions/runner dir on every pool box |
| `--remote-work-base` | `BAZELSCALESET_REMOTE_WORK_BASE` | `/mnt/ci-data/bazelwork` | warm work base on every pool box (one slot per host at `<base>/slot-0`) |
| `--heartbeat-file` | `BAZELSCALESET_HEARTBEAT_FILE` | _(unset)_ | if set, stamp a unix timestamp on each successful poll for an external watchdog to check (e.g. `/run/bazelscaleset/heartbeat`) |
| `--app-client-id` | `BAZELSCALESET_APP_CLIENT_ID` | — | GitHub App client/app id |
| `--app-installation-id` | `BAZELSCALESET_APP_INSTALLATION_ID` | — | GitHub App installation id |

## GitHub App setup (preferred over a PAT)

A GitHub App authenticates the supervisor to register the scale set and mint JIT configs.

1. **Create the App** (org or repo owner → Settings → Developer settings → GitHub Apps → New).
   - Permissions → **Repository → Self-hosted runners: Read & write** (and **Administration:
     Read & write** if your org scopes runner groups there).
   - No webhook needed. Note the **App ID / Client ID**.
2. **Generate a private key** (PEM) and keep it secret.
3. **Install the App** on `birdayz/fdb-go` (or the org) and note the **Installation ID** (it is
   in the install URL: `.../installations/<id>`).
4. Provide them to the daemon:
   - `--app-client-id <client-or-app-id>`, `--app-installation-id <installation-id>` (flags/env),
   - `BAZELSCALESET_APP_PRIVATE_KEY` = the PEM contents (env, e.g. via a systemd `EnvironmentFile`
     with `0600` perms).

To bootstrap without an App, set `BAZELSCALESET_TOKEN` to a PAT with the self-hosted-runner admin
scope on the repo.

## Smoke test

Against a throwaway scale set and label, with the App or a PAT exported:

```sh
./bazelscaleset \
  --url https://github.com/birdayz/fdb-go \
  --name smoke-test --labels smoke-test \
  --runner-dir /home/runner/actions-runner \
  --work-base /mnt/ci-data/bazelwork \
  --log-level debug
```

Push a trivial workflow with `runs-on: smoke-test`; confirm a JIT runner spawns, runs the job,
exits, frees its slot, and a second run reuses the warm slot. `Ctrl-C` closes the message
session; the `smoke-test` scale set stays registered and a subsequent run reuses it (delete it
manually via `DeleteRunnerScaleSet`/the GitHub API once you're done with a throwaway name).

## Run under systemd

Run as `User=runner` (the stock runner refuses to run as root) with `Restart=always`. Production
wiring (unit file, slot dirs, secret `EnvironmentFile`, binary pinning) lives in `infra/`.
