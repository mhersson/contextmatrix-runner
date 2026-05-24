#!/bin/bash
# Explicit allowlist replaces the old unconditional permission-bypass flag.
# To add a tool, justify in PR + update this list + add a test.
set -euo pipefail

# ----- Secrets file (shared host-directory bind mount) -----
# The runner mounts a host directory at /run/cm-secrets/ and writes
# /run/cm-secrets/env before starting this container. Contents:
#   CM_GIT_TOKEN               — GitHub App installation token (rotated by
#                                tokenRefresher before expiry)
#   CLAUDE_CODE_OAUTH_TOKEN    — OAuth token for claude CLI
#   ANTHROPIC_API_KEY          — API key for claude CLI
# CM_MCP_API_KEY is passed via Container.Env, not the file.
# The runner's tokenRefresher owns the file and rewrites it in place;
# workers never unlink it.
CM_SECRETS_FILE="/run/cm-secrets/env"
if [ -f "$CM_SECRETS_FILE" ]; then
    set -a
    # shellcheck disable=SC1090
    . "$CM_SECRETS_FILE"
    set +a
else
    # Backward-compat fallback: older callers may still pass secrets
    # via env vars. Warn so operators notice and migrate.
    if [ -n "${CM_GIT_TOKEN:-}${CLAUDE_CODE_OAUTH_TOKEN:-}${ANTHROPIC_API_KEY:-}${CM_MCP_API_KEY:-}" ]; then
        echo "WARN: secrets provided via environment; prefer /run/cm-secrets/env bind mount" >&2
    fi
fi

# ----- Tool allowlist -----
# Passed to `claude --allowed-tools` so the worker can only call pre-approved
# tools. Built from a shared base plus per-mode additions:
#
#   ALLOWED_TOOLS_COMMON       — everything safe across all modes.
#   ALLOWED_TOOLS_AUTO_EXTRAS  — autonomous-task-mode additions (Task).
#   ALLOWED_TOOLS_KB           — knowledge-refresh mode (built inline below):
#                                COMMON + Task + KB-specific MCP tools.
#
# Why three axes (HITL vs autonomous vs knowledge-refresh):
#   HITL (interactive task mode): Task (sub-agents) excluded — the user
#     expects to review every change before a commit lands. Sub-agents
#     making autonomous commits during an interactive session would bypass
#     that gate.
#   Autonomous (task mode with autonomous: true): Task included — the
#     top-level agent is already committing without human review, so
#     sub-agents doing the same is fine and lets the orchestrator
#     parallelise research/subtasks.
#   Knowledge-refresh: Task included — the refresh-knowledge skill spawns
#     one sub-agent per doc by design (parallel doc generation), and the
#     only write surface is commit_knowledge_docs which is server-side
#     atomic. The skill never pushes or opens PRs against the cloned
#     target repo, so sub-agents cannot land code changes from inside
#     the container.
#
# Destructive ContextMatrix RPCs (delete_project, update_project) are
# excluded in all modes — nothing spawned in a worker needs those.
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
    "Bash(golangci-lint run:*)"
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
    "mcp__contextmatrix__get_knowledge_base"
    "mcp__contextmatrix__get_ready_tasks"
    "mcp__contextmatrix__get_skill"
    "mcp__contextmatrix__get_subtask_summary"
    "mcp__contextmatrix__get_task_context"
    "mcp__contextmatrix__heartbeat"
    "mcp__contextmatrix__increment_review_attempts"
    "mcp__contextmatrix__list_cards"
    "mcp__contextmatrix__list_knowledge_bases"
    "mcp__contextmatrix__list_projects"
    "mcp__contextmatrix__promote_to_autonomous"
    "mcp__contextmatrix__read_knowledge_doc"
    "mcp__contextmatrix__recalculate_costs"
    "mcp__contextmatrix__release_card"
    "mcp__contextmatrix__report_push"
    "mcp__contextmatrix__report_usage"
    "mcp__contextmatrix__start_review"
    "mcp__contextmatrix__start_workflow"
    "mcp__contextmatrix__transition_card"
    "mcp__contextmatrix__update_card"
    "mcp__contextmatrix__chat_rehydration_complete"
    # Permission gate for tool calls whose checkPermissions returns
    # behavior:"ask" (currently AskUserQuestion). Wired via
    # --permission-prompt-tool below; without it Claude Code auto-denies
    # AskUserQuestion in headless mode and the model improvises.
    "mcp__contextmatrix__permission_prompt"
)

