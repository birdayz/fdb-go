#!/bin/sh
echo "[FDB-ENTRYPOINT] Waiting for FoundationDB configuration to be injected..."

# Wait for FDB config file with timeout (30 seconds)
TIMEOUT=300
COUNTER=0
until [ -f /tmp/fdb.conf ] && grep -q "# Injected by testcontainers" /tmp/fdb.conf 2>/dev/null; do
    sleep 0.1
    COUNTER=$((COUNTER + 1))
    if [ $COUNTER -ge $TIMEOUT ]; then
        echo "[FDB-ENTRYPOINT] ERROR: Timeout waiting for configuration"
        exit 1
    fi
done

echo "[FDB-ENTRYPOINT] ✓ Configuration injected! Reading port..."

# Extract the port from config
FDB_PORT=$(grep "FDB_PORT=" /tmp/fdb.conf | cut -d'=' -f2)
export FDB_PORT
export FDB_COORDINATOR_PORT=$FDB_PORT

echo "[FDB-ENTRYPOINT] Starting FoundationDB on port $FDB_PORT..."

# Start FoundationDB with the original entrypoint
exec /usr/bin/tini -g -- /var/fdb/scripts/fdb.bash