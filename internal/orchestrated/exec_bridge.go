package orchestrated

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/mhersson/contextmatrix-runner/internal/claudeclient"
	"github.com/mhersson/contextmatrix-runner/internal/spawn"
)

// workerExecAPI adapts a spawn.Worker to claudeclient.ExecAPI.
//
// claudeclient.ExecAPI splits the lifecycle into ExecCreate (build the
// command) and ExecAttach (start streaming). spawn.Worker's ExecStream
// does both atomically. We bridge the two by stashing the per-create
// options keyed by a synthetic execID and starting the actual stream
// only on ExecAttach.
type workerExecAPI struct {
	worker  spawn.Worker
	logger  *slog.Logger
	counter int64

	mu      sync.Mutex
	pending map[string]spawn.ExecOptions
}

func newWorkerExecAPI(w spawn.Worker, logger *slog.Logger) *workerExecAPI {
	if logger == nil {
		logger = slog.Default()
	}

	return &workerExecAPI{
		worker:  w,
		logger:  logger,
		pending: make(map[string]spawn.ExecOptions),
	}
}

// ExecCreate stashes the requested command and returns a synthetic execID.
func (a *workerExecAPI) ExecCreate(_ context.Context, _ string, cfg claudeclient.ExecConfig) (string, error) {
	n := atomic.AddInt64(&a.counter, 1)
	id := fmt.Sprintf("exec-%d", n)

	a.mu.Lock()
	a.pending[id] = spawn.ExecOptions{
		Cmd:          cfg.Cmd,
		Env:          envSliceToMap(cfg.Env),
		WorkingDir:   cfg.WorkingDir,
		AttachStdin:  cfg.AttachStdin,
		AttachStdout: cfg.AttachStdout,
		AttachStderr: cfg.AttachStderr,
	}
	a.mu.Unlock()

	return id, nil
}

// ExecAttach pulls the stashed options and starts a live ExecStream.
func (a *workerExecAPI) ExecAttach(ctx context.Context, execID string) (io.ReadCloser, io.WriteCloser, error) {
	a.mu.Lock()

	opts, ok := a.pending[execID]
	if !ok {
		a.mu.Unlock()

		return nil, nil, fmt.Errorf("workerExecAPI: unknown execID %q", execID)
	}

	delete(a.pending, execID)
	a.mu.Unlock()

	a.logger.Info("workerExec attach",
		"exec_id", execID,
		"worker", a.worker.ID(),
		"cmd", strings.Join(opts.Cmd, " "),
		"working_dir", opts.WorkingDir,
		"env_keys", mapKeys(opts.Env),
	)

	stream, err := a.worker.ExecStream(ctx, opts)
	if err != nil {
		a.logger.Warn("workerExec ExecStream failed", "exec_id", execID, "err", err)

		return nil, nil, err
	}

	return stream.Stdout(), stream.Stdin(), nil
}

func mapKeys(m map[string]string) []string {
	if len(m) == 0 {
		return nil
	}

	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}

	return out
}

// envSliceToMap converts ["KEY=VALUE", ...] back into a map. claudeclient
// builds the slice from a map originally; this round-trip is harmless.
func envSliceToMap(env []string) map[string]string {
	if len(env) == 0 {
		return nil
	}

	out := make(map[string]string, len(env))

	for _, kv := range env {
		for i := 0; i < len(kv); i++ {
			if kv[i] == '=' {
				out[kv[:i]] = kv[i+1:]

				break
			}
		}
	}

	return out
}

// workspaceExec adapts spawn.Worker to workspace.Exec for git/gh
// commands and injects a freshly-minted GitHub token as CM_GIT_TOKEN /
// GH_TOKEN env on every exec.
//
// Per-exec injection (not Config.Env, not a startup-time on-disk file)
// is what lets long-running cards stay authenticated past the GitHub
// App installation token's 1-hour TTL: the runner host holds a
// CachingProvider, every Exec asks it for a token, and the cache mints
// fresh transparently when within the refresh skew of expiry.
//
// tokenSource is optional; when nil (e.g. unit tests) Exec runs with no
// token env, which leaves git operations unauthenticated. Public-repo
// clones still work; private/git-push/gh-pr-create surface a clear
// "auth required" error from the underlying tool.
type workspaceExec struct {
	worker      spawn.Worker
	tokenSource func(context.Context) (string, error)
}

func newWorkspaceExec(w spawn.Worker, tokenSource func(context.Context) (string, error)) *workspaceExec {
	return &workspaceExec{worker: w, tokenSource: tokenSource}
}

func (e *workspaceExec) Exec(ctx context.Context, cmd []string) (string, string, error) {
	var env map[string]string

	if e.tokenSource != nil {
		tok, err := e.tokenSource(ctx)
		if err != nil {
			return "", "", fmt.Errorf("mint github token: %w", err)
		}

		if tok != "" {
			env = map[string]string{
				"CM_GIT_TOKEN": tok,
				"GH_TOKEN":     tok,
			}
		}
	}

	res, err := e.worker.Exec(ctx, spawn.ExecOptions{
		Cmd:          cmd,
		Env:          env,
		AttachStdout: true,
		AttachStderr: true,
	})
	if err != nil {
		return "", "", err
	}

	if res.ExitCode != 0 {
		return res.Stdout, res.Stderr, fmt.Errorf("exit %d", res.ExitCode)
	}

	return res.Stdout, res.Stderr, nil
}