# Autonomous-mode-only additions. Task (sub-agent spawning) is allowed here
# because autonomous mode has no human review gate on commits; parallel
# sub-agents committing is the intended behaviour.
ALLOWED_TOOLS_AUTO_EXTRAS=(
    "Task"
)

# ----- Claude Code Authentication -----
# Bulk-copy /claude-auth into $HOME/.claude/, skipping entries that are
# useless inside a worker. They're either keyed on host paths the worker
# can't see (projects, file-history, shell-snapshots, session-env), or
# host-specific scratch state (paste-cache, cache, history.jsonl, tasks).
# Everything else (plugins, skills, statusline, settings) is small and
# worth bringing along so plugins/skills behave like the host.
mkdir -p "$HOME/.claude"
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
# Chat-mode containers forward CM_CHAT_SESSION so CM can gate session-scoped
# tools (chat_rehydration_complete) to the caller's own session. Card-mode
# workers leave CM_CHAT_SESSION unset, so the header is omitted and the
# server-side gate is skipped.
if [ -n "${CM_CHAT_SESSION:-}" ]; then
    MCP_HEADERS=$(jq -n \
        --argjson base "$MCP_HEADERS" \
        --arg session "$CM_CHAT_SESSION" \
        '$base + {"X-CM-Chat-Session": $session}')
fi

CLAUDE_JSON="$HOME/.claude.json"
[ -f "$CLAUDE_JSON" ] || echo '{}' > "$CLAUDE_JSON"

# Skip the MCP merge entirely when CM_MCP_URL is empty/unset. Chat-mode
# containers may opt out of MCP wiring (StartChat only sets CM_MCP_URL when
# opts.MCPURL is non-empty); under `set -u`, dereferencing $CM_MCP_URL
# directly would crash the entrypoint before claude starts. The merge below
# is the only place that needs the URL, so guarding the block is sufficient.
if [ -n "${CM_MCP_URL:-}" ]; then
    MCP_ENTRY=$(jq -n \
        --arg url "${CM_MCP_URL:-}" \
        --argjson headers "$MCP_HEADERS" \
        '{"contextmatrix": {"type": "http", "url": $url, "headers": $headers, "alwaysLoad": true}}')

    jq --argjson mcp "$MCP_ENTRY" '.mcpServers = ((.mcpServers // {}) * $mcp)' "$CLAUDE_JSON" > "${CLAUDE_JSON}.tmp"
    mv "${CLAUDE_JSON}.tmp" "$CLAUDE_JSON"
fi

# Disable Claude Code's default cloud-only MCP servers — the worker has no
# need for Gmail / Calendar / Drive and they'd just produce auth errors at
# startup. Merged with `unique` so any operator-supplied disabled entries
# (and future additions to this list) are preserved.
DISABLED_DEFAULTS=$(jq -n '[
    "claude.ai Gmail",
    "claude.ai Google Calendar",
    "claude.ai Google Drive"
]')

jq --argjson disabled "$DISABLED_DEFAULTS" \
    '.disabledMcpServers = ((.disabledMcpServers // []) + $disabled | unique)' \
    "$CLAUDE_JSON" > "${CLAUDE_JSON}.tmp"
mv "${CLAUDE_JSON}.tmp" "$CLAUDE_JSON"

# ----- Input validation (defense-in-depth) -----
# Validate CM_CARD_ID early — we interpolate it into prompts and container logs.
# Use `case` (whole-string match) rather than grep (line-oriented) so embedded
# newline/CR/NUL bytes fall into the reject pattern.
# Skip in knowledge-refresh mode: no card ID is set; the runner uses a synthetic
# kb-refresh:<repo> key internally but does not pass it as CM_CARD_ID.
if [ "${CM_MODE:-}" != "knowledge-refresh" ]; then
    if [ -n "${CM_CARD_ID:-}" ]; then
        case "$CM_CARD_ID" in
            -*|*[!A-Za-z0-9._-]*)
                echo "ERROR: invalid CM_CARD_ID" >&2
                exit 1
                ;;
        esac
    fi
fi

