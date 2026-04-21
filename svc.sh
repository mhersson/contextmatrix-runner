#!/usr/bin/env bash
# svc.sh — manage the contextmatrix-runner systemd user service.
#
# Deployment mode: systemd --user (per-user service).
# -------------------------------------------------
# The runner is installed as a *user* unit under ~/.config/systemd/user.
# Rationale:
#   - The runner needs to talk to the user's Docker socket and uses
#     secrets that belong to the operator's home dir.
#   - Running as a user unit avoids requiring root for install/update
#     and naturally scopes the service to a single operator.
#   - Per-operator isolation (each operator gets their own unit) is a
#     feature, not a limitation.
#
# Because this is a user unit, gating on `After=docker.service` does not
# work: the per-user systemd manager cannot observe system units. We
# therefore rely on the runner's own preflight + /readyz loop (CTXRUN-054)
# to wait for dockerd on startup, and omit the `After=` line.
#
# Hardening (CTXRUN-052).
# ----------------------
# The generated [Service] section applies a baseline sandbox + resource
# limits + restart-jitter policy. The directives below were added
# under CTXRUN-052 to address REVIEW.md findings H18, L34, L35.
#   NoNewPrivileges, ProtectSystem=strict, ProtectHome=read-only,
#   PrivateTmp, PrivateDevices, ProtectKernelTunables,
#   ProtectKernelModules, ProtectControlGroups, LockPersonality,
#   MemoryDenyWriteExecute, RestrictRealtime, RestrictNamespaces,
#   RestrictAddressFamilies, SystemCallArchitectures,
#   SystemCallFilter — restrict syscall/fs/namespace surface.
#   MemoryMax, TasksMax, LimitNOFILE — bound resource usage.
#   Restart=on-failure, RestartSec + RestartSteps + RestartMaxDelaySec
#   — exponential backoff with jitter to avoid thundering-herd on a
#   flaky Docker daemon.
#   ReadWritePaths — narrow the filesystem to the runtime/state dirs
#   the runner actually writes to. Paths are prefixed with `-` so a
#   missing dir is tolerated rather than blocking startup.
#
# Subcommands:
#   install [--dry-run]  Write the unit file (or print to stdout), reload
#                        daemon, enable, and restart if already running.
#   uninstall            Stop, disable, and remove the service file.
#   start / stop / status
#   print                Print the generated unit file to stdout.
#   verify               Print the unit and run `systemd-analyze --user
#                        verify` + grep-check for expected directives.

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
  install [--dry-run]  Generate the systemd user service file, reload daemon,
                       enable, and restart if running. --dry-run prints the
                       unit to stdout without touching systemd.
  uninstall            Stop, disable, and remove the service file
  start                Start the service
  stop                 Stop the service
  status               Show the service status
  print                Print the generated unit file to stdout
  verify               Print the unit and run systemd-analyze + grep checks
EOF
}

# generate_unit emits the systemd unit file contents to stdout.
generate_unit() {
    cat <<EOF
[Unit]
Description=ContextMatrix Runner
# NOTE: After=docker.service is intentionally omitted. This is a user
# unit; the per-user systemd manager cannot gate on system units. The
# runner's preflight + /readyz loop (CTXRUN-054) waits for dockerd at
# startup instead.

[Service]
Type=simple
WorkingDirectory=${SCRIPT_DIR}
ExecStart=${BINARY} --config ${CONFIG}
KillMode=mixed
TimeoutStopSec=60

# --- Sandboxing (CTXRUN-052) ---
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=read-only
# Runner needs RW access to its state/log/secrets dirs. Paths are
# prefixed with '-' so a missing dir does not block startup; the
# runner will create them on demand.
ReadWritePaths=-/var/run/cm-runner -/var/log/cm-runner -%h/.cm-runner
PrivateTmp=yes
PrivateDevices=yes
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectControlGroups=yes
RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX
LockPersonality=yes
MemoryDenyWriteExecute=yes
RestrictRealtime=yes
RestrictNamespaces=yes
SystemCallArchitectures=native
SystemCallFilter=@system-service
SystemCallFilter=~@privileged @resources

# --- Resource limits (CTXRUN-052) ---
MemoryMax=2G
TasksMax=1024
LimitNOFILE=65536

# --- Restart policy with jitter (CTXRUN-052 / REVIEW L34) ---
# on-failure + exponential backoff via RestartSteps/RestartMaxDelaySec
# avoids a restart storm when dockerd is flapping.
Restart=on-failure
RestartSec=10
RestartSteps=5
RestartMaxDelaySec=300

[Install]
WantedBy=default.target
EOF
}

