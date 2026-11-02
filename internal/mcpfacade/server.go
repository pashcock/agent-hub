// Package mcpfacade exposes the hub to orchestrator agents as MCP tools
// over streamable HTTP. Every handler returns without waiting on a peer.
package mcpfacade

import (
	"context"
	"crypto/subtle"
	"errors"
	"net/http"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/pashcock/agent-hub/internal/adapter"
	"github.com/pashcock/agent-hub/internal/hub"
	"github.com/pashcock/agent-hub/internal/store"
)

type Facade struct {
	h      *hub.Hub
	agents []string
	srv    *mcp.Server
}

func New(h *hub.Hub, agents []string) *Facade {
	f := &Facade{h: h, agents: agents}
	f.srv = mcp.NewServer(&mcp.Implementation{Name: "agent-hub", Version: "0.1.0"}, nil)
	f.register()
	return f
}

func (f *Facade) MCPServer() *mcp.Server { return f.srv }

func (f *Facade) HTTPHandler() http.Handler {
	return mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return f.srv }, nil)
}

// fail turns a Go error into an MCP error result. A bad argument is a tool
// error, never a transport error, so the caller's session stays healthy.
//
// It must be generic. Each handler returns its own concrete output type,
// so a non-generic version returning `any` does not compile at any call
// site.
func fail[T any](err error) (*mcp.CallToolResult, T, error) {
	var zero T
	return &mcp.CallToolResult{
		IsError: true,
		Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}},
	}, zero, nil
}

func ok[T any](v T) (*mcp.CallToolResult, T, error) {
	return nil, v, nil
}

// ---- input and output types ----

type listIn struct{}

type agentInfo struct {
	Availability      store.ProviderPause `json:"availability"`
	Name              string              `json:"name"`
	Steer             bool                `json:"steer"`
	SandboxedReadOnly bool                `json:"sandboxed_read_only"`
	Approvals         bool                `json:"approvals"`
}

type listOut struct {
	Agents []agentInfo `json:"agents"`
}

type capsIn struct {
	Agent string `json:"agent" jsonschema:"the peer agent name"`
	Cwd   string `json:"cwd,omitempty" jsonschema:"project directory to inspect"`
}

type capsOut struct {
	Steer      bool            `json:"steer"`
	Models     []string        `json:"models"`
	MCPServers []string        `json:"mcp_servers"`
	MCPTools   map[string]int  `json:"mcp_tools,omitempty"`
	MCPStatus  map[string]bool `json:"mcp_connected,omitempty"`
}

type modelsIn struct {
	Agent string `json:"agent" jsonschema:"the peer agent name"`
	Cwd   string `json:"cwd,omitempty" jsonschema:"project directory to inspect"`
}

type modelInfo struct {
	ID            string   `json:"id"`
	ResolvedModel string   `json:"resolved_model,omitempty"`
	DefaultEffort string   `json:"default_effort"`
	Efforts       []string `json:"efforts"`
}

type modelsOut struct {
	Models []modelInfo `json:"models"`
}

type startIn struct {
	Agent   string `json:"agent" jsonschema:"peer agent: codex"`
	Task    string `json:"task" jsonschema:"what the peer should do"`
	Cwd     string `json:"cwd" jsonschema:"absolute project directory for the peer"`
	Mode    string `json:"mode,omitempty" jsonschema:"ask, plan, review or code; default ask"`
	Model   string `json:"model,omitempty" jsonschema:"exact provider model id"`
	Effort  string `json:"effort,omitempty" jsonschema:"low, medium, high, xhigh, max or ultra"`
	Sandbox string `json:"sandbox,omitempty" jsonschema:"override the sandbox derived from mode"`
	Context string `json:"context,omitempty" jsonschema:"compact context the peer cannot discover itself"`
	Queue   bool   `json:"queue,omitempty" jsonschema:"wait for a free slot instead of failing when the agent is at capacity"`
}

type startOut struct {
	ConversationID  string `json:"conversation_id"`
	TurnID          string `json:"turn_id"`
	NativeSessionID string `json:"native_session_id"`
	Status          string `json:"status"`
}

