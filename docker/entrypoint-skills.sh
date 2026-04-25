#!/bin/bash
# entrypoint-skills.sh — Task skills copy helper.
# Sourced by entrypoint.sh just before exec claude.
# Also tested standalone by internal/container/entrypoint_host_test.go.
#
# Environment variables consumed:
#   CM_HOST_SKILLS_DIR   Override the bind-mount path (default: /host-skills).
#                        Set in tests to sandbox the copy without root or mounts.
#   CM_TASK_SKILLS_SET   Non-empty → explicit selection mode.
#                        Unset/empty → copy full set from CM_HOST_SKILLS_DIR.
#   CM_TASK_SKILLS       Comma-separated skill names to copy (only read when
#                        CM_TASK_SKILLS_SET is set).

HOST_SKILLS_DIR="${CM_HOST_SKILLS_DIR:-/host-skills}"
mkdir -p "$HOME/.claude/skills"

if [ -d "$HOST_SKILLS_DIR" ]; then
    if [ -n "${CM_TASK_SKILLS_SET:-}" ]; then
        # Explicit list (may be empty). Copy listed skills only.
        IFS=',' read -ra _cm_task_skills <<< "${CM_TASK_SKILLS:-}"
        for s in "${_cm_task_skills[@]}"; do
            # Validate name: same charset as the runner-side allowlist.
            # Reject empty, leading dash, leading dot, anything with non-alphanumeric+._-
            case "$s" in
                ""|*[!A-Za-z0-9._-]*|.*|-*) continue ;;
            esac
            if [ -d "${HOST_SKILLS_DIR}/$s" ]; then
                cp -r "${HOST_SKILLS_DIR}/$s" "$HOME/.claude/skills/"
            else
                echo "WARN: requested task skill '$s' not found in ${HOST_SKILLS_DIR}" >&2
            fi
        done
        unset _cm_task_skills s
    else
        # No constraint — mount the full set.
        for d in "${HOST_SKILLS_DIR}"/*/; do
            [ -d "$d" ] || continue
            cp -r "$d" "$HOME/.claude/skills/"
        done
        unset d
    fi
fi
