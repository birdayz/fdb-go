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
  description = "GitHub Actions runner registration token (gh api repos/OWNER/REPO/actions/runners/registration-token -X POST --jq .token)"
  type        = string
  sensitive   = true
}

variable "ssh_public_key_file" {
  description = "Path to SSH public key"
  type        = string
  default     = "~/.ssh/id_rsa.pub"
}

variable "server_type" {
  description = "Hetzner server type"
  type        = string
  default     = "cx33"
}

variable "location" {
  description = "Hetzner datacenter location"
  type        = string
  default     = "fsn1"
}

variable "fdb_version" {
  description = "FoundationDB client version — MUST match MODULE.bazel + the testcontainers default so the host libfdb_c the differential harness links is the same FDB the tests run against (RFC-108 §2)."
  type        = string
  default     = "7.3.75"
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
    # bazelisk launcher (reads .bazelversion → Bazel 9.0.1; this is just the launcher).
    bazelisk_version = "1.28.1"
    bazelisk_sha256  = "22e7d3a188699982f661cf4687137ee52d1f24fec1ec893d91a6c4d791a75de8"
    # just task runner (was curl|bash — now a pinned, verified release tarball).
    just_version = "1.48.1"
    just_sha256  = "9293e553ce401d1b524bf4e104918f72f268e3f9c6827e0055fe98d84a1b2522"
    # MinIO client (was the rolling :latest object — now a dated, verified release).
    mc_release = "mc.RELEASE.2025-05-21T01-59-54Z"
    mc_sha256  = "fb11c542a9d781fb228de1126c267a7933e98bee831654462fb352d5c9e94d24"
    # FoundationDB clients .deb (host libfdb_c for the cgo differential harness).
    fdb_clients_sha256 = "642841a90acd7f2cc0ae08297245f4f9df76fe250b7b1331f2f99702fec3bee8"
  }
}

variable "github_repo" {
  description = "GitHub repository (owner/repo)"
  type        = string
  default     = "birdayz/fdb-record-layer-go"
}

variable "runner_labels" {
  description = "Comma-separated runner labels"
  type        = string
  default     = "self-hosted,linux,x64,hetzner"
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
  location    = var.location
  # Rolling Hetzner system-image label (RFC-108 §3): Hetzner refreshes the 24.04 image
  # over time, so the base OS point-release can drift between provisions. Every TOOL the
  # tests use (runner, bazelisk, just, mc, FDB client) is pinned + checksummed on top of
  # this, so the drift is bounded to the apt baseline; pinning to a captured user-snapshot
  # is a follow-up (Hetzner does not expose a stable id for the maintained system images).
  image    = "ubuntu-24.04"
  ssh_keys = [hcloud_ssh_key.runner.id]

  user_data = templatefile("${path.module}/cloud-init.yaml", {
    fdb_version         = var.fdb_version
    github_repo         = var.github_repo
    github_runner_token = var.github_runner_token
    runner_labels       = var.runner_labels
    runner_ephemeral    = var.runner_ephemeral
    runner_version      = local.versions.runner_version
    runner_sha256       = local.versions.runner_sha256
    bazelisk_version    = local.versions.bazelisk_version
    bazelisk_sha256     = local.versions.bazelisk_sha256
    just_version        = local.versions.just_version
    just_sha256         = local.versions.just_sha256
    mc_release          = local.versions.mc_release
    mc_sha256           = local.versions.mc_sha256
    fdb_clients_sha256  = local.versions.fdb_clients_sha256
  })
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