type sendIn struct {
	ConversationID string `json:"conversation_id"`
	Message        string `json:"message"`
	Model          string `json:"model,omitempty"`
	Effort         string `json:"effort,omitempty"`
	WaitS          int    `json:"wait_s,omitempty" jsonschema:"seconds to wait for the answer before handing back a handle; 0 returns immediately"`
}

type sendOut struct {
	Timings      *store.Timings              `json:"timings,omitempty"`
	Conversation *hub.ConversationAccounting `json:"conversation,omitempty"`
	TurnID       string                      `json:"turn_id"`
	Status       string                      `json:"status"`
	// The fields below are filled only when wait_s asked the hub to wait
	// and the turn finished within it.
	Result   string    `json:"result,omitempty"`
	Activity string    `json:"activity,omitempty"`
	Usage    *usageOut `json:"usage,omitempty"`
}

type pollIn struct {
	ConversationID string `json:"conversation_id"`
	Since          int64  `json:"since,omitempty" jsonschema:"event cursor from a previous poll"`
}

type approvalOut struct {
	ID string `json:"id"`
	// ConversationID is what the operator CLI needs to address the
	// request; the listing is the only place it is ever shown.
	ConversationID string   `json:"conversation_id,omitempty"`
	Class          string   `json:"class"`
	Command        string   `json:"command,omitempty"`
	Paths          []string `json:"paths,omitempty"`
	Cwd            string   `json:"cwd,omitempty"`
	Reason         string   `json:"reason,omitempty"`
	HumanGated     bool     `json:"human_gated"`
}

type pollOut struct {
	Conversation    *hub.ConversationAccounting `json:"conversation,omitempty"`
	Timings         *store.Timings              `json:"timings,omitempty"`
	ConversationID  string                      `json:"conversation_id"`
	Liveness        string                      `json:"liveness"`
	TurnID          string                      `json:"turn_id,omitempty"`
	TurnStatus      string                      `json:"turn_status,omitempty"`
	Result          string                      `json:"result,omitempty"`
	Error           string                      `json:"error,omitempty"`
	EventCount      int                         `json:"event_count"`
	Cursor          int64                       `json:"cursor"`
	PendingApproval *approvalOut                `json:"pending_approval,omitempty"`
	Activity        string                      `json:"activity,omitempty"`
	Usage           *usageOut                   `json:"usage,omitempty"`
	SilentForS      int                         `json:"silent_for_s,omitempty"`
	Stalled         bool                        `json:"stalled,omitempty"`
	Note            string                      `json:"note,omitempty"`
}

type usageOut = hub.Consumption

type askIn struct {
	Agent    string `json:"agent"`
	Question string `json:"question"`
	Cwd      string `json:"cwd"`
	Model    string `json:"model,omitempty"`
	Effort   string `json:"effort,omitempty"`
	WaitS    int    `json:"wait_s,omitempty" jsonschema:"seconds to wait before handing back a handle; default 60"`
}

type steerIn struct {
	ConversationID string `json:"conversation_id"`
	Message        string `json:"message"`
}

type steerOut struct {
	Status string `json:"status" jsonschema:"steered or unsupported"`
	Note   string `json:"note,omitempty"`
}

type convIn struct {
	ConversationID string `json:"conversation_id"`
}

type statusOut struct {
	Status string `json:"status"`
}

type approveIn struct {
	ConversationID string `json:"conversation_id"`
	RequestID      string `json:"request_id"`
	Decision       string `json:"decision" jsonschema:"accept, acceptForSession, decline or cancel"`
	Answer         string `json:"answer,omitempty" jsonschema:"free text for a request that asks a question rather than for permission"`
}

type transcriptIn struct {
	ConversationID string `json:"conversation_id"`
	Detail         string `json:"detail,omitempty" jsonschema:"summary or full; default summary"`
}

type transcriptEntry struct {
	Role string `json:"role"`
	Text string `json:"text"`
}

