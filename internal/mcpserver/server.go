// Package mcpserver exposes an agent's memory spaces as MCP tools. The
// tools are internal/agent's operations.
package mcpserver

import (
	"context"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/haileyok/engram-garden/internal/agent"
)

// Version is reported to MCP clients.
var Version = "0.4.0"

const instructions = `Engram Garden memory spaces are shared by groups of agents. Use recall before starting work that may have been done or discussed before, and remember durable facts, decisions, preferences and lessons other agents (or a future you) would want. Write each memory so it stands on its own: say who/what/why, not "as discussed above". Other agents in a space can read everything you remember there, so never store secrets or credentials. If a result's note or list_spaces says the appview can't read a space, what's stored there won't turn up in recall: tell the person, who can fix it by approving indexing (see index_space).`

// The tools' inputs and outputs are the agent's.
type (
	RememberIn    = agent.RememberIn
	RememberOut   = agent.RememberOut
	RecallIn      = agent.RecallIn
	MemoriesOut   = agent.MemoriesOut
	GetIn         = agent.GetIn
	GetOut        = agent.GetOut
	ListIn        = agent.ListIn
	ForgetIn      = agent.ForgetIn
	ForgetOut     = agent.ForgetOut
	ListSpacesIn  = agent.ListSpacesIn
	ListSpacesOut = agent.ListSpacesOut
)

// serverInstructions adds the spaces to the instructions, so the agent knows
// them without a call.
func serverInstructions(s *agent.Spaces) string {
	var b strings.Builder
	b.WriteString(instructions)
	def, _ := s.Settings.Default()
	if len(s.Settings.Spaces) > 1 {
		b.WriteString("\n\nYou use these spaces (pass a name as the space argument; list_spaces describes them):")
		for _, e := range s.Settings.Spaces {
			mark := ""
			if e.URI == def.URI {
				mark = " (default for remember and list_memories)"
			}
			fmt.Fprintf(&b, "\n- %s%s: %s", e.Name, mark, e.URI)
		}
		b.WriteString("\nrecall searches all of them unless you name one.")
	} else if len(s.Settings.Spaces) == 1 {
		fmt.Fprintf(&b, "\n\nYou use one space, %q (%s).", def.Name, def.URI)
	}
	return b.String()
}

// NewServer builds an MCP server with the agent's memory tools.
func NewServer(s *agent.Spaces, opts ...Option) *mcp.Server {
	var o options
	for _, opt := range opts {
		opt(&o)
	}
	srv := mcp.NewServer(&mcp.Implementation{Name: "engram-garden", Title: "Engram Garden", Version: Version},
		&mcp.ServerOptions{Instructions: serverInstructions(s)})
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "list_spaces",
		Description: "List the memory spaces you use, which one is the default, the embedding model each requires (memories and queries are embedded with exactly that model), and whether the appview can read it (indexing: granted, missing or lapsed; anything but granted means memories stored there aren't searchable, with a warning saying what to do). Also lists other spaces your account belongs to that aren't set up.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, handler(s.ListSpaces))
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "remember",
		Description: "Store a memory in a memory space (the default one unless you pass space). It becomes searchable by every agent in that space within a few seconds, unless the result's note says the appview can't read the space.",
		Annotations: &mcp.ToolAnnotations{IdempotentHint: false},
	}, handler(s.Remember))
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "recall",
		Description: "Semantic search over every agent's memories, in all your spaces unless you pass space. Returns the closest matches first, each with its space and a similarity from 0 to 1000.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, handler(o.queryLog.logged(s.Recall)))
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "get_memory",
		Description: "Fetch one memory by its URI.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, handler(s.Get))
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "list_memories",
		Description: "List a space's memories newest first (the default space unless you pass space), optionally filtered by author or tags. Use the returned cursor for the next page.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, handler(s.List))
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "forget",
		Description: "Delete one of your own memories by URI. You can't delete memories other agents wrote.",
		Annotations: &mcp.ToolAnnotations{DestructiveHint: ptr(true), IdempotentHint: true},
	}, handler(s.Forget))

	// Running spaces this account governs.
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "create_space",
		Description: "Create a new memory space governed by your account, and start using it. Optionally declare its embedding model. Then add members and let the appview index it.",
		Annotations: &mcp.ToolAnnotations{IdempotentHint: false},
	}, handler(func(ctx context.Context, in agent.CreateSpaceIn) (agent.CreateSpaceOut, error) {
		out, err := s.CreateSpace(ctx, in)
		if err == nil {
			err = s.SaveSettings()
		}
		return out, err
	}))
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "list_members",
		Description: "List the members of a space you govern, and whether each can read and write.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, handler(s.ListMembers))
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "add_member",
		Description: "Add an account (handle or DID) to a space you govern, so its agents can recall and remember there; readOnly lets them only recall. Also changes an existing member's access.",
		Annotations: &mcp.ToolAnnotations{IdempotentHint: true},
	}, handler(s.AddMember))
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "remove_member",
		Description: "Remove an account from a space you govern. Its memories stop being searchable there.",
		Annotations: &mcp.ToolAnnotations{DestructiveHint: ptr(true), IdempotentHint: true},
	}, handler(s.RemoveMember))
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "set_model",
		Description: "Declare or change the embedding model of a space you govern (list_spaces shows the current one). declare sets it; next starts moving to another model while agents re-embed; promote finishes the move; cancel abandons it. The model must be installed in this machine's Ollama.",
		Annotations: &mcp.ToolAnnotations{IdempotentHint: true},
	}, handler(s.SetModel))
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "index_space",
		Description: "Check whether the appview may index a space you govern (state: granted, missing if its authority never approved, lapsed if the approval stopped working), and get the link to approve it (or, with stop, to stop it). Approving needs a person signed in as the space's account in a browser: give them the link.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, handler(s.IndexSpace))
	return srv
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
