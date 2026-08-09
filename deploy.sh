#!/usr/bin/env bash
# deploy.sh - build music-utils, install the new binary, and restart the service.
set -euo pipefail

BIN="bin/music-utils"
DEST="/opt/music-utils/bin/music-utils"
DEST_NEW="${DEST}.new"
SERVICE="music-utils"
VERSION_URL="http://localhost:2956/version"

# Build the binary from source (see ./build.sh for the build flags).
./build.sh

# Copy to a fresh file, then atomically rename over the running binary.
# mv (rename) works while the old binary is still executing; a plain cp onto
# the running file would fail with "Text file busy".
sudo cp "$BIN" "$DEST_NEW"
sudo mv "$DEST_NEW" "$DEST"

# Restart so the running process picks up the new binary.
sudo systemctl restart "$SERVICE"

# Wait for the API to come back up and report the deployed version.
for _ in $(seq 1 20); do
    if version=$(curl -fsS --max-time 2 "$VERSION_URL" 2>/dev/null); then
        echo "deployed: $version"
        exit 0
    fi
    sleep 0.5
done

echo "service restarted but $VERSION_URL did not respond yet" >&2
echo "check it with: journalctl -u $SERVICE -n 50" >&2
exit 1