if [ "${CM_MODE:-}" = "knowledge-refresh" ]; then
    case "${CM_PROJECT:-}" in
        ""|-*|*[!A-Za-z0-9._-]*)
            echo "ERROR: invalid CM_PROJECT for knowledge-refresh mode" >&2
            exit 1
            ;;
    esac
    case "${CM_KB_REPO:-}" in
        ""|-*|*[!A-Za-z0-9._-]*)
            echo "ERROR: invalid CM_KB_REPO for knowledge-refresh mode" >&2
            exit 1
            ;;
    esac
    case "${CM_AGENT_ID:-}" in
        human:?*) ;;
        *)
            echo "ERROR: CM_AGENT_ID must start with human: and have a non-empty suffix in knowledge-refresh mode" >&2
            exit 1
            ;;
    esac
    # Defence-in-depth: webhook validator already enforces the doc allowlist,
    # but if a future caller sets this env var directly (skipping the webhook
    # path), the value would otherwise interpolate unchecked into the prompt.
    # Empty value is permitted — most refresh runs have no overwrite_docs.
    case "${CM_KB_OVERWRITE_DOCS:-}" in
        ""|-*|*[!A-Za-z0-9._,-]*)
            if [ -n "${CM_KB_OVERWRITE_DOCS:-}" ]; then
                echo "ERROR: CM_KB_OVERWRITE_DOCS contains invalid characters" >&2
                exit 1
            fi
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

# Validate CM_CHAT_SESSION (interpolated into prompt + workspace paths).
if [ -n "${CM_CHAT_SESSION:-}" ]; then
    case "$CM_CHAT_SESSION" in
        -*|*[!A-Za-z0-9._-]*)
            echo "ERROR: invalid CM_CHAT_SESSION" >&2
            exit 1
            ;;
    esac
fi
# Validate CM_CHAT_PROJECT (used as directory name).
if [ -n "${CM_CHAT_PROJECT:-}" ]; then
    case "$CM_CHAT_PROJECT" in
        -*|*[!A-Za-z0-9._-]*)
            echo "ERROR: invalid CM_CHAT_PROJECT" >&2
            exit 1
            ;;
    esac
fi
# CM_CHAT_REPO_URL: same validation as CM_REPO_URL — must start with https://
# and contain only safe chars. Skip the GIT_HOST extraction since chat mode
# doesn't piggy-back on the existing GIT_HOST var.
if [ -n "${CM_CHAT_REPO_URL:-}" ]; then
    case "$CM_CHAT_REPO_URL" in
        https://*)
            _rest="${CM_CHAT_REPO_URL#https://}"
            case "$_rest" in
                -*|*[!A-Za-z0-9._/:@-]*)
                    echo "ERROR: invalid CM_CHAT_REPO_URL" >&2
                    exit 1
                    ;;
            esac
            unset _rest
            ;;
        *)
            echo "ERROR: CM_CHAT_REPO_URL must be https://" >&2
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

# Configure git to authenticate via a credential helper that re-sources
# the bind-mounted secrets file on every `get`. The runner rewrites the
# file before the GitHub App installation token expires; any git op
# started after the rewrite picks up the new value automatically.
if [ -f /run/cm-secrets/env ]; then
    mkdir -p "$HOME/.local/lib"
    _cred_helper="$HOME/.local/lib/cm-cred-helper.sh"
    cat > "$_cred_helper" <<'HELPER_EOF'
#!/bin/sh
if [ "${1:-}" = "get" ]; then
    . /run/cm-secrets/env
    printf 'username=x-access-token\n'
    printf 'password=%s\n' "$CM_GIT_TOKEN"
fi
HELPER_EOF
    chmod 700 "$_cred_helper"
    git config --global --replace-all "credential.https://${GIT_HOST}.helper" "!${_cred_helper}"
    unset _cred_helper
fi

# Install a gh wrapper that re-sources the bind-mounted secrets on every
# invocation. gh honours GH_TOKEN from its process env; a static export
# would pin the value at entrypoint time and break token refresh.
if [ -f /run/cm-secrets/env ]; then
    mkdir -p "$HOME/.local/bin"
    cat > "$HOME/.local/bin/gh" <<'GH_EOF'
#!/bin/sh
. /run/cm-secrets/env
export GH_TOKEN="$CM_GIT_TOKEN"
exec /usr/bin/gh "$@"
GH_EOF
    chmod 700 "$HOME/.local/bin/gh"
