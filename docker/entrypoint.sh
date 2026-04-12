#!/bin/bash
set -euo pipefail

# ----- Claude Code Authentication -----
# If OAuth tokens were mounted from the host, copy them to the writable home.
if [ -d "/claude-auth" ]; then
    cp -r /claude-auth/. "$HOME/.claude/" 2>/dev/null || true
fi
mkdir -p "$HOME/.claude"

# Write claude settings.json if provided via env var.
# This runs after the optional claude-auth copy so it always wins.
if [ -n "${CM_CLAUDE_SETTINGS:-}" ]; then
    printf '%s' "$CM_CLAUDE_SETTINGS" > "$HOME/.claude/settings.json"
fi

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

# Validate branch name to prevent git option injection.
if [ -n "${CM_BASE_BRANCH:-}" ]; then
    if printf '%s' "$CM_BASE_BRANCH" | grep -qE '^-|[[:space:]]'; then
        echo "ERROR: Invalid branch name: ${CM_BASE_BRANCH}" >&2
        exit 1
    fi
fi

# ----- Clone and Execute -----
if [ -n "${CM_BASE_BRANCH:-}" ]; then
    echo "Cloning ${CM_REPO_URL} (branch: ${CM_BASE_BRANCH})..."
    git clone -b "${CM_BASE_BRANCH}" "${CM_REPO_URL}" /home/user/workspace
else
    echo "Cloning ${CM_REPO_URL}..."
    git clone "${CM_REPO_URL}" /home/user/workspace
fi
cd /home/user/workspace

BASE_BRANCH_CONTEXT=""
if [ -n "${CM_BASE_BRANCH:-}" ]; then
    BASE_BRANCH_CONTEXT="The base branch for this task is ${CM_BASE_BRANCH}. Create PRs targeting this branch using 'gh pr create --base ${CM_BASE_BRANCH}'."
fi

echo "Starting Claude Code for card ${CM_CARD_ID}..."
exec claude -p --model claude-sonnet-4-6 --output-format stream-json --verbose --dangerously-skip-permissions \
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
- On completion, call release_card after transitioning to done — do NOT skip this.
${BASE_BRANCH_CONTEXT}"
