#!/bin/bash
set -e

WG_CONF="/config/wg0.conf"

if [ ! -f "$WG_CONF" ]; then
    echo "ERROR: No WireGuard config found at $WG_CONF"
    exit 1
fi

# Bring up WireGuard interface
echo "Starting WireGuard..."
wg-quick up "$WG_CONF" 2>&1

# Verify WireGuard is running
if ! wg show wg0 > /dev/null 2>&1; then
    echo "ERROR: WireGuard failed to start"
    exit 1
fi

echo "WireGuard is up"
wg show wg0

# Start aria2c with RPC enabled
echo "Starting aria2c RPC server on port 6800..."
exec aria2c \
    --enable-rpc=true \
    --rpc-listen-all=true \
    --rpc-listen-port=6800 \
    --rpc-allow-origin-all=true \
    --dir=/downloads \
    --file-allocation=falloc \
    --continue=true \
    --max-connection-per-server=4 \
    --min-split-size=1M \
    --split=4 \
    --max-tries=0 \
    --retry-wait=5 \
    --timeout=60 \
    --connect-timeout=30 \
    --max-concurrent-downloads=1 \
    --auto-file-renaming=false \
    --allow-overwrite=true \
    --summary-interval=1 \
    --console-log-level=notice \
    --download-result=full
