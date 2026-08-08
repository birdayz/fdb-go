terraform {
  required_version = ">= 1.7"

  required_providers {
    hcloud = {
      source  = "hetznercloud/hcloud"
      version = "~> 1.60"
    }
    minio = {
      source  = "aminueza/minio"
      version = "~> 3.3"
    }
  }
}

provider "hcloud" {
  # Set HCLOUD_TOKEN env var
}

# Hetzner Object Storage (S3-compatible).
# Set MINIO_USER and MINIO_PASSWORD env vars (from Hetzner Console → S3 Credentials).
provider "minio" {
  minio_server = var.s3_endpoint
  minio_region = var.location # fsn1
  minio_ssl    = true
}

# --- Variables ---

variable "github_runner_token" {
  description = "GitHub Actions runner registration token (gh api repos/birdayz/fdb-go/actions/runners/registration-token -X POST --jq .token). The one token infra/README.md documents: it registers the grandfathered box AND, via local.pool_registration_token, every pool box. Only used when runner_mode=classic."
  type        = string
  sensitive   = true
  default     = ""
}

# --- Runner mode ---
#
# The fleet runs 'classic': every box registers itself once and listens. The scale-set
# supervisor ('scaleset') is retired but kept buildable because its variables and
# cloud-init branch are still referenced; do not point the fleet back at it without
# fixing the two defects that retired it — JIT runners were never unregistered (1907
# offline registrations against 5 live slots) and one supervisor process gated every
# box in the fleet.
variable "runner_mode" {
  description = "Which runner the box provisions: 'classic' (register-and-listen, one durable registration per box — the fleet default) or 'scaleset' (bazelscaleset supervisor + JIT runners)."
  type        = string
  default     = "classic"
}

variable "bazelscaleset_app_client_id" {
  description = "GitHub App client id for bazelscaleset (not secret)."
  type        = string
  default     = "Iv23litTscHcbrnmhtKa"
}

variable "bazelscaleset_app_installation_id" {
  description = "GitHub App installation id for bazelscaleset (not secret)."
  type        = string
  default     = "143183909"
}

variable "bazelscaleset_app_private_key" {
  description = "GitHub App private key (PEM) for bazelscaleset. Supplied via TF_VAR_bazelscaleset_app_private_key from the BAZELSCALESET_APP_PRIVATE_KEY repo secret in a deploy workflow; never committed."
  type        = string
  sensitive   = true
  default     = ""
}

variable "ssh_public_key_file" {
  description = "Path to SSH public key"
  type        = string
  default     = "~/.ssh/id_rsa.pub"
}

variable "server_type" {
  description = "Hetzner server type. cpx32 (4 shared vCPU, 8 GB), the same shape and roughly the same price as the cx33 this fleet used to run. cx33 is out of stock in every datacenter, and the dedicated-core alternative (ccx23) exceeds this account's dedicated-core quota, so cpx32 is what can actually be provisioned. A larger box would relieve the 4-way contention behind the test timeouts, but that is a cost decision, not a provisioning one — the timeouts are being fixed by sharding the oversized targets instead."
  type        = string
  default     = "cpx32"
}

variable "location" {
  description = "Hetzner datacenter location"
  type        = string
  default     = "fsn1"
}

variable "runner_ephemeral" {
  description = "Run the GitHub Actions runner in --ephemeral mode (one job per runner process, fresh state each time — removes the orphaned-bazel/wedged-listener failure mode at the cost of a cold cache per job). Default false keeps the persistent + watchdog model (RFC-108 §4)."
  type        = bool
  default     = false
}

