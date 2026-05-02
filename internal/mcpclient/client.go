// Package mcpclient provides a runner-side MCP client that satisfies
// orchestrator.MCPClient. It speaks Streamable HTTP to ContextMatrix's
// MCP endpoint with Bearer auth.
package mcpclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mhersson/contextmatrix-runner/internal/orchestrator"
	"github.com/mhersson/contextmatrix-runner/internal/workspace"
)

// bearerTransport wraps an http.RoundTripper to inject Authorization on
// every request. The MCP SDK's StreamableClientTransport accepts a custom
// *http.Client, so the cleanest place to attach the bearer is the
// underlying client.Transport.
type bearerTransport struct {
	bearer string
	base   http.RoundTripper
}

func (t *bearerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.bearer != "" {
		req.Header.Set("Authorization", "Bearer "+t.bearer)
	}

	rt := t.base
	if rt == nil {
		rt = http.DefaultTransport
	}

	return rt.RoundTrip(req)
}

// Client is the runner-side MCP client. It implements
// orchestrator.MCPClient and is constructed once per /trigger payload
// using the per-card MCPAPIKey from the trigger payload.
type Client struct {
	session *mcp.ClientSession
}

// New connects to ContextMatrix's MCP endpoint at baseURL with the
// supplied bearer token. baseURL must include the "/mcp" path
// (e.g. "http://contextmatrix:8080/mcp").
func New(ctx context.Context, baseURL, bearer string) (*Client, error) {
	httpClient := &http.Client{
		Transport: &bearerTransport{bearer: bearer},
	}
	transport := &mcp.StreamableClientTransport{
		Endpoint:   baseURL,
		HTTPClient: httpClient,
	}
	cli := mcp.NewClient(&mcp.Implementation{
		Name:    "contextmatrix-runner",
		Version: "0.1",
	}, nil)

	sess, err := cli.Connect(ctx, transport, nil)
	if err != nil {
		return nil, fmt.Errorf("mcp connect: %w", err)
	}

	return &Client{session: sess}, nil
}

// Close shuts down the MCP client session.
func (c *Client) Close() error {
	if c.session == nil {
		return nil
	}

	return c.session.Close()
}

// Compile-time assertion that *Client satisfies orchestrator.MCPClient.
var _ orchestrator.MCPClient = (*Client)(nil)

// call invokes a tool by name with the provided arguments. When dst is
// non-nil the tool's TextContent[0] is JSON-decoded into dst. Returns an
// error when the tool result is flagged IsError or transport fails.
func (c *Client) call(ctx context.Context, name string, args map[string]any, dst any) error {
	res, err := c.session.CallTool(ctx, &mcp.CallToolParams{
		Name:      name,
		Arguments: args,
	})
	if err != nil {
		return fmt.Errorf("mcp tool %s: %w", name, err)
	}

	if res.IsError {
		if len(res.Content) > 0 {
			if tc, ok := res.Content[0].(*mcp.TextContent); ok {
				return fmt.Errorf("mcp tool %s: %s", name, tc.Text)
			}
		}

		return fmt.Errorf("mcp tool %s: error result", name)
	}

	if dst == nil {
		return nil
	}

	if len(res.Content) == 0 {
		return nil
	}

	tc, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		return fmt.Errorf("mcp tool %s: unexpected content type %T", name, res.Content[0])
	}

	if err := json.Unmarshal([]byte(tc.Text), dst); err != nil {
		return fmt.Errorf("mcp tool %s: decode result: %w", name, err)
	}

	return nil
}

// --- orchestrator.MCPClient implementation ---

// ClaimCard claims a card for an agent.
func (c *Client) ClaimCard(ctx context.Context, project, cardID, agentID string) error {
	return c.call(ctx, "claim_card", map[string]any{
		"project":  project,
		"card_id":  cardID,
		"agent_id": agentID,
	}, nil)
}

