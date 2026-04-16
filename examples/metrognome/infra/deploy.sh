#!/bin/bash
# Deploy metrognome (backend + frontend + envoy) to all nodes.
# Usage: ./deploy.sh
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
BINARY="$ROOT/bazel-bin/examples/metrognome/cmd/metrognome/metrognome_/metrognome"
DIST="$ROOT/bazel-bin/examples/metrognome/app/dist"
ENVOY_CFG="$SCRIPT_DIR/envoy.yaml"

# Build if needed
if [ ! -f "$BINARY" ] || [ ! -d "$DIST" ]; then
  echo "Building backend + frontend..."
  cd "$ROOT"
  bazelisk build //examples/metrognome/cmd/metrognome //examples/metrognome/app:bundle
fi

# Get node IPs from terraform
cd "$SCRIPT_DIR"
NODE_IPS=$(tofu output -json node_ips 2>/dev/null || terraform output -json node_ips)
if [ -z "$NODE_IPS" ] || [ "$NODE_IPS" = "null" ]; then
  echo "No nodes found. Run 'tofu apply' first."
  exit 1
fi

LB_IP=$(tofu output -raw lb_ip 2>/dev/null || terraform output -raw lb_ip)

echo "Deploying metrognome to nodes..."
for ip in $(echo "$NODE_IPS" | jq -r '.[]'); do
  echo "  → $ip"

  # Stop services
  ssh -o StrictHostKeyChecking=no "root@${ip}" "systemctl stop metrognome envoy metrognome-static 2>/dev/null; systemctl disable metrognome-static 2>/dev/null; killall metrognome envoy 2>/dev/null; true"

  # Deploy backend binary
  scp -o StrictHostKeyChecking=no "$BINARY" "root@${ip}:/usr/local/bin/metrognome"

  # Deploy frontend dist
  ssh "root@${ip}" "rm -rf /var/www/metrognome && mkdir -p /var/www/metrognome"
  scp -r "$DIST/"* "root@${ip}:/var/www/metrognome/"

  # Deploy envoy config + SPA server script
  ssh "root@${ip}" "mkdir -p /etc/envoy"
  scp "$ENVOY_CFG" "root@${ip}:/etc/envoy/envoy.yaml"
  scp "$SCRIPT_DIR/spa-server.py" "root@${ip}:/usr/local/bin/spa-server.py"

  # Install func-e (envoy wrapper) if not present
  ssh "root@${ip}" 'which func-e >/dev/null 2>&1 || (curl -fsSL https://func-e.io/install.sh | bash -s -- -b /usr/local/bin)'

  # Create env file ONLY if it doesn't exist (preserves credentials across deploys)
  ssh "root@${ip}" "test -f /etc/metrognome.env || cat > /etc/metrognome.env << ENVEOF
LISTEN_ADDR=0.0.0.0:9090
FRONTEND_URL=http://${LB_IP}:8080
FDB_CLUSTER_FILE=/etc/foundationdb/fdb.cluster
STATIC_DIR=/var/www/metrognome
GITHUB_CLIENT_ID=FILL_ME
GITHUB_CLIENT_SECRET=FILL_ME
GITHUB_REDIRECT_URL=http://${LB_IP}:8080/auth/callback
ENVEOF"

  # Ensure STATIC_DIR is set in existing env files
  ssh "root@${ip}" "grep -q STATIC_DIR /etc/metrognome.env || echo 'STATIC_DIR=/var/www/metrognome' >> /etc/metrognome.env"

  # Write systemd units (idempotent — always overwrite with correct config)
  ssh "root@${ip}" 'cat > /etc/systemd/system/metrognome.service << EOF
[Unit]
Description=Metrognome Backend
After=network.target

[Service]
Type=simple
ExecStart=/usr/local/bin/metrognome
EnvironmentFile=/etc/metrognome.env
Restart=always
RestartSec=3

[Install]
WantedBy=multi-user.target
EOF

cat > /etc/systemd/system/envoy.service << EOF
[Unit]
Description=Envoy Proxy
After=network.target metrognome.service

[Service]
Type=simple
ExecStart=/usr/local/bin/func-e run -c /etc/envoy/envoy.yaml
Restart=always
RestartSec=3

[Install]
WantedBy=multi-user.target
EOF

systemctl daemon-reload
systemctl enable metrognome envoy
systemctl start metrognome envoy'

done

sleep 3

echo ""
echo "Deployed. UI: http://${LB_IP}:8080"
echo "API: http://${LB_IP}:8080/health"