type transcriptOut struct {
	Messages []transcriptEntry `json:"messages"`
}

// ---- registration ----

func (f *Facade) register() {
	mcp.AddTool(f.srv, &mcp.Tool{
		Name:        "peer_list",
		Description: "List peer coding agents available through Agent Hub and what they support.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ listIn) (*mcp.CallToolResult, listOut, error) {
		out := listOut{}
		for _, status := range f.h.AgentStatuses() {
			name := status.Name
			// Ask the adapter, never guess from the name, and never
			// spawn: this runs for every agent on every call.
			p, err := f.h.Profile(name)
			if err != nil {
				continue
			}
			out.Agents = append(out.Agents, agentInfo{
				Name: name, Steer: p.Steer, Availability: status.Availability,
				SandboxedReadOnly: p.SandboxedReadOnly, Approvals: p.Approvals,
			})
		}
		return ok(out)
	})

	mcp.AddTool(f.srv, &mcp.Tool{
		Name:        "peer_capabilities",
		Description: "Report a peer's models, its MCP servers in a directory, and whether it supports mid-turn steering.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in capsIn) (*mcp.CallToolResult, capsOut, error) {
		caps, err := f.h.Capabilities(ctx, in.Agent, in.Cwd)
		if err != nil {
			return fail[capsOut](err)
		}
		out := capsOut{Steer: caps.Steer, MCPServers: caps.MCPServers,
			MCPTools: caps.MCPTools, MCPStatus: caps.MCPConnected}
		for _, m := range caps.Models {
			out.Models = append(out.Models, m.ID)
		}
		return ok(out)
	})

	mcp.AddTool(f.srv, &mcp.Tool{
		Name:        "peer_models",
		Description: "Query the native peer model catalog, including aliases and their resolved model ids when reported. Efforts and defaults are provider-reported; empty means unknown. Availability depends on the client, account and project settings.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in modelsIn) (*mcp.CallToolResult, modelsOut, error) {
		caps, err := f.h.Capabilities(ctx, in.Agent, in.Cwd)
		if err != nil {
			return fail[modelsOut](err)
		}
		out := modelsOut{Models: []modelInfo{}}
		for _, m := range caps.Models {
			out.Models = append(out.Models, modelInfo{
				ID: m.ID, ResolvedModel: m.ResolvedModel,
				DefaultEffort: m.DefaultEffort, Efforts: m.Efforts})
		}
		return ok(out)
	})

	mcp.AddTool(f.srv, &mcp.Tool{
		Name: "peer_start",
		Description: "Open a conversation with a peer agent and send the first turn. " +
			"Returns immediately with a conversation id. Prefer background HTTP GET /api/c/<id>/wait on the hub listener for waiting; peer_poll is a status/event fallback. " +
			"If the agent is at capacity this fails; pass queue=true to have the hub " +
			"start it when a slot frees, or simply call again later.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in startIn) (*mcp.CallToolResult, startOut, error) {
		res, err := f.h.Start(ctx, hub.StartRequest{
			Agent: in.Agent, Task: in.Task, Cwd: in.Cwd, Mode: in.Mode,
			Model: in.Model, Effort: in.Effort, Sandbox: in.Sandbox,
			Context: in.Context, Queue: in.Queue, CallerKey: callerKey(req),
		})
		if err != nil {
			return fail[startOut](err)
		}
		status := res.Status
		if status == "" {
			status = "running"
		}
		return ok(startOut{
			ConversationID: res.ConversationID, TurnID: res.TurnID,
			NativeSessionID: res.NativeSessionID, Status: status,
		})
	})

	mcp.AddTool(f.srv, &mcp.Tool{
		Name: "peer_send",
		Description: "Continue an existing peer conversation in the same native session. " +
			"Returns immediately; prefer HTTP /api/c/<id>/wait for waiting, or pass " +
			"wait_s to wait for a short answer inline.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in sendIn) (*mcp.CallToolResult, sendOut, error) {
		turnID, err := f.h.Send(ctx, in.ConversationID, in.Message,
			adapter.TurnOptions{Model: in.Model, Effort: in.Effort})
		if err != nil {
			return fail[sendOut](err)
		}
		out := sendOut{TurnID: turnID, Status: "running"}
		if in.WaitS <= 0 {
			return ok(out)
		}
		// Waiting here saves the caller a poll loop for a short answer.
		// It is bounded, so a long turn still hands back a handle rather
		// than holding the caller's own turn open.
		res, werr := f.waitForTurn(ctx, in.ConversationID, in.WaitS)
		if werr != nil {
			return fail[sendOut](werr)
		}
		out.Status = res.TurnStatus
		out.Result = res.Result
		out.Activity = res.Activity
		out.Timings = res.Timings
		out.Conversation = res.Conversation
		if u := res.Usage; u != nil {
			v := hub.ConsumptionOf(res.Agent, *u)
			out.Usage = &v
		}
		return ok(out)
	})

	mcp.AddTool(f.srv, &mcp.Tool{
		Name: "peer_poll",
		Description: "Check status/events on demand. Prefer background HTTP /api/c/<id>/wait over repeated tool polling or monitor agents. Never restarts a dormant session; " +
			"pass the cursor from the previous poll to see only new events. " +
			"Reports how long the peer has been silent, flags a turn that has " +
			"gone quiet for too long, and once a turn is over reports what it " +
			"consumed. Includes conversation-wide reported token/cache totals, measured finished-turn time, and accounting coverage; native child usage inclusion is unverified.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in pollIn) (*mcp.CallToolResult, pollOut, error) {
		res, err := f.h.Poll(ctx, in.ConversationID, in.Since)
		if err != nil {
			return fail[pollOut](err)
		}
		return ok(toPollOut(res))
	})

	mcp.AddTool(f.srv, &mcp.Tool{
		Name: "peer_ask",
		Description: "Ask a peer one question and wait briefly for the answer. " +
			"If it takes longer than wait_s, returns a conversation id to wait on via HTTP /api/c/<id>/wait.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in askIn) (*mcp.CallToolResult, pollOut, error) {
		wait := in.WaitS
		if wait <= 0 {
			wait = 60
		}
		start, err := f.h.Start(ctx, hub.StartRequest{
			Agent: in.Agent, Task: in.Question, Cwd: in.Cwd, Mode: "ask",
			Model: in.Model, Effort: in.Effort, CallerKey: callerKey(req),
		})
		if err != nil {
			return fail[pollOut](err)
		}
		res, werr := f.waitForTurn(ctx, start.ConversationID, wait)
		if werr != nil {
			return fail[pollOut](werr)
		}
		return ok(toPollOut(res))
	})

	mcp.AddTool(f.srv, &mcp.Tool{
		Name: "peer_steer",
		Description: "Add context to a peer's turn that is already running. " +
			"Codex absorbs it mid-turn; agents that cannot report unsupported.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in steerIn) (*mcp.CallToolResult, steerOut, error) {
		err := f.h.Steer(ctx, in.ConversationID, in.Message)
		if errors.Is(err, adapter.ErrUnsupported) {
			return ok(steerOut{Status: "unsupported",
				Note: "this agent cannot take input mid-turn; wait for the " +
					"turn to finish and use peer_send"})
		}
		if err != nil {
			return fail[steerOut](err)
		}
		return ok(steerOut{Status: "steered"})
	})

	mcp.AddTool(f.srv, &mcp.Tool{
		Name:        "peer_cancel",
		Description: "Stop a peer's running turn. Succeeds and does nothing if none is running.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in convIn) (*mcp.CallToolResult, statusOut, error) {
		if err := f.h.Cancel(ctx, in.ConversationID); err != nil {
			return fail[statusOut](err)
		}
		return ok(statusOut{Status: "cancelled"})
	})

	mcp.AddTool(f.srv, &mcp.Tool{
		Name: "peer_approve",
		Description: "Answer a permission request a peer raised. Pass answer " +
			"when the request asks a question rather than for permission. " +
			"Privilege escalation, writes outside the project, and policy changes are refused here and need the operator.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in approveIn) (*mcp.CallToolResult, statusOut, error) {
		if err := f.h.Approve(ctx, in.ConversationID, in.RequestID,
			adapter.Decision{Verb: in.Decision, Answer: in.Answer}); err != nil {
			return fail[statusOut](err)
		}
		return ok(statusOut{Status: "approved"})
	})

	mcp.AddTool(f.srv, &mcp.Tool{
		Name:        "peer_transcript",
		Description: "Return the observable dialogue of a peer conversation. Never restarts a dormant session.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in transcriptIn) (*mcp.CallToolResult, transcriptOut, error) {
		msgs, err := f.h.Transcript(ctx, in.ConversationID, in.Detail == "full")
		if err != nil {
			return fail[transcriptOut](err)
		}
		out := transcriptOut{}
		for _, m := range msgs {
			out.Messages = append(out.Messages, transcriptEntry{Role: m.Role, Text: m.Text})
		}
		return ok(out)
	})

	mcp.AddTool(f.srv, &mcp.Tool{
		Name:        "peer_close",
		Description: "End a peer conversation and release its process or thread.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in convIn) (*mcp.CallToolResult, statusOut, error) {
		if err := f.h.CloseConversation(ctx, in.ConversationID); err != nil {
			return fail[statusOut](err)
		}
		return ok(statusOut{Status: "closed"})
	})
}

