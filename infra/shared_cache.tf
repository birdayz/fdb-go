# --- Fleet-shared Bazel cache (RFC-257) ---
#
# Every runner keeps its local --disk_cache and also reads/writes one shared
# bazel-remote cache on gh-runner-fdb, reachable only over this private network
# (bazel-remote publishes on the private IP alone). So a job is cache-warm on
# whichever box runs it. eu-central spans nbg1 (gh-runner-fdb) and fsn1 (pool).
#
# Live boxes ignore user_data changes; after `tofu apply` attaches the network,
# enable the cache on them with infra/enable-shared-cache.sh (see infra/README.md).

locals {
  # The cache server's private address; every box's /etc/bazel.bazelrc points here.
  shared_cache_ip = "10.77.0.2"
  # bazel-remote v2.4.4, pinned by digest (RFC-108 §1).
  shared_cache_image = "buchgr/bazel-remote-cache@sha256:8e17332e2ceb8b69f67bb08d6a1a51b3e11aaa5a25644c8a34f09d1ff1811a53"
}

resource "hcloud_network" "ci" {
  name     = "ci-fleet"
  ip_range = "10.77.0.0/16"
}

resource "hcloud_network_subnet" "ci" {
  network_id   = hcloud_network.ci.id
  type         = "cloud"
  network_zone = "eu-central"
  ip_range     = "10.77.0.0/24"
}

# Attaching a server to a network does not replace it.
resource "hcloud_server_network" "runner" {
  server_id  = hcloud_server.runner.id
  network_id = hcloud_network.ci.id
  ip         = local.shared_cache_ip
  depends_on = [hcloud_network_subnet.ci]
}

resource "hcloud_server_network" "runner_pool" {
  count      = var.runner_count
  server_id  = hcloud_server.runner_pool[count.index].id
  network_id = hcloud_network.ci.id
  ip         = "10.77.0.${10 + count.index}"
  depends_on = [hcloud_network_subnet.ci]
}

output "shared_cache_endpoint" {
  value = "grpc://${local.shared_cache_ip}:9092"
}
