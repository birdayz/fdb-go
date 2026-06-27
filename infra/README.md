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
