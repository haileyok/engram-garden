package mcpserver

import (
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/haileyok/engram-garden/internal/agent"
)

// WithArchive adds the session tools, for an agent with its own identity:
// its own session's transcript, and every session in the archive.
func WithArchive(a *agent.Archive) Option {
	return func(o *options) { o.archive = a }
}

func addArchiveTools(srv *mcp.Server, a *agent.Archive) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "list_sessions",
		Description: "List the agent sessions in the archive, newest first: each one's agent (author DID), title, summary, status, kind (session or subagent) and parent, repo, branch, working directory, machine, and how much of its transcript is stored. author lists one agent's sessions.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, handler(a.ListSessions))
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "pull_session",
		Description: "Fetch a session's whole transcript (complete JSONL lines, as the harness wrote them) with its record, by session ID or record URI. For resuming a session; it can be megabytes.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, handler(a.PullSession))
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "push_session",
		Description: "Store this agent's session: what its transcript grew by since fromBytes (at most 8 MiB a push; replace starts it over), and the whole record (title, summary, status, cwd, machine, harness, model, repo, branch, gitHead). For the harness, not the model.",
		Annotations: &mcp.ToolAnnotations{IdempotentHint: false},
	}, handler(a.PushSession))
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "delete_session",
		Description: "Delete this agent's own session from the archive, transcript included.",
		Annotations: &mcp.ToolAnnotations{DestructiveHint: ptr(true), IdempotentHint: true},
	}, handler(a.DeleteSession))
}
