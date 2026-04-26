#!/bin/bash
# Explicit allowlist (CTXRUN-045) replaces the old unconditional
# permission-bypass flag. To add a tool, justify in PR + update this list +
# add a test.
set -euo pipefail

# ----- Secrets file (preferred: off-env, tmpfs bind-mount) -----
# contextmatrix-runner may write a read-only bind-mounted file at
# /run/cm-secrets/env containing the sensitive values (CM_GIT_TOKEN,
# CLAUDE_CODE_OAUTH_TOKEN, ANTHROPIC_API_KEY, CM_MCP_API_KEY). Sourcing it
# here keeps those values out of Docker's visible container env
# (`docker inspect` shows HostConfig.Env, not runtime-sourced vars).
CM_SECRETS_FILE="/run/cm-secrets/env"
if [ -f "$CM_SECRETS_FILE" ]; then
    set -a
    # shellcheck disable=SC1090
    . "$CM_SECRETS_FILE"
    set +a
    # Best-effort secure wipe from the container's view. The bind mount
    # is read-only so shred/rm will typically fail; the runner unlinks
    # the host-side file when the container exits.
    shred -u "$CM_SECRETS_FILE" 2>/dev/null || rm -f "$CM_SECRETS_FILE" 2>/dev/null || true
else
    # Backward-compat fallback: older callers may still pass secrets
    # via env vars. Warn so operators notice and migrate.
    if [ -n "${CM_GIT_TOKEN:-}${CLAUDE_CODE_OAUTH_TOKEN:-}${ANTHROPIC_API_KEY:-}${CM_MCP_API_KEY:-}" ]; then
        echo "WARN: secrets provided via environment; prefer /run/cm-secrets/env bind mount" >&2
    fi
fi

# ----- Tool allowlist (CTXRUN-045) -----
# Passed to `claude --allowed-tools` so the worker can only call pre-approved
# tools. Split into two arrays keyed on run mode:
#
#   ALLOWED_TOOLS_COMMON — everything safe for both modes.
#   ALLOWED_TOOLS_AUTO   — autonomous-only additions (currently just Task).
#
# Why split:
#   Sub-agents (Task tool) are kept out of HITL mode because the user expects
#   to review every change before a commit lands — sub-agents making
#   autonomous commits during an interactive session would bypass that gate.
#   In autonomous mode the top-level agent is already committing without
#   human review, so sub-agents doing the same is fine and lets the orchestrator
#   parallelise research/subtasks.
#
# Destructive ContextMatrix RPCs (delete_project, update_project) are
# excluded in both modes — nothing spawned in a worker needs those.
#
# Shell utilities are allowlisted by exact command prefix (e.g. "Bash(sed:*)")
# so a compromised model can't promote "Bash(sed:*)" into "Bash(rm -rf /:*)"
# — claude evaluates each Bash invocation against the longest matching prefix.
ALLOWED_TOOLS_COMMON=(
    "Read"
    "Edit"
    "Write"
    "Skill"
    "MultiEdit"
    "NotebookEdit"
    "Glob"
    "Grep"
    "TodoWrite"
    "WebFetch"
    "WebSearch"
    # Version control + language toolchain.
    "Bash(git:*)"
    "Bash(gh:*)"
    "Bash(go test:*)"
    "Bash(go build:*)"
    "Bash(go vet:*)"
    "Bash(go mod:*)"
    "Bash(go run:*)"
    "Bash(go install:*)"
    "Bash(make:*)"
    # Node.js / frontend workflow (npm install/test/build, node scripts).
    "Bash(npm:*)"
    "Bash(node:*)"
    "Bash(npx:*)"
    # Python — generic scripts, pip for packages; pytest etc. run via python3 -m.
    "Bash(python3:*)"
    "Bash(pip3:*)"
    # Filesystem basics. rm is intentionally broad because worker containers
    # are disposable; the real blast-radius control is container isolation,
    # not shell argument filtering.
    "Bash(mv:*)"
    "Bash(cp:*)"
    "Bash(rm:*)"
    "Bash(mkdir:*)"
    "Bash(ls:*)"
    "Bash(find:*)"
    "Bash(which:*)"
    "Bash(command:*)"
    # Text inspection + transformation. All are read-or-stdout-only unless
    # paired with > redirection (which claude reports per-command).
    "Bash(cat:*)"
    "Bash(head:*)"
    "Bash(tail:*)"
    "Bash(wc:*)"
    "Bash(echo:*)"
    "Bash(printenv:*)"
    "Bash(sed:*)"
    "Bash(awk:*)"
    "Bash(grep:*)"
    "Bash(sort:*)"
    "Bash(uniq:*)"
    "Bash(diff:*)"
    "Bash(tr:*)"
    "Bash(cut:*)"
    "Bash(tee:*)"
    "Bash(xargs:*)"
    "Bash(date:*)"
    "Bash(jq:*)"
    "mcp__contextmatrix__add_log"
    "mcp__contextmatrix__check_agent_health"
    "mcp__contextmatrix__claim_card"
    "mcp__contextmatrix__complete_task"
    "mcp__contextmatrix__create_card"
    "mcp__contextmatrix__get_card"
    "mcp__contextmatrix__get_ready_tasks"
    "mcp__contextmatrix__get_skill"
    "mcp__contextmatrix__get_subtask_summary"
    "mcp__contextmatrix__get_task_context"
    "mcp__contextmatrix__heartbeat"
    "mcp__contextmatrix__increment_review_attempts"
    "mcp__contextmatrix__list_cards"
    "mcp__contextmatrix__list_projects"
    "mcp__contextmatrix__promote_to_autonomous"
    "mcp__contextmatrix__recalculate_costs"
    "mcp__contextmatrix__release_card"
    "mcp__contextmatrix__report_push"
    "mcp__contextmatrix__report_usage"
    "mcp__contextmatrix__start_workflow"
    "mcp__contextmatrix__transition_card"
    "mcp__contextmatrix__update_card"
)

