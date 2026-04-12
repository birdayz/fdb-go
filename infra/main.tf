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
  description = "FoundationDB client version"
  type        = string
  default     = "7.3.46"
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
  image       = "ubuntu-24.04"
  ssh_keys    = [hcloud_ssh_key.runner.id]

  user_data = templatefile("${path.module}/cloud-init.yaml", {
    fdb_version          = var.fdb_version
    github_repo          = var.github_repo
    github_runner_token  = var.github_runner_token
    runner_labels        = var.runner_labels
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
