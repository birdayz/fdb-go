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

## Prerequisites

- `tofu` (or `terraform`) ≥ 1.7, the `hcloud` + `minio` providers (`tofu init`).
- Environment / variables:
  - `HCLOUD_TOKEN` — Hetzner Cloud API token (provider auth).
  - `MINIO_USER` / `MINIO_PASSWORD` — Hetzner Object Storage S3 credentials (for report upload).
  - `-var github_runner_token=<TOKEN>` — a **short-lived (~1 h)** runner registration token:
    `gh api -X POST repos/birdayz/fdb-record-layer-go/actions/runners/registration-token --jq .token`.
    It is consumed once by `config.sh` at first boot and is useless afterward; nothing secret
    is baked into a persisted image layer.

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

- **`runner-watchdog`** (every 10 min): restarts the runner only when GitHub has **queued
  runs** AND either the listener has been silent > 8 min (a wedged connection) OR the 1-min
  load has exceeded `4×ncpu` for two consecutive checks (a *thrash*-wedge that keeps the box
  busy, not silent — the failure mode that took the runner down under rapid-push
  concurrency-cancel churn). A 15-min cooldown prevents restart storms mid-spike.
- **`orphan-fdb-sweep`** (every 5 min): kills FDB testcontainers running > 30 min (orphans
  whose parent test died) and pins Ryuk's OOM score so the kernel reaps it last.

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