# Autonomous-mode-only additions. Task (sub-agent spawning) is allowed here
# because autonomous mode has no human review gate on commits; parallel
# sub-agents committing is the intended behaviour.
ALLOWED_TOOLS_AUTO_EXTRAS=(
    "Task"
)

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

# ----- Input validation (defense-in-depth) -----
# Validate CM_CARD_ID early — we interpolate it into prompts and container logs.
# Use `case` (whole-string match) rather than grep (line-oriented) so embedded
# newline/CR/NUL bytes fall into the reject pattern.
if [ -n "${CM_CARD_ID:-}" ]; then
    case "$CM_CARD_ID" in
        -*|*[!A-Za-z0-9._-]*)
            echo "ERROR: invalid CM_CARD_ID" >&2
            exit 1
            ;;
    esac
fi

# Validate branch name to prevent git option injection.
if [ -n "${CM_BASE_BRANCH:-}" ]; then
    case "$CM_BASE_BRANCH" in
        -*|*[!A-Za-z0-9._/-]*)
            echo "ERROR: invalid CM_BASE_BRANCH" >&2
            exit 1
            ;;
    esac
fi

# Validate CM_REPO_URL — must start with https:// and contain only safe chars.
# Host is extracted via parameter expansion (no sed), then re-validated to
# close the .netrc/credential-helper injection surface.
GIT_HOST=""
case "${CM_REPO_URL:-}" in
    "")
        : # may be validated again at the git clone step below
        ;;
    https://*)
        _rest="${CM_REPO_URL#https://}"
        case "$_rest" in
            -*|*[!A-Za-z0-9._/:@-]*)
                echo "ERROR: invalid CM_REPO_URL" >&2
                exit 1
                ;;
        esac
        GIT_HOST="${_rest%%/*}"
        unset _rest
        ;;
    *)
        echo "ERROR: CM_REPO_URL must be https://" >&2
        exit 1
        ;;
esac
case "$GIT_HOST" in
    -*|*[!A-Za-z0-9.-]*)
        GIT_HOST=""
        ;;
esac
[ -z "$GIT_HOST" ] && GIT_HOST="github.com"

# ----- Git Configuration -----
git config --global user.name "ContextMatrix Runner"
git config --global user.email "runner@contextmatrix.local"

# Configure git to use the GitHub App / PAT token via a credential helper that
# reads from a local file. This is preferred over .netrc because the helper
# script is sh-parsed (not the line-oriented parser in git's netrc reader),
# and because the token only leaves memory through git's documented credential
# protocol — no risk of a newline in a host value injecting an extra
# `machine` clause.
if [ -n "${CM_GIT_TOKEN:-}" ]; then
    # Reject tokens containing anything outside the standard GitHub token
    # charset — a newline here would let the heredoc below close early and
    # subsequent lines would be evaluated as shell commands.
    case "$CM_GIT_TOKEN" in
        *[!A-Za-z0-9_-]*)
            echo "ERROR: CM_GIT_TOKEN contains invalid characters" >&2
            exit 1
            ;;
    esac

    _cred_dir="$HOME/.cm-git-cred"
    mkdir -p "$_cred_dir"
    chmod 700 "$_cred_dir"
    _cred_file="$_cred_dir/token"
    umask_prev=$(umask)
    umask 077
    printf '%s\n' "$CM_GIT_TOKEN" > "$_cred_file"
    umask "$umask_prev"
    unset umask_prev
    chmod 600 "$_cred_file"

    _cred_helper="$_cred_dir/helper.sh"
    # The helper is invoked by git with `get` on argv[1] and reads the
    # credential request on stdin. We only need to respond to `get`; for
    # other actions we silently succeed. The token is read from a mode-0600
    # file rather than inlined into the script so its bytes are never
    # parsed by the shell.
    cat > "$_cred_helper" <<'HELPER_EOF'
