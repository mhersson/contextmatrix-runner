// Package container manages operational concerns for Docker containers
// spawned by the runner: kill by tracker entry, list labeled containers,
// force-remove by labels, cleanup orphans, prune dangling images.
//
// The container LIFECYCLE (spawn, run, stream logs, deliver secrets, write
// priming) is owned by internal/orchestrated/dispatcher and internal/spawn.
// Manager only implements the management operations the webhook handler
// (/kill, /containers, /stop-all) and the periodic maintenance loop call
// into.
package container

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"

	"github.com/mhersson/contextmatrix-runner/internal/metrics"
	"github.com/mhersson/contextmatrix-runner/internal/tracker"
)

const (
	// LabelRunner marks containers managed by contextmatrix-runner.
	LabelRunner = "contextmatrix.runner"
	// LabelCardID stores the card ID on the container.
	LabelCardID = "contextmatrix.card_id"
	// LabelProject stores the project name on the container.
	LabelProject = "contextmatrix.project"

	// dockerCleanupTimeout bounds the detached contexts used for
	// best-effort Docker cleanup (Stop / Remove / Kill) that must run
	// even when the parent ctx has already been cancelled. A hung
	// dockerd used to stall shutdown forever; now every such call has
	// a hard cap. See CTXRUN-040.
	dockerCleanupTimeout = 5 * time.Second

	// imagePruneMaxAge bounds PruneImages: only dangling images older than
	// this are pruned, so an image-pull race against a simultaneous prune
	// can't delete an image we just pulled.
	imagePruneMaxAge = "24h"
)

// Manager exposes operational management of runner-labeled Docker containers.
// The fields are intentionally narrow — lifecycle (spawn, run, stream) is
// owned by the orchestrated dispatcher.
type Manager struct {
	docker  DockerClient
	tracker *tracker.Tracker
	logger  *slog.Logger
	metrics *metrics.Metrics
}

// NewManager constructs the operational manager.
func NewManager(docker DockerClient, trk *tracker.Tracker, logger *slog.Logger) *Manager {
	return &Manager{
		docker:  docker,
		tracker: trk,
		logger:  logger,
	}
}

// WithMetrics attaches a metrics bundle. Pass nil to disable metric observation.
func (m *Manager) WithMetrics(mx *metrics.Metrics) *Manager {
	m.metrics = mx

	return m
}

// Wait is a no-op. Orchestrated containers are managed by the per-card
// driver, which installs its own cancellation hooks. The method is kept
// because shutdown call sites still invoke it.
func (m *Manager) Wait() {}

// Kill cancels the run context for a tracked (project, card) entry. Returns
// an error if no entry is tracked.
func (m *Manager) Kill(project, cardID string) error {
	if !m.tracker.Cancel(project, cardID) {
		return fmt.Errorf("no container tracked for %s/%s", project, cardID)
	}

	return nil
}

// withDockerCleanupTimeout returns a context bounded by dockerCleanupTimeout,
// detached from any parent. Used for best-effort Docker calls that must run
// even when the caller's ctx has already been cancelled.
func withDockerCleanupTimeout(_ context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), dockerCleanupTimeout)
}

// truncateID returns the first 12 characters of a Docker container ID, the
// standard short form used by `docker ps` output.
func truncateID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}

	return id
}

// ManagedContainer describes a Docker container labeled as runner-managed.
// It is the ground-truth unit consumed by CM's reconcile sweep: a container
// is listed here iff docker ps says so, regardless of whether the runner's
// in-memory tracker still knows about it. That divergence is the failure
// mode the Docker-authoritative sweep is designed to catch.
type ManagedContainer struct {
	ContainerID   string
	ContainerName string
	CardID        string
	Project       string
	State         string
	StartedAt     time.Time
	Tracked       bool
}

