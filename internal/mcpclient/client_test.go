package mcpclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mhersson/contextmatrix-runner/internal/orchestrator"
)

const testBearer = "test-bearer-key-32-characters-long"

// setupServer registers a handful of mock tools that mirror the shapes
// CM exposes, then wraps the StreamableHTTPHandler in a Bearer-checking
// httptest server. Returns the server URL and a cleanup func.
func setupServer(t *testing.T, register func(*mcp.Server), seenAuth *[]string) string {
	t.Helper()

	server := mcp.NewServer(&mcp.Implementation{Name: "test-cm", Version: "0.1"}, nil)
	register(server)

	handler := mcp.NewStreamableHTTPHandler(
		func(_ *http.Request) *mcp.Server { return server },
		&mcp.StreamableHTTPOptions{DisableLocalhostProtection: true},
	)

	var mu sync.Mutex

	httpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		if seenAuth != nil {
			*seenAuth = append(*seenAuth, r.Header.Get("Authorization"))
		}
		mu.Unlock()
		handler.ServeHTTP(w, r)
	}))

	t.Cleanup(httpSrv.Close)

	return httpSrv.URL
}

func TestClient_BearerInjected(t *testing.T) {
	var seen []string

	url := setupServer(t, func(server *mcp.Server) {
		mcp.AddTool(server, &mcp.Tool{Name: "ping"}, func(_ context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: "ok"}},
			}, nil, nil
		})
	}, &seen)

	ctx := t.Context()
	cli, err := New(ctx, url, testBearer)
	require.NoError(t, err)

	defer func() { _ = cli.Close() }()

	require.NoError(t, cli.call(ctx, "ping", nil, nil))

	require.NotEmpty(t, seen)

	// Every request must include the Bearer header.
	for _, h := range seen {
		assert.Equal(t, "Bearer "+testBearer, h, "expected Bearer header on every request")
	}
}

func TestClient_ClaimCard(t *testing.T) {
	url := setupServer(t, func(server *mcp.Server) {
		mcp.AddTool(server, &mcp.Tool{Name: "claim_card"}, func(_ context.Context, _ *mcp.CallToolRequest, args map[string]any) (*mcp.CallToolResult, any, error) {
			if args["card_id"] != "PROJ-001" || args["agent_id"] != "agent-A" {
				return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: "bad args"}}}, nil, nil
			}

			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "{}"}}}, nil, nil
		})
	}, nil)

	cli, err := New(t.Context(), url, testBearer)
	require.NoError(t, err)

	defer func() { _ = cli.Close() }()

	require.NoError(t, cli.ClaimCard(t.Context(), "p1", "PROJ-001", "agent-A"))
}

func TestClient_GetTaskContext(t *testing.T) {
	url := setupServer(t, func(server *mcp.Server) {
		mcp.AddTool(server, &mcp.Tool{Name: "get_task_context"}, func(_ context.Context, _ *mcp.CallToolRequest, _ map[string]any) (*mcp.CallToolResult, any, error) {
			body := map[string]any{
				"card": map[string]any{
					"id":      "PROJ-001",
					"project": "p1",
					"title":   "Hello world",
					"state":   "in_progress",
				},
				"config": map[string]any{
					"repos": []map[string]string{
						{"slug": "r1", "url": "https://example.com/r1.git", "description": "primary"},
					},
				},
			}
			b, _ := json.Marshal(body)

			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(b)}}}, nil, nil
		})
	}, nil)

	cli, err := New(t.Context(), url, testBearer)
	require.NoError(t, err)

	defer func() { _ = cli.Close() }()

	cc, err := cli.GetTaskContext(t.Context(), "p1", "PROJ-001", "agent-A")
	require.NoError(t, err)
	require.NotNil(t, cc.Card)
	assert.Equal(t, "PROJ-001", cc.Card.ID)
	assert.Equal(t, "Hello world", cc.Card.Title)
	require.Len(t, cc.ProjectRepos, 1)
	assert.Equal(t, "r1", cc.ProjectRepos[0].Slug)
}

func TestClient_CreateCard(t *testing.T) {
	url := setupServer(t, func(server *mcp.Server) {
		mcp.AddTool(server, &mcp.Tool{Name: "create_card"}, func(_ context.Context, _ *mcp.CallToolRequest, args map[string]any) (*mcp.CallToolResult, any, error) {
			if args["title"] != "subtask one" || args["parent"] != "PROJ-001" {
				return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: "bad args"}}}, nil, nil
			}

			b, _ := json.Marshal(map[string]any{"id": "PROJ-002"})

			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(b)}}}, nil, nil
		})
	}, nil)

	cli, err := New(t.Context(), url, testBearer)
	require.NoError(t, err)

	defer func() { _ = cli.Close() }()

	id, err := cli.CreateCard(t.Context(), "p1", orchestrator.CreateCardInput{
		Title:    "subtask one",
		Type:     "task",
		Parent:   "PROJ-001",
		Priority: "medium",
	})
	require.NoError(t, err)
	assert.Equal(t, "PROJ-002", id)
}

func TestClient_GetProjectKB(t *testing.T) {
	url := setupServer(t, func(server *mcp.Server) {
		mcp.AddTool(server, &mcp.Tool{Name: "get_project_kb"}, func(_ context.Context, _ *mcp.CallToolRequest, args map[string]any) (*mcp.CallToolResult, any, error) {
			out := map[string]any{
				"repos":        map[string]string{"r1": "notes"},
				"jira_project": "jira",
				"project":      "p1 notes",
			}

			if args["repo_slug"] == "r1" {
				out["repos"] = map[string]string{"r1": "filtered"}
			}

			b, _ := json.Marshal(out)

			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(b)}}}, nil, nil
		})
	}, nil)

	cli, err := New(t.Context(), url, testBearer)
	require.NoError(t, err)

	defer func() { _ = cli.Close() }()

	kb, err := cli.GetProjectKB(t.Context(), "p1")
	require.NoError(t, err)
	assert.Equal(t, "jira", kb.JiraProject)
	assert.Equal(t, "notes", kb.Repos["r1"])

	kb, err = cli.GetProjectKB(t.Context(), "p1", "r1")
	require.NoError(t, err)
	assert.Equal(t, "filtered", kb.Repos["r1"])
}

func TestClient_ErrorResultPropagated(t *testing.T) {
	url := setupServer(t, func(server *mcp.Server) {
		mcp.AddTool(server, &mcp.Tool{Name: "claim_card"}, func(_ context.Context, _ *mcp.CallToolRequest, _ map[string]any) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: "already claimed"}}}, nil, nil
		})
	}, nil)

	cli, err := New(t.Context(), url, testBearer)
	require.NoError(t, err)

	defer func() { _ = cli.Close() }()

	err = cli.ClaimCard(t.Context(), "p1", "PROJ-001", "agent-A")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "already claimed")
}

func TestReplaceSection(t *testing.T) {
	body := "## Plan\n\nold plan\n\n## Notes\n\nfoo\n"
	got := replaceSection(body, "Plan", "new plan")

	assert.Contains(t, got, "## Plan")
	assert.Contains(t, got, "new plan")
	assert.NotContains(t, got, "old plan")
	assert.Contains(t, got, "## Notes")
	assert.Contains(t, got, "foo")
}

func TestReplaceSection_AppendsWhenMissing(t *testing.T) {
	body := "## Notes\n\nfoo\n"
	got := replaceSection(body, "Design", "design body")

	assert.Contains(t, got, "## Notes")
	assert.Contains(t, got, "## Design")
	assert.Contains(t, got, "design body")
}