// cardWire is the over-the-wire shape we extract a Card from. CM's
// board.Card has many fields the orchestrator doesn't care about; we
// project just what we need so unknown fields are silently ignored.
type cardWire struct {
	ID                string            `json:"id"`
	Project           string            `json:"project"`
	Title             string            `json:"title"`
	Description       string            `json:"description"`
	Type              string            `json:"type"`
	State             string            `json:"state"`
	Autonomous        bool              `json:"autonomous"`
	Body              string            `json:"body"`
	Repos             []string          `json:"repos"`
	ChosenRepos       []string          `json:"chosen_repos"`
	BlockerCards      []string          `json:"blocker_cards"`
	BranchName        string            `json:"branch_name"`
	BaseBranch        string            `json:"base_branch"`
	RevisionAttempts  int               `json:"revision_attempts"`
	RevisionRequested bool              `json:"revision_requested"`
	PlanApproved      bool              `json:"plan_approved"`
	ReviewApproved    bool              `json:"review_approved"`
	DiscoveryComplete bool              `json:"discovery_complete"`
	AgentSessions     map[string]string `json:"agent_sessions"`
	DocsWritten       bool              `json:"docs_written"`
}

func (cw *cardWire) toCard() *orchestrator.Card {
	if cw == nil {
		return nil
	}

	return &orchestrator.Card{
		ID:                cw.ID,
		Project:           cw.Project,
		Title:             cw.Title,
		Description:       cw.Description,
		Type:              cw.Type,
		State:             cw.State,
		Autonomous:        cw.Autonomous,
		Body:              cw.Body,
		Repos:             cw.Repos,
		ChosenRepos:       cw.ChosenRepos,
		BlockerCards:      cw.BlockerCards,
		BranchName:        cw.BranchName,
		BaseBranch:        cw.BaseBranch,
		RevisionAttempts:  cw.RevisionAttempts,
		RevisionRequested: cw.RevisionRequested,
		PlanApproved:      cw.PlanApproved,
		ReviewApproved:    cw.ReviewApproved,
		DiscoveryComplete: cw.DiscoveryComplete,
		AgentSessions:     cw.AgentSessions,
		DocsWritten:       cw.DocsWritten,
	}
}

// projectConfigWire mirrors the small slice of board.ProjectConfig the
// orchestrator needs from get_task_context — currently the repo registry.
type projectConfigWire struct {
	Repos []struct {
		Slug        string `json:"slug"`
		URL         string `json:"url"`
		Description string `json:"description"`
	} `json:"repos"`
}

// taskContextWire is the JSON shape returned by the get_task_context
// tool (see CM's getTaskContextOutput).
type taskContextWire struct {
	Card     *cardWire          `json:"card"`
	Parent   *cardWire          `json:"parent,omitempty"`
	Siblings []*cardWire        `json:"siblings,omitempty"`
	Config   *projectConfigWire `json:"config,omitempty"`
}

// GetTaskContext fetches a card with its parent, siblings, and project config.
func (c *Client) GetTaskContext(ctx context.Context, project, cardID, agentID string) (*orchestrator.CardContext, error) {
	args := map[string]any{
		"project":  project,
		"card_id":  cardID,
		"agent_id": agentID,
	}

	var wire taskContextWire
	if err := c.call(ctx, "get_task_context", args, &wire); err != nil {
		return nil, err
	}

	out := &orchestrator.CardContext{
		Card:   wire.Card.toCard(),
		Parent: wire.Parent.toCard(),
	}

	for _, s := range wire.Siblings {
		out.Siblings = append(out.Siblings, s.toCard())
	}

	if wire.Config != nil {
		for _, r := range wire.Config.Repos {
			out.ProjectRepos = append(out.ProjectRepos, orchestrator.RepoSpec{
				Slug:        r.Slug,
				URL:         r.URL,
				Description: r.Description,
			})
		}
	}

	return out, nil
}

