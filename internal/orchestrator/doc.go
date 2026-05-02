// Package orchestrator hosts the vectorsigma-generated state machine that
// drives the per-card lifecycle. The canonical design lives in
// orchestrator.md.
//
// To regenerate the FSM runtime after editing orchestrator.md, run from
// the repo root:
//
//	make gen-fsm
//
// vectorsigma rejects relative paths with leading `./` or `..` and requires
// the output to be a sub-directory of CWD, so a `//go:generate` directive
// inside this file cannot produce the right layout — the Makefile target
// runs from the repo root with `-o internal -p orchestrator`, which writes
// to internal/orchestrator/. Do not add a `//go:generate` line here that
// would silently produce a nested internal/orchestrator/orchestrator/.
package orchestrator