// ListManaged returns every Docker container labeled LabelRunner=true,
// regardless of running/exited state. Tracked reflects whether the in-memory
// tracker currently has a matching entry; consumers can use the field to
// detect tracker/Docker divergence without needing a second round-trip.
//
// Containers missing the card_id or project label are skipped — they are
// neither reachable via /kill (which routes by labels) nor the sweep's
// responsibility (the sweep correlates against CM cards, not arbitrary
// docker containers).
func (m *Manager) ListManaged(ctx context.Context) ([]ManagedContainer, error) {
	containers, err := m.docker.ContainerList(ctx, container.ListOptions{
		Filters: filters.NewArgs(filters.Arg("label", LabelRunner+"=true")),
		All:     true,
	})
	if err != nil {
		return nil, fmt.Errorf("list managed containers: %w", err)
	}

	result := make([]ManagedContainer, 0, len(containers))

	for _, ctr := range containers {
		project := ctr.Labels[LabelProject]
		cardID := ctr.Labels[LabelCardID]

		if project == "" || cardID == "" {
			continue
		}

		name := ""
		if len(ctr.Names) > 0 {
			// Docker prefixes container names with "/"; strip it so the
			// wire shape matches what `docker ps` prints.
			name = strings.TrimPrefix(ctr.Names[0], "/")
		}

		result = append(result, ManagedContainer{
			ContainerID:   ctr.ID,
			ContainerName: name,
			CardID:        cardID,
			Project:       project,
			State:         ctr.State,
			StartedAt:     time.Unix(ctr.Created, 0).UTC(),
			Tracked:       m.tracker != nil && m.tracker.Has(project, cardID),
		})
	}

	return result, nil
}

// ForceRemoveByLabels is the /kill fallback path: when the tracker has no
// entry for (project, cardID) but Docker still holds a labeled container, we
// bypass the tracker-driven cancel flow entirely and go straight to
// docker rm -f. The only sane way to get here is tracker/Docker divergence
// (a prior cleanup returned early before removal succeeded) — in which case
// every additional layer that "properly" cancels the missing tracker entry
// is a no-op, and the container leaks unless we reach past them.
//
// Returns the number of containers removed. An error from any single removal
// is joined into the final error but does not stop the sweep over the rest
// of the matches.
func (m *Manager) ForceRemoveByLabels(ctx context.Context, project, cardID string) (int, error) {
	if project == "" || cardID == "" {
		return 0, fmt.Errorf("force-remove: project and card_id are both required")
	}

	args := filters.NewArgs()
	args.Add("label", LabelRunner+"=true")
	args.Add("label", LabelProject+"="+project)
	args.Add("label", LabelCardID+"="+cardID)

	containers, err := m.docker.ContainerList(ctx, container.ListOptions{
		Filters: args,
		All:     true,
	})
	if err != nil {
		return 0, fmt.Errorf("list containers by label: %w", err)
	}

	removed := 0

	var errs []error

	for _, ctr := range containers {
		idShort := truncateID(ctr.ID)

		m.logger.Info("force-removing container by label",
			"container_id", idShort,
			"card_id", cardID,
			"project", project,
		)

		rmCtx, cancel := withDockerCleanupTimeout(ctx)
		if err := m.docker.ContainerRemove(rmCtx, ctr.ID, container.RemoveOptions{Force: true}); err != nil {
			m.logger.Warn("force-remove by label failed",
				"container_id", idShort,
				"card_id", cardID,
				"project", project,
				"error", err,
			)

			errs = append(errs, fmt.Errorf("remove %s: %w", idShort, err))

			cancel()

			continue
		}

		cancel()

		removed++
	}

	return removed, errors.Join(errs...)
}

