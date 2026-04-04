#!/usr/bin/env bash
# svc.sh — manage the contextmatrix-runner systemd user service

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
SERVICE_NAME="contextmatrix-runner"
SERVICE_FILE="${HOME}/.config/systemd/user/${SERVICE_NAME}.service"
BINARY="${SCRIPT_DIR}/contextmatrix-runner"
CONFIG="${SCRIPT_DIR}/config.yaml"

usage() {
    cat <<EOF
Usage: $(basename "$0") <subcommand>

Subcommands:
  install    Generate the systemd user service file, reload daemon, and enable
  uninstall  Stop, disable, and remove the service file
  start      Start the service
  stop       Stop the service
  status     Show the service status
EOF
}

cmd_install() {
    mkdir -p "$(dirname "${SERVICE_FILE}")"
    cat > "${SERVICE_FILE}" <<EOF
[Unit]
Description=ContextMatrix Runner
After=docker.service

[Service]
WorkingDirectory=${SCRIPT_DIR}
ExecStart=${BINARY} --config ${CONFIG}
Restart=always
RestartSec=10
KillMode=mixed
TimeoutStopSec=60

[Install]
WantedBy=default.target
EOF
    echo "Service file written to ${SERVICE_FILE}"
    systemctl --user daemon-reload
    systemctl --user enable "${SERVICE_NAME}"
    echo "Service '${SERVICE_NAME}' enabled. Run '$(basename "$0") start' to start it."
}

cmd_uninstall() {
    systemctl --user stop "${SERVICE_NAME}" 2>/dev/null || true
    systemctl --user disable "${SERVICE_NAME}" 2>/dev/null || true
    if [ -f "${SERVICE_FILE}" ]; then
        rm -f "${SERVICE_FILE}"
        echo "Removed ${SERVICE_FILE}"
    fi
    systemctl --user daemon-reload
    echo "Service '${SERVICE_NAME}' uninstalled."
}

cmd_start() {
    systemctl --user start "${SERVICE_NAME}"
}

cmd_stop() {
    systemctl --user stop "${SERVICE_NAME}"
}

cmd_status() {
    systemctl --user status "${SERVICE_NAME}"
}

if [ $# -eq 0 ]; then
    usage
    exit 1
fi

case "$1" in
    install)   cmd_install ;;
    uninstall) cmd_uninstall ;;
    start)     cmd_start ;;
    stop)      cmd_stop ;;
    status)    cmd_status ;;
    *)
        echo "Unknown subcommand: $1" >&2
        echo "" >&2
        usage >&2
        exit 1
        ;;
esac
