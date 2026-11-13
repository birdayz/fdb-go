#!/usr/bin/env bash
# The Playwright suite against the compose stack (docker-compose.yaml), once per
# Grafana version. Needs dist/ (just grafana-build; npm run build), bin/seed
# (go build -o bin/seed ./cmd/seed), npm ci, and Docker with Compose v2.
#
#   tests/e2e.sh                          # Grafana 13.1.9, then 12.0.0
#   GRAFANA_VERSIONS=13.1.9 tests/e2e.sh
#
# Playwright runs in its pinned image (no browser or system packages on the
# host); PLAYWRIGHT=host runs the local `npx playwright` instead. The stack
# publishes Grafana on :3000 and is removed on exit.
set -euo pipefail
cd "$(dirname "$0")/.."

compose=(${COMPOSE:-docker compose} -p "${COMPOSE_PROJECT:-fdbgrafanae2e}")
versions=${GRAFANA_VERSIONS:-13.1.9 12.0.0}
pw_version=$(node -p 'require("./package-lock.json").packages["node_modules/@playwright/test"].version')

down() { "${compose[@]}" down --remove-orphans > /dev/null 2>&1 || true; }
trap down EXIT

for v in $versions; do
  echo "=== Grafana $v"
  down
  GRAFANA_VERSION=$v "${compose[@]}" up -d --build
  for _ in $(seq 100); do
    curl -fs localhost:3000/api/health > /dev/null && break
    sleep 3
  done
  curl -fs localhost:3000/api/health
  rm -rf playwright/.auth
  if [ "${PLAYWRIGHT:-docker}" = host ]; then
    npx playwright test --reporter=list
  else
    docker run --rm --network host --user "$(id -u):$(id -g)" -e HOME=/tmp -e CI="${CI:-}" \
      -v "$PWD:/plugin" -w /plugin "mcr.microsoft.com/playwright:v${pw_version}-noble" \
      npx playwright test --reporter=list
  fi
done
