package spawn

import "context"

// Spawner creates, manages, and tears down worker containers.
type Spawner interface {
	Spawn(ctx context.Context, spec WorkerSpec) (Worker, error)
}

// Worker represents one running worker container.
type Worker interface {
	ID() string
	Exec(ctx context.Context, opts ExecOptions) (ExecResult, error)
	ExecStream(ctx context.Context, opts ExecOptions) (ExecStream, error)
	Stop(ctx context.Context) error
	Remove(ctx context.Context) error
	Status() WorkerStatus
}
