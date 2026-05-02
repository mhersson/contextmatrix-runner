package docker

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/network"
	dockerclient "github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/mhersson/contextmatrix-runner/internal/spawn"
)

// Client is the small SDK surface this package depends on.
// Production callers use *client.Client; tests use a fake.
type Client interface {
	ContainerCreate(ctx context.Context, cfg *container.Config, host *container.HostConfig, net *network.NetworkingConfig, platform *ocispec.Platform, name string) (container.CreateResponse, error)
	ContainerStart(ctx context.Context, id string, opts container.StartOptions) error
	ContainerStop(ctx context.Context, id string, opts container.StopOptions) error
	ContainerRemove(ctx context.Context, id string, opts container.RemoveOptions) error
	ContainerExecCreate(ctx context.Context, id string, opts container.ExecOptions) (container.ExecCreateResponse, error)
	ContainerExecAttach(ctx context.Context, execID string, opts container.ExecAttachOptions) (types.HijackedResponse, error)
	ContainerExecInspect(ctx context.Context, execID string) (container.ExecInspect, error)
}

// Spawner is the Docker-backed implementation.
type Spawner struct {
	cli    Client
	logger *slog.Logger
}

// NewSpawner constructs a Spawner using a real Docker SDK client.
func NewSpawner(logger *slog.Logger) (*Spawner, error) {
	cli, err := dockerclient.NewClientWithOpts(dockerclient.FromEnv, dockerclient.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("docker client: %w", err)
	}

	if logger == nil {
		logger = slog.Default()
	}

	return &Spawner{cli: cli, logger: logger}, nil
}

// NewSpawnerWithClient is the test constructor.
func NewSpawnerWithClient(cli Client) *Spawner {
	return &Spawner{cli: cli, logger: slog.Default()}
}

func (s *Spawner) Spawn(ctx context.Context, spec spawn.WorkerSpec) (spawn.Worker, error) {
	cfg := &container.Config{
		Image:  spec.Image,
		Env:    envSlice(spec.Env),
		Labels: spec.Labels,
	}

	host := &container.HostConfig{
		Mounts:      mountsToDocker(spec.Mounts),
		NetworkMode: container.NetworkMode(spec.NetworkMode),
		ExtraHosts:  spec.ExtraHosts,
		// Security hardening: the dispatcher populates these to drop
		// every ambient Linux capability, refuse setuid escalation, and
		// freeze the container rootfs. Zero values stay at Docker
		// defaults so call sites that don't opt in still work.
		CapDrop:        spec.Security.CapDrop,
		SecurityOpt:    spec.Security.SecurityOpt,
		ReadonlyRootfs: spec.Security.ReadonlyRootfs,
		Tmpfs:          tmpfsMap(spec.Tmpfs),
	}
	if spec.Resources.MemoryBytes > 0 {
		host.Memory = spec.Resources.MemoryBytes
	}

	if spec.Resources.CPUShares > 0 {
		host.CPUShares = spec.Resources.CPUShares
	}

	if spec.Resources.PIDs > 0 {
		pids := spec.Resources.PIDs
		host.PidsLimit = &pids
	}

	resp, err := s.cli.ContainerCreate(ctx, cfg, host, nil, nil, spec.Name)
	if err != nil {
		return nil, fmt.Errorf("container create: %w", err)
	}

	if err := s.cli.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		return nil, fmt.Errorf("container start: %w", err)
	}

	return &dockerWorker{
		cli:    s.cli,
		id:     resp.ID,
		logger: s.logger,
		status: spawn.WorkerRunning,
	}, nil
}

type dockerWorker struct {
	cli    Client
	id     string
	logger *slog.Logger
	status spawn.WorkerStatus
}

func (w *dockerWorker) ID() string                 { return w.id }
func (w *dockerWorker) Status() spawn.WorkerStatus { return w.status }

