// Package mcpserver exposes an agent's view of a memory space as MCP tools:
// remember embeds a memory and writes it to the agent's own repo, and
// recall embeds the query and searches the whole space through the
// appview. Embedding happens here, on the agent's side, with the model the
// space declares.
package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bluesky-social/indigo/atproto/atdata"
	"github.com/haileyok/cocoon/space"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/haileyok/engram-garden/internal/embed"
	"github.com/haileyok/engram-garden/internal/lex"
	"github.com/haileyok/engram-garden/internal/spaceclient"
	"github.com/haileyok/engram-garden/internal/vec"
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
	// Provider embeds with the space's model, after checking the local
	// model matches it.
	Provider embed.Provider
	Log      *slog.Logger
	// ConfigTTL is how long the space's config is cached (default 5m).
	ConfigTTL time.Duration

	mu       sync.Mutex
	cfg      *lex.Config
	cfgAt    time.Time
	reembeds int // memories rewritten with missing vectors, for tests
}

// Version is reported to MCP clients.
var Version = "0.2.0"

const instructions = `Engram Garden is a memory space shared by a group of agents. Use recall before starting work that may have been done or discussed before, and remember durable facts, decisions, preferences and lessons other agents (or a future you) would want. Write each memory so it stands on its own: say who/what/why, not "as discussed above". Other agents can read everything you remember, so never store secrets or credentials.`

func (t *Tools) log() *slog.Logger {
	if t.Log == nil {
		return slog.Default()
	}
	return t.Log
}

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

// ---- the space's model ----

func (t *Tools) authority() (string, error) {
	ref, err := space.ParseRef(t.Space)
	if err != nil {
		return "", err
	}
	return ref.Authority, nil
}

// Config returns the space's declared model, from the authority's
// garden.engram.config record, cached for ConfigTTL.
func (t *Tools) Config(ctx context.Context, refresh bool) (*lex.Config, error) {
	ttl := t.ConfigTTL
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	t.mu.Lock()
	if !refresh && t.cfg != nil && time.Since(t.cfgAt) < ttl {
		c := t.cfg
		t.mu.Unlock()
		return c, nil
	}
	t.mu.Unlock()
	auth, err := t.authority()
	if err != nil {
		return nil, err
	}
	host, err := t.Client.PDSHost(ctx, auth)
	if err != nil {
		return nil, err
	}
	var out struct {
		Value json.RawMessage `json:"value"`
	}
	params := url.Values{"space": {t.Space}, "repo": {auth}, "collection": {lex.ConfigCollection}, "rkey": {lex.ConfigRkey}}
	if err := t.Client.Query(ctx, host, t.Space, auth, "com.atproto.space.getRecord", params, &out); err != nil {
		if spaceclient.IsError(err, "RecordNotFound") || spaceclient.IsError(err, "RepoNotFound") {
			return nil, errors.New("the space hasn't declared an embedding model yet: its authority needs to run engram-config")
		}
		return nil, fmt.Errorf("reading the space's config: %w", err)
	}
	rec, err := atdata.UnmarshalJSON(out.Value)
	if err != nil {
		return nil, fmt.Errorf("the space's config: %w", err)
	}
	c, err := lex.ParseConfig(rec)
	if err != nil {
		return nil, err
	}
	t.mu.Lock()
	t.cfg, t.cfgAt = c, time.Now()
	t.mu.Unlock()
	return c, nil
}

// embedOne embeds a text with a model, normalized.
func (t *Tools) embedOne(ctx context.Context, m lex.ModelInfo, text string) ([]float32, error) {
	e, err := t.Provider.For(ctx, m)
	if err != nil {
		return nil, err
	}
	vs, err := e.Embed(ctx, []string{text})
	if err != nil {
		return nil, fmt.Errorf("embedding: %w", err)
	}
	v := vs[0]
	if len(v) != m.Dims {
		return nil, fmt.Errorf("the model returned %d dimensions; the space expects %d", len(v), m.Dims)
	}
	if !vec.Normalize(v) {
		return nil, errors.New("the model returned a zero vector")
	}
	return v, nil
}

// addEmbeddings sets a memory record's vectors for the space's model and,
// during a model change, the next one. The next model is best effort: an
// agent without it still writes a memory the active index can use.
func (t *Tools) addEmbeddings(ctx context.Context, cfg *lex.Config, rec map[string]any, text string, tags []string) error {
	v, err := t.embedOne(ctx, cfg.ModelInfo, lex.EmbedText(cfg.DocumentPrefix, text, tags))
	if err != nil {
		return err
	}
	rec["embedding"] = lex.EmbeddingRecord(cfg.ModelInfo, v)
	delete(rec, "nextEmbedding")
	if cfg.Next != nil {
		nv, err := t.embedOne(ctx, *cfg.Next, lex.EmbedText(cfg.DocumentPrefix, text, tags))
		if err != nil {
			t.log().Warn("the space is moving to a new model this agent can't use; writing the current model's vector only", "next", cfg.Next, "err", err)
			return nil
		}
		rec["nextEmbedding"] = lex.EmbeddingRecord(*cfg.Next, nv)
	}
	return nil
}

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
		"$type":     lex.MemoryCollection,
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
	cfg, err := t.Config(ctx, false)
	if err != nil {
		return nil, RememberOut{}, err
	}
	if err := t.addEmbeddings(ctx, cfg, rec, text, tags); err != nil {
		return nil, RememberOut{}, fmt.Errorf("not stored: %w", err)
	}
	var out RememberOut
	body := map[string]any{"space": t.Space, "repo": t.Client.DID().String(), "collection": lex.MemoryCollection, "record": rec}
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
	// Note explains results that may be less precise than usual.
	Note string `json:"note,omitempty"`
}

