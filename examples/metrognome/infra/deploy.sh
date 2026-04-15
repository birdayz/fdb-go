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

echo "Deploying metrognome to nodes..."
for ip in $(echo "$NODE_IPS" | jq -r '.[]'); do
  echo "  → $ip"

  # Stop services
  ssh -o StrictHostKeyChecking=no "root@${ip}" "systemctl stop metrognome envoy metrognome-static 2>/dev/null; killall metrognome envoy 2>/dev/null; true"

  # Deploy backend binary
  scp -o StrictHostKeyChecking=no "$BINARY" "root@${ip}:/usr/local/bin/metrognome"

  # Deploy frontend dist
  ssh "root@${ip}" "rm -rf /var/www/metrognome && mkdir -p /var/www/metrognome"
  scp -r "$DIST/"* "root@${ip}:/var/www/metrognome/"

  # Deploy envoy config
  ssh "root@${ip}" "mkdir -p /etc/envoy"
  scp "$ENVOY_CFG" "root@${ip}:/etc/envoy/envoy.yaml"

  # Install envoy if not present
  ssh "root@${ip}" 'which envoy >/dev/null 2>&1 || (apt-get update -qq && apt-get install -y -qq envoy 2>/dev/null || (curl -fsSL https://func-e.io/install.sh | bash -s -- -b /usr/local/bin && ln -sf /usr/local/bin/func-e /usr/local/bin/envoy))'

  # Create systemd units
  ssh "root@${ip}" 'cat > /etc/systemd/system/metrognome.service << EOF
[Unit]
Description=Metrognome Backend
After=network.target

[Service]
Type=simple
ExecStart=/usr/local/bin/metrognome
Environment=FDB_CLUSTER_FILE=/etc/foundationdb/fdb.cluster
Environment=LISTEN_ADDR=0.0.0.0:9090
Restart=always
RestartSec=3

[Install]
WantedBy=multi-user.target
EOF

cat > /etc/systemd/system/metrognome-static.service << EOF
[Unit]
Description=Metrognome Static File Server
After=network.target

[Service]
Type=simple
WorkingDirectory=/var/www/metrognome
ExecStart=/usr/bin/python3 -m http.server 9091 --bind 127.0.0.1
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
ExecStart=/usr/bin/envoy -c /etc/envoy/envoy.yaml
Restart=always
RestartSec=3

[Install]
WantedBy=multi-user.target
EOF

systemctl daemon-reload
systemctl enable metrognome metrognome-static envoy
systemctl start metrognome metrognome-static envoy'

done

sleep 3

# Show LB IP
LB_IP=$(tofu output -raw lb_ip 2>/dev/null || terraform output -raw lb_ip)
echo ""
echo "Deployed. UI: http://${LB_IP}:8080"
echo "API: http://${LB_IP}:8080/health"
