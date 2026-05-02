package orchestrator

import "errors"

// ErrStopped signals that the FSM was halted by an inbound stop event.
var ErrStopped = errors.New("orchestrator: stopped")

// ErrPromoted signals that a HITL chat loop exited because the card was
// promoted to autonomous mid-run. The driver flips ExtendedState.Mode
// atomically before injecting the canned promotion chat message; the
// chat loop checks the mode at end-of-turn and returns this sentinel so
// the phase action can fall back to the autonomous code path.
var ErrPromoted = errors.New("orchestrator: chat loop exited on promotion to autonomous")