func (t *Tools) recall(ctx context.Context, _ *mcp.CallToolRequest, in RecallIn) (*mcp.CallToolResult, MemoriesOut, error) {
	if strings.TrimSpace(in.Query) == "" {
		return nil, MemoriesOut{}, errors.New("query is required")
	}
	var out struct {
		MemoriesOut
		Approximate bool `json:"approximate"`
	}
	for attempt := range 2 {
		cfg, err := t.Config(ctx, attempt > 0)
		if err != nil {
			return nil, MemoriesOut{}, err
		}
		v, err := t.embedOne(ctx, cfg.ModelInfo, cfg.QueryPrefix+in.Query)
		if err != nil {
			return nil, MemoriesOut{}, err
		}
		params := url.Values{
			"space": {t.Space}, "q": {truncate(in.Query, 4000)}, "vector": {lex.EncodeQueryVector(v)},
			"model": {cfg.Model}, "modelDigest": {cfg.ModelDigest},
		}
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
		err = t.query(ctx, "garden.engram.searchMemories", params, &out)
		var xe *spaceclient.Error
		if attempt == 0 && errors.As(err, &xe) && xe.Name == "ModelMismatch" {
			continue // the space changed models: re-read the config
		}
		if err != nil {
			return nil, MemoriesOut{}, unwrap(err)
		}
		break
	}
	res := normalize(out.MemoriesOut)
	if out.Approximate {
		res.Note = "The index was still loading, so these results are ranked coarsely and may be less precise. Recalling again shortly gives exact ranking."
	}
	return nil, res, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
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
	return nil, out, unwrap(err)
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
	return nil, normalize(out), unwrap(err)
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
	if coll != lex.MemoryCollection {
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
	return t.Client.Query(ctx, t.AppviewURL, t.Space, t.AppviewDID, nsid, params, out)
}

// unwrap turns an XRPC error into a short message for the agent.
func unwrap(err error) error {
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

// ---- session start and model changes ----

// Warm asks the appview to start loading the space's index, so the first
// recall is fast.
func (t *Tools) Warm(ctx context.Context) error {
	return t.Client.Procedure(ctx, t.AppviewURL, t.Space, t.AppviewDID, "garden.engram.warmSpace", map[string]string{"space": t.Space}, nil)
}

// Reembed rewrites this agent's memories that lack a vector for the space's
// model or, during a model change, for the next model. It returns how many
// it rewrote.
func (t *Tools) Reembed(ctx context.Context) (int, error) {
	cfg, err := t.Config(ctx, true)
	if err != nil {
		return 0, err
	}
	want := []lex.ModelInfo{cfg.ModelInfo}
	if cfg.Next != nil {
		want = append(want, *cfg.Next)
	}
	me := t.Client.DID().String()
	rewritten := 0
	cursor := ""
	for {
		params := map[string]any{"space": t.Space, "repo": me, "collection": lex.MemoryCollection, "limit": 100}
		if cursor != "" {
			params["cursor"] = cursor
		}
		var page struct {
			Cursor  string `json:"cursor"`
			Records []struct {
				URI   string          `json:"uri"`
				Value json.RawMessage `json:"value"`
			} `json:"records"`
		}
		if err := t.Client.Session.Get(ctx, "com.atproto.space.listRecords", params, &page); err != nil {
			return rewritten, fmt.Errorf("listing my memories: %w", err)
		}
		for _, r := range page.Records {
			rec, err := atdata.UnmarshalJSON(r.Value)
			if err != nil {
				continue
			}
			have := map[string]bool{}
			for _, e := range lex.ParseEmbeddings(rec) {
				have[e.Key()] = true
			}
			missing := false
			for _, m := range want {
				missing = missing || !have[m.Key()]
			}
			if !missing {
				continue
			}
			text, _ := rec["text"].(string)
			if strings.TrimSpace(text) == "" {
				continue
			}
			var tags []string
			if ts, ok := rec["tags"].([]any); ok {
				for _, tg := range ts {
					if s, ok := tg.(string); ok {
						tags = append(tags, s)
					}
				}
			}
			// Rewrite from the JSON form, which keeps $bytes and $link
			// values intact.
			var out map[string]any
			if err := json.Unmarshal(r.Value, &out); err != nil {
				continue
			}
			if err := t.addEmbeddings(ctx, cfg, out, text, tags); err != nil {
				return rewritten, err
			}
			_, _, rkey, err := t.parseURI(r.URI)
			if err != nil {
				continue
			}
			body := map[string]any{"space": t.Space, "repo": me, "collection": lex.MemoryCollection, "rkey": rkey, "record": out}
			if err := t.Client.Session.Post(ctx, "com.atproto.space.putRecord", body, nil); err != nil {
				return rewritten, fmt.Errorf("rewriting %s: %w", r.URI, err)
			}
			rewritten++
		}
		if page.Cursor == "" || len(page.Records) == 0 {
			break
		}
		cursor = page.Cursor
	}
	t.mu.Lock()
	t.reembeds += rewritten
	t.mu.Unlock()
	return rewritten, nil
}

// Run warms the space, then keeps this agent's memories embedded with the
// space's model(s) until ctx ends.
func (t *Tools) Run(ctx context.Context, every time.Duration) {
	if err := t.Warm(ctx); err != nil {
		t.log().Debug("warming the space failed", "err", err)
	}
	for {
		if n, err := t.Reembed(ctx); err != nil {
			t.log().Warn("re-embedding memories failed", "err", err)
		} else if n > 0 {
			t.log().Info("re-embedded memories for the space's model", "count", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(every):
		}
	}
}
