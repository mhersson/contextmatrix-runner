package docker

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net"
	"sync"
	"testing"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/contextmatrix-runner/internal/spawn"
)

type fakeDockerClient struct {
	mu sync.Mutex

	createdCfg  *container.Config
	createdHost *container.HostConfig
	createdName string
	started     bool
	stopped     bool
	removed     bool

	lastExecOpts        container.ExecOptions
	execAttached        bool
	execInspectExitCode int
	attachBody          io.Reader
}

func (f *fakeDockerClient) ContainerCreate(_ context.Context, cfg *container.Config, host *container.HostConfig, _ *network.NetworkingConfig, _ *ocispec.Platform, name string) (container.CreateResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.createdCfg = cfg
	f.createdHost = host
	f.createdName = name

	return container.CreateResponse{ID: "container-id-1"}, nil
}

func (f *fakeDockerClient) ContainerStart(_ context.Context, _ string, _ container.StartOptions) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.started = true

	return nil
}

func (f *fakeDockerClient) ContainerStop(_ context.Context, _ string, _ container.StopOptions) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.stopped = true

	return nil
}

func (f *fakeDockerClient) ContainerRemove(_ context.Context, _ string, _ container.RemoveOptions) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.removed = true

	return nil
}

func (f *fakeDockerClient) ContainerExecCreate(_ context.Context, _ string, opts container.ExecOptions) (container.ExecCreateResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.lastExecOpts = opts

	return container.ExecCreateResponse{ID: "exec-id-1"}, nil
}

func (f *fakeDockerClient) ContainerExecAttach(_ context.Context, _ string, _ container.ExecAttachOptions) (types.HijackedResponse, error) {
	f.mu.Lock()
	f.execAttached = true
	body := f.attachBody
	f.mu.Unlock()

	a, _ := net.Pipe()

	if body == nil {
		body = bytes.NewBufferString("")
	}

	return types.HijackedResponse{
		Conn:   a,
		Reader: bufio.NewReader(body),
	}, nil
}

func (f *fakeDockerClient) ContainerExecInspect(_ context.Context, _ string) (container.ExecInspect, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	return container.ExecInspect{ExitCode: f.execInspectExitCode}, nil
}

func TestDockerSpawnerCreatesAndStartsContainer(t *testing.T) {
	fake := &fakeDockerClient{}
	sp := NewSpawnerWithClient(fake)

	worker, err := sp.Spawn(context.Background(), spawn.WorkerSpec{
		Image: "harbor/contextmatrix-worker:latest",
		Name:  "test-worker",
		Env:   map[string]string{"FOO": "bar"},
	})
	require.NoError(t, err)
	require.NotNil(t, worker)
	require.Equal(t, "container-id-1", worker.ID())
	require.Equal(t, spawn.WorkerRunning, worker.Status())

	fake.mu.Lock()
	defer fake.mu.Unlock()

	require.Equal(t, "harbor/contextmatrix-worker:latest", fake.createdCfg.Image)
	require.Equal(t, "test-worker", fake.createdName)
	require.Contains(t, fake.createdCfg.Env, "FOO=bar")
	require.True(t, fake.started)
}

func TestSpawnPropagatesLabels(t *testing.T) {
	fake := &fakeDockerClient{}
	sp := NewSpawnerWithClient(fake)

	_, err := sp.Spawn(context.Background(), spawn.WorkerSpec{
		Image: "x",
		Labels: map[string]string{
			"contextmatrix.runner":  "true",
			"contextmatrix.project": "alpha",
			"contextmatrix.card_id": "ALPHA-007",
		},
	})
	require.NoError(t, err)

	fake.mu.Lock()
	defer fake.mu.Unlock()

	require.Equal(t, "true", fake.createdCfg.Labels["contextmatrix.runner"])
	require.Equal(t, "alpha", fake.createdCfg.Labels["contextmatrix.project"])
	require.Equal(t, "ALPHA-007", fake.createdCfg.Labels["contextmatrix.card_id"])
}

