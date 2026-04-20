// Package container manages Docker container lifecycle for task execution.
package container

import (
	"context"
	"io"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	dockertypes "github.com/docker/docker/api/types"
)

// HijackedResponse wraps the Docker SDK's hijacked connection and exposes only
// the write side. This avoids leaking the SDK type through the DockerClient
// interface, keeping it fully mockable.
type HijackedResponse struct {
	// Conn is the write side of the hijacked stdin connection.
	Conn io.WriteCloser
	// close releases the underlying network connection.
	close func()
}

// Close releases the underlying network connection.
func (h *HijackedResponse) Close() {
	if h.close != nil {
		h.close()
	}
}

// DockerClient abstracts the Docker SDK methods used by the manager.
// This interface enables testing with mocks.
type DockerClient interface {
	ImagePull(ctx context.Context, ref string, options image.PullOptions) (io.ReadCloser, error)
	ImageInspectWithRaw(ctx context.Context, imageID string) (dockertypes.ImageInspect, []byte, error)
	ContainerCreate(ctx context.Context, config *container.Config, hostConfig *container.HostConfig, networkConfig *network.NetworkingConfig, platform *ocispec.Platform, name string) (container.CreateResponse, error)
	ContainerStart(ctx context.Context, containerID string, options container.StartOptions) error
	ContainerWait(ctx context.Context, containerID string, condition container.WaitCondition) (<-chan container.WaitResponse, <-chan error)
	ContainerStop(ctx context.Context, containerID string, options container.StopOptions) error
	ContainerRemove(ctx context.Context, containerID string, options container.RemoveOptions) error
	ContainerLogs(ctx context.Context, containerID string, options container.LogsOptions) (io.ReadCloser, error)
	ContainerList(ctx context.Context, options container.ListOptions) ([]DockerContainer, error)
	ContainerAttach(ctx context.Context, containerID string, options container.AttachOptions) (*HijackedResponse, error)
	Close() error
}

// DockerContainer is a simplified container info struct used by ContainerList.
type DockerContainer struct {
	ID     string
	Labels map[string]string
}

// RealDockerClient wraps the Docker SDK client.
type RealDockerClient struct {
	cli *client.Client
}

// NewRealDockerClient creates a Docker client from the environment.
func NewRealDockerClient() (*RealDockerClient, error) {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, err
	}

	return &RealDockerClient{cli: cli}, nil
}

func (c *RealDockerClient) ImagePull(ctx context.Context, ref string, options image.PullOptions) (io.ReadCloser, error) {
	return c.cli.ImagePull(ctx, ref, options)
}

func (c *RealDockerClient) ImageInspectWithRaw(ctx context.Context, imageID string) (dockertypes.ImageInspect, []byte, error) {
	return c.cli.ImageInspectWithRaw(ctx, imageID)
}

func (c *RealDockerClient) ContainerCreate(ctx context.Context, config *container.Config, hostConfig *container.HostConfig, networkConfig *network.NetworkingConfig, platform *ocispec.Platform, name string) (container.CreateResponse, error) {
	return c.cli.ContainerCreate(ctx, config, hostConfig, networkConfig, platform, name)
}

func (c *RealDockerClient) ContainerStart(ctx context.Context, containerID string, options container.StartOptions) error {
	return c.cli.ContainerStart(ctx, containerID, options)
}

func (c *RealDockerClient) ContainerWait(ctx context.Context, containerID string, condition container.WaitCondition) (<-chan container.WaitResponse, <-chan error) {
	return c.cli.ContainerWait(ctx, containerID, condition)
}

func (c *RealDockerClient) ContainerStop(ctx context.Context, containerID string, options container.StopOptions) error {
	return c.cli.ContainerStop(ctx, containerID, options)
}

func (c *RealDockerClient) ContainerRemove(ctx context.Context, containerID string, options container.RemoveOptions) error {
	return c.cli.ContainerRemove(ctx, containerID, options)
}

func (c *RealDockerClient) ContainerLogs(ctx context.Context, containerID string, options container.LogsOptions) (io.ReadCloser, error) {
	return c.cli.ContainerLogs(ctx, containerID, options)
}

func (c *RealDockerClient) ContainerList(ctx context.Context, options container.ListOptions) ([]DockerContainer, error) {
	containers, err := c.cli.ContainerList(ctx, options)
	if err != nil {
		return nil, err
	}

	result := make([]DockerContainer, len(containers))
	for i, ctr := range containers {
		result[i] = DockerContainer{ID: ctr.ID, Labels: ctr.Labels}
	}

	return result, nil
}

func (c *RealDockerClient) ContainerAttach(ctx context.Context, containerID string, options container.AttachOptions) (*HijackedResponse, error) {
	resp, err := c.cli.ContainerAttach(ctx, containerID, options)
	if err != nil {
		return nil, err
	}

	return &HijackedResponse{
		Conn:  resp.Conn,
		close: resp.Close,
	}, nil
}

func (c *RealDockerClient) Close() error {
	return c.cli.Close()
}
