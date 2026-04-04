package container

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os/user"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/strslice"
	"github.com/docker/docker/pkg/stdcopy"

	"github.com/mhersson/contextmatrix-runner/internal/callback"
	"github.com/mhersson/contextmatrix-runner/internal/config"
	"github.com/mhersson/contextmatrix-runner/internal/github"
	"github.com/mhersson/contextmatrix-runner/internal/logparser"
	"github.com/mhersson/contextmatrix-runner/internal/tracker"
)

// RunConfig holds the parameters needed to start a container.
// This mirrors the webhook TriggerPayload but avoids an import cycle.
type RunConfig struct {
	CardID      string
	Project     string
	RepoURL     string
	MCPURL      string
	MCPAPIKey   string
	RunnerImage string
}

const (
	// LabelRunner marks containers managed by contextmatrix-runner.
	LabelRunner = "contextmatrix.runner"
	// LabelCardID stores the card ID on the container.
	LabelCardID = "contextmatrix.card_id"
	// LabelProject stores the project name on the container.
	LabelProject = "contextmatrix.project"

	imagePullTimeout = 5 * time.Minute
	stopGracePeriod  = 10 // seconds
)

// Manager handles the lifecycle of Docker containers for task execution.
type Manager struct {
	docker   DockerClient
	tracker  *tracker.Tracker
	callback *callback.Client
	token    *github.TokenProvider
	cfg      *config.Config
	logger   *slog.Logger
	wg       sync.WaitGroup
}

// NewManager creates a container manager.
func NewManager(
	docker DockerClient,
	tracker *tracker.Tracker,
	cb *callback.Client,
	token *github.TokenProvider,
	cfg *config.Config,
	logger *slog.Logger,
) *Manager {
	return &Manager{
		docker:   docker,
		tracker:  tracker,
		callback: cb,
		token:    token,
		cfg:      cfg,
		logger:   logger,
	}
}

// Run launches the full container lifecycle for a triggered task in a goroutine.
// Use Wait to block until all launched goroutines have finished.
func (m *Manager) Run(ctx context.Context, payload RunConfig) {
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		defer func() {
			if r := recover(); r != nil {
				m.logger.Error("container run panicked",
					"panic", r, "card_id", payload.CardID, "project", payload.Project)
				m.tracker.Remove(payload.Project, payload.CardID)
				m.reportFailure(context.Background(), payload, fmt.Sprintf("internal error: %v", r))
			}
		}()
		m.run(ctx, payload)
	}()
}

// Wait blocks until all container goroutines launched by Run have finished.
func (m *Manager) Wait() {
	m.wg.Wait()
}

func (m *Manager) run(ctx context.Context, payload RunConfig) {
	log := m.logger.With("card_id", payload.CardID, "project", payload.Project)

	containerID, err := m.startContainer(ctx, payload)
	if err != nil {
		log.Error("failed to start container", "error", err)
		m.reportFailure(ctx, payload, fmt.Sprintf("start failed: %v", err))
		m.tracker.Remove(payload.Project, payload.CardID)
		return
	}

	m.tracker.UpdateContainerID(payload.Project, payload.CardID, containerID)

	// Report running status to CM.
	if err := m.callback.ReportStatus(ctx, payload.CardID, payload.Project, "running", "container started"); err != nil {
		log.Warn("failed to report running status", "error", err)
	}

	// Wait for container to finish.
	m.waitAndCleanup(ctx, containerID, payload, log)
}

