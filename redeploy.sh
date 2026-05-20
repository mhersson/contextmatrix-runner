#!/usr/bin/env bash
# Rebuild the runner binary + worker image, pin the new digest in
# config.yaml, and restart the user service. Intended for a local
# bumblebee-style single-host deployment. Run from the runner repo root.
#
# Overridable env vars:
#   CMR_CONFIG       path to config.yaml (default ~/.config/contextmatrix-runner/config.yaml)
#   CMR_WORKER_IMAGE image ref used for `docker inspect` (default contextmatrix/worker:latest)
#   CMR_SERVICE      systemd user unit name (default contextmatrix-runner)
set -euo pipefail

CONFIG="${CMR_CONFIG:-$HOME/.config/contextmatrix-runner/config.yaml}"
WORKER_IMAGE="${CMR_WORKER_IMAGE:-contextmatrix/worker:latest}"
SERVICE="${CMR_SERVICE:-contextmatrix-runner}"

[ -f "$CONFIG" ] || {
  echo "ERROR: $CONFIG not found" >&2
  exit 1
}
[ -w "$CONFIG" ] || {
  echo "ERROR: $CONFIG not writable" >&2
  exit 1
}
command -v docker >/dev/null || {
  echo "ERROR: docker not in PATH" >&2
  exit 1
}
command -v systemctl >/dev/null || {
  echo "ERROR: systemctl not in PATH" >&2
  exit 1
}

echo "==> make build"
make build

echo "==> make docker-worker"
make docker-worker

echo "==> capturing RepoDigest for ${WORKER_IMAGE}"
digest=$(docker image inspect "$WORKER_IMAGE" \
  --format '{{range .RepoDigests}}{{println .}}{{end}}' \
  | grep '^contextmatrix/worker@sha256:' | head -n 1)
if [ -z "$digest" ]; then
  echo "ERROR: no contextmatrix/worker@sha256 RepoDigest on ${WORKER_IMAGE}" >&2
  echo "       rebuild produced an image without a digest — push to a registry or retag" >&2
  exit 1
fi
echo "    ${digest}"

echo "==> pinning base_image in ${CONFIG}"
# Match base_image:" ... " regardless of previous digest content. Uses | as the
# sed delimiter so the / in the image path does not need escaping.
sed -i -E "s|^(base_image:[[:space:]]*\")[^\"]*(\")|\\1${digest}\\2|" "$CONFIG"
grep -E '^base_image:' "$CONFIG"

echo "==> systemctl --user restart ${SERVICE}"
systemctl --user restart "$SERVICE"

# port=$(awk '/^port:/ {print $2; exit}' "$CONFIG")
# port="${port:-19090}"
# echo "==> waiting for /readyz on :${port}"
# for _ in $(seq 1 20); do
#     if curl -sf "http://localhost:${port}/readyz" >/dev/null; then
#         echo "OK"
#         exit 0
#     fi
#     sleep 1
# done
#
# echo "ERROR: runner not ready after 20s — check 'journalctl --user -u ${SERVICE}'" >&2
# exit 1
