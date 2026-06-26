package webhook

import protocol "github.com/mhersson/contextmatrix-protocol"

// Wire DTOs are defined in contextmatrix-protocol; aliased so handlers,
// validators, and tests keep compiling unchanged. MessagePayload.IsChat
// travels with the protocol type.
type (
	TriggerPayload         = protocol.TriggerPayload
	KillPayload            = protocol.KillPayload
	StopAllPayload         = protocol.StopAllPayload
	MessagePayload         = protocol.MessagePayload
	PromotePayload         = protocol.PromotePayload
	EndSessionPayload      = protocol.EndSessionPayload
	SuccessResponse        = protocol.SuccessResponse
	ErrorResponse          = protocol.ErrorResponse
	CardKillResult         = protocol.CardKillResult
	ContainerListItem      = protocol.ContainerListItem
	ListContainersResponse = protocol.ListContainersResponse
	StopAllResponse        = protocol.StopAllResponse
	ChatStartPayload       = protocol.ChatStartPayload
	ChatResumeContext      = protocol.ChatResumeContext
	ChatResumeTurn         = protocol.ChatResumeTurn
	ChatEndPayload         = protocol.ChatEndPayload
	ChatStartResponse      = protocol.ChatStartResponse
	HealthResponse         = protocol.HealthResponse
)
