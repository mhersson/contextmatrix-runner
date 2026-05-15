//go:build integration

package container

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/client"
	"github.com/stretchr/testify/require"
)

// TestDockerClient_RunAndWait_Hello smoke-tests the Docker SDK against
// the real daemon. It pulls a small image, starts a container, waits
// for it to exit, and removes it. The point isn't deep coverage — it's
// a build-time canary that the SDK version we depend on still talks to
// dockerd at all.
func TestDockerClient_RunAndWait_Hello(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	require.NoError(t, err)
	t.Cleanup(func() { _ = cli.Close() })

	const imageRef = "hello-world"

	pullOut, err := cli.ImagePull(ctx, imageRef, image.PullOptions{})
	require.NoError(t, err)
	_, _ = io.Copy(io.Discard, pullOut)
	require.NoError(t, pullOut.Close())

	resp, err := cli.ContainerCreate(ctx, &container.Config{
		Image: imageRef,
	}, nil, nil, nil, "")
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = cli.ContainerRemove(context.Background(), resp.ID, container.RemoveOptions{Force: true})
	})

	require.NoError(t, cli.ContainerStart(ctx, resp.ID, container.StartOptions{}))

	waitCh, errCh := cli.ContainerWait(ctx, resp.ID, container.WaitConditionNotRunning)
	select {
	case res := <-waitCh:
		require.Equal(t, int64(0), res.StatusCode)
	case err := <-errCh:
		t.Fatalf("ContainerWait error: %v", err)
	case <-ctx.Done():
		t.Fatalf("timeout")
	}
}
