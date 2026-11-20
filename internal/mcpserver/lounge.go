package mcpserver

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/haileyok/engram-garden/internal/agent"
)

// WithLounge adds the lounge tools, for an agent with its own identity: who's
// online, messages, and its own presence.
func WithLounge(l *agent.Lounge) Option {
	return func(o *options) { o.lounge = l }
}

// LoungeStatus says who the agent is in the lounge.
type LoungeStatus struct {
	DID        string `json:"did"`
	Lounge     string `json:"lounge"`
	Connection string `json:"connection"`
}

type empty struct{}

func addLoungeTools(srv *mcp.Server, l *agent.Lounge) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "lounge_status",
		Description: "Your own DID in the lounge, the lounge's URI, and whether the live stream is connected.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, handler(func(context.Context, empty) (LoungeStatus, error) {
		return LoungeStatus{DID: l.DID(), Lounge: l.Space, Connection: l.Connection()}, nil
	}))
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "list_agents",
		Description: "List the agents in the lounge: who's online (state working, idle or waiting on a person), what each is doing (title, activity, repo, branch, cwd), and which agent started each subagent (parent). self is your own DID.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, handler(l.Agents))
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "send_message",
		Description: "Send a message to other agents in the lounge (to: their DIDs; empty: everyone). kind is note (no answer needed), request (asks for an answer or work), reply (answers replyTo; goes to its author unless you set to) or handoff. Every member of the lounge can read every message, so never send secrets.",
		Annotations: &mcp.ToolAnnotations{IdempotentHint: false},
	}, handler(l.Send))
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "list_messages",
		Description: "List the lounge's messages for you (and to everyone), oldest first; all lists every message.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, handler(l.Messages))
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "set_presence",
		Description: "Change your presence in the lounge (state, title, activity, ...); empty fields keep their value. Written with a fresh lease, which is renewed while you run.",
		Annotations: &mcp.ToolAnnotations{IdempotentHint: true},
	}, handler(l.SetPresence))
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "lounge_events",
		Description: "Events received from the lounge after a seq (presence changes, messages for you, deleted messages), waiting up to wait seconds when there are none. next is the seq to ask after next time.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, handler(func(ctx context.Context, in agent.EventsIn) (agent.EventsOut, error) {
		return l.Events(ctx, in), nil
	}))
}
