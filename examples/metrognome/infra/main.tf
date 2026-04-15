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

variable "ssh_public_key_file" {
  description = "Path to SSH public key"
  type        = string
  default     = "~/.ssh/id_rsa.pub"
}

variable "server_type" {
  description = "Hetzner server type (cx23 = 2 vCPU, 4GB RAM, 4.75 EUR/mo)"
  type        = string
  default     = "cx23"
}

variable "location" {
  description = "Hetzner datacenter"
  type        = string
  default     = "nbg1"
}

variable "node_count" {
  description = "Number of metrognome+FDB nodes"
  type        = number
  default     = 3
}

variable "fdb_version" {
  description = "FoundationDB version"
  type        = string
  default     = "7.3.46"
}

# --- SSH Key ---

data "hcloud_ssh_key" "default" {
  name = "gh-runner"
}

# --- Private Network (VPC) ---
# All inter-node traffic (FDB cluster, internal APIs) stays on the private network.
# Public internet only reaches the load balancer.

resource "hcloud_network" "vpc" {
  name     = "metrognome-vpc"
  ip_range = "10.0.0.0/16"
}

resource "hcloud_network_subnet" "nodes" {
  network_id   = hcloud_network.vpc.id
  type         = "cloud"
  network_zone = "eu-central"
  ip_range     = "10.0.1.0/24"
}

# --- Firewall ---
# Only SSH and LB health checks from public internet.
# FDB (4500) and metrognome API (8080) are VPC-only.

resource "hcloud_firewall" "nodes" {
  name = "metrognome-nodes"

  # SSH from anywhere
  rule {
    direction  = "in"
    protocol   = "tcp"
    port       = "22"
    source_ips = ["0.0.0.0/0", "::/0"]
  }

  # ICMP (ping) for health checks
  rule {
    direction  = "in"
    protocol   = "icmp"
    source_ips = ["0.0.0.0/0", "::/0"]
  }

  # Allow all traffic from VPC (FDB cluster + API)
  rule {
    direction  = "in"
    protocol   = "tcp"
    port       = "any"
    source_ips = ["10.0.0.0/16"]
  }

  rule {
    direction  = "in"
    protocol   = "udp"
    port       = "any"
    source_ips = ["10.0.0.0/16"]
  }
}

# --- Servers ---

resource "hcloud_server" "node" {
  count       = var.node_count
  name        = "metrognome-${count.index}"
  server_type = var.server_type
  location    = var.location
  image       = "ubuntu-24.04"
  ssh_keys    = [data.hcloud_ssh_key.default.id]

  firewall_ids = [hcloud_firewall.nodes.id]

  network {
    network_id = hcloud_network.vpc.id
    ip         = "10.0.1.${count.index + 10}"
  }

  user_data = templatefile("${path.module}/cloud-init.yaml", {
    node_index     = count.index
    node_count     = var.node_count
    fdb_version    = var.fdb_version
    private_ip     = "10.0.1.${count.index + 10}"
    coordinator_ip = "10.0.1.10" # node 0 is always the coordinator
    all_node_ips   = join(",", [for i in range(var.node_count) : "10.0.1.${i + 10}"])
  })

  depends_on = [hcloud_network_subnet.nodes]
}

# --- Load Balancer ---
# Single static IP for all nodes. Routes :8080 (ConnectRPC) to the backend.

resource "hcloud_load_balancer" "lb" {
  name               = "metrognome-lb"
  load_balancer_type = "lb11" # cheapest
  location           = var.location
}

resource "hcloud_load_balancer_network" "lb" {
  load_balancer_id = hcloud_load_balancer.lb.id
  network_id       = hcloud_network.vpc.id
  ip               = "10.0.1.254"
}

resource "hcloud_load_balancer_service" "api" {
  load_balancer_id = hcloud_load_balancer.lb.id
  protocol         = "tcp"
  listen_port      = 8080
  destination_port = 8080

  health_check {
    protocol = "http"
    port     = 8080
    interval = 10
    timeout  = 5
    retries  = 3

    http {
      path         = "/health"
      status_codes = ["200"]
    }
  }
}

resource "hcloud_load_balancer_target" "nodes" {
  count            = var.node_count
  load_balancer_id = hcloud_load_balancer.lb.id
  type             = "server"
  server_id        = hcloud_server.node[count.index].id
  use_private_ip   = true

  depends_on = [hcloud_load_balancer_network.lb]
}

# --- Outputs ---

output "lb_ip" {
  description = "Load balancer public IP (ConnectRPC on :8080)"
  value       = hcloud_load_balancer.lb.ipv4
}

output "node_ips" {
  description = "Node public IPs (SSH only)"
  value       = [for s in hcloud_server.node : s.ipv4_address]
}

output "node_private_ips" {
  description = "Node private IPs (VPC)"
  value       = [for i in range(var.node_count) : "10.0.1.${i + 10}"]
}

output "ssh_commands" {
  description = "SSH commands for each node"
  value       = [for s in hcloud_server.node : "ssh root@${s.ipv4_address}"]
}

output "api_url" {
  description = "Metrognome API endpoint"
  value       = "http://${hcloud_load_balancer.lb.ipv4}:8080"
}
