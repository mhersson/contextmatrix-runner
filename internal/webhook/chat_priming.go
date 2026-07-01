package webhook

import "fmt"

// buildChatRehydrationPriming returns the stream-json user-message text that
// is written to the chat container's stdin immediately after attach when a
// rehydration payload is present. Claude executes this as a normal user
// message — which means it actually runs (unlike the -p positional prompt
// in stream-json input mode, which the model treats as system context and
// does not auto-execute). The instructions tell the agent to read the
// mounted resume.jsonl, re-establish workspace state, and call the
// chat_rehydration_complete MCP tool when done.
//
// All rehydration rules live in this function.
func buildChatRehydrationPriming(sessionID string) string {
	return fmt.Sprintf(`You are resuming chat session %s in a FRESH container. The
prior conversation is in /run/cm-chat/resume.jsonl (one JSON object per line,
role + content, chronological). Metadata: /run/cm-chat/resume.meta.json — if
"clipped":true, older turns were dropped to fit the context budget; only the
first user turn and the last 20 turns are guaranteed present.

CRITICAL CONTEXT: this is a brand-new container. Everything the prior agent
created on disk (cloned repos, scratch files, branch checkouts) is GONE —
the workspace at /home/user/workspace is empty unless this entrypoint cloned
something automatically. Do NOT assume any prior workspace state persists.
Always verify disk state with a command before deciding whether to act.

Before greeting the user, do these things in order:

1. Read /run/cm-chat/resume.jsonl in full. Also read /run/cm-chat/resume.meta.json.

2. Run %s/home/user/workspace/%s to see what is ACTUALLY on disk right now.
   Use the result, not the transcript, as the source of truth for workspace
   state.

3. Re-establish workspace state required to continue. For every "tool_call"
   entry in the transcript whose content is a "git clone <URL> <PATH>"
   command (typically run by Bash):
   - If the target path is NOT on disk (per step 2), re-run the clone now.
   - If the target path IS on disk, skip the clone.
   Also restore any working directory and checked-out branches implied by
   the transcript.

4. Do NOT replay destructive or externally-visible actions (deploys, API
   writes, package publishes, mutations to remote services). If unsure
   whether something is safe, skip it and say so.

5. Keep restoration narration MINIMAL — the operator's UI will collapse
   your restoration tool_calls into a single block. Do not write a long
   prose recap of every step.

6. When restoration is complete, call the MCP tool
       mcp__contextmatrix__chat_rehydration_complete(session_id=%q, summary="...")
   passing a one-paragraph summary of where you and the operator left off
   (e.g. "Picking up where we left off — we were debugging the OAuth flow
   in foo. I have re-cloned foo and bar."). This call is MANDATORY: it
   ends the "Restoring workspace…" banner in the UI and your summary
   becomes the first visible message of the resumed chat. The tool is in
   the allowlist; do NOT fall back to plain text if you are unsure — call
   the tool.

If the resume file is missing or unreadable, say so and start fresh
(no tool call required in that case).`, sessionID, "`ls -la ", "`", sessionID)
}