// UpdateCardBody updates a single section of the card body. CM's
// update_card tool takes the full body, so this implementation reads
// the current body, replaces the named section, and writes it back.
//
// HEURISTIC v1: this is a thin best-effort wrapper. The orchestrator
// currently uses this only for the brainstorming Design section.
func (c *Client) UpdateCardBody(ctx context.Context, project, cardID, sectionName, content string) error {
	// First read current body via get_task_context so we can splice the section.
	cc, err := c.GetTaskContext(ctx, project, cardID, "")
	if err != nil {
		return err
	}

	body := ""
	if cc.Card != nil {
		body = cc.Card.Body
	}

	newBody := replaceSection(body, sectionName, content)

	return c.call(ctx, "update_card", map[string]any{
		"project": project,
		"card_id": cardID,
		"body":    newBody,
	}, nil)
}

// UpdateCardField sends a field-only patch. The accepted field set is
// defined by CM's update_card MCP tool input (see
// contextmatrix/internal/mcp/tools.go: updateCardInput). The map is
// forwarded verbatim with project/card_id added; the MCP SDK's typed
// input dispatch will silently drop any keys not in the schema, so
// callers that need confirmation a field landed should re-read the
// card via GetTaskContext.
func (c *Client) UpdateCardField(ctx context.Context, project, cardID string, fields map[string]any) error {
	args := map[string]any{
		"project": project,
		"card_id": cardID,
	}

	for k, v := range fields {
		args[k] = v
	}

	return c.call(ctx, "update_card", args, nil)
}

// Heartbeat refreshes the card's last_heartbeat timestamp.
func (c *Client) Heartbeat(ctx context.Context, project, cardID, agentID string) error {
	return c.call(ctx, "heartbeat", map[string]any{
		"project":  project,
		"card_id":  cardID,
		"agent_id": agentID,
	}, nil)
}

// CompleteTask atomically transitions the card to its terminal state
// and releases the claim.
func (c *Client) CompleteTask(ctx context.Context, project, cardID, agentID, summary string) error {
	return c.call(ctx, "complete_task", map[string]any{
		"project":  project,
		"card_id":  cardID,
		"agent_id": agentID,
		"summary":  summary,
	}, nil)
}

// ReleaseCard releases the agent's claim on a card.
func (c *Client) ReleaseCard(ctx context.Context, project, cardID, agentID string) error {
	return c.call(ctx, "release_card", map[string]any{
		"project":  project,
		"card_id":  cardID,
		"agent_id": agentID,
	}, nil)
}

// AddLog appends an activity log entry.
func (c *Client) AddLog(ctx context.Context, project, cardID, agentID, action, message string) error {
	return c.call(ctx, "add_log", map[string]any{
		"project":  project,
		"card_id":  cardID,
		"agent_id": agentID,
		"action":   action,
		"message":  message,
	}, nil)
}

// ReportUsage records token usage for a card.
func (c *Client) ReportUsage(ctx context.Context, project, cardID, agentID string, prompt, completion int, model string) error {
	return c.call(ctx, "report_usage", map[string]any{
		"project":           project,
		"card_id":           cardID,
		"agent_id":          agentID,
		"prompt_tokens":     prompt,
		"completion_tokens": completion,
		"model":             model,
	}, nil)
}

// ReportPush records a completed git push.
func (c *Client) ReportPush(ctx context.Context, project, cardID, agentID, repo, branch, prURL string) error {
	return c.call(ctx, "report_push", map[string]any{
		"project":  project,
		"card_id":  cardID,
		"agent_id": agentID,
		"repo":     repo,
		"branch":   branch,
		"pr_url":   prURL,
	}, nil)
}

// TransitionCard changes a card's state.
func (c *Client) TransitionCard(ctx context.Context, project, cardID, agentID, toState string) error {
	return c.call(ctx, "transition_card", map[string]any{
		"project":   project,
		"card_id":   cardID,
		"agent_id":  agentID,
		"new_state": toState,
	}, nil)
}