func (m *Manager) startContainer(ctx context.Context, payload RunConfig) (string, error) {
	img := payload.RunnerImage
	if img == "" {
		img = m.cfg.BaseImage
	}

	// Validate image against allowlist.
	if len(m.cfg.AllowedImages) > 0 {
		allowed := false
		for _, ai := range m.cfg.AllowedImages {
			if img == ai {
				allowed = true
				break
			}
		}
		if !allowed {
			return "", fmt.Errorf("image %q not in allowed_images list", img)
		}
	} else if img != m.cfg.BaseImage {
		return "", fmt.Errorf("image %q not allowed (only base_image %q permitted when allowed_images is empty)", img, m.cfg.BaseImage)
	}

	// Pull image according to policy.
	if err := m.pullImage(ctx, img); err != nil {
		return "", err
	}

	// Generate GitHub App token.
	gitToken, err := m.token.GenerateToken(ctx)
	if err != nil {
		return "", fmt.Errorf("generate git token: %w", err)
	}

	// Build environment variables.
	env := []string{
		"CM_CARD_ID=" + payload.CardID,
		"CM_PROJECT=" + payload.Project,
		"CM_MCP_URL=" + payload.MCPURL,
		"CM_REPO_URL=" + payload.RepoURL,
		"CM_GIT_TOKEN=" + gitToken,
	}
	if payload.MCPAPIKey != "" {
		env = append(env, "CM_MCP_API_KEY="+payload.MCPAPIKey)
	}
	if m.cfg.AnthropicAPIKey != "" {
		env = append(env, "ANTHROPIC_API_KEY="+m.cfg.AnthropicAPIKey)
	}
	if u, err := user.Current(); err != nil {
		m.logger.Warn("failed to get current user; HOST_UID/HOST_GID will not be set", "error", err)
	} else {
		env = append(env, "HOST_UID="+u.Uid, "HOST_GID="+u.Gid)
	}

	// Build mounts.
	var mounts []mount.Mount
	if m.cfg.ClaudeAuthDir != "" {
		mounts = append(mounts, mount.Mount{
			Type:     mount.TypeBind,
			Source:   m.cfg.ClaudeAuthDir,
			Target:   "/claude-auth",
			ReadOnly: true,
		})
	}

	name := sanitizeContainerName(payload.Project, payload.CardID)

	resp, err := m.docker.ContainerCreate(ctx,
		&container.Config{
			Image: img,
			Env:   env,
			Labels: map[string]string{
				LabelRunner:  "true",
				LabelCardID:  payload.CardID,
				LabelProject: payload.Project,
			},
		},
		&container.HostConfig{
			Mounts:      mounts,
			ExtraHosts:  []string{"host.docker.internal:host-gateway"},
			CapDrop:     strslice.StrSlice{"ALL"},
			SecurityOpt: []string{"no-new-privileges"},
			Resources: container.Resources{
				Memory:    m.cfg.ContainerMemoryLimit,
				PidsLimit: &m.cfg.ContainerPidsLimit,
			},
		},
		nil, nil, name,
	)
	if err != nil {
		return "", fmt.Errorf("create container: %w", err)
	}

	if err := m.docker.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		// Clean up the created-but-not-started container.
		if rmErr := m.docker.ContainerRemove(ctx, resp.ID, container.RemoveOptions{Force: true}); rmErr != nil {
			m.logger.Warn("failed to remove container after start failure", "container_id", resp.ID, "error", rmErr)
		}
		return "", fmt.Errorf("start container: %w", err)
	}

	return resp.ID, nil
}

func (m *Manager) waitAndCleanup(ctx context.Context, containerID string, payload RunConfig, log *slog.Logger) {
	defer m.tracker.Remove(payload.Project, payload.CardID)
	defer m.removeContainer(context.Background(), containerID, log)

	// Stream container logs in real time.
	logDone := m.streamLogs(ctx, containerID, log)

	// Apply container timeout.
	timeout := m.cfg.ContainerTimeoutDuration()
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	waitCh, errCh := m.docker.ContainerWait(waitCtx, containerID, container.WaitConditionNotRunning)

	select {
	case result := <-waitCh:
		<-logDone // drain remaining log output
		if result.StatusCode != 0 {
			msg := fmt.Sprintf("container exited with code %d", result.StatusCode)
			log.Warn(msg, "exit_code", result.StatusCode)
			reportCtx := context.Background()
			m.reportFailure(reportCtx, payload, msg)
			return
		}
		log.Info("container completed successfully")
		reportCtx := context.Background()
		m.reportCompleted(reportCtx, payload)

	case err := <-errCh:
		if waitCtx.Err() != nil {
			// Timeout.
			msg := fmt.Sprintf("container timed out after %s", timeout)
			log.Warn(msg)
			m.killContainer(context.Background(), containerID, log)
			reportCtx := context.Background()
			m.reportFailure(reportCtx, payload, msg)
			return
		}
		msg := fmt.Sprintf("wait error: %v", err)
		log.Error(msg)
		reportCtx := context.Background()
		m.reportFailure(reportCtx, payload, msg)

	case <-ctx.Done():
		// Parent context canceled (e.g., kill or shutdown).
		log.Info("container canceled")
		m.killContainer(context.Background(), containerID, log)
	}
}

// streamLogs follows container stdout/stderr and logs each line. The returned
// channel is closed when the stream ends (container exit or context cancel).
func (m *Manager) streamLogs(ctx context.Context, containerID string, log *slog.Logger) <-chan struct{} {
	done := make(chan struct{})

	reader, err := m.docker.ContainerLogs(ctx, containerID, container.LogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Follow:     true,
	})
	if err != nil {
		log.Warn("failed to attach to container logs", "error", err)
		close(done)
		return done
	}

	go func() {
		defer close(done)
		defer func() { _ = reader.Close() }()

		stdoutPr, stdoutPw := io.Pipe()
		stderrPr, stderrPw := io.Pipe()

		go func() {
			defer func() { _ = stdoutPw.Close(); _ = stderrPw.Close() }()
			_, _ = stdcopy.StdCopy(stdoutPw, stderrPw, reader)
		}()

		// Log stderr lines as warnings.
		go func() {
			scanner := bufio.NewScanner(stderrPr)
			scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
			for scanner.Scan() {
				log.Warn("container stderr", "line", scanner.Text())
			}
		}()

		logparser.ProcessStream(stdoutPr, log)
	}()

	return done
}

