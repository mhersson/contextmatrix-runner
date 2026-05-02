package claudeclient

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
)

// ExecAPI abstracts the Docker exec surface used by Wrapper. A real
// Docker-backed implementation lives in a separate task (Spawner); tests
// inject a fake implementation.
type ExecAPI interface {
	ExecCreate(ctx context.Context, container string, cfg ExecConfig) (string, error)
	ExecAttach(ctx context.Context, execID string) (io.ReadCloser, io.WriteCloser, error)
}

// ExecConfig describes one CC exec to spawn inside an existing container.
type ExecConfig struct {
	Cmd          []string
	Env          []string
	WorkingDir   string
	AttachStdin  bool
	AttachStdout bool
	AttachStderr bool
}

// Wrapper builds and spawns Claude Code subprocesses through an ExecAPI.
// A callback registered via SetEventCallback, if set, is invoked
// synchronously for every parsed StreamEvent emitted by the spawned
// process. The callback is stored via atomic.Pointer so it is safe to
// register or replace before or after Spawn returns; the per-process
// pump goroutine reads the current value on every event.
type Wrapper struct {
	api    ExecAPI
	logger *slog.Logger

	eventCallback atomic.Pointer[func(ev StreamEvent)]
}

// NewWrapperWithExecAPI constructs a Wrapper bound to the given ExecAPI.
// A nil logger is replaced with slog.Default().
func NewWrapperWithExecAPI(api ExecAPI, logger *slog.Logger) *Wrapper {
	if logger == nil {
		logger = slog.Default()
	}

	return &Wrapper{api: api, logger: logger}
}

// SetEventCallback stores a callback to be invoked for every parsed
// StreamEvent. Safe to call before or after Spawn. Pass nil to clear.
func (w *Wrapper) SetEventCallback(fn func(ev StreamEvent)) {
	if fn == nil {
		w.eventCallback.Store(nil)

		return
	}

	w.eventCallback.Store(&fn)
}

// callbackForCurrentRun returns the currently registered callback or
// nil. Used by the per-process pump goroutine on every event.
func (w *Wrapper) callbackForCurrentRun() func(ev StreamEvent) {
	p := w.eventCallback.Load()
	if p == nil {
		return nil
	}

	return *p
}

// SpawnOptions configures one CC invocation.
type SpawnOptions struct {
	Container    string
	SystemPrompt string
	Model        string
	AllowedTools []string
	WorkingDir   string
	Env          map[string]string
	Resume       string
}

// Spawn launches a Claude Code subprocess inside the named container and
// returns a Process for stdin/stdout interaction.
func (w *Wrapper) Spawn(ctx context.Context, opts SpawnOptions) (Process, error) {
	cmd := []string{
		"claude",
		"--print",
		"--verbose", // required by claude-code when --print + --output-format stream-json
		"--input-format", "stream-json",
		"--output-format", "stream-json",
	}

	if opts.SystemPrompt != "" {
		cmd = append(cmd, "--append-system-prompt", opts.SystemPrompt)
	}

	if opts.Model != "" {
		cmd = append(cmd, "--model", opts.Model)
	}

	if len(opts.AllowedTools) > 0 {
		cmd = append(cmd, "--allowed-tools", strings.Join(opts.AllowedTools, ","))
	}

	if opts.Resume != "" {
		cmd = append(cmd, "--resume", opts.Resume)
	}

	env := make([]string, 0, len(opts.Env))
	for k, v := range opts.Env {
		env = append(env, k+"="+v)
	}

	execID, err := w.api.ExecCreate(ctx, opts.Container, ExecConfig{
		Cmd:          cmd,
		Env:          env,
		WorkingDir:   opts.WorkingDir,
		AttachStdin:  true,
		AttachStdout: true,
		AttachStderr: true,
	})
	if err != nil {
		return nil, err
	}

	stdout, stdin, err := w.api.ExecAttach(ctx, execID)
	if err != nil {
		return nil, err
	}

	return newProcess(ctx, w, execID, stdout, stdin), nil
}