// CreateCard creates a new card and returns its server-generated ID.
func (c *Client) CreateCard(ctx context.Context, project string, in orchestrator.CreateCardInput) (string, error) {
	args := map[string]any{
		"project":  project,
		"title":    in.Title,
		"type":     in.Type,
		"priority": in.Priority,
	}
	if in.Parent != "" {
		args["parent"] = in.Parent
	}

	if in.Description != "" {
		args["body"] = in.Description
	}

	// CM's create_card maps `depends_on` onto the new card so the UI
	// shows blocked/deps-met badges. Optional on the CM side; only
	// include when set so we don't overwrite anything CM might default.
	if len(in.DependsOn) > 0 {
		args["depends_on"] = in.DependsOn
	}

	// `repos` is intentionally NOT sent: CM's create_card MCP tool has
	// strict-schema validation and rejects unknown properties with
	// "unexpected additional property". The orchestrator keeps Repos
	// in its in-memory Plan.Subtasks (which drives CloneRepo and
	// CreateWorktree), so dropping it from the persisted card costs
	// only UI visibility ("which repos does this subtask touch?") —
	// not orchestration correctness. When CM's createCardInput grows a
	// repos field, add `args["repos"] = in.Repos` back.

	var wire cardWire
	if err := c.call(ctx, "create_card", args, &wire); err != nil {
		return "", err
	}

	return wire.ID, nil
}

// GetProjectKB fetches the tiered project knowledge base.
func (c *Client) GetProjectKB(ctx context.Context, project string, repoSlug ...string) (orchestrator.ProjectKB, error) {
	args := map[string]any{
		"project": project,
	}
	if len(repoSlug) > 0 && repoSlug[0] != "" {
		args["repo_slug"] = repoSlug[0]
	}

	var wire struct {
		Repos       map[string]string `json:"repos"`
		JiraProject string            `json:"jira_project"`
		Project     string            `json:"project"`
	}

	if err := c.call(ctx, "get_project_kb", args, &wire); err != nil {
		return orchestrator.ProjectKB{}, err
	}

	return orchestrator.ProjectKB{
		Repos:       wire.Repos,
		JiraProject: wire.JiraProject,
		Project:     wire.Project,
	}, nil
}

// replaceSection rewrites the named ## <name> section of a markdown
// body, preserving all other content. If the section does not exist the
// new section is appended at the end. Used by UpdateCardBody.
func replaceSection(body, name, content string) string {
	header := "## " + name
	lines := splitLines(body)

	var (
		out      []string
		inSec    bool
		replaced bool
	)

	for _, line := range lines {
		if !inSec {
			if line == header || (len(line) >= len(header)+1 && line[:len(header)+1] == header+" ") {
				out = append(out, header)
				out = append(out, "")
				out = append(out, content)

				inSec = true
				replaced = true

				continue
			}

			out = append(out, line)

			continue
		}
		// inSec == true
		if len(line) >= 3 && line[:3] == "## " {
			inSec = false

			out = append(out, line)
		}
		// drop lines that belonged to the old section
	}

	if !replaced {
		out = append(out, "", header, "", content)
	}

	return joinLines(out)
}

func splitLines(s string) []string {
	if s == "" {
		return nil
	}

	var (
		out  []string
		last int
	)

	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			out = append(out, s[last:i])
			last = i + 1
		}
	}

	if last < len(s) {
		out = append(out, s[last:])
	}

	return out
}

func joinLines(lines []string) string {
	if len(lines) == 0 {
		return ""
	}

	n := len(lines) - 1
	for _, l := range lines {
		n += len(l)
	}

	out := make([]byte, 0, n)

	for i, l := range lines {
		if i > 0 {
			out = append(out, '\n')
		}

		out = append(out, l...)
	}

	return string(out)
}

// Compile-time confirmation that workspace.RepoSpec is reachable from
// this package even when no method directly references it. The
// orchestrator interface signature ties workspace types into the
// reachable set without an explicit field; this no-op assertion makes
// the dependency visible to readers and the build.
var _ workspace.RepoSpec
