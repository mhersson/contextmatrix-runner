#!/bin/bash
set -euo pipefail

# ----- Create user with host-matching UID/GID -----
TARGET_UID="${HOST_UID:-1000}"
TARGET_GID="${HOST_GID:-1000}"

addgroup -g "$TARGET_GID" user 2>/dev/null || true
adduser -D -u "$TARGET_UID" -G user -s /bin/bash -h /home/user user 2>/dev/null || true

# Drop to unprivileged user for all remaining work.
exec su-exec user /setup-and-run.sh