func (w *dockerWorker) Exec(ctx context.Context, opts spawn.ExecOptions) (spawn.ExecResult, error) {
	execID, err := w.cli.ContainerExecCreate(ctx, w.id, container.ExecOptions{
		Cmd:          opts.Cmd,
		Env:          envSlice(opts.Env),
		WorkingDir:   opts.WorkingDir,
		AttachStdout: true,
		AttachStderr: true,
	})
	if err != nil {
		return spawn.ExecResult{}, fmt.Errorf("exec create: %w", err)
	}

	attach, err := w.cli.ContainerExecAttach(ctx, execID.ID, container.ExecAttachOptions{})
	if err != nil {
		return spawn.ExecResult{}, fmt.Errorf("exec attach: %w", err)
	}
	defer attach.Close()

	// Docker exec without TTY returns a multiplexed stream (8-byte frame
	// headers + interleaved stdout/stderr payload). Reading the
	// hijacked connection with io.ReadAll produces the raw multiplex
	// bytes — including those headers — which then leak into the
	// caller's parsed stdout/stderr. Demultiplex with stdcopy.StdCopy.
	var stdout, stderr bytes.Buffer
	if _, copyErr := stdcopy.StdCopy(&stdout, &stderr, attach.Reader); copyErr != nil {
		return spawn.ExecResult{}, fmt.Errorf("exec demux: %w", copyErr)
	}

	inspect, err := w.cli.ContainerExecInspect(ctx, execID.ID)
	if err != nil {
		return spawn.ExecResult{}, fmt.Errorf("exec inspect: %w", err)
	}

	return spawn.ExecResult{
		ExitCode: inspect.ExitCode,
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
	}, nil
}

func (w *dockerWorker) ExecStream(ctx context.Context, opts spawn.ExecOptions) (spawn.ExecStream, error) {
	execID, err := w.cli.ContainerExecCreate(ctx, w.id, container.ExecOptions{
		Cmd:          opts.Cmd,
		Env:          envSlice(opts.Env),
		WorkingDir:   opts.WorkingDir,
		AttachStdin:  opts.AttachStdin,
		AttachStdout: true,
		AttachStderr: true,
	})
	if err != nil {
		return nil, fmt.Errorf("exec create: %w", err)
	}

	attach, err := w.cli.ContainerExecAttach(ctx, execID.ID, container.ExecAttachOptions{})
	if err != nil {
		return nil, fmt.Errorf("exec attach: %w", err)
	}

	// Docker exec without TTY returns a multiplexed stream (8-byte frame
	// headers + interleaved stdout/stderr). The orchestrator's stream-json
	// parser needs raw stdout, so demultiplex via stdcopy.StdCopy into a
	// pair of pipes. Stderr is forwarded to the logger so Claude's error
	// output is visible.
	//
	// Each pipe is wrapped in a previewWriter that buffers the first ~2 KiB
	// of bytes so we can log a preview when the exec ends. Without this we
	// cannot tell whether the consumer is missing valid stream-json frames
	// or whether Claude is producing unparseable output.
	stdoutR, stdoutWPipe := io.Pipe()
	stderrR, stderrWPipe := io.Pipe()

	stdoutW := &previewWriter{w: stdoutWPipe, kind: "stdout", logger: w.logger, execID: execID.ID}
	stderrW := &previewWriter{w: stderrWPipe, kind: "stderr", logger: w.logger, execID: execID.ID}

	go func() {
		w.logger.Info("exec stdcopy starting", "exec_id", execID.ID)

		n, copyErr := stdcopy.StdCopy(stdoutW, stderrW, attach.Reader)
		_ = stdoutWPipe.Close()
		_ = stderrWPipe.Close()

		stdoutW.logPreview()
		stderrW.logPreview()

		w.logger.Info("exec stdcopy ended",
			"exec_id", execID.ID,
			"bytes_copied", n,
			"stdout_bytes", stdoutW.total,
			"stderr_bytes", stderrW.total,
			"err", copyErr,
		)
	}()

	go func() {
		// Drain stderr line-by-line into the logger.
		sc := bufio.NewScanner(stderrR)
		sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

		for sc.Scan() {
			w.logger.Warn("exec stderr", "line", sc.Text())
		}
	}()

	return &execStream{
		cli:    w.cli,
		execID: execID.ID,
		attach: attach,
		stdout: stdoutR,
		logger: w.logger,
	}, nil
}