# --- Pinned tool versions + SHA-256 checksums (RFC-108 §1) ---
# Single source of truth: every runner download is pinned to a version AND verified
# against the checksum here before use. A version bump is ONE reviewed diff that changes
# {version, sha256} together — never one without the other. Checksums fetched + verified
# 2026-06-14 (runner/just/mc from upstream-published sums; bazelisk/fdb-deb computed).
locals {
  versions = {
    # GitHub Actions runner (was releases/latest — now pinned). SHA from the release body.
    runner_version = "2.335.1"
    runner_sha256  = "4ef2f25285f0ae4477f1fe1e346db76d2f3ebf03824e2ddd1973a2819bf6c8cf"
    # Go toolchain. MUST match go.mod's `go` directive: the workflows that do not go
    # through Bazel run the system `go` directly (the libfdbc cgo build hardcodes
    # GO_BIN="go", and the Bazel-SDK resolver falls back to it), so a box without Go on
    # PATH fails those jobs with `go: command not found` — exit 127, not a test failure.
    # Nothing installed it before: the old boxes were provisioned by hand.
    go_version = "1.26.5"
    go_sha256  = "5c2c3b16caefa1d968a94c1daca04a7ca301a496d9b086e17ad77bb81393f053"
    # bazelisk launcher (reads .bazelversion → Bazel 9.0.1; this is just the launcher).
    bazelisk_version = "1.28.1"
    bazelisk_sha256  = "22e7d3a188699982f661cf4687137ee52d1f24fec1ec893d91a6c4d791a75de8"
    # just task runner (was curl|bash — now a pinned, verified release tarball).
    just_version = "1.48.1"
    just_sha256  = "9293e553ce401d1b524bf4e104918f72f268e3f9c6827e0055fe98d84a1b2522"
    # MinIO client (was the rolling :latest object — now a dated, verified release).
    mc_release = "mc.RELEASE.2025-05-21T01-59-54Z"
    mc_sha256  = "fb11c542a9d781fb228de1126c267a7933e98bee831654462fb352d5c9e94d24"
    # FoundationDB clients .deb — host libfdb_c for the cgo differential harness. The
    # version lives HERE (not a tofu variable) so it is LOCKED to its checksum: overriding
    # the version without updating the SHA would make fetch-verified.sh abort (codex). MUST
    # match MODULE.bazel + the testcontainers default (RFC-108 §2) — a bump is one reviewed
    # edit of both fields.
    fdb_version        = "7.3.77"
    fdb_clients_sha256 = "b6c263723009bddbc30b6ae8cf9854340bd35cef14152abb0da8fb5af2b91254"
  }
}

variable "github_repo" {
  description = "GitHub repository (owner/repo) the runners register against. This is the REMOTE name (birdayz/fdb-go), which differs from the local checkout directory (fdb-record-layer-go) — registration POSTs to https://github.com/<this>, and the old value 404'd because no such repo exists. Nothing caught it: the pool ran in worker mode (no registration at all) and the supervisor box was configured out-of-band, so this value was never once exercised."
  type        = string
  default     = "birdayz/fdb-go"
}

variable "runner_labels" {
  description = "Comma-separated runner labels. Every workflow's `runs-on` must name a label here. NOT 'hetzner-fdb': that name belongs to the retired runner scale set, and GitHub routes `runs-on: <scale-set-name>` to the scale set — which has no listener — so those jobs queue forever instead of reaching these runners."
  type        = string
  default     = "hetzner-fdb-vm"
}

variable "s3_endpoint" {
  description = "Hetzner Object Storage endpoint (without https://)"
  type        = string
  default     = "fsn1.your-objectstorage.com"
}

# --- CI Runner ---

resource "hcloud_ssh_key" "runner" {
  name       = "gh-runner"
  public_key = file(pathexpand(var.ssh_public_key_file))
}

resource "hcloud_server" "runner" {
  name        = "gh-runner-fdb"
  server_type = var.server_type
  # The live grandfathered box is in nbg1, NOT var.location (fsn1) — discovered
  # 2026-08-01 when a plan wanted to replace it over the drift. Hard-pinned to
  # reality; var.location remains the default for everything else.
  location = "nbg1"
  # Rolling Hetzner system-image label (RFC-108 §3): Hetzner refreshes the 24.04 image
  # over time, so the base OS point-release can drift between provisions. Every TOOL the
  # tests use (runner, bazelisk, just, mc, FDB client) is pinned + checksummed on top of
  # this, so the drift is bounded to the apt baseline; pinning to a captured user-snapshot
  # is a follow-up (Hetzner does not expose a stable id for the maintained system images).
  image    = "ubuntu-24.04"
  ssh_keys = [hcloud_ssh_key.runner.id]

  user_data = templatefile("${path.module}/cloud-init.yaml", {
    fdb_version         = local.versions.fdb_version
    github_repo         = var.github_repo
    github_runner_token = var.github_runner_token
    runner_name         = "gh-runner-fdb"
    runner_labels       = var.runner_labels
    runner_ephemeral    = var.runner_ephemeral
    runner_version      = local.versions.runner_version
    runner_sha256       = local.versions.runner_sha256
    go_version          = local.versions.go_version
    go_sha256           = local.versions.go_sha256
    bazelisk_version    = local.versions.bazelisk_version
    bazelisk_sha256     = local.versions.bazelisk_sha256
    just_version        = local.versions.just_version
    just_sha256         = local.versions.just_sha256
    mc_release          = local.versions.mc_release
    mc_sha256           = local.versions.mc_sha256
    fdb_clients_sha256  = local.versions.fdb_clients_sha256
    # bazelscaleset (RFC-155)
    runner_mode                       = var.runner_mode
    bazelscaleset_app_client_id       = var.bazelscaleset_app_client_id
    bazelscaleset_app_installation_id = var.bazelscaleset_app_installation_id
    bazelscaleset_app_private_key     = var.bazelscaleset_app_private_key
  })

  lifecycle {
    # This box carried prevent_destroy because it sat on Hetzner's old, cheaper price
    # tier — a replacement re-prices at current rates. That block was dropped on owner
    # authorisation to move the whole fleet off the scale set, which could not be done
    # in place: the box's user_data was also frozen by ignore_changes, so its mode only
    # ever changes by re-provisioning.
    #
    # Registration tokens expire and rotate on every apply, so without ignore_changes
    # each re-apply would ForceNew a live box. A template change therefore reaches this
    # box the same way it reaches the pool: an explicit `tofu apply -replace=...`, which
    # provisions from the current template.
    #
    # ssh_keys for the same reason and it is not hypothetical: it is ForceNew in the
    # provider, and hcloud_ssh_key.runner reads `~/.ssh/id_rsa.pub` off whatever machine
    # runs the apply — so an operator with a different default key silently plans a
    # destroy+create of EVERY box in the fleet. The key only matters at first boot
    # anyway; Hetzner cannot change it on a live server.
    ignore_changes = [user_data, ssh_keys]
  }
}