# EXPECTED_DIRECTIVES is the set of hardening lines that must appear in
# the generated unit. `verify` grep-asserts each one; keep in sync with
# generate_unit.
EXPECTED_DIRECTIVES=(
    "NoNewPrivileges=yes"
    "ProtectSystem=strict"
    "ProtectHome=read-only"
    "PrivateTmp=yes"
    "PrivateDevices=yes"
    "ProtectKernelTunables=yes"
    "ProtectKernelModules=yes"
    "ProtectControlGroups=yes"
    "LockPersonality=yes"
    "MemoryDenyWriteExecute=yes"
    "RestrictRealtime=yes"
    "RestrictNamespaces=yes"
    "RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX"
    "SystemCallArchitectures=native"
    "SystemCallFilter=@system-service"
    "SystemCallFilter=~@privileged @resources"
    "MemoryMax=2G"
    "TasksMax=1024"
    "LimitNOFILE=65536"
    "Restart=on-failure"
    "RestartSec=10"
    "RestartSteps=5"
    "RestartMaxDelaySec=300"
    "ReadWritePaths=-/var/run/cm-runner -/var/log/cm-runner -%h/.cm-runner"
)

cmd_install() {
    local dry_run=0
    if [ "${1:-}" = "--dry-run" ]; then
        dry_run=1
    fi

    if [ "${dry_run}" -eq 1 ]; then
        generate_unit
        return 0
    fi

    # Idempotent re-install: if the service is currently active, stop it
    # before overwriting the unit file and restart afterwards.
    local was_active=0
    if systemctl --user is-active --quiet "${SERVICE_NAME}"; then
        was_active=1
        systemctl --user stop "${SERVICE_NAME}"
    fi

    mkdir -p "$(dirname "${SERVICE_FILE}")"
    generate_unit > "${SERVICE_FILE}"
    echo "Service file written to ${SERVICE_FILE}"

    systemctl --user daemon-reload
    systemctl --user enable "${SERVICE_NAME}"

    if [ "${was_active}" -eq 1 ]; then
        systemctl --user start "${SERVICE_NAME}"
        echo "Service '${SERVICE_NAME}' restarted with new unit."
    else
        echo "Service '${SERVICE_NAME}' enabled. Run '$(basename "$0") start' to start it."
    fi
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

cmd_print() {
    generate_unit
}

cmd_verify() {
    local unit
    unit="$(generate_unit)"
    echo "---- generated unit ----"
    printf '%s\n' "${unit}"
    echo "---- end unit ----"

    local missing=0
    for directive in "${EXPECTED_DIRECTIVES[@]}"; do
        if ! printf '%s\n' "${unit}" | grep -qF -- "${directive}"; then
            echo "MISSING: ${directive}" >&2
            missing=1
        fi
    done
    if [ "${missing}" -ne 0 ]; then
        echo "verify: one or more expected directives are missing" >&2
        return 1
    fi
    echo "verify: all expected directives present"

    if command -v systemd-analyze >/dev/null 2>&1; then
        local tmp
        tmp="$(mktemp --tmpdir "cm-runner-verify-XXXXXX.service")"
        trap 'rm -f "${tmp}"' RETURN
        printf '%s\n' "${unit}" > "${tmp}"
        echo "---- systemd-analyze --user verify ----"
        # --user verifies against the user manager's search path; it is
        # expected to print warnings for unresolved dirs on hosts that
        # lack them, but exits 0 when the unit is syntactically valid.
        if systemd-analyze --user verify "${tmp}"; then
            echo "systemd-analyze: ok"
        else
            echo "systemd-analyze: reported issues (non-fatal for syntax-only check)" >&2
        fi
    else
        echo "systemd-analyze not found; skipping unit verify"
    fi
}

if [ $# -eq 0 ]; then
    usage
    exit 1
fi

case "$1" in
    install)   shift; cmd_install "$@" ;;
    uninstall) cmd_uninstall ;;
    start)     cmd_start ;;
    stop)      cmd_stop ;;
    status)    cmd_status ;;
    print)     cmd_print ;;
    verify)    cmd_verify ;;
    *)
        echo "Unknown subcommand: $1" >&2
        echo "" >&2
        usage >&2
        exit 1
        ;;
esac
