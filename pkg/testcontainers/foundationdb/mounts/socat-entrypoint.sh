#!/bin/sh
echo "[SOCAT-ENTRYPOINT] Waiting for socat configuration to be injected..."

# Wait for socat config file with timeout (30 seconds)
TIMEOUT=300
COUNTER=0
until [ -f /tmp/socat.conf ] && grep -q "# Injected by testcontainers" /tmp/socat.conf 2>/dev/null; do
    sleep 0.1
    COUNTER=$((COUNTER + 1))
    if [ $COUNTER -ge $TIMEOUT ]; then
        echo "[SOCAT-ENTRYPOINT] ERROR: Timeout waiting for configuration"
        exit 1
    fi
done

echo "[SOCAT-ENTRYPOINT] ✓ Configuration injected! Starting socat..."

# Extract target port from config (safer than exec sh -c)
TARGET_PORT=$(grep "TARGET_PORT=" /tmp/socat.conf | cut -d'=' -f2)

if [ -z "$TARGET_PORT" ]; then
    echo "[SOCAT-ENTRYPOINT] ERROR: TARGET_PORT not found in config"
    exit 1
fi

# Validate port is numeric
if ! echo "$TARGET_PORT" | grep -q '^[0-9]\+$'; then
    echo "[SOCAT-ENTRYPOINT] ERROR: Invalid TARGET_PORT: $TARGET_PORT"
    exit 1
fi

echo "[SOCAT-ENTRYPOINT] Forwarding 4500 -> foundationdb:$TARGET_PORT"

# Execute socat directly (no shell expansion)
exec socat TCP-LISTEN:4500,fork,reuseaddr TCP:foundationdb:$TARGET_PORT