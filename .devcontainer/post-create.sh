#!/usr/bin/env bash
set -euo pipefail

# Named volumes and their parent /go/pkg (go writes the sumdb cache there) are created as root;
# the mount on .cache/go-build also resets its parent to root.
sudo chown vscode:vscode /go/pkg 2>/dev/null || true
sudo chown -R vscode:vscode /go/pkg/mod 2>/dev/null || true
sudo chown vscode:vscode /home/vscode/.cache 2>/dev/null || true
sudo chown -R vscode:vscode /home/vscode/.cache/go-build 2>/dev/null || true

echo "[post-create] $(go version)"
echo "[post-create] $(vips --version)"
echo "[post-create] $(ffmpeg -hide_banner -version | head -1)"
