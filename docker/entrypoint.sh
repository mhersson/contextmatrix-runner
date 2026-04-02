#!/bin/bash
set -euo pipefail

# ----- Claude Code Authentication -----
# If OAuth tokens were mounted from the host, copy them to the writable home.
if [ -d "/claude-auth" ]; then
    cp -r /claude-auth/. "$HOME/.claude/" 2>/dev/null || true
fi
mkdir -p "$HOME/.claude"

# Write MCP config for ContextMatrix server.
MCP_HEADERS="{}"
if [ -n "${CM_MCP_API_KEY:-}" ]; then
    MCP_HEADERS=$(jq -n --arg key "$CM_MCP_API_KEY" '{"Authorization": ("Bearer " + $key)}')
fi

jq -n \
    --arg url "$CM_MCP_URL" \
    --argjson headers "$MCP_HEADERS" \
    '{"mcpServers": {"contextmatrix": {"type": "http", "url": $url, "headers": $headers}}}' \
    > "$HOME/.claude/claude.json"

# ----- Git Configuration -----
git config --global user.name "ContextMatrix Runner"
git config --global user.email "runner@contextmatrix.local"

# Configure git to use GitHub App token via .netrc (keeps token out of git config).
if [ -n "${CM_GIT_TOKEN:-}" ]; then
    printf 'machine github.com\nlogin x-access-token\npassword %s\n' "$CM_GIT_TOKEN" > "$HOME/.netrc"
    chmod 600 "$HOME/.netrc"
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
exec claude -p --dangerously-skip-permissions \
    "You are running inside a disposable container spawned by contextmatrix-runner.
Use the contextmatrix MCP server to execute the run-autonomous workflow for card ${CM_CARD_ID}.

Steps:
1. Call get_skill(skill_name='run-autonomous', card_id='${CM_CARD_ID}', caller_model='claude-opus-4-6')
2. Follow the returned skill instructions exactly.

IMPORTANT:
- Always use MCP tools for all ContextMatrix interactions.
- Never push to main or master.
- Call heartbeat every 5 minutes during idle waits.
- On completion, call complete_task via MCP."