#!/bin/sh
# Generated by contextmatrix-runner entrypoint. Do not edit.
if [ "${1:-}" = "get" ]; then
    printf 'username=x-access-token\n'
    printf 'password=%s\n' "$(cat "$(dirname "$0")/token")"
fi
HELPER_EOF
    chmod 700 "$_cred_helper"
    git config --global --replace-all "credential.https://${GIT_HOST}.helper" "!${_cred_helper}"
    unset _cred_dir _cred_helper _cred_file

    # Expose token for GitHub CLI (gh pr create, etc.). This keeps GH_TOKEN in
    # claude's env — acceptable given gh CLI reads only via env — but the
    # original CM_GIT_TOKEN can be dropped below.
    export GH_TOKEN="$CM_GIT_TOKEN"
    # Required by gh CLI when targeting a non-github.com host.
    export GH_HOST="$GIT_HOST"
fi

# ----- Clone and Execute -----
# `--` stops git from interpreting later args as options even if CM_REPO_URL
# ever begins with "-" (the case-based validators above already reject that,
# but defense in depth is cheap).
if [ -n "${CM_BASE_BRANCH:-}" ]; then
    echo "Cloning ${CM_REPO_URL} (branch: ${CM_BASE_BRANCH})..."
    git clone -b "${CM_BASE_BRANCH}" -- "${CM_REPO_URL}" /home/user/workspace
else
    echo "Cloning ${CM_REPO_URL}..."
    git clone -- "${CM_REPO_URL}" /home/user/workspace
fi
cd /home/user/workspace

BASE_BRANCH_CONTEXT=""
if [ -n "${CM_BASE_BRANCH:-}" ]; then
    BASE_BRANCH_CONTEXT="The base branch for this task is ${CM_BASE_BRANCH}. Create PRs targeting this branch using 'gh pr create --base ${CM_BASE_BRANCH}'."
fi

# Scrub secrets that downstream consumers no longer need from the process env.
# CM_GIT_TOKEN → already copied into the credential helper file.
# CM_MCP_API_KEY → already written into ~/.claude.json.
# Both of these would otherwise leak into every Bash/Tool subprocess claude
# spawns (defence-in-depth — the --allowed-tools allowlist from CTXRUN-045
# restricts which tools claude will invoke, but env hygiene still matters).
# CLAUDE_CODE_OAUTH_TOKEN / ANTHROPIC_API_KEY are intentionally preserved:
# the Claude CLI reads them at startup, and removing them here breaks auth in
# the env-fallback path.
unset CM_GIT_TOKEN CM_MCP_API_KEY

# ----- Task skills (filesystem-mounted Claude Code skills) -----
# shellcheck source=docker/entrypoint-skills.sh
. "$(dirname "$0")/entrypoint-skills.sh"

echo "Starting Claude Code for card ${CM_CARD_ID}..."
# Space-separated allowlist passed via a single --allowed-tools flag, per
# `claude --help`: "--allowedTools, --allowed-tools <tools...>  Comma or
# space-separated list of tool names to allow (e.g. \"Bash(git *) Edit\")".
# HITL mode uses the common list only; autonomous mode appends the extras
# (Task — sub-agent spawning).
if [ "${CM_INTERACTIVE:-}" = "1" ]; then
    ALLOWED_TOOLS_HITL=("${ALLOWED_TOOLS_COMMON[@]}")
    # `--` terminates option parsing. Without it, claude's variadic
    # `--allowed-tools <tools...>` greedily consumes the following positional
    # prompt as yet another allowed-tool entry and exits with
    # "Input must be provided either through stdin or as a prompt argument".
    exec claude -p --model "${CM_ORCHESTRATOR_MODEL:-claude-sonnet-4-6}" \
        --input-format stream-json \
        --output-format stream-json \
        --verbose --allowed-tools "${ALLOWED_TOOLS_HITL[*]}" \
        -- \
        "You are running inside a disposable container spawned by contextmatrix-runner for card ${CM_CARD_ID}.
A human user may send you approval messages at interactive gates.

IMPORTANT:
- Always use MCP tools for all ContextMatrix interactions.
- Never push to main or master.
- Call heartbeat every 5 minutes during idle waits.
- Call report_usage after every heartbeat call.
- On completion, call release_card after transitioning to done — do NOT skip this.
${BASE_BRANCH_CONTEXT}"
else
    ALLOWED_TOOLS_AUTO=("${ALLOWED_TOOLS_COMMON[@]}" "${ALLOWED_TOOLS_AUTO_EXTRAS[@]}")
    # See HITL branch above for why `--` is required before the prompt.
    exec claude -p --model "${CM_ORCHESTRATOR_MODEL:-claude-sonnet-4-6}" --output-format stream-json --verbose --allowed-tools "${ALLOWED_TOOLS_AUTO[*]}" \
        -- \
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
fi
