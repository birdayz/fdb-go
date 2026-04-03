terraform {
  required_version = ">= 1.7"

  required_providers {
    hcloud = {
      source  = "hetznercloud/hcloud"
      version = "~> 1.60"
    }
  }
}

provider "hcloud" {
  # Set HCLOUD_TOKEN env var
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

# --- Resources ---

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

# --- Outputs ---

output "server_ip" {
  value = hcloud_server.runner.ipv4_address
}

output "ssh_command" {
  value = "ssh root@${hcloud_server.runner.ipv4_address}"
}
