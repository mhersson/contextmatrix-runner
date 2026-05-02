// Package spawn defines the Worker abstraction over the worker-container
// lifecycle. Use the docker subpackage for the Docker-backed
// implementation; alternative backends (e.g., Kubernetes) plug in here.
package spawn

import (
	"context"
	"io"
	"time"
)

// WorkerSpec describes a worker container to spawn.
type WorkerSpec struct {
	Image  string
	Name   string
	Env    map[string]string
	Mounts []Mount
	// Labels are propagated to the container metadata so the runner's
	// label-aware management paths (ListManaged, ForceRemoveByLabels,
	// CleanupOrphans) can discover the container. Without these labels
	// every label-driven sweep silently no-ops and orphaned containers
	// leak on every runner crash.
	Labels      map[string]string
	NetworkMode string
	// ExtraHosts are appended to the container's /etc/hosts. Each entry
	// is "host:ip" or "host:host-gateway" (Docker resolves host-gateway
	// to the host's address). Required on Linux for `host.docker.internal`
	// to resolve from inside the container.
	ExtraHosts []string
	Resources  ResourceLimits
	// Security carries optional sandbox hardening for the spawned
	// container. A zero value falls back to Docker defaults; the
	// orchestrated dispatcher populates it with CapDrop=ALL,
	// no-new-privileges, and ReadonlyRootfs so a compromised Claude exec
	// runs without ambient privileges and cannot mutate the image
	// rootfs.
	Security SecuritySpec
	// Tmpfs mounts are anonymous in-memory filesystems mounted into the
	// container at the listed targets. Required when ReadonlyRootfs is
	// true so the container still has writable scratch space at /tmp,
	// $HOME/.cm-git-cred, $HOME/.claude, etc.
	Tmpfs []TmpfsMount
}

type Mount struct {
	Source   string
	Target   string
	ReadOnly bool
}

// TmpfsMount is an anonymous in-memory filesystem mount. Mode follows the
// `mode=` option syntax (e.g. "0700") if non-empty. UID/GID let the runner
// hand the mount to a non-root container user; the kernel default is
// root:root which would prevent the worker (UID 1000) from writing.
//
// Exec, when true, includes the "exec" mount option so the worker can run
// binaries it wrote into the tmpfs (e.g. `go test` build outputs under
// /workspace, npm-installed CLIs under /home/user). Docker's tmpfs default
// is noexec; without this flag the worker hits "Permission denied" when
// invoking newly-built test binaries.
type TmpfsMount struct {
	Target string
	Mode   string
	UID    int
	GID    int
	Exec   bool
}

// SecuritySpec captures the container-runtime sandbox knobs the runner
// applies to every spawned worker. All fields are independently optional;
// a zero value retains Docker defaults.
type SecuritySpec struct {
	// CapDrop is the list of Linux capabilities to drop. Setting it to
	// ["ALL"] removes every ambient capability the worker would
	// otherwise inherit (CAP_NET_RAW, CAP_DAC_OVERRIDE, …).
	CapDrop []string
	// SecurityOpt mirrors Docker's --security-opt list. The runner
	// supplies "no-new-privileges:true" so a setuid binary inside the
	// worker cannot escalate privileges via execve.
	SecurityOpt []string
	// ReadonlyRootfs makes the container's root filesystem read-only.
	// Caller MUST also supply Tmpfs mounts for any path the entrypoint
	// or Claude exec writes to (see Tmpfs above) — otherwise the
	// container fails on first write.
	ReadonlyRootfs bool
}

type ResourceLimits struct {
	MemoryBytes int64
	CPUShares   int64
	PIDs        int64
}

type ExecOptions struct {
	Cmd          []string
	Env          map[string]string
	WorkingDir   string
	AttachStdin  bool
	AttachStdout bool
	AttachStderr bool
}

type ExecResult struct {
	ExitCode int
	Stdout   string
	Stderr   string
	Duration time.Duration
}

type ExecStream interface {
	Stdout() io.ReadCloser
	Stdin() io.WriteCloser
	Wait(ctx context.Context) (exitCode int, err error)
	Kill() error
}

type WorkerStatus int

const (
	WorkerCreated WorkerStatus = iota
	WorkerRunning
	WorkerStopped
	WorkerRemoved
)