fi

# GH_HOST is not a secret and is safe to export statically.
export GH_HOST="$GIT_HOST"

# Prepend $HOME/.local/bin so the gh wrapper above shadows /usr/bin/gh.
export PATH="$HOME/.local/bin:$PATH"

# ----- Clone and Execute -----
# `--` stops git from interpreting later args as options even if CM_REPO_URL
# ever begins with "-" (the case-based validators above already reject that,
# but defense in depth is cheap).
# Chat-mode containers handle their own clone inside the dispatch branch
# (CM_CHAT_REPO_URL / CM_CHAT_PROJECT) and don't set CM_REPO_URL, so skip
# the git-clone step under `set -u`. The cd into /home/user/workspace runs
# in both modes — chat will override it from its dispatch branch but the
# code between here and the dispatch (skills source, secret scrub) is
# happier with a known cwd that exists.
if [ -z "${CM_CHAT_SESSION:-}" ]; then
    if [ -n "${CM_BASE_BRANCH:-}" ]; then
        echo "Cloning ${CM_REPO_URL:-} (branch: ${CM_BASE_BRANCH})..."
        git clone -b "${CM_BASE_BRANCH}" -- "${CM_REPO_URL:-}" /home/user/workspace
    else
        echo "Cloning ${CM_REPO_URL:-}..."
        git clone -- "${CM_REPO_URL:-}" /home/user/workspace
    fi
fi
mkdir -p /home/user/workspace
cd /home/user/workspace

BASE_BRANCH_CONTEXT=""
if [ -n "${CM_BASE_BRANCH:-}" ]; then
    BASE_BRANCH_CONTEXT="The base branch for this task is ${CM_BASE_BRANCH}. Create PRs targeting this branch using 'gh pr create --base ${CM_BASE_BRANCH}'."
fi

# Scrub secrets that downstream consumers no longer need from the process env.
# CM_GIT_TOKEN → already copied into the credential helper file.
# CM_MCP_API_KEY → already written into ~/.claude.json.
# Both of these would otherwise leak into every Bash/Tool subprocess claude
# spawns (defence-in-depth — the --allowed-tools allowlist restricts which
# tools claude will invoke, but env hygiene still matters).
# CLAUDE_CODE_OAUTH_TOKEN / ANTHROPIC_API_KEY are intentionally preserved:
# the Claude CLI reads them at startup, and removing them here breaks auth in
# the env-fallback path.
unset CM_GIT_TOKEN CM_MCP_API_KEY

# ----- Task skills (filesystem-mounted Claude Code skills) -----
# shellcheck source=docker/entrypoint-skills.sh
. "$(dirname "$0")/entrypoint-skills.sh"

# Space-separated allowlist passed via a single --allowed-tools flag, per
# `claude --help`: "--allowedTools, --allowed-tools <tools...>  Comma or
# space-separated list of tool names to allow (e.g. \"Bash(git *) Edit\")".
# Four-way dispatch:
#   1. chat (CM_CHAT_SESSION set) — non-card-bound interactive session.
#   2. knowledge-refresh — KB refresh mode with its own tool allowlist and prompt.
#   3. HITL (CM_INTERACTIVE=1) — common list only; sub-agents excluded.
#   4. autonomous (default) — common list + Task sub-agent spawning.
if [ -n "${CM_CHAT_SESSION:-}" ]; then
    # Chat mode — non-card-bound interactive session.
    # /home/user/workspace is already created+cd'd above; chat sub-clones land
    # underneath it. Using a path under $HOME keeps writes inside the non-root
    # user's writable tree (the container can't mkdir /workspace at the root).
    if [ -n "${CM_CHAT_REPO_URL:-}" ] && [ -n "${CM_CHAT_PROJECT:-}" ]; then
        if ! git clone --depth=1 -- "$CM_CHAT_REPO_URL" "/home/user/workspace/$CM_CHAT_PROJECT"; then
            echo "[entrypoint] initial clone failed" >&2
            exit 1
        fi
        cd "/home/user/workspace/$CM_CHAT_PROJECT" || exit 1
    fi

    ALLOWED_TOOLS_CHAT=("${ALLOWED_TOOLS_COMMON[@]}" "${ALLOWED_TOOLS_AUTO_EXTRAS[@]}")

    # When CM signals a rehydration phase, the runner side primes the agent
    # via stdin AFTER attach (see runner internal/webhook/chat.go) — the -p
    # positional prompt is ignored by Claude in --input-format stream-json
    # mode, so the rehydration instructions have to come over stdin. We
    # still log that the rehydration file is present so an operator
    # debugging the container can see it landed correctly.
    if [ "${CM_CHAT_RESUME:-0}" = "1" ]; then
        if [ -r /run/cm-chat/resume.jsonl ]; then
            echo "[entrypoint] rehydration payload detected at /run/cm-chat/resume.jsonl" >&2
        else
            echo "[entrypoint] WARN: CM_CHAT_RESUME=1 but /run/cm-chat/resume.jsonl is missing or unreadable; runner-side priming may fail" >&2
        fi
    fi

    echo "Starting Claude Code in chat mode (session ${CM_CHAT_SESSION})..."
    exec claude -p \
        --thinking adaptive --thinking-display summarized \
        --model "${CM_ORCHESTRATOR_MODEL:-claude-sonnet-4-6}" \
        --input-format stream-json \
        --output-format stream-json \
        --verbose --allowed-tools "${ALLOWED_TOOLS_CHAT[*]}" \
        --permission-prompt-tool mcp__contextmatrix__permission_prompt \
        -- \
        "Chat session ${CM_CHAT_SESSION}. Wait for the operator's first message."
