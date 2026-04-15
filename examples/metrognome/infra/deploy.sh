#!/bin/bash
# Deploy metrognome binary to all nodes.
# Usage: ./deploy.sh [binary_path]
#   binary_path defaults to bazel-built binary.
set -euo pipefail

BINARY="${1:-../../bazel-bin/examples/metrognome/cmd/metrognome/metrognome_/metrognome}"
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"

if [ ! -f "$BINARY" ]; then
  echo "Binary not found: $BINARY"
  echo "Build first: bazelisk build //examples/metrognome/cmd/metrognome"
  exit 1
fi

# Get node IPs from terraform
cd "$SCRIPT_DIR"
NODE_IPS=$(tofu output -json node_ips 2>/dev/null || terraform output -json node_ips)
if [ -z "$NODE_IPS" ] || [ "$NODE_IPS" = "null" ]; then
  echo "No nodes found. Run 'tofu apply' first."
  exit 1
fi

echo "Deploying metrognome to nodes..."
for ip in $(echo "$NODE_IPS" | jq -r '.[]'); do
  echo "  → $ip"
  scp -o StrictHostKeyChecking=no "$BINARY" "root@${ip}:/usr/local/bin/metrognome"
  ssh -o StrictHostKeyChecking=no "root@${ip}" "systemctl restart metrognome"
done

# Show LB IP
LB_IP=$(tofu output -raw lb_ip 2>/dev/null || terraform output -raw lb_ip)
echo ""
echo "Deployed. API endpoint: http://${LB_IP}:8080"
echo "Health check: curl http://${LB_IP}:8080/ready"