# --- Runner pool (count-parameterized) ---
#
# Every pool box registers itself and listens, same as the box above; the pool exists
# to add capacity, not a different kind of runner. Each serves one job at a time, which
# is what the box can actually do: .bazelrc caps --local_test_jobs=4 because each test
# job runs its own FoundationDB container, and a 4-core box is saturated by one such job.
#
# Scale up: raise runner_count and apply with a fresh registration token:
#   TOKEN=$(gh api repos/birdayz/fdb-go/actions/runners/registration-token -X POST --jq .token)
#   tofu apply -var "github_runner_token=$TOKEN" -var "runner_count=N"
# Scale down: lower runner_count (destroys highest indices first); deregister the
# removed runners in GitHub afterwards (gh api -X DELETE .../actions/runners/{id}).
# ignore_changes[user_data] keeps token rotation and template evolution from
# replacing live pool boxes; a template fix reaches a pool box via taint/replace.

variable "runner_count" {
  description = "Number of pool runners (in addition to the grandfathered scaleset box)"
  type        = number
  default     = 1
}

variable "runner_registration_token" {
  description = "Registration token for the POOL boxes only, when they must differ from github_runner_token. Leave unset and the pool uses github_runner_token — the one token infra/README.md documents. Both defaulting to \"\" independently is how a fresh apply rendered `config.sh --token ` on all four pool boxes and failed registration during cloud-init, on a path nothing exercised: the live pool is held by ignore_changes[user_data], so a routine apply never re-renders it."
  type        = string
  sensitive   = true
  default     = ""
}

# The pool's token, with the documented one as the fallback. A registration token is
# repo-scoped and single-purpose, so the same token registering every box is exactly what
# the documented one-token setup means; a separate value stays possible for the case it
# was added for (replacing pool boxes on a token the grandfathered box must not rotate to).
locals {
  pool_registration_token = var.runner_registration_token != "" ? var.runner_registration_token : var.github_runner_token
}

variable "runner_pool_types" {
  description = "Per-index server type for pool runners (falls back to the last entry when runner_count exceeds the list; mixed because Hetzner pools go out of stock — cx33 was unavailable at first provision)"
  type        = list(string)
  default     = ["cpx32", "cpx32", "cpx32", "cpx32"]
}

variable "runner_pool_locations" {
  description = "Per-index location for pool runners (same fallback rule as runner_pool_types)"
  type        = list(string)
  default     = ["fsn1", "fsn1", "fsn1", "nbg1"]
}

