#!/usr/bin/env bash
set -euo pipefail

# Self-hosted GitHub Actions runner on Hetzner CX33
# CX33: 4 vCPU (shared), 8GB RAM, 80GB disk — €7.49/mo
# Runs Ubuntu 24.04, installs Docker + Bazelisk + FDB client + GH runner

SERVER_NAME="gh-runner-fdb"
SERVER_TYPE="cx33"
LOCATION="fsn1"
IMAGE="ubuntu-24.04"
SSH_KEY="birdy"
REPO="birdayz/fdb-record-layer-go"
RUNNER_LABELS="self-hosted,linux,x64,hetzner"
FDB_VERSION="7.3.46"

# --- Preflight ---
command -v hcloud >/dev/null 2>&1 || { echo "hcloud not found"; exit 1; }
[[ -n "${HCLOUD_TOKEN:-}" ]] || { echo "HCLOUD_TOKEN not set"; exit 1; }
command -v gh >/dev/null 2>&1 || { echo "gh not found"; exit 1; }

# Check if server already exists
if hcloud server describe "$SERVER_NAME" &>/dev/null; then
    echo "Server '$SERVER_NAME' already exists:"
    hcloud server describe "$SERVER_NAME"
    echo ""
    read -p "Delete and recreate? [y/N] " -n 1 -r
    echo
    [[ $REPLY =~ ^[Yy]$ ]] || exit 0
    hcloud server delete "$SERVER_NAME"
    echo "Deleted. Waiting 5s..."
    sleep 5
fi

# --- Get runner registration token ---
echo "==> Getting GitHub runner registration token..."
REG_TOKEN=$(gh api "repos/$REPO/actions/runners/registration-token" -X POST --jq '.token')
echo "    Token: ${REG_TOKEN:0:8}..."

# --- Create server ---
echo "==> Creating $SERVER_TYPE in $LOCATION..."
hcloud server create \
    --name "$SERVER_NAME" \
    --type "$SERVER_TYPE" \
    --location "$LOCATION" \
    --image "$IMAGE" \
    --ssh-key "$SSH_KEY"

IP=$(hcloud server ip "$SERVER_NAME")
echo "    IP: $IP"

# --- Wait for SSH ---
echo "==> Waiting for SSH..."
for i in $(seq 1 30); do
    if ssh -o StrictHostKeyChecking=no -o ConnectTimeout=3 "root@$IP" true 2>/dev/null; then
        break
    fi
    sleep 2
done

# --- Provision via SSH ---
echo "==> Provisioning server..."
ssh -o StrictHostKeyChecking=no "root@$IP" bash -s -- "$REG_TOKEN" "$REPO" "$RUNNER_LABELS" "$FDB_VERSION" <<'PROVISION'
set -euo pipefail
REG_TOKEN="$1"
REPO="$2"
RUNNER_LABELS="$3"
FDB_VERSION="$4"

export DEBIAN_FRONTEND=noninteractive

echo "--- Installing Docker ---"
apt-get update -qq
apt-get install -y -qq docker.io curl git jq build-essential
systemctl enable --now docker

# Expand Docker network pool (same as CI)
cat > /etc/docker/daemon.json <<EOF
{"default-address-pools": [{"base": "172.17.0.0/12", "size": 24}, {"base": "192.168.0.0/16", "size": 24}]}
EOF
systemctl restart docker

echo "--- Installing FDB client ---"
curl -fsSLO "https://github.com/apple/foundationdb/releases/download/${FDB_VERSION}/foundationdb-clients_${FDB_VERSION}-1_amd64.deb"
dpkg -i "foundationdb-clients_${FDB_VERSION}-1_amd64.deb"
rm -f "foundationdb-clients_${FDB_VERSION}-1_amd64.deb"

echo "--- Installing Bazelisk ---"
curl -fsSL -o /usr/local/bin/bazelisk \
    https://github.com/bazelbuild/bazelisk/releases/latest/download/bazelisk-linux-amd64
chmod +x /usr/local/bin/bazelisk
ln -sf /usr/local/bin/bazelisk /usr/local/bin/bazel

echo "--- Creating runner user ---"
useradd -m -s /bin/bash runner
usermod -aG docker runner

echo "--- Installing GitHub Actions runner ---"
RUNNER_VERSION=$(curl -fsSL https://api.github.com/repos/actions/runner/releases/latest | jq -r '.tag_name' | tr -d v)
RUNNER_DIR="/home/runner/actions-runner"
mkdir -p "$RUNNER_DIR"
cd "$RUNNER_DIR"
curl -fsSL -o runner.tar.gz \
    "https://github.com/actions/runner/releases/download/v${RUNNER_VERSION}/actions-runner-linux-x64-${RUNNER_VERSION}.tar.gz"
tar xzf runner.tar.gz
rm runner.tar.gz
chown -R runner:runner "$RUNNER_DIR"

echo "--- Configuring runner ---"
su - runner -c "cd $RUNNER_DIR && ./config.sh --unattended \
    --url https://github.com/$REPO \
    --token $REG_TOKEN \
    --name gh-runner-fdb \
    --labels $RUNNER_LABELS \
    --replace"

echo "--- Installing runner as systemd service ---"
cd "$RUNNER_DIR"
./svc.sh install runner
./svc.sh start

echo "--- Done! Runner status: ---"
./svc.sh status
PROVISION

echo ""
echo "==> Runner provisioned!"
echo "    Server: $SERVER_NAME ($SERVER_TYPE)"
echo "    IP:     $IP"
echo "    SSH:    ssh root@$IP"
echo ""
echo "Now update .github/workflows/ci.yml:"
echo "  runs-on: [self-hosted, linux, x64, hetzner]"
echo ""
echo "To tear down:"
echo "  hcloud server delete $SERVER_NAME"
