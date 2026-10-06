// Package mcpserver exposes an agent's view of a memory space as MCP tools:
// remember writes to the agent's own repo, and recall searches the whole
// space through the appview.
package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/haileyok/cocoon/space"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/haileyok/engram-garden/internal/indexer"
	"github.com/haileyok/engram-garden/internal/spaceclient"
)

// Tools are one agent's memory tools.
type Tools struct {
	// Client acts as the agent's account.
	Client *spaceclient.Client
	// Space is the memory space URI.
	Space string
	// AppviewURL and AppviewDID locate the appview that indexes the space.
	AppviewURL string
	AppviewDID string
}

// Version is reported to MCP clients.
var Version = "0.1.0"

const instructions = `Engram Garden is a memory space shared by a group of agents. Use recall before starting work that may have been done or discussed before, and remember durable facts, decisions, preferences and lessons other agents (or a future you) would want. Write each memory so it stands on its own: say who/what/why, not "as discussed above". Other agents can read everything you remember, so never store secrets or credentials.`

// NewServer builds an MCP server with the memory tools.
func (t *Tools) NewServer() *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "engram-garden", Title: "Engram Garden", Version: Version}, &mcp.ServerOptions{Instructions: instructions})
	mcp.AddTool(s, &mcp.Tool{
		Name:        "remember",
		Description: "Store a memory in the shared memory space. It becomes searchable by every agent in the space within a few seconds.",
		Annotations: &mcp.ToolAnnotations{IdempotentHint: false},
	}, t.remember)
	mcp.AddTool(s, &mcp.Tool{
		Name:        "recall",
		Description: "Semantic search over every agent's memories in the shared space. Returns the closest matches first, each with a similarity from 0 to 1000.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, t.recall)
	mcp.AddTool(s, &mcp.Tool{
		Name:        "get_memory",
		Description: "Fetch one memory by its URI.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, t.getMemory)
	mcp.AddTool(s, &mcp.Tool{
		Name:        "list_memories",
		Description: "List memories newest first, optionally filtered by author or tags. Use the returned cursor for the next page.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, t.listMemories)
	mcp.AddTool(s, &mcp.Tool{
		Name:        "forget",
		Description: "Delete one of your own memories by URI. You can't delete memories other agents wrote.",
		Annotations: &mcp.ToolAnnotations{DestructiveHint: ptr(true), IdempotentHint: true},
	}, t.forget)
	return s
}

func ptr[T any](v T) *T { return &v }

// Memory is a memory as tools return it.
type Memory struct {
	URI        string   `json:"uri"`
	Author     string   `json:"author"`
	Text       string   `json:"text"`
	Tags       []string `json:"tags"`
	Source     string   `json:"source,omitempty"`
	CreatedAt  string   `json:"createdAt"`
	Similarity *int     `json:"similarity,omitempty"`
}

// ---- remember ----

type RememberIn struct {
	Text   string   `json:"text" jsonschema:"the memory, written to make sense on its own when recalled later without this conversation"`
	Tags   []string `json:"tags,omitempty" jsonschema:"short labels for filtering, such as a project, topic or kind (decision, preference, gotcha)"`
	Source string   `json:"source,omitempty" jsonschema:"where this came from: a URL, file path, PR or task"`
}

type RememberOut struct {
	URI string `json:"uri"`
	CID string `json:"cid"`
}

func (t *Tools) remember(ctx context.Context, _ *mcp.CallToolRequest, in RememberIn) (*mcp.CallToolResult, RememberOut, error) {
	text := strings.TrimSpace(in.Text)
	if text == "" {
		return nil, RememberOut{}, errors.New("text is required")
	}
	if len(text) > 30000 {
		return nil, RememberOut{}, errors.New("text is too long (max 30000 bytes); split it into several memories")
	}
	if len(in.Tags) > 16 {
		return nil, RememberOut{}, errors.New("at most 16 tags")
	}
	rec := map[string]any{
		"$type":     indexer.Collection,
		"text":      text,
		"createdAt": time.Now().UTC().Format("2006-01-02T15:04:05.000Z"),
	}
	var tags []string
	for _, tag := range in.Tags {
		if tag = strings.TrimSpace(tag); tag != "" {
			if len(tag) > 128 {
				return nil, RememberOut{}, fmt.Errorf("tag %q is too long", tag)
			}
			tags = append(tags, tag)
		}
	}
	if len(tags) > 0 {
		rec["tags"] = tags
	}
	if s := strings.TrimSpace(in.Source); s != "" {
		rec["source"] = s
	}
	var out RememberOut
	body := map[string]any{"space": t.Space, "repo": t.Client.DID().String(), "collection": indexer.Collection, "record": rec}
	if err := t.Client.Session.Post(ctx, "com.atproto.space.createRecord", body, &out); err != nil {
		return nil, out, fmt.Errorf("storing memory: %w", err)
	}
	return nil, out, nil
}

// ---- recall ----

type RecallIn struct {
	Query  string   `json:"query" jsonschema:"what to look for, in natural language"`
	Limit  int      `json:"limit,omitempty" jsonschema:"maximum results, 1-50 (default 10)"`
	Author string   `json:"author,omitempty" jsonschema:"only memories by this agent DID"`
	Tags   []string `json:"tags,omitempty" jsonschema:"only memories carrying all of these tags"`
	Since  string   `json:"since,omitempty" jsonschema:"only memories created at or after this RFC 3339 time"`
}

type MemoriesOut struct {
	Memories []Memory `json:"memories"`
	Cursor   string   `json:"cursor,omitempty"`
}

func (t *Tools) recall(ctx context.Context, _ *mcp.CallToolRequest, in RecallIn) (*mcp.CallToolResult, MemoriesOut, error) {
	if strings.TrimSpace(in.Query) == "" {
		return nil, MemoriesOut{}, errors.New("query is required")
	}
	params := url.Values{"space": {t.Space}, "q": {in.Query}}
	if in.Limit != 0 {
		params.Set("limit", strconv.Itoa(min(max(in.Limit, 1), 50)))
	}
	if in.Author != "" {
		params.Set("author", in.Author)
	}
	if in.Since != "" {
		params.Set("since", in.Since)
	}
	params["tags"] = in.Tags
	var out MemoriesOut
	err := t.query(ctx, "garden.engram.searchMemories", params, &out)
	return nil, normalize(out), err
}

// ---- get_memory ----

type GetIn struct {
	URI string `json:"uri" jsonschema:"the memory's URI, as returned by recall or remember"`
}

type GetOut struct {
	Memory Memory `json:"memory"`
}

func (t *Tools) getMemory(ctx context.Context, _ *mcp.CallToolRequest, in GetIn) (*mcp.CallToolResult, GetOut, error) {
	var out GetOut
	err := t.query(ctx, "garden.engram.getMemory", url.Values{"space": {t.Space}, "uri": {in.URI}}, &out)
	if out.Memory.Tags == nil {
		out.Memory.Tags = []string{}
	}
	return nil, out, err
}

// ---- list_memories ----

type ListIn struct {
	Limit  int      `json:"limit,omitempty" jsonschema:"maximum results, 1-100 (default 25)"`
	Cursor string   `json:"cursor,omitempty" jsonschema:"cursor from a previous call, for the next page"`
	Author string   `json:"author,omitempty" jsonschema:"only memories by this agent DID"`
	Tags   []string `json:"tags,omitempty" jsonschema:"only memories carrying all of these tags"`
}

func (t *Tools) listMemories(ctx context.Context, _ *mcp.CallToolRequest, in ListIn) (*mcp.CallToolResult, MemoriesOut, error) {
	params := url.Values{"space": {t.Space}}
	if in.Limit != 0 {
		params.Set("limit", strconv.Itoa(min(max(in.Limit, 1), 100)))
	}
	if in.Cursor != "" {
		params.Set("cursor", in.Cursor)
	}
	if in.Author != "" {
		params.Set("author", in.Author)
	}
	params["tags"] = in.Tags
	var out MemoriesOut
	err := t.query(ctx, "garden.engram.listMemories", params, &out)
	return nil, normalize(out), err
}

// ---- forget ----

type ForgetIn struct {
	URI string `json:"uri" jsonschema:"URI of one of your own memories"`
}

type ForgetOut struct {
	Deleted string `json:"deleted"`
}

func (t *Tools) forget(ctx context.Context, _ *mcp.CallToolRequest, in ForgetIn) (*mcp.CallToolResult, ForgetOut, error) {
	author, coll, rkey, err := t.parseURI(in.URI)
	if err != nil {
		return nil, ForgetOut{}, err
	}
	if author != t.Client.DID().String() {
		return nil, ForgetOut{}, fmt.Errorf("that memory belongs to %s; you can only forget your own", author)
	}
	if coll != indexer.Collection {
		return nil, ForgetOut{}, fmt.Errorf("not a memory: %s", in.URI)
	}
	body := map[string]any{"space": t.Space, "repo": author, "collection": coll, "rkey": rkey}
	if err := t.Client.Session.Post(ctx, "com.atproto.space.deleteRecord", body, nil); err != nil {
		return nil, ForgetOut{}, fmt.Errorf("deleting memory: %w", err)
	}
	return nil, ForgetOut{Deleted: in.URI}, nil
}

// parseURI splits a space record URI into author, collection and rkey.
func (t *Tools) parseURI(uri string) (string, string, string, error) {
	ref, err := space.ParseRef(t.Space)
	if err != nil {
		return "", "", "", err
	}
	rest, ok := strings.CutPrefix(uri, ref.String()+"/")
	parts := strings.Split(rest, "/")
	if !ok || len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return "", "", "", fmt.Errorf("%q is not a record URI in %s", uri, t.Space)
	}
	return parts[0], parts[1], parts[2], nil
}

func (t *Tools) query(ctx context.Context, nsid string, params url.Values, out any) error {
	err := t.Client.Query(ctx, t.AppviewURL, t.Space, t.AppviewDID, nsid, params, out)
	var xe *spaceclient.Error
	if errors.As(err, &xe) && xe.Message != "" {
		return fmt.Errorf("%s: %s", xe.Name, xe.Message)
	}
	return err
}

func normalize(out MemoriesOut) MemoriesOut {
	if out.Memories == nil {
		out.Memories = []Memory{}
	}
	for i := range out.Memories {
		if out.Memories[i].Tags == nil {
			out.Memories[i].Tags = []string{}
		}
	}
	return out
}
