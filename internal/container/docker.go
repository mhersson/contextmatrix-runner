// Package container manages Docker container lifecycle for task execution.
package container

import (
	"context"
	"io"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
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
	// Ping verifies that the Docker daemon is reachable. Used by the startup
	// preflight and the background health monitor. The long-lived
	// RealDockerClient is expected to reconnect automatically across most
	// dockerd restarts via the docker SDK's internal connection handling;
	// the dockerd health monitor's os.Exit(1) on 3 consecutive failures is
	// the escape hatch for cases where the SDK does not recover on its own.
	Ping(ctx context.Context) error
	ImagePull(ctx context.Context, ref string, options image.PullOptions) (io.ReadCloser, error)
	// ImageInspect returns image metadata. The manager only uses this to
	// confirm presence (nil error = image exists locally); the full
	// response is not consumed today but is threaded through the interface
	// so tests can assert inspection calls happened.
	ImageInspect(ctx context.Context, imageID string) (image.InspectResponse, error)
	ContainerCreate(ctx context.Context, config *container.Config, hostConfig *container.HostConfig, networkConfig *network.NetworkingConfig, platform *ocispec.Platform, name string) (container.CreateResponse, error)
	ContainerStart(ctx context.Context, containerID string, options container.StartOptions) error
	ContainerWait(ctx context.Context, containerID string, condition container.WaitCondition) (<-chan container.WaitResponse, <-chan error)
	ContainerStop(ctx context.Context, containerID string, options container.StopOptions) error
	ContainerRemove(ctx context.Context, containerID string, options container.RemoveOptions) error
	ContainerLogs(ctx context.Context, containerID string, options container.LogsOptions) (io.ReadCloser, error)
	ContainerList(ctx context.Context, options container.ListOptions) ([]DockerContainer, error)
	ContainerAttach(ctx context.Context, containerID string, options container.AttachOptions) (*HijackedResponse, error)
	// ImagesPrune asks dockerd to delete dangling/unused images matching the
	// given filter args. Used by the periodic maintenance loop (CTXRUN-058)
	// to keep local image cache bounded — without this, the host image store
	// grows unbounded across worker-image upgrades.
	ImagesPrune(ctx context.Context, pruneFilter filters.Args) (image.PruneReport, error)
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

// Ping checks whether the Docker daemon is reachable. It returns nil on
// success and a non-nil error on connectivity or daemon-internal failures.
//
// Reconnect contract: the long-lived *client.Client created in
// NewRealDockerClient is expected to re-establish its underlying connection
// across most dockerd restarts. That behavior comes from the docker SDK, not
// this wrapper. The dockerd health monitor (see cmd/contextmatrix-runner)
// exits the process after 3 consecutive failures, which delegates recovery
// from pathological SDK states (e.g. a stuck keepalive) to systemd restart.
func (c *RealDockerClient) Ping(ctx context.Context) error {
	_, err := c.cli.Ping(ctx)

	return err
}

func (c *RealDockerClient) ImagePull(ctx context.Context, ref string, options image.PullOptions) (io.ReadCloser, error) {
	return c.cli.ImagePull(ctx, ref, options)
}

func (c *RealDockerClient) ImageInspect(ctx context.Context, imageID string) (image.InspectResponse, error) {
	return c.cli.ImageInspect(ctx, imageID)
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

func (c *RealDockerClient) ImagesPrune(ctx context.Context, pruneFilter filters.Args) (image.PruneReport, error) {
	return c.cli.ImagesPrune(ctx, pruneFilter)
}

func (c *RealDockerClient) Close() error {
	return c.cli.Close()
}