func (w *dockerWorker) Stop(ctx context.Context) error {
	if err := w.cli.ContainerStop(ctx, w.id, container.StopOptions{}); err != nil {
		return err
	}

	w.status = spawn.WorkerStopped

	return nil
}

func (w *dockerWorker) Remove(ctx context.Context) error {
	if err := w.cli.ContainerRemove(ctx, w.id, container.RemoveOptions{Force: true}); err != nil {
		return err
	}

	w.status = spawn.WorkerRemoved

	return nil
}

type execStream struct {
	cli    Client
	execID string
	attach types.HijackedResponse
	stdout io.ReadCloser // demultiplexed stdout pipe
	logger *slog.Logger
}

func (s *execStream) Stdout() io.ReadCloser { return s.stdout }
func (s *execStream) Stdin() io.WriteCloser {
	return &hijackStdin{conn: s.attach.Conn, halfClose: s.attach.CloseWrite}
}

func (s *execStream) Wait(ctx context.Context) (int, error) {
	inspect, err := s.cli.ContainerExecInspect(ctx, s.execID)
	if err != nil {
		return 0, err
	}

	return inspect.ExitCode, nil
}

func (s *execStream) Kill() error {
	s.attach.Close()

	return nil
}

// previewWriter wraps an io.Writer and captures the first 2 KiB of bytes
// written through it for diagnostic logging when the exec finishes.
type previewWriter struct {
	w       io.Writer
	kind    string // "stdout" | "stderr"
	logger  *slog.Logger
	execID  string
	preview []byte
	total   int64
}

const previewLimit = 2048

func (p *previewWriter) Write(b []byte) (int, error) {
	if len(p.preview) < previewLimit {
		room := previewLimit - len(p.preview)
		if room > len(b) {
			room = len(b)
		}

		p.preview = append(p.preview, b[:room]...)
	}

	p.total += int64(len(b))

	return p.w.Write(b)
}

func (p *previewWriter) logPreview() {
	if len(p.preview) == 0 {
		return
	}

	p.logger.Info("exec preview",
		"exec_id", p.execID,
		"kind", p.kind,
		"total_bytes", p.total,
		"preview", string(p.preview),
	)
}

// hijackStdin wraps a Docker hijacked Conn so that Close() actually
// half-closes the write side via CloseWrite. Without this, Claude
// running with --input-format stream-json never sees stdin EOF and
// either blocks waiting for more frames or exits with no response.
type hijackStdin struct {
	conn      io.Writer
	halfClose func() error
}

func (h *hijackStdin) Write(p []byte) (int, error) { return h.conn.Write(p) }

func (h *hijackStdin) Close() error {
	if h.halfClose == nil {
		return nil
	}

	return h.halfClose()
}

func envSlice(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k, v := range m {
		out = append(out, k+"="+v)
	}

	return out
}

func mountsToDocker(in []spawn.Mount) []mount.Mount {
	out := make([]mount.Mount, 0, len(in))
	for _, m := range in {
		out = append(out, mount.Mount{
			Type:     mount.TypeBind,
			Source:   m.Source,
			Target:   m.Target,
			ReadOnly: m.ReadOnly,
		})
	}

	return out
}

// tmpfsMap builds the map[string]string Docker expects for HostConfig.Tmpfs.
// Each entry maps a target path to the comma-separated mount-options string;
// an empty Mode + zero UID/GID produces an empty value, which Docker treats
// as default (root:root, rw, noexec, nosuid).
func tmpfsMap(in []spawn.TmpfsMount) map[string]string {
	if len(in) == 0 {
		return nil
	}

	out := make(map[string]string, len(in))
	for _, t := range in {
		var opts []string
		if t.Mode != "" {
			opts = append(opts, "mode="+t.Mode)
		}

		if t.UID != 0 {
			opts = append(opts, fmt.Sprintf("uid=%d", t.UID))
		}

		if t.GID != 0 {
			opts = append(opts, fmt.Sprintf("gid=%d", t.GID))
		}

		if t.Exec {
			opts = append(opts, "exec")
		}

		out[t.Target] = strings.Join(opts, ",")
	}

	return out
}