// CleanupOrphans removes any leftover containers from a previous runner crash.
// A container is "orphan" iff it is labeled LabelRunner=true in Docker AND
// has no corresponding entry in the in-memory tracker — i.e. the current
// runner process does not know about it. Containers that are actively
// tracked are skipped so the periodic maintenance sweep does not kill
// live work.
//
// Per-container Stop/Remove failures are logged individually and collected
// into the returned error via errors.Join so that callers can see which
// containers failed without aborting cleanup of the rest.
func (m *Manager) CleanupOrphans(ctx context.Context) error {
	containers, err := m.docker.ContainerList(ctx, container.ListOptions{
		Filters: filters.NewArgs(filters.Arg("label", LabelRunner+"=true")),
		All:     true,
	})
	if err != nil {
		return fmt.Errorf("list orphan containers: %w", err)
	}

	type ctrSpec struct {
		id      string
		project string
		cardID  string
	}

	orphans := make([]ctrSpec, 0, len(containers))
	skipped := 0

	for _, ctr := range containers {
		project := ctr.Labels[LabelProject]
		cardID := ctr.Labels[LabelCardID]

		if m.tracker != nil && project != "" && cardID != "" && m.tracker.Has(project, cardID) {
			skipped++

			continue
		}

		orphans = append(orphans, ctrSpec{id: ctr.ID, project: project, cardID: cardID})
	}

	var errs []error

	for _, o := range orphans {
		idShort := truncateID(o.id)
		m.logger.Info("cleaning up orphan container",
			"container_id", idShort,
			"card_id", o.cardID,
			"project", o.project,
		)

		// Each per-container Stop/Remove is bounded so one wedged
		// orphan can't stall cleanup of the rest. See CTXRUN-040.
		stopCtx, stopCancel := withDockerCleanupTimeout(ctx)
		stopTimeout := 5

		if stopErr := m.docker.ContainerStop(stopCtx, o.id, container.StopOptions{Timeout: &stopTimeout}); stopErr != nil {
			m.logger.Warn("orphan stop failed",
				"container_id", idShort,
				"card_id", o.cardID,
				"project", o.project,
				"error", stopErr,
			)

			errs = append(errs, fmt.Errorf("stop orphan %s: %w", idShort, stopErr))
		}

		stopCancel()

		rmCtx, rmCancel := withDockerCleanupTimeout(ctx)

		if rmErr := m.docker.ContainerRemove(rmCtx, o.id, container.RemoveOptions{Force: true}); rmErr != nil {
			m.logger.Warn("orphan remove failed",
				"container_id", idShort,
				"card_id", o.cardID,
				"project", o.project,
				"error", rmErr,
			)

			errs = append(errs, fmt.Errorf("remove orphan %s: %w", idShort, rmErr))
		}

		rmCancel()
	}

	if len(orphans) > 0 || skipped > 0 {
		m.logger.Info("orphan cleanup complete",
			"removed", len(orphans)-len(errs),
			"attempted", len(orphans),
			"tracked_skipped", skipped,
			"errors", len(errs),
		)
	}

	return errors.Join(errs...)
}

// ForceKillContainer is the shutdown-path backstop: stop + force-remove a
// container directly by ID, ignoring the tracker. Used by the main shutdown
// sequence's force-cleanup phase, after the normal Kill + mgr.Wait has
// already timed out. The caller must pass a bounded ctx.
func (m *Manager) ForceKillContainer(ctx context.Context, containerID string) error {
	grace := 0

	var errs []error

	if err := m.docker.ContainerStop(ctx, containerID, container.StopOptions{Timeout: &grace}); err != nil {
		errs = append(errs, fmt.Errorf("stop: %w", err))
	}

	if err := m.docker.ContainerRemove(ctx, containerID, container.RemoveOptions{Force: true}); err != nil {
		errs = append(errs, fmt.Errorf("remove: %w", err))
	}

	return errors.Join(errs...)
}

// PruneImages removes dangling images older than imagePruneMaxAge. Called
// from the periodic maintenance loop.
func (m *Manager) PruneImages(ctx context.Context) error {
	args := filters.NewArgs()
	args.Add("dangling", "true")
	args.Add("until", imagePruneMaxAge)

	report, err := m.docker.ImagesPrune(ctx, args)
	if err != nil {
		return fmt.Errorf("images prune: %w", err)
	}

	m.logger.Info("image prune complete",
		"deleted", len(report.ImagesDeleted),
		"space_reclaimed_bytes", report.SpaceReclaimed,
	)

	return nil
}