elif [ "${CM_MODE:-}" = "knowledge-refresh" ]; then
    ALLOWED_TOOLS_KB=("${ALLOWED_TOOLS_COMMON[@]}"
        "Task"
        "mcp__contextmatrix__refresh_knowledge_base"
        "mcp__contextmatrix__commit_knowledge_docs"
        "mcp__contextmatrix__update_refresh_progress")

    echo "Starting Claude Code in knowledge-refresh mode for ${CM_PROJECT}/${CM_KB_REPO}..."
    exec claude -p \
        --thinking adaptive --thinking-display summarized \
        --model "${CM_ORCHESTRATOR_MODEL:-claude-sonnet-4-6}" \
        --output-format stream-json --verbose \
        --allowed-tools "${ALLOWED_TOOLS_KB[*]}" \
        --permission-prompt-tool mcp__contextmatrix__permission_prompt \
        -- \
        "You are running inside a contextmatrix-runner container in knowledge-refresh mode.

Steps:
1. Call get_skill(skill_name='refresh-knowledge', caller_model='sonnet')
2. Follow the returned skill instructions exactly.
   - project: ${CM_PROJECT}
   - repo: ${CM_KB_REPO}
   - target repo working tree: /home/user/workspace (already cloned)
   - confirmed overwrite_docs: ${CM_KB_OVERWRITE_DOCS}
   - agent_id for all MCP calls: ${CM_AGENT_ID}

IMPORTANT:
- Always use MCP tools for ContextMatrix interactions.
- Do not modify the target repo working tree."
elif [ "${CM_INTERACTIVE:-}" = "1" ]; then
    ALLOWED_TOOLS_HITL=("${ALLOWED_TOOLS_COMMON[@]}")
    echo "Starting Claude Code for card ${CM_CARD_ID}..."
    # `--` terminates option parsing. Without it, claude's variadic
    # `--allowed-tools <tools...>` greedily consumes the following positional
    # prompt as yet another allowed-tool entry and exits with
    # "Input must be provided either through stdin or as a prompt argument".
    exec claude -p \
        --thinking adaptive --thinking-display summarized \
        --model "${CM_ORCHESTRATOR_MODEL:-claude-sonnet-4-6}" \
        --input-format stream-json \
        --output-format stream-json \
        --verbose --allowed-tools "${ALLOWED_TOOLS_HITL[*]}" \
        --permission-prompt-tool mcp__contextmatrix__permission_prompt \
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
    echo "Starting Claude Code for card ${CM_CARD_ID}..."
    # See HITL branch above for why `--` is required before the prompt.
    exec claude -p \
        --thinking adaptive --thinking-display summarized \
        --model "${CM_ORCHESTRATOR_MODEL:-claude-sonnet-4-6}" --output-format stream-json --verbose --allowed-tools "${ALLOWED_TOOLS_AUTO[*]}" \
        --permission-prompt-tool mcp__contextmatrix__permission_prompt \
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
