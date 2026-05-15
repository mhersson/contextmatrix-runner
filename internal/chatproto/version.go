// Package chatproto holds constants shared between the chat webhook
// handler (CM's contract) and the container manager (host-side
// payload writer).
package chatproto

// PromptVersion identifies the rehydration prompt text version. Bumped
// when buildChatRehydrationPriming output changes meaningfully. CM
// reads resume.meta.json's prompt_version to decide whether a stale
// container's resume payload is still valid.
const PromptVersion = 1
