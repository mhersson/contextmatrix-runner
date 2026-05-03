#!/bin/sh
# Worker entrypoint for the cc-orchestrated runner path.
#
# Bootstraps:
#   - /workspace (created empty; the runner-driven plan/execute phases
#     populate it via git clone / git worktree)
#   - $HOME/.claude.json (MCP server config) from MCP_URL + MCP_API_KEY env
#   - A git credential helper that reads CM_GIT_TOKEN from the per-exec
#     env at git-call time (the runner injects a freshly-minted token on
#     every docker exec, so long-running cards stay authenticated past
#     the App installation token's 1-hour TTL)
#   - $HOME/.claude/skills (best-effort; may be populated by a sidecar)
#   - Identifies as runner@contextmatrix.local for any commits the agents
#     produce
#
# Then sleeps forever. The runner uses Docker SDK ContainerExecCreate
# to spawn each Claude subprocess on demand.
#
# Ordering matters: every file Claude needs at first-exec time
# (.credentials.json, .claude.json, settings.json) is staged BEFORE the
# /tmp/.cm-entrypoint-ready marker is touched, and the dispatcher waits
# for that marker before issuing any docker-exec. The marker keeps
# claude --print from racing against a half-populated $HOME.
#
# IMPORTANT: the worker container runs as a non-root user (USER user in
# Dockerfile.orchestrated). Use $HOME, not /root, for any user-owned
# config; otherwise mkdir/write fails and `set -eu` exits the entrypoint
# before `sleep infinity`, causing the container to die immediately.
set -eu

# Source secrets file if present (same pattern as the cc-legacy entrypoint).
if [ -f /run/cm-secrets/env ]; then
    set -a
    # shellcheck disable=SC1091
    . /run/cm-secrets/env
    set +a
    rm -f /run/cm-secrets/env 2>/dev/null || true
fi

# Workspace dirs (writable by the running user).
mkdir -p /workspace 2>/dev/null || true
mkdir -p "$HOME/.claude/skills"

# ----- Stage credentials FIRST (fast operations) -----
# The runner's first docker-exec races against the entrypoint, so the
# critical files Claude reads on startup must be in place before we
# touch the ready marker. Bulk operations (the full /claude-auth tree,
# task skills, git credential helper) come AFTER the marker.

# Copy just the OAuth credentials and the host's .claude.json. These
# are the two files Claude needs to authenticate; everything else under
# ~/.claude/ (project history, file-history, telemetry caches) is
# non-critical for auth and gets copied in the bulk pass below.
if [ -f /claude-auth/.credentials.json ]; then
    cp /claude-auth/.credentials.json "$HOME/.claude/.credentials.json"
    chmod 600 "$HOME/.claude/.credentials.json" 2>/dev/null || true
fi

if [ -f /claude-auth.json ]; then
    cp /claude-auth.json "$HOME/.claude.json"
    chmod 600 "$HOME/.claude.json" 2>/dev/null || true
fi

# MCP config: merge the contextmatrix MCP server into $HOME/.claude.json
# (Claude Code reads MCP servers from this file). Merging — not
# overwriting — preserves the host's account state and any other
# mcpServers entries the operator already had.
if [ -n "${MCP_URL:-}" ] && [ -n "${MCP_API_KEY:-}" ]; then
    CLAUDE_JSON="$HOME/.claude.json"
    [ -f "$CLAUDE_JSON" ] || echo '{}' > "$CLAUDE_JSON"

    MCP_HEADERS=$(jq -n --arg key "$MCP_API_KEY" '{"Authorization": ("Bearer " + $key)}')
    MCP_ENTRY=$(jq -n \
        --arg url "$MCP_URL" \
        --argjson headers "$MCP_HEADERS" \
        '{"contextmatrix": {"type": "http", "url": $url, "headers": $headers}}')

    jq --argjson mcp "$MCP_ENTRY" \
        '.mcpServers = ((.mcpServers // {}) * $mcp)' \
        "$CLAUDE_JSON" > "${CLAUDE_JSON}.tmp"
    mv "${CLAUDE_JSON}.tmp" "$CLAUDE_JSON"
fi

# Disable Claude Code's default cloud-only MCP servers — the worker has no
# need for Gmail / Calendar / Drive and they'd just produce auth errors at
# startup. Merged with `unique` so any operator-supplied disabled entries
# (and future additions to this list) are preserved.
CLAUDE_JSON="$HOME/.claude.json"
[ -f "$CLAUDE_JSON" ] || echo '{}' > "$CLAUDE_JSON"

DISABLED_DEFAULTS=$(jq -n '[
    "claude.ai Gmail",
    "claude.ai Google Calendar",
    "claude.ai Google Drive"
]')