func TestSpawnPropagatesSecurity(t *testing.T) {
	fake := &fakeDockerClient{}
	sp := NewSpawnerWithClient(fake)

	_, err := sp.Spawn(context.Background(), spawn.WorkerSpec{
		Image: "x",
		Security: spawn.SecuritySpec{
			CapDrop:        []string{"ALL"},
			SecurityOpt:    []string{"no-new-privileges:true"},
			ReadonlyRootfs: true,
		},
		Tmpfs: []spawn.TmpfsMount{
			{Target: "/tmp", Mode: "1777"},
			{Target: "/home/user", Mode: "0700"},
		},
	})
	require.NoError(t, err)

	fake.mu.Lock()
	defer fake.mu.Unlock()

	require.Equal(t, []string{"ALL"}, []string(fake.createdHost.CapDrop))
	require.Equal(t, []string{"no-new-privileges:true"}, fake.createdHost.SecurityOpt)
	require.True(t, fake.createdHost.ReadonlyRootfs)

	require.Equal(t, "mode=1777", fake.createdHost.Tmpfs["/tmp"])
	require.Equal(t, "mode=0700", fake.createdHost.Tmpfs["/home/user"])
}

func TestSpawnPropagatesMounts(t *testing.T) {
	fake := &fakeDockerClient{}
	sp := NewSpawnerWithClient(fake)

	_, err := sp.Spawn(context.Background(), spawn.WorkerSpec{
		Image: "x",
		Mounts: []spawn.Mount{
			{Source: "/host/auth", Target: "/workspace/auth", ReadOnly: false},
			{Source: "/host/ro", Target: "/etc/ro", ReadOnly: true},
		},
	})
	require.NoError(t, err)

	fake.mu.Lock()
	defer fake.mu.Unlock()

	require.Len(t, fake.createdHost.Mounts, 2)
	require.Equal(t, "/host/auth", fake.createdHost.Mounts[0].Source)
	require.Equal(t, "/workspace/auth", fake.createdHost.Mounts[0].Target)
	require.False(t, fake.createdHost.Mounts[0].ReadOnly)
	require.True(t, fake.createdHost.Mounts[1].ReadOnly)
}

func TestWorkerExec(t *testing.T) {
	fake := &fakeDockerClient{execInspectExitCode: 0}
	sp := NewSpawnerWithClient(fake)

	worker, err := sp.Spawn(context.Background(), spawn.WorkerSpec{Image: "x"})
	require.NoError(t, err)

	res, err := worker.Exec(context.Background(), spawn.ExecOptions{
		Cmd:        []string{"git", "status"},
		WorkingDir: "/workspace/r1",
	})
	require.NoError(t, err)
	require.Equal(t, 0, res.ExitCode)

	fake.mu.Lock()
	defer fake.mu.Unlock()

	require.Equal(t, []string{"git", "status"}, fake.lastExecOpts.Cmd)
	require.Equal(t, "/workspace/r1", fake.lastExecOpts.WorkingDir)
	require.True(t, fake.execAttached)
}

func TestWorkerExecDemultiplexesStdout(t *testing.T) {
	// Docker exec without TTY returns a multiplexed stream (8-byte
	// frame headers + payload). Without demuxing, the multiplex bytes
	// leak into the caller's stdout — and a previous bug in CloneRepo's
	// `git remote get-url origin` adoption check rejected the URL
	// because it carried a leading `\x01\x00\x00\x00\x00\x00\x00+`
	// header that string-mismatched the registry URL. Exec must
	// demultiplex stdout/stderr the same way ExecStream does.
	const payload = "https://github.com/acme/auth-svc.git\n"

	frame := make([]byte, 8+len(payload))
	frame[0] = 1 // stdout

	binary.BigEndian.PutUint32(frame[4:8], uint32(len(payload)))
	copy(frame[8:], payload)

	fake := &fakeDockerClient{
		execInspectExitCode: 0,
		attachBody:          bytes.NewReader(frame),
	}
	sp := NewSpawnerWithClient(fake)

	worker, err := sp.Spawn(context.Background(), spawn.WorkerSpec{Image: "x"})
	require.NoError(t, err)

	res, err := worker.Exec(context.Background(), spawn.ExecOptions{
		Cmd: []string{"git", "remote", "get-url", "origin"},
	})
	require.NoError(t, err)
	require.Equal(t, payload, res.Stdout)
	require.Empty(t, res.Stderr)
}

func TestWorkerStopRemove(t *testing.T) {
	fake := &fakeDockerClient{}
	sp := NewSpawnerWithClient(fake)

	worker, err := sp.Spawn(context.Background(), spawn.WorkerSpec{Image: "x"})
	require.NoError(t, err)

	require.NoError(t, worker.Stop(context.Background()))
	require.Equal(t, spawn.WorkerStopped, worker.Status())

	require.NoError(t, worker.Remove(context.Background()))
	require.Equal(t, spawn.WorkerRemoved, worker.Status())

	fake.mu.Lock()
	defer fake.mu.Unlock()

	require.True(t, fake.stopped)
	require.True(t, fake.removed)
}
