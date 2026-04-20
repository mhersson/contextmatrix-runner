package container

import (
	"context"
	"fmt"
	"io"

	dockertypes "github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// MockDockerClient implements DockerClient for testing.
type MockDockerClient struct {
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
}

func (m *MockDockerClient) ImagePull(ctx context.Context, ref string, options image.PullOptions) (io.ReadCloser, error) {
	if m.ImagePullFn != nil {
		return m.ImagePullFn(ctx, ref, options)
	}

	return io.NopCloser(io.LimitReader(nil, 0)), nil
}

func (m *MockDockerClient) ImageInspectWithRaw(ctx context.Context, imageID string) (dockertypes.ImageInspect, []byte, error) {
	if m.ImageInspectWithRawFn != nil {
		return m.ImageInspectWithRawFn(ctx, imageID)
	}

	return dockertypes.ImageInspect{}, nil, fmt.Errorf("image not found")
}

func (m *MockDockerClient) ContainerCreate(ctx context.Context, config *container.Config, hostConfig *container.HostConfig, networkConfig *network.NetworkingConfig, platform *ocispec.Platform, name string) (container.CreateResponse, error) {
	if m.ContainerCreateFn != nil {
		return m.ContainerCreateFn(ctx, config, hostConfig, networkConfig, platform, name)
	}

	return container.CreateResponse{ID: "mock-container-id"}, nil
}

func (m *MockDockerClient) ContainerStart(ctx context.Context, containerID string, options container.StartOptions) error {
	if m.ContainerStartFn != nil {
		return m.ContainerStartFn(ctx, containerID, options)
	}

	return nil
}

func (m *MockDockerClient) ContainerWait(ctx context.Context, containerID string, condition container.WaitCondition) (<-chan container.WaitResponse, <-chan error) {
	if m.ContainerWaitFn != nil {
		return m.ContainerWaitFn(ctx, containerID, condition)
	}

	ch := make(chan container.WaitResponse, 1)
	ch <- container.WaitResponse{StatusCode: 0}

	return ch, make(chan error)
}

func (m *MockDockerClient) ContainerStop(ctx context.Context, containerID string, options container.StopOptions) error {
	if m.ContainerStopFn != nil {
		return m.ContainerStopFn(ctx, containerID, options)
	}

	return nil
}

func (m *MockDockerClient) ContainerRemove(ctx context.Context, containerID string, options container.RemoveOptions) error {
	if m.ContainerRemoveFn != nil {
		return m.ContainerRemoveFn(ctx, containerID, options)
	}

	return nil
}

func (m *MockDockerClient) ContainerLogs(ctx context.Context, containerID string, options container.LogsOptions) (io.ReadCloser, error) {
	if m.ContainerLogsFn != nil {
		return m.ContainerLogsFn(ctx, containerID, options)
	}

	return io.NopCloser(io.LimitReader(nil, 0)), nil
}

func (m *MockDockerClient) ContainerList(ctx context.Context, options container.ListOptions) ([]DockerContainer, error) {
	if m.ContainerListFn != nil {
		return m.ContainerListFn(ctx, options)
	}

	return nil, nil
}

func (m *MockDockerClient) ContainerAttach(ctx context.Context, containerID string, options container.AttachOptions) (*HijackedResponse, error) {
	if m.ContainerAttachFn != nil {
		return m.ContainerAttachFn(ctx, containerID, options)
	}
	// Default: discard all writes so priming writes never block.
	return &HijackedResponse{Conn: nopWriteCloser{}}, nil
}

// nopWriteCloser discards all writes and is always open.
type nopWriteCloser struct{}

func (nopWriteCloser) Write(p []byte) (int, error) { return len(p), nil }
func (nopWriteCloser) Close() error                { return nil }

func (m *MockDockerClient) Close() error { return nil }