resource "hcloud_server" "runner_pool" {
  count       = var.runner_count
  name        = "gh-runner-drain-${count.index}"
  server_type = element(var.runner_pool_types, min(count.index, length(var.runner_pool_types) - 1))
  location    = element(var.runner_pool_locations, min(count.index, length(var.runner_pool_locations) - 1))
  image       = "ubuntu-24.04"
  ssh_keys    = [hcloud_ssh_key.runner.id]

  user_data = templatefile("${path.module}/cloud-init.yaml", {
    fdb_version                       = local.versions.fdb_version
    github_repo                       = var.github_repo
    github_runner_token               = local.pool_registration_token
    runner_name                       = "gh-runner-drain-${count.index}"
    runner_labels                     = var.runner_labels
    runner_ephemeral                  = false
    runner_version                    = local.versions.runner_version
    runner_sha256                     = local.versions.runner_sha256
    go_version                        = local.versions.go_version
    go_sha256                         = local.versions.go_sha256
    bazelisk_version                  = local.versions.bazelisk_version
    bazelisk_sha256                   = local.versions.bazelisk_sha256
    just_version                      = local.versions.just_version
    just_sha256                       = local.versions.just_sha256
    mc_release                        = local.versions.mc_release
    mc_sha256                         = local.versions.mc_sha256
    fdb_clients_sha256                = local.versions.fdb_clients_sha256
    runner_mode                       = var.runner_mode
    bazelscaleset_app_client_id       = var.bazelscaleset_app_client_id
    bazelscaleset_app_installation_id = var.bazelscaleset_app_installation_id
    bazelscaleset_app_private_key     = ""
  })

  lifecycle {
    # Registration tokens expire and rotate per apply; without this every re-apply
    # would ForceNew the whole live pool. ssh_keys is here for the same reason: it is
    # ForceNew, and it is derived from the applying operator's own default public key.
    ignore_changes = [user_data, ssh_keys]
  }
}

# Each pool runner needs its own ci-data volume: cloud-init fails loud (by design)
# when /mnt/HC_Volume_* never mounts, and Docker's data-root + the bazel caches
# live on it.
resource "hcloud_volume" "runner_pool_data" {
  count     = var.runner_count
  name      = "gh-runner-drain-data-${count.index}"
  size      = 100
  server_id = hcloud_server.runner_pool[count.index].id
  format    = "ext4"
  automount = true
}

output "runner_pool_ips" {
  value = hcloud_server.runner_pool[*].ipv4_address
}

# --- Persistent build/cache data volume (RFC-115 §7 / RFC-108 CI hardening) ---
#
# The wire-oracle CI job builds FoundationDB from source (the fdb_cmake_build genrule:
# ~14 min, multi-GB build tree + a 0.76 GB tar) inside Docker. On the 75 GB root disk —
# already ~64 GB used by the 21.5 GB FDB build image + the 13 GB Bazel cache — the cold
# build+tar runs the disk out of space, the genrule's action fails, and since Bazel never
# caches a FAILED action it cold-rebuilds and re-fails every run. This 100 GB ext4 volume
# holds Docker's data-root AND Bazel's output base so that build has real headroom and the
# action succeeds ONCE, then caches normally (no remote/disk_cache needed). cloud-init.yaml
# links it to a stable /mnt/ci-data and points Docker (daemon.json data-root) + the runner's
# Bazel cache (~/.cache/bazel symlink) at it — without that wiring the volume would just sit
# idle and the build would still fill the root disk.
#
# server_id references the managed hcloud_server.runner (NOT a data lookup) so a fresh
# `tofu apply` provisions server-then-volume in ONE graph. Nothing declares that reference
# safe any more: the prevent_destroy that used to guard it was removed to move the fleet
# off the scale set, so replacing the server destroys and re-creates this volume with it.
# What keeps a routine apply from doing that is the servers' ignore_changes on the two
# ForceNew inputs that drift on their own (user_data, ssh_keys) — an explicit
# `tofu apply -replace=...` is still, deliberately, able to replace the pair.
# ext4 (not xfs): the Bazel cache + Docker layers are millions of small files, ext4's
# conservative sweet spot; xfs's large-file/parallelism edge isn't the bottleneck here.
resource "hcloud_volume" "runner_data" {
  name      = "gh-runner-fdb-data"
  size      = 100
  server_id = hcloud_server.runner.id
  format    = "ext4"
  automount = true
}

# --- Object Storage (test reports) ---

resource "minio_s3_bucket" "reports" {
  bucket = "fdb-record-layer-go-reports"
  acl    = "public-read"
}

resource "minio_s3_bucket_policy" "reports_public_read" {
  bucket = minio_s3_bucket.reports.bucket
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect    = "Allow"
      Principal = "*"
      Action    = ["s3:GetObject"]
      Resource  = ["arn:aws:s3:::${minio_s3_bucket.reports.bucket}/*"]
    }]
  })
}

# --- Outputs ---

output "server_ip" {
  value = hcloud_server.runner.ipv4_address
}

output "ssh_command" {
  value = "ssh root@${hcloud_server.runner.ipv4_address}"
}

output "report_url" {
  value = "https://${minio_s3_bucket.reports.bucket}.${var.s3_endpoint}/reports/master/latest.html"
}