jq --argjson disabled "$DISABLED_DEFAULTS" \
    '.disabledMcpServers = ((.disabledMcpServers // []) + $disabled | unique)' \
    "$CLAUDE_JSON" > "${CLAUDE_JSON}.tmp"
mv "${CLAUDE_JSON}.tmp" "$CLAUDE_JSON"

# Operator-supplied claude_settings: written before the bulk copy so
# Claude reads the operator's settings on its first invocation.
if [ -n "${CM_CLAUDE_SETTINGS:-}" ]; then
    printf '%s' "$CM_CLAUDE_SETTINGS" > "$HOME/.claude/settings.json"
fi
unset CM_CLAUDE_SETTINGS

# Scrub MCP credentials from the env so spawned processes (every claude
# exec, every Bash tool) cannot read them directly. Claude Code reads
# the bearer from $HOME/.claude.json — the env vars are only needed at
# bootstrap. (CM_GIT_TOKEN / GH_TOKEN are not in this shell at all —
# they arrive only as per-exec env from the runner.)
unset MCP_API_KEY MCP_URL

# Identity for any commits agents land in worktrees inside this container.
git config --global user.name "ContextMatrix Runner"
git config --global user.email "runner@contextmatrix.local"

# ----- Git credential helper (start) -----
# The helper script reads CM_GIT_TOKEN from its own process env at
# git-call time — the runner injects a freshly-minted token on every
# docker exec, so long-running cards stay authenticated. No token ever
# lands on disk inside the worker.
_cred_helper="$HOME/.cm-git-cred-helper.sh"
cat > "$_cred_helper" <<'HELPER_EOF'
#!/bin/sh
# Generated by contextmatrix-runner orchestrated entrypoint. Do not edit.
if [ "${1:-}" = "get" ]; then
    printf 'username=x-access-token\n'
    printf 'password=%s\n' "${CM_GIT_TOKEN:-}"
fi
HELPER_EOF
chmod 700 "$_cred_helper"
git config --global --replace-all credential.helper "!${_cred_helper}"
unset _cred_helper
# ----- Git credential helper (end) -----

# Mark the worker ready: every file Claude needs to authenticate and
# load MCP is in place. The dispatcher polls for this marker before
# issuing any docker-exec.
touch /tmp/.cm-entrypoint-ready

# ----- Bulk operations (post-ready) -----
# Everything below is non-critical for auth/MCP and may run after the
# dispatcher starts execing claude. cp -r on the full host .claude/
# is the slow part — file-history, history.jsonl, projects/ can total
# hundreds of megabytes on an active developer machine.

# Bulk-copy the rest of /claude-auth into $HOME/.claude/, skipping the
# entries that are useless inside a worker. They're either keyed on host
# paths the worker can't see (projects, file-history, shell-snapshots,
# session-env), or host-specific scratch state (paste-cache, cache,
# history.jsonl, tasks). Everything else (plugins, skills, statusline,
# settings) is small and worth bringing along so plugins/skills behave
# like the host.
if [ -d /claude-auth ]; then
    for src in /claude-auth/* /claude-auth/.[!.]*; do
        [ -e "$src" ] || continue
        name="${src##*/}"
        case "$name" in
            projects|file-history|paste-cache|session-env|shell-snapshots|tasks|cache|history.jsonl) continue ;;
        esac
        cp -r "$src" "$HOME/.claude/" 2>/dev/null || true
    done
fi

# ----- Task skills (start) -----
# Filesystem-mounted Claude Code skills.
HOST_SKILLS_DIR="${CM_HOST_SKILLS_DIR:-/host-skills}"
if [ -d "$HOST_SKILLS_DIR" ]; then
    if [ -n "${CM_TASK_SKILLS_SET:-}" ]; then
        OLDIFS="$IFS"
        IFS=','
        # shellcheck disable=SC2086
        set -- ${CM_TASK_SKILLS:-}
        IFS="$OLDIFS"
        for s in "$@"; do
            case "$s" in
                ""|*[!A-Za-z0-9._-]*|.*|-*) continue ;;
            esac
            if [ -d "$HOST_SKILLS_DIR/$s" ]; then
                cp -r "$HOST_SKILLS_DIR/$s" "$HOME/.claude/skills/"
            else
                echo "WARN: requested task skill '$s' not found in $HOST_SKILLS_DIR" >&2
            fi
        done
    else
        for d in "$HOST_SKILLS_DIR"/*/; do
            [ -d "$d" ] || continue
            cp -r "$d" "$HOME/.claude/skills/"
        done
    fi
fi
# ----- Task skills (end) -----

# Sleep forever; the runner drives all CC invocations via docker exec.
exec sleep infinity
