package codex

import "encoding/json"

// Request and notification method names used by the adapter.
const (
	MethodInitialize    = "initialize"
	MethodInitialized   = "initialized"
	MethodThreadStart   = "thread/start"
	MethodThreadResume  = "thread/resume"
	MethodTurnStart     = "turn/start"
	MethodTurnInterrupt = "turn/interrupt"
	MethodTurnSteer     = "turn/steer"
	MethodModelList     = "model/list"
	MethodMCPStatusList = "mcpServerStatus/list"
)

// Notification method names emitted by app-server.
const (
	NotifyTurnStarted   = "turn/started"
	NotifyTurnCompleted = "turn/completed"
	NotifyItemStarted   = "item/started"
	NotifyItemCompleted = "item/completed"
	NotifyMessageDelta  = "item/agentMessage/delta"
	NotifyTokenUsage    = "thread/tokenUsage/updated"
	NotifyError         = "error"
)

// Server-to-client request method names. Every one of these blocks the
// turn until the client answers, so all five must be parked and answered.
const (
	ReqCommandApproval    = "item/commandExecution/requestApproval"
	ReqFileChangeApproval = "item/fileChange/requestApproval"
	ReqPermissionApproval = "item/permissions/requestApproval"
	ReqToolUserInput      = "item/tool/requestUserInput"
	ReqMCPElicitation     = "mcpServer/elicitation/request"
)

type ClientInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type InitializeCapabilities struct {
	ExperimentalAPI bool `json:"experimentalApi"`
}

type InitializeParams struct {
	ClientInfo   ClientInfo             `json:"clientInfo"`
	Capabilities InitializeCapabilities `json:"capabilities"`
}

type ThreadStartParams struct {
	Cwd            string `json:"cwd,omitempty"`
	Model          string `json:"model,omitempty"`
	Sandbox        string `json:"sandbox,omitempty"`
	ApprovalPolicy string `json:"approvalPolicy,omitempty"`
	ServiceTier    string `json:"serviceTier,omitempty"`
	Personality    string `json:"personality,omitempty"`
}

type ThreadResumeParams struct {
	ThreadID       string `json:"threadId"`
	Cwd            string `json:"cwd,omitempty"`
	Sandbox        string `json:"sandbox,omitempty"`
	ApprovalPolicy string `json:"approvalPolicy,omitempty"`
	Model          string `json:"model,omitempty"`
}

type Thread struct {
	ID string `json:"id"`
}

type ThreadResult struct {
	Thread Thread `json:"thread"`
}

// UserInput is the text variant of the app-server UserInput union.
type UserInput struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func TextInput(s string) []UserInput { return []UserInput{{Type: "text", Text: s}} }

type TurnStartParams struct {
	ThreadID string      `json:"threadId"`
	Input    []UserInput `json:"input"`
	Model    string      `json:"model,omitempty"`
	Effort   string      `json:"effort,omitempty"`
	Cwd      string      `json:"cwd,omitempty"`
}

type TurnSteerParams struct {
	ThreadID       string      `json:"threadId"`
	ExpectedTurnID string      `json:"expectedTurnId"`
	Input          []UserInput `json:"input"`
}

type TurnInterruptParams struct {
	ThreadID string `json:"threadId"`
	TurnID   string `json:"turnId"`
}

// TurnItem is the subset of ThreadItem the adapter reads. The union has
// nineteen variants; these are the fields that say what a peer is doing.
type TurnItem struct {
	Type    string `json:"type"`
	ID      string `json:"id"`
	Text    string `json:"text"`
	Command string `json:"command"`
	Cwd     string `json:"cwd"`
	Server  string `json:"server"`
	Query   string `json:"query"`
	Path    string `json:"path"`
	Kind    string `json:"kind"`
	Status  string `json:"status"`
}

type Turn struct {
	ID     string     `json:"id"`
	Status string     `json:"status"`
	Items  []TurnItem `json:"items"`
	Error  any        `json:"error"`
}

type TurnResult struct {
	Turn Turn `json:"turn"`
}

// TurnNotification is the payload of turn/started and turn/completed.
type TurnNotification struct {
	ThreadID string `json:"threadId"`
	Turn     Turn   `json:"turn"`
}

// ItemNotification is the payload of item/started and item/completed.
type ItemNotification struct {
	ThreadID string   `json:"threadId"`
	TurnID   string   `json:"turnId"`
	Item     TurnItem `json:"item"`
}

// DeltaNotification is the payload of item/agentMessage/delta.
type DeltaNotification struct {
	ThreadID string `json:"threadId"`
	TurnID   string `json:"turnId"`
	Delta    string `json:"delta"`
}

type ModelInfo struct {
	ID                        string `json:"id"`
	Model                     string `json:"model"`
	DefaultReasoningEffort    string `json:"defaultReasoningEffort"`
	SupportedReasoningEfforts []struct {
		ReasoningEffort string `json:"reasoningEffort"`
	} `json:"supportedReasoningEfforts"`
	Hidden bool `json:"hidden"`
}

type ModelListResult struct {
	Data       []ModelInfo `json:"data"`
	NextCursor string      `json:"nextCursor"`
}

type MCPServerStatus struct {
	Name  string                     `json:"name"`
	Tools map[string]json.RawMessage `json:"tools"`
}

type MCPStatusListResult struct {
	Data []MCPServerStatus `json:"data"`
}

// Approval payloads. Only the fields the hub acts on are typed; the whole
// payload is carried through in Event.Raw.
type CommandApprovalParams struct {
	ThreadID                        string          `json:"threadId"`
	TurnID                          string          `json:"turnId"`
	ItemID                          string          `json:"itemId"`
	Command                         string          `json:"command"`
	Cwd                             string          `json:"cwd"`
	Reason                          string          `json:"reason"`
	ProposedExecpolicyAmendment     json.RawMessage `json:"proposedExecpolicyAmendment"`
	ProposedNetworkPolicyAmendments json.RawMessage `json:"proposedNetworkPolicyAmendments"`
}

type FileChangeApprovalParams struct {
	ThreadID  string `json:"threadId"`
	TurnID    string `json:"turnId"`
	ItemID    string `json:"itemId"`
	GrantRoot string `json:"grantRoot"`
	Reason    string `json:"reason"`
}

// TokenUsageBreakdown is one side of a thread/tokenUsage/updated payload.
type TokenUsageBreakdown struct {
	CacheWriteInputTokens int `json:"cacheWriteInputTokens"`
	CachedInputTokens     int `json:"cachedInputTokens"`
	InputTokens           int `json:"inputTokens"`
	OutputTokens          int `json:"outputTokens"`
	ReasoningOutputTokens int `json:"reasoningOutputTokens"`
	TotalTokens           int `json:"totalTokens"`
}

// TokenUsageNotification is the payload of thread/tokenUsage/updated.
// `last` covers the turn that just ran; `total` is cumulative for the
// thread, which is why the adapter reports `last` against a turn.
type TokenUsageNotification struct {
	ThreadID   string `json:"threadId"`
	TurnID     string `json:"turnId"`
	TokenUsage struct {
		Last               TokenUsageBreakdown `json:"last"`
		Total              TokenUsageBreakdown `json:"total"`
		ModelContextWindow int                 `json:"modelContextWindow"`
	} `json:"tokenUsage"`
}
