package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	githubauth "github.com/mhersson/contextmatrix-githubauth"

	"github.com/mhersson/contextmatrix-runner/internal/callback"
	"github.com/mhersson/contextmatrix-runner/internal/config"
	ctr "github.com/mhersson/contextmatrix-runner/internal/container"
)

// stubToken is a minimal githubauth.TokenGenerator used by buildProbes tests.
type stubToken struct {
	err error
}

var _ githubauth.TokenGenerator = stubToken{}

func (s stubToken) GenerateToken(_ context.Context) (string, time.Time, error) {
	return "tok", time.Now().Add(time.Hour), s.err
}

// fakeDocker implements ctr.DockerClient. Only the methods called by
// buildProbes' returned closures are configured; the rest panic on use
// to surface surprises.
type fakeDocker struct {
	pingErr    error
	inspectErr error
	inspected  *string // records the image ref the inspect probe passed
}

func (f *fakeDocker) Ping(_ context.Context) error { return f.pingErr }

func (f *fakeDocker) ImageInspect(_ context.Context, imageID string) (image.InspectResponse, error) {
	if f.inspected != nil {
		*f.inspected = imageID
	}

	return image.InspectResponse{}, f.inspectErr
}

func (f *fakeDocker) ImagePull(_ context.Context, _ string, _ image.PullOptions) (io.ReadCloser, error) {
	panic("not called")
}

func (f *fakeDocker) ContainerCreate(_ context.Context, _ *container.Config, _ *container.HostConfig, _ *network.NetworkingConfig, _ *ocispec.Platform, _ string) (container.CreateResponse, error) {
	panic("not called")
}

func (f *fakeDocker) ContainerStart(_ context.Context, _ string, _ container.StartOptions) error {
	panic("not called")
}

func (f *fakeDocker) ContainerWait(_ context.Context, _ string, _ container.WaitCondition) (<-chan container.WaitResponse, <-chan error) {
	panic("not called")
}

func (f *fakeDocker) ContainerStop(_ context.Context, _ string, _ container.StopOptions) error {
	panic("not called")
}

func (f *fakeDocker) ContainerRemove(_ context.Context, _ string, _ container.RemoveOptions) error {
	panic("not called")
}

func (f *fakeDocker) ContainerLogs(_ context.Context, _ string, _ container.LogsOptions) (io.ReadCloser, error) {
	panic("not called")
}

func (f *fakeDocker) ContainerList(_ context.Context, _ container.ListOptions) ([]ctr.DockerContainer, error) {
	panic("not called")
}

func (f *fakeDocker) ContainerAttach(_ context.Context, _ string, _ container.AttachOptions) (*ctr.HijackedResponse, error) {
	panic("not called")
}

func (f *fakeDocker) ImagesPrune(_ context.Context, _ filters.Args) (image.PruneReport, error) {
	panic("not called")
}
func (f *fakeDocker) Close() error { return nil }

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestBuildProbes_PullNeverIncludesImageInspect asserts the four probe
// slots expected under the default "never" policy are wired and return
// the errors from their underlying dependencies, so a real preflight
// failure propagates instead of being silently swallowed.
func TestBuildProbes_PullNeverIncludesImageInspect(t *testing.T) {
	var inspected string

	docker := &fakeDocker{
		pingErr:    errors.New("dockerd down"),
		inspectErr: errors.New("image missing"),
		inspected:  &inspected,
	}

	cfg := &config.Config{
		ImagePullPolicy: config.PullNever,
		BaseImage:       "ghcr.io/example/worker@sha256:aaaabbbbccccddddeeeeffff00001111aaaabbbbccccddddeeeeffff00001111",
	}

	cb := callback.NewClient("http://127.0.0.1:1", "test-secret-key-that-is-long-enough", testLogger())
	probes := buildProbes(cfg, docker, stubToken{err: errors.New("gh down")}, cb)

	require.Error(t, probes.DockerPing(context.Background()), "DockerPing should propagate the dockerd error")
	require.Error(t, probes.GitHubToken(context.Background()), "GitHubToken should propagate the PAT/App error")

	// ImageInspect must be populated and must target the configured
	// base image exactly — drift between config.BaseImage and the
	// probe would quietly pass preflight against the wrong reference.
	require.NotNil(t, probes.ImageInspect)
	require.Error(t, probes.ImageInspect(context.Background()))
	assert.Equal(t, cfg.BaseImage, inspected)

	// ContextMatrixPing is wired to a closed port, so it must fail
	// during the preflight window.
	ctx, cancel := context.WithTimeout(context.Background(), 1e9)
	defer cancel()

	require.Error(t, probes.ContextMatrixPing(ctx))
}

// TestBuildProbes_PullIfNotPresentSkipsInspect covers the card's
// explicit contract: ImageInspect only runs when pull policy is "never".
// Any other policy defers image presence to the manager and preflight
// must not synthesise a failure here.
func TestBuildProbes_PullIfNotPresentSkipsInspect(t *testing.T) {
	docker := &fakeDocker{}
	cfg := &config.Config{
		ImagePullPolicy: config.PullIfNotPresent,
		BaseImage:       "ghcr.io/example/worker@sha256:aaaabbbbccccddddeeeeffff00001111aaaabbbbccccddddeeeeffff00001111",
	}

	cb := callback.NewClient("http://127.0.0.1:1", "x", testLogger())
	probes := buildProbes(cfg, docker, stubToken{}, cb)

	assert.Nil(t, probes.ImageInspect, "image inspect should be unset when pulls are permitted")
}