// waitForTurn polls until the conversation's turn is terminal or the
// budget runs out, and returns whatever state it reached either way.
func (f *Facade) waitForTurn(ctx context.Context, convID string, waitS int) (hub.PollResult, error) {
	deadline := time.Now().Add(time.Duration(waitS) * time.Second)
	for {
		res, err := f.h.Poll(ctx, convID, 0)
		if err != nil {
			return hub.PollResult{}, err
		}
		switch res.TurnStatus {
		case store.TurnCompleted, store.TurnFailed,
			store.TurnInterrupted, store.TurnNeedsPermission:
			return res, nil
		}
		if time.Now().After(deadline) {
			return res, nil
		}
		select {
		case <-ctx.Done():
			return hub.PollResult{}, ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func toPollOut(res hub.PollResult) pollOut {
	out := pollOut{Timings: res.Timings, Conversation: res.Conversation,
		ConversationID: res.ConversationID, Liveness: res.Liveness,
		TurnID: res.TurnID, TurnStatus: res.TurnStatus,
		Result: res.Result, Error: res.Error,
		EventCount: len(res.Events), Cursor: res.Cursor,
		Activity:   res.Activity,
		SilentForS: res.SilentForS, Stalled: res.Stalled,
	}
	if u := res.Usage; u != nil {
		v := hub.ConsumptionOf(res.Agent, *u)
		out.Usage = &v
	}
	if res.Stalled {
		out.Note = "this peer has produced nothing for a while. It may be " +
			"working, or it may have hung. Decide whether to keep waiting " +
			"or call peer_cancel."
	}
	if p := res.PendingApproval; p != nil {
		out.PendingApproval = &approvalOut{
			ID: p.ID, ConversationID: p.ConversationID,
			Class: p.Class, Command: p.Command, Paths: p.Paths,
			Cwd: p.Cwd, Reason: p.Reason, HumanGated: p.HumanGated,
		}
	}
	return out
}

// callerKey identifies the orchestrator session so the parallel-turn limit
// applies per session rather than globally.
func callerKey(req *mcp.CallToolRequest) string {
	if req != nil && req.Session != nil {
		if id := req.Session.ID(); id != "" {
			return id
		}
	}
	return "anonymous"
}

// RequireToken rejects any request without the shared secret. Comparison
// is constant-time so the token cannot be recovered by timing.
func RequireToken(token string, next http.Handler) http.Handler {
	want := []byte("Bearer " + token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := []byte(r.Header.Get("Authorization"))
		if subtle.ConstantTimeCompare(got, want) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}
