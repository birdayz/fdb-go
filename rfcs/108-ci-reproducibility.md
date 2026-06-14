# RFC-108: CI reproducibility & supply chain — pin + checksum the runner, document the stand-up

**Status:** DRAFT — awaiting Torvalds + codex ACK. CI/infra item (TODO-production P1.8, client
launch-readiness #5). **No product-code change** — touches only `infra/` (OpenTofu + cloud-init)
and `.github/workflows/` (a hosted-fallback smoke job + a docs pointer). No query-engine or
client/wire behaviour change ⇒ no Graefe / FDB-C++ gate; the gate is Torvalds (infra/code
quality) + codex, same as RFC-107.

## Problem — the green check is not reproducible, and the supply chain is unpinned

CI runs on a single self-hosted personal Hetzner box (`runs-on: [self-hosted, linux, x64,
hetzner]`, provisioned by `infra/main.tf` + `infra/cloud-init.yaml`). Two distinct problems
(the bus-factor-of-one is acknowledged won't-fix in TODO-production; this RFC attacks the
*reproducibility* and *supply-chain* axes, which are fixable):

1. **The runner image is not reproducible.** Re-provisioning the box on two different days
   produces two different toolchains, because several inputs float:
   - **GitHub Actions runner = `releases/latest`** (`cloud-init.yaml` runcmd: `…/releases/latest
     | jq .tag_name`). Whatever Actions shipped that morning. No version pin, no checksum.
   - **`mc` (MinIO client) = `release/linux-amd64/mc`** — the moving "latest" object. No pin,
     no checksum.
   - **`just` via `curl https://just.systems/install.sh | bash`** — a remote script piped to a
     root shell at provision time; the script and what it fetches can change.
   - **Base image `ubuntu-24.04`** (`main.tf`) is a rolling Hetzner label, not a snapshot id.
   - **apt** (`package_update: true`, unpinned `docker.io`, `build-essential`, …) installs
     whatever the Ubuntu mirror serves that day.

2. **Even the pinned inputs aren't verified, and one is the wrong version.**
   - FDB client `.deb`, **bazelisk v1.28.1**, **just 1.48.1** are version-pinned but downloaded
     with **no checksum** — a compromised or corrupted artifact installs silently.
   - **`fdb_version` defaults to `7.3.46`** (`main.tf`) while the test oracle pinned everywhere
     else — `MODULE.bazel`, the testcontainers default — is **7.3.75**. The host's `libfdb_c`
     (what the `pkg/fdbgo/bench` differential compares against) is a *different FDB version* than
     the container the tests run against. That is a latent correctness skew, not just a
     reproducibility nit.

3. **No external adopter can reproduce the green signal.** There is no documented way to stand
   up an equivalent runner, and no job runs on a public (GitHub-hosted) runner, so a fork sees
   a green check it cannot reproduce or re-run on its own infrastructure.

This is the launch-readiness gap: "a datastore's CI passing on one person's unversioned box" is
not a trustworthy signal for anyone betting a production workload on the library.

What is **already good** (and this RFC keeps): the runner is OpenTofu-managed (infra-as-code, so
it is reproducible *in structure*); a `runner-watchdog` restarts a wedged listener; an
`orphan-fdb-sweep` kills leaked FDB containers and protects Ryuk's OOM score. RFC-107 already
pinned the FDB *test container* to 7.3.75 by tag.

## Proposed change — pin everything by (version + SHA-256), fix the skew, document the stand-up

Five parts, all in `infra/` + `.github/`, no product code.

### 1. Pin + checksum every downloaded artifact (the supply-chain hard line)

Replace every floating/unchecked download in `cloud-init.yaml` with a `{version, sha256}` pair
verified before use. A helper used by each step:

```bash
fetch_verified() { # url sha256 dest
  curl -fsSL -o "$3" "$1"
  echo "$2  $3" | sha256sum -c - || { echo "::error::checksum mismatch for $1"; exit 1; }
}
```

- **GitHub Actions runner:** drop `releases/latest`; pin `RUNNER_VERSION` to an explicit tag and
  verify the tarball against the SHA-256 the runner release publishes (the
  `actions-runner-linux-x64-<v>.tar.gz` digest in the release notes). A new runner version is a
  deliberate, reviewed bump, not a silent daily drift.
- **`mc`:** pin to a dated MinIO release path (`.../mc.RELEASE.<ts>`) + its published SHA-256,
  not the rolling `release/linux-amd64/mc`.
- **`just`:** drop `curl … | bash`. Download the pinned `just-1.48.1-x86_64-unknown-linux-musl.tar.gz`
  release tarball + verify its SHA-256, then extract. No remote script in the root shell.
- **bazelisk / FDB `.deb`:** keep the version pins, **add** SHA-256 verification via
  `fetch_verified`.
- **apt:** pin the key packages to explicit versions where it buys real reproducibility
  (`docker.io` in particular, since the test path is Docker-heavy); document that the rest ride
  the Ubuntu point-release. (Full apt determinism needs a snapshot mirror — out of scope; we pin
  what materially affects the tests and note the residue, per RFC-107's "no silent caps" rule.)

All checksums live in **one `infra/versions.lock` file** (or `locals` in `main.tf`) so a version
bump is a single reviewed diff that updates `{version, sha256}` together — never one without the
other.

**Bazel-side anchors (already pinned — this RFC just states + asserts them).** The
reproducibility of the *build* itself does not depend on the runner image: `.bazelversion`
(`9.0.1`) pins Bazel, and `MODULE.bazel.lock` (committed, ~82 KB) pins every bzlmod dependency
by hash. bazelisk on the runner reads `.bazelversion`, so the box's bazelisk is just a launcher —
the actual Bazel + all deps are pinned in-repo. The RFC adds a CI assertion that `.bazelversion`
and `MODULE.bazel.lock` are present and that `bazelisk mod deps`/`tofu`-installed bazelisk resolve
to them (a drift between `.bazelversion` and the installed bazelisk is caught, not silent).

**Cache provenance.** `.bazelrc` configures **no remote or disk cache** — every build is from
source on the runner, so there is no external cache trust boundary to pin and no cross-build
contamination vector. (An external adopter therefore pays a full cold build; acceptable, and the
honest trade for zero cache-poisoning surface. If a remote cache is ever added, its trust boundary
comes back here as a follow-up RFC.)

**Secrets.** Nothing secret is committed or baked into a persisted image layer. The GitHub runner
**registration token** is a short-lived (~1 h) credential passed as a `sensitive` tofu variable at
`apply` time and consumed once by `config.sh` (cloud-init runs once on first boot; the token is
useless after registration). `HCLOUD_TOKEN` and the S3 creds are `apply`-time env / CI secrets,
never in the repo. The pin work changes none of this; the RFC records it so the supply-chain
picture is complete.

### 2. Fix the FDB version skew

Set the `fdb_version` default to **7.3.75** to match `MODULE.bazel` and the testcontainers
default, so the host `libfdb_c` the differential harness links is the *same* FDB the tests run
against. If there is a real reason the host client must differ (there isn't one known), document
it at the variable; otherwise this is a one-line correctness fix folded into the pin work.

### 3. Pin the base image

Pin the Hetzner server `image` to a specific Ubuntu 24.04 **snapshot id** (or, if Hetzner only
exposes the rolling label, record the exact image id `tofu` resolved into `infra/versions.lock`
and assert it on apply). The cloud-init pins ride on top; the base must not float underneath them.

### 4. Reproducible-from-code + ephemeral option (kill the wedge at the source)

The runner is already rebuildable via `tofu apply`. Two hardening steps:

- **Document the one-command stand-up** in `infra/README.md`: the exact `tofu` invocation, the
  required env (`HCLOUD_TOKEN`, runner registration token, S3 creds), and the pinned versions —
  so *anyone* can provision a byte-for-byte-equivalent runner. The green check becomes
  reproducible by construction.
- **Offer an ephemeral runner** (`config.sh --ephemeral` + a re-register loop / `--once` under a
  restart unit): the runner processes exactly one job, then exits with fresh state. This removes
  the *root cause* of the failure mode we just hit in production — a wedged listener and an
  orphaned bazel server surviving a concurrency-cancelled job — instead of relying on the
  watchdog's ≤30-min-latency restart. Trade-off (state per RFC, Torvalds to weigh): ephemeral
  loses the warm bazel/Docker-layer cache between jobs (slower cold builds) and adds
  re-registration latency. **Proposal:** make it opt-in via a `runner_ephemeral` tofu variable
  (default keep the persistent+watchdog model that already self-heals), and additionally **lower
  the watchdog's silence threshold** from 30 min to ~8 min for the persistent model so a wedge
  self-heals in one CI-job-length rather than half an hour.

  **Name the actual trigger (Torvalds).** The wedge this shift was not random: rapid pushes to a
  PR each fire `ci.yml`, whose `concurrency: cancel-in-progress` cancels the prior run and
  requeues; on a *single serial* runner a heavy FDB-container job is killed mid-flight and a fresh
  one starts, repeatedly — load hit ~45–63 on a 4-vCPU box and the cancelled job left an orphaned
  bazel server + a wedged listener. The silence-threshold drop only treats the *symptom* (slow
  recovery) — and a restart fired *during* the spike can re-wedge. Two root-cause fixes, both here:
  (a) **ephemeral-on-cancel** — an ephemeral runner exits after each job, so a cancelled job leaves
  *no* surviving bazel server or wedged listener for the next job to trip over, making a load-based
  trigger unnecessary; (b) for the persistent default, add a **load-aware arm** to the watchdog —
  restart only if there are queued runs AND 1-min load > `4×ncpu` for two consecutive checks (a
  thrash-wedge the silence timer misses because the box is *busy*, not silent), with a cooldown so
  the restart can't fire mid-spike repeatedly. Debouncing the push-churn itself (a `ci.yml`
  concurrency/scheduling tweak) is a smaller follow-up — noted, not solved here.

### 5. A public, reproducible smoke signal for adopters

Add a small **GitHub-hosted** workflow (`ubuntu-latest`) that runs the subset needing no
self-hosted state — `go vet`, `nogo`/lint, `bazelisk build //...`, and the **non-Docker** unit
tests (pure wire/types/tuple, the planner unit tests). This gives a fork a green check it can
reproduce on infrastructure it controls, without moving the heavy 1M-stress / full-race / fuzz /
differential load off the Hetzner box. It is a **reproducibility floor**, explicitly not a
replacement for the self-hosted gates. A labelled GitHub-hosted *FDB-testcontainer* smoke (Docker
is available on hosted Linux, but slow) is **future work** — the no-Docker build+vet+unit floor is
the value; the Docker smoke is gold-plating and would rot, so it is out of this RFC's scope.

## What this does NOT do

- It does **not** eliminate the bus-factor-of-one (one human, one Hetzner account) — that is the
  acknowledged won't-fix in TODO-production. It makes the *environment* reproducible by anyone
  with the pinned config; it does not add a second maintainer.
- It does **not** move the heavy gates (1M stress, full `-race`, the 23 fuzz targets, the
  differential `libfdb_c` suite) to hosted runners — those need the self-hosted box's resources.
- It does **not** introduce a snapshot apt mirror (full apt hermeticity) — disproportionate;
  we pin the packages that affect the tests and document the residue.
- It does **not** pin the `just generate` codegen toolchain (`buf`, `protoc-gen-go`, the C++
  schema extractor, the ANTLR parser gen). Those run in the **pre-commit / dev path**, not the CI
  test gate (the runner consumes already-generated, committed code), so they are out of the
  CI-reproducibility scope — a separate dev-env-pinning concern if it ever matters.

## Test plan (validate the pins + the stand-up, since CI itself can't be unit-tested)

- **Checksum gate is load-bearing:** corrupt one byte of a pinned artifact locally and confirm
  `fetch_verified` aborts the provision (red), then restore. Every `{version, sha256}` pair is
  verified by actually downloading and `sha256sum -c`-ing it once before committing.
- **Clean-room provision:** `tofu apply` a throwaway runner from the pinned config on a second
  Hetzner project/region; confirm it registers, a real CI run goes green, and the installed
  versions (`gh-actions runner --version`, `bazelisk version`, `just --version`, `mc --version`,
  `fdbcli --version`) match the lock file exactly. Tear it down.
- **Ephemeral kill-test:** with `runner_ephemeral=true`, cancel an in-flight run and confirm the
  next job lands on a fresh runner process with no orphaned bazel server (the exact state that
  wedged the box this shift).
- **Watchdog latency:** confirm the lowered threshold restarts a deliberately-paused listener
  within one job length while still not restarting a legitimately-idle runner (no queued runs).
- **Hosted smoke:** the new `ubuntu-latest` workflow is green on a PR and runs real work
  (build + vet + the non-Docker unit tests resolve > 0 targets — the RFC-107 no-op guard).
- **Bazel anchors asserted:** a CI check fails if `.bazelversion` or `MODULE.bazel.lock` is
  missing, or if the runner's bazelisk resolves a Bazel version other than `.bazelversion`
  (`9.0.1`) — so a drift between the in-repo pin and the installed launcher is caught, not silent.
- **actionlint** on every new/changed workflow; `tofu validate` + `tofu fmt -check` on `infra/`.

## Rollout

One PR, stacked on the items-1–4 PR (#291) per the launch-readiness plan. Pins land first
(safe — same versions, now verified), then the skew fix, then the ephemeral variable (default
off) + watchdog tightening, then the hosted smoke workflow. Each is independently revertable.