func (m *Manager) killContainer(ctx context.Context, containerID string, log *slog.Logger) {
	grace := stopGracePeriod
	if err := m.docker.ContainerStop(ctx, containerID, container.StopOptions{Timeout: &grace}); err != nil {
		log.Warn("failed to stop container", "error", err)
	}
}

func (m *Manager) removeContainer(ctx context.Context, containerID string, log *slog.Logger) {
	if err := m.docker.ContainerRemove(ctx, containerID, container.RemoveOptions{Force: true}); err != nil {
		log.Warn("failed to remove container", "error", err)
	}
}

func (m *Manager) reportFailure(ctx context.Context, payload RunConfig, message string) {
	if err := m.callback.ReportStatus(ctx, payload.CardID, payload.Project, "failed", message); err != nil {
		m.logger.Error("failed to report failure callback", "card_id", payload.CardID, "error", err)
	}
}

// reportCompleted notifies ContextMatrix that the container exited normally.
// This acts as a safety net: if Claude didn't call complete_task, the claim
// is still released server-side when ContextMatrix receives the "completed" status.
func (m *Manager) reportCompleted(ctx context.Context, payload RunConfig) {
	if err := m.callback.ReportStatus(ctx, payload.CardID, payload.Project, "completed", "container exited normally"); err != nil {
		m.logger.Error("failed to report completed callback", "card_id", payload.CardID, "error", err)
	}
}

// Kill stops and removes a specific container by project and card ID.
func (m *Manager) Kill(project, cardID string) error {
	info, ok := m.tracker.Get(project, cardID)
	if !ok {
		return fmt.Errorf("no container tracked for %s/%s", project, cardID)
	}

	// Cancel the container's context to trigger cleanup in Run.
	if info.Cancel != nil {
		info.Cancel()
	}

	return nil
}

// CleanupOrphans removes any leftover containers from a previous runner crash.
func (m *Manager) CleanupOrphans(ctx context.Context) error {
	containers, err := m.docker.ContainerList(ctx, container.ListOptions{
		Filters: filters.NewArgs(filters.Arg("label", LabelRunner+"=true")),
		All:     true,
	})
	if err != nil {
		return fmt.Errorf("list orphan containers: %w", err)
	}

	for _, ctr := range containers {
		m.logger.Info("cleaning up orphan container",
			"container_id", truncateID(ctr.ID),
			"card_id", ctr.Labels[LabelCardID],
			"project", ctr.Labels[LabelProject],
		)
		_ = m.docker.ContainerStop(ctx, ctr.ID, container.StopOptions{Timeout: intPtr(5)})
		_ = m.docker.ContainerRemove(ctx, ctr.ID, container.RemoveOptions{Force: true})
	}

	if len(containers) > 0 {
		m.logger.Info("orphan cleanup complete", "removed", len(containers))
	}
	return nil
}

func (m *Manager) pullImage(ctx context.Context, img string) error {
	policy := m.cfg.ImagePullPolicy
	if policy == "" {
		policy = config.PullAlways
	}

	if policy == config.PullNever {
		return nil
	}

	if policy == config.PullIfNotPresent {
		if _, _, err := m.docker.ImageInspectWithRaw(ctx, img); err == nil {
			m.logger.Debug("image already present locally, skipping pull", "image", img)
			return nil
		}
	}

	pullCtx, cancel := context.WithTimeout(ctx, imagePullTimeout)
	defer cancel()

	reader, err := m.docker.ImagePull(pullCtx, img, image.PullOptions{})
	if err != nil {
		return fmt.Errorf("pull image %s: %w", img, err)
	}
	if _, err := io.Copy(io.Discard, reader); err != nil {
		m.logger.Warn("failed to drain image pull output", "error", err)
	}
	if err := reader.Close(); err != nil {
		m.logger.Warn("failed to close image pull reader", "error", err)
	}

	return nil
}

var containerNameRe = regexp.MustCompile(`[^a-zA-Z0-9_.-]`)

func sanitizeContainerName(project, cardID string) string {
	name := fmt.Sprintf("cmr-%s-%s", project, cardID)
	name = strings.ToLower(name)
	return containerNameRe.ReplaceAllString(name, "-")
}

func intPtr(v int) *int { return &v }

func truncateID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}
