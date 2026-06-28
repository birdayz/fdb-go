# bazelscaleset

A small supervisor for a **GitHub Actions runner scale set** on a single self-hosted box.
It replaces the wedge-prone classic register-and-listen runner with **JIT-ephemeral**
runners (one job per process, then exit) that are pinned to **warm bazel work-slots**, so
there is no long-lived listener to wedge and bazel stays warm across jobs.

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

1. On start it registers a runner scale set (`--name`, `--labels`) against `--url`, opens a
   long-poll message session, and runs the listener. A stale scale set left by a crashed
   previous run (same name) is deleted first so a `Restart=always` daemon never crash-loops.
2. When GitHub reports assigned jobs, the listener calls the scaler, which launches the stock
   `actions/runner` (`run.sh`) as a subprocess with `ACTIONS_RUNNER_INPUT_JITCONFIG`. Each
   runner is handed a **stable per-slot `WorkFolder`**, so its bazel `output_base` (server +
   analysis + action cache + `bazel-out`) persists across the ephemeral runners that cycle
   through that slot.
3. The runner takes exactly one job and exits. A per-runner goroutine reaps it, frees the slot,
   and — when the box goes idle — sweeps orphaned `foundationdb/foundationdb` containers (a dead
   test can leak them) **without** touching the bazel cache.
4. On `SIGTERM`/`SIGINT` it signals every in-flight runner's process group, waits up to
   `--grace-period`, force-kills stragglers, then deletes the scale set and closes the session.

At `--max-runners=1` (the default for a 7.6 GiB box) the backlog is serialized through one
always-warm slot. Raise it (and add RAM) to run more slots concurrently; each stays
independently warm.

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
| `--max-runners` | `BAZELSCALESET_MAX_RUNNERS` | `1` | concurrent runners = warm slots |
| `--min-runners` | `BAZELSCALESET_MIN_RUNNERS` | `0` | pre-warmed idle runners |
| `--runner-dir` | `BAZELSCALESET_RUNNER_DIR` | `/home/runner/actions-runner` | dir with `run.sh` |
| `--work-base` | `BAZELSCALESET_WORK_BASE` | `/srv/bazelwork` | base dir for warm slots |
| `--sweep-fdb` | `BAZELSCALESET_SWEEP_FDB` | `true` | sweep orphaned FDB containers when idle |
| `--grace-period` | `BAZELSCALESET_GRACE_PERIOD` | `60s` | shutdown grace before SIGKILL |
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
  --work-base /srv/bazelwork \
  --log-level debug
```

Push a trivial workflow with `runs-on: smoke-test`; confirm a JIT runner spawns, runs the job,
exits, frees its slot, and a second run reuses the warm slot. `Ctrl-C` deletes the scale set.

## Run under systemd

Run as `User=runner` (the stock runner refuses to run as root) with `Restart=always`. Production
wiring (unit file, slot dirs, secret `EnvironmentFile`, binary pinning) lives in `infra/`.
