package container

import (
	"context"
	"io"

	dockertypes "github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// MockDockerClient implements DockerClient for testing.
//
// Every function field defaults to panic("MockDockerClient.<Method> not
// configured") when called. This forces every test to wire the mock methods
// it actually expects to be called, eliminating the silent-success footgun
// where a method invocation the test never reasoned about returned the zero
// value and hid a bug. Tests that legitimately want no-op success must set
// the field explicitly (see helpers like newSuccessfulMock below or configure
// it inline).
type MockDockerClient struct {
	PingFn                func(ctx context.Context) error
	ImagePullFn           func(ctx context.Context, ref string, options image.PullOptions) (io.ReadCloser, error)
	ImageInspectWithRawFn func(ctx context.Context, imageID string) (dockertypes.ImageInspect, []byte, error)
	ContainerCreateFn     func(ctx context.Context, config *container.Config, hostConfig *container.HostConfig, networkConfig *network.NetworkingConfig, platform *ocispec.Platform, name string) (container.CreateResponse, error)
	ContainerStartFn      func(ctx context.Context, containerID string, options container.StartOptions) error
	ContainerWaitFn       func(ctx context.Context, containerID string, condition container.WaitCondition) (<-chan container.WaitResponse, <-chan error)
	ContainerStopFn       func(ctx context.Context, containerID string, options container.StopOptions) error
	ContainerRemoveFn     func(ctx context.Context, containerID string, options container.RemoveOptions) error
	ContainerLogsFn       func(ctx context.Context, containerID string, options container.LogsOptions) (io.ReadCloser, error)
	ContainerListFn       func(ctx context.Context, options container.ListOptions) ([]DockerContainer, error)
	ContainerAttachFn     func(ctx context.Context, containerID string, options container.AttachOptions) (*HijackedResponse, error)
	ImagesPruneFn         func(ctx context.Context, pruneFilter filters.Args) (image.PruneReport, error)
}

func (m *MockDockerClient) Ping(ctx context.Context) error {
	if m.PingFn != nil {
		return m.PingFn(ctx)
	}

	panic("MockDockerClient.Ping not configured")
}

func (m *MockDockerClient) ImagePull(ctx context.Context, ref string, options image.PullOptions) (io.ReadCloser, error) {
	if m.ImagePullFn != nil {
		return m.ImagePullFn(ctx, ref, options)
	}

	panic("MockDockerClient.ImagePull not configured")
}

func (m *MockDockerClient) ImageInspectWithRaw(ctx context.Context, imageID string) (dockertypes.ImageInspect, []byte, error) {
	if m.ImageInspectWithRawFn != nil {
		return m.ImageInspectWithRawFn(ctx, imageID)
	}

	panic("MockDockerClient.ImageInspectWithRaw not configured")
}

func (m *MockDockerClient) ContainerCreate(ctx context.Context, config *container.Config, hostConfig *container.HostConfig, networkConfig *network.NetworkingConfig, platform *ocispec.Platform, name string) (container.CreateResponse, error) {
	if m.ContainerCreateFn != nil {
		return m.ContainerCreateFn(ctx, config, hostConfig, networkConfig, platform, name)
	}

	panic("MockDockerClient.ContainerCreate not configured")
}

func (m *MockDockerClient) ContainerStart(ctx context.Context, containerID string, options container.StartOptions) error {
	if m.ContainerStartFn != nil {
		return m.ContainerStartFn(ctx, containerID, options)
	}

	panic("MockDockerClient.ContainerStart not configured")
}

func (m *MockDockerClient) ContainerWait(ctx context.Context, containerID string, condition container.WaitCondition) (<-chan container.WaitResponse, <-chan error) {
	if m.ContainerWaitFn != nil {
		return m.ContainerWaitFn(ctx, containerID, condition)
	}

	panic("MockDockerClient.ContainerWait not configured")
}

func (m *MockDockerClient) ContainerStop(ctx context.Context, containerID string, options container.StopOptions) error {
	if m.ContainerStopFn != nil {
		return m.ContainerStopFn(ctx, containerID, options)
	}

	panic("MockDockerClient.ContainerStop not configured")
}

func (m *MockDockerClient) ContainerRemove(ctx context.Context, containerID string, options container.RemoveOptions) error {
	if m.ContainerRemoveFn != nil {
		return m.ContainerRemoveFn(ctx, containerID, options)
	}

	panic("MockDockerClient.ContainerRemove not configured")
}

func (m *MockDockerClient) ContainerLogs(ctx context.Context, containerID string, options container.LogsOptions) (io.ReadCloser, error) {
	if m.ContainerLogsFn != nil {
		return m.ContainerLogsFn(ctx, containerID, options)
	}

	panic("MockDockerClient.ContainerLogs not configured")
}

func (m *MockDockerClient) ContainerList(ctx context.Context, options container.ListOptions) ([]DockerContainer, error) {
	if m.ContainerListFn != nil {
		return m.ContainerListFn(ctx, options)
	}

	panic("MockDockerClient.ContainerList not configured")
}

func (m *MockDockerClient) ContainerAttach(ctx context.Context, containerID string, options container.AttachOptions) (*HijackedResponse, error) {
	if m.ContainerAttachFn != nil {
		return m.ContainerAttachFn(ctx, containerID, options)
	}

	panic("MockDockerClient.ContainerAttach not configured")
}

func (m *MockDockerClient) ImagesPrune(ctx context.Context, pruneFilter filters.Args) (image.PruneReport, error) {
	if m.ImagesPruneFn != nil {
		return m.ImagesPruneFn(ctx, pruneFilter)
	}

	panic("MockDockerClient.ImagesPrune not configured")
}

// nopWriteCloser discards all writes and is always open.
type nopWriteCloser struct{}

func (nopWriteCloser) Write(p []byte) (int, error) { return len(p), nil }
func (nopWriteCloser) Close() error                { return nil }

func (m *MockDockerClient) Close() error { return nil }

// successfulMock returns a MockDockerClient with every method wired to a
// no-op success response. Use this as a baseline and override specific fields
// when a test wants a particular Docker call to fail or capture state.
//
// With the panic-on-unconfigured defaults, silent-success is now explicit:
// callers see exactly which methods their code will invoke.
func successfulMock() *MockDockerClient {
	return &MockDockerClient{
		PingFn: func(_ context.Context) error {
			return nil
		},
		ImagePullFn: func(_ context.Context, _ string, _ image.PullOptions) (io.ReadCloser, error) {
			return io.NopCloser(io.LimitReader(nil, 0)), nil
		},
		ImageInspectWithRawFn: func(_ context.Context, _ string) (dockertypes.ImageInspect, []byte, error) {
			return dockertypes.ImageInspect{}, nil, nil
		},
		ContainerCreateFn: func(_ context.Context, _ *container.Config, _ *container.HostConfig, _ *network.NetworkingConfig, _ *ocispec.Platform, _ string) (container.CreateResponse, error) {
			return container.CreateResponse{ID: "mock-container-id"}, nil
		},
		ContainerStartFn: func(_ context.Context, _ string, _ container.StartOptions) error {
			return nil
		},
		ContainerWaitFn: func(_ context.Context, _ string, _ container.WaitCondition) (<-chan container.WaitResponse, <-chan error) {
			ch := make(chan container.WaitResponse, 1)
			ch <- container.WaitResponse{StatusCode: 0}

			return ch, make(chan error)
		},
		ContainerStopFn: func(_ context.Context, _ string, _ container.StopOptions) error {
			return nil
		},
		ContainerRemoveFn: func(_ context.Context, _ string, _ container.RemoveOptions) error {
			return nil
		},
		ContainerLogsFn: func(_ context.Context, _ string, _ container.LogsOptions) (io.ReadCloser, error) {
			return io.NopCloser(io.LimitReader(nil, 0)), nil
		},
		ContainerListFn: func(_ context.Context, _ container.ListOptions) ([]DockerContainer, error) {
			return nil, nil
		},
		ContainerAttachFn: func(_ context.Context, _ string, _ container.AttachOptions) (*HijackedResponse, error) {
			return &HijackedResponse{Conn: nopWriteCloser{}}, nil
		},
		ImagesPruneFn: func(_ context.Context, _ filters.Args) (image.PruneReport, error) {
			return image.PruneReport{}, nil
		},
	}
}
