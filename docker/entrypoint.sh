#!/bin/bash
set -euo pipefail

# Running as root. Set HOME explicitly (USER instruction removed from Dockerfile).
export HOME=/home/user

# ----- Dynamic UID/GID Realignment -----
# If the host passes HOST_UID / HOST_GID, realign the "user" account so that
# files written inside the container are owned by the same uid/gid as the host
# user. Falls back gracefully when the vars are not set.
if [ -n "${HOST_UID:-}" ]; then
    CURRENT_UID=$(id -u user)
    if [ "$CURRENT_UID" != "$HOST_UID" ]; then
        # Remove any existing non-system user that holds the target UID
        BLOCKING_USER=$(getent passwd "$HOST_UID" | cut -d: -f1 || true)
        if [ -n "$BLOCKING_USER" ] && [ "$HOST_UID" -ge 1000 ]; then
            userdel "$BLOCKING_USER"
        fi
        usermod -u "$HOST_UID" user
    fi
fi
if [ -n "${HOST_GID:-}" ]; then
    CURRENT_GID=$(id -g user)
    if [ "$CURRENT_GID" != "$HOST_GID" ]; then
        # Remove any existing non-system group that holds the target GID
        BLOCKING_GROUP=$(getent group "$HOST_GID" | cut -d: -f1 || true)
        if [ -n "$BLOCKING_GROUP" ] && [ "$BLOCKING_GROUP" != "user" ] && [ "$HOST_GID" -ge 1000 ]; then
            groupdel "$BLOCKING_GROUP"
        fi
        groupmod -g "$HOST_GID" user
    fi
fi

# ----- Claude Code Authentication -----
# If OAuth tokens were mounted from the host, copy them to the writable home.
# Running as root here so the source files are always readable.
if [ -d "/claude-auth" ]; then
    cp -r /claude-auth/. "$HOME/.claude/" 2>/dev/null || true
fi
mkdir -p "$HOME/.claude"
chown -R user:user "$HOME/.claude"

# Write MCP config for ContextMatrix server into ~/.claude.json
# (Claude Code reads MCP servers from this file, not settings.json).
MCP_HEADERS="{}"
if [ -n "${CM_MCP_API_KEY:-}" ]; then
    MCP_HEADERS=$(jq -n --arg key "$CM_MCP_API_KEY" '{"Authorization": ("Bearer " + $key)}')
fi

MCP_ENTRY=$(jq -n \
    --arg url "$CM_MCP_URL" \
    --argjson headers "$MCP_HEADERS" \
    '{"contextmatrix": {"type": "http", "url": $url, "headers": $headers}}')

CLAUDE_JSON="$HOME/.claude.json"
[ -f "$CLAUDE_JSON" ] || echo '{}' > "$CLAUDE_JSON"
jq --argjson mcp "$MCP_ENTRY" '.mcpServers = ((.mcpServers // {}) * $mcp)' "$CLAUDE_JSON" > "${CLAUDE_JSON}.tmp"
mv "${CLAUDE_JSON}.tmp" "$CLAUDE_JSON"

# ----- Git Configuration -----
git config --global user.name "ContextMatrix Runner"
git config --global user.email "runner@contextmatrix.local"

# Configure git to use GitHub App token via .netrc (keeps token out of git config).
if [ -n "${CM_GIT_TOKEN:-}" ]; then
    printf 'machine github.com\nlogin x-access-token\npassword %s\n' "$CM_GIT_TOKEN" > "$HOME/.netrc"
    chmod 600 "$HOME/.netrc"
    # Expose token for GitHub CLI (gh pr create, etc.).
    export GH_TOKEN="$CM_GIT_TOKEN"
    # Redirect SSH-style URLs to HTTPS so .netrc credentials are used.
    git config --global url."https://github.com/".insteadOf "git@github.com:"
    git config --global url."https://github.com/".insteadOf "ssh://git@github.com/"
    git config --global url."https://github.com/".insteadOf "ssh://github.com/"
fi

# ----- Clone and Execute -----
echo "Cloning ${CM_REPO_URL}..."
git clone "${CM_REPO_URL}" /home/user/workspace
cd /home/user/workspace

echo "Starting Claude Code for card ${CM_CARD_ID}..."
chown -R user:user /home/user
exec gosu user claude -p --model claude-sonnet-4-6 --output-format stream-json --verbose --dangerously-skip-permissions \
    "You are running inside a disposable container spawned by contextmatrix-runner.
Use the contextmatrix MCP server to execute the run-autonomous workflow for card ${CM_CARD_ID}.

Steps:
1. Call get_skill(skill_name='run-autonomous', card_id='${CM_CARD_ID}', caller_model='sonnet')
2. Follow the returned skill instructions exactly.

IMPORTANT:
- Always use MCP tools for all ContextMatrix interactions.
- Never push to main or master.
- Call heartbeat every 5 minutes during idle waits.
- Call report_usage after every heartbeat call.
- On completion, call release_card after transitioning to done — do NOT skip this."
