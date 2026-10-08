// Package mcpserver exposes an agent's view of a memory space as MCP tools.
// The tools are internal/agent's operations.
package mcpserver

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/haileyok/engram-garden/internal/agent"
)

// Version is reported to MCP clients.
var Version = "0.3.0"

const instructions = `Engram Garden is a memory space shared by a group of agents. Use recall before starting work that may have been done or discussed before, and remember durable facts, decisions, preferences and lessons other agents (or a future you) would want. Write each memory so it stands on its own: say who/what/why, not "as discussed above". Other agents can read everything you remember, so never store secrets or credentials.`

// The tools' inputs and outputs are the agent's.
type (
	RememberIn  = agent.RememberIn
	RememberOut = agent.RememberOut
	RecallIn    = agent.RecallIn
	MemoriesOut = agent.MemoriesOut
	GetIn       = agent.GetIn
	GetOut      = agent.GetOut
	ListIn      = agent.ListIn
	ForgetIn    = agent.ForgetIn
	ForgetOut   = agent.ForgetOut
)

// NewServer builds an MCP server with the agent's memory tools.
func NewServer(a *agent.Agent) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "engram-garden", Title: "Engram Garden", Version: Version}, &mcp.ServerOptions{Instructions: instructions})
	mcp.AddTool(s, &mcp.Tool{
		Name:        "remember",
		Description: "Store a memory in the shared memory space. It becomes searchable by every agent in the space within a few seconds.",
		Annotations: &mcp.ToolAnnotations{IdempotentHint: false},
	}, handler(a.Remember))
	mcp.AddTool(s, &mcp.Tool{
		Name:        "recall",
		Description: "Semantic search over every agent's memories in the shared space. Returns the closest matches first, each with a similarity from 0 to 1000.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, handler(a.Recall))
	mcp.AddTool(s, &mcp.Tool{
		Name:        "get_memory",
		Description: "Fetch one memory by its URI.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, handler(a.Get))
	mcp.AddTool(s, &mcp.Tool{
		Name:        "list_memories",
		Description: "List memories newest first, optionally filtered by author or tags. Use the returned cursor for the next page.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, handler(a.List))
	mcp.AddTool(s, &mcp.Tool{
		Name:        "forget",
		Description: "Delete one of your own memories by URI. You can't delete memories other agents wrote.",
		Annotations: &mcp.ToolAnnotations{DestructiveHint: ptr(true), IdempotentHint: true},
	}, handler(a.Forget))
	return s
}

// handler adapts an agent operation to an MCP tool handler.
func handler[In, Out any](op func(context.Context, In) (Out, error)) mcp.ToolHandlerFor[In, Out] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in In) (*mcp.CallToolResult, Out, error) {
		out, err := op(ctx, in)
		if err != nil {
			err = agent.Explain(err)
		}
		return nil, out, err
	}
}

func ptr[T any](v T) *T { return &v }
