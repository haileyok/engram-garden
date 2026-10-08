// Package agent is one agent's view of a memory space, shared by the
// engram CLI and the engram-mcp server: Remember embeds a memory and writes
// it to the agent's own repo, and Recall embeds the query and searches the
// whole space through the appview. Embedding happens here, on the agent's
// side, with the model the space declares.
package agent

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

	"github.com/haileyok/engram-garden/internal/embed"
	"github.com/haileyok/engram-garden/internal/lex"
	"github.com/haileyok/engram-garden/internal/spaceclient"
	"github.com/haileyok/engram-garden/internal/vec"
)

// Agent is one agent's access to a memory space.
type Agent struct {
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

	mu         sync.Mutex
	cfg        *lex.Config
	cfgAt      time.Time
	indexing   string    // the appview's access when last asked
	indexingAt time.Time // when it was last asked
	reembeds   int       // memories rewritten with missing vectors, for tests
}

func (t *Agent) log() *slog.Logger {
	if t.Log == nil {
		return slog.Default()
	}
	return t.Log
}

// ---- the space's model ----

func (t *Agent) authority() (string, error) {
	ref, err := space.ParseRef(t.Space)
	if err != nil {
		return "", err
	}
	return ref.Authority, nil
}

// Config returns the space's declared model, from the authority's
// garden.engram.config record, cached for ConfigTTL.
func (t *Agent) Config(ctx context.Context, refresh bool) (*lex.Config, error) {
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
			return nil, errors.New("the space hasn't declared an embedding model yet: its authority needs to declare one (engram model --set <model>)")
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
func (t *Agent) embedOne(ctx context.Context, m lex.ModelInfo, text string) ([]float32, error) {
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
func (t *Agent) addEmbeddings(ctx context.Context, cfg *lex.Config, rec map[string]any, text string, tags []string) error {
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
	// Space names the memory space the memory is in.
	Space      string   `json:"space,omitempty"`
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
	Space  string   `json:"space,omitempty" jsonschema:"the space to store it in, by name or URI (default: the default space; see list_spaces)"`
}

type RememberOut struct {
	URI string `json:"uri"`
	CID string `json:"cid"`
	// Note says what to know about the memory, such as that it can't be
	// found by searching yet.
	Note string `json:"note,omitempty"`
}

// Remember embeds a memory and stores it in the agent's own repo.
func (t *Agent) Remember(ctx context.Context, in RememberIn) (RememberOut, error) {
	text := strings.TrimSpace(in.Text)
	if text == "" {
		return RememberOut{}, errors.New("text is required")
	}
	if len(text) > 30000 {
		return RememberOut{}, errors.New("text is too long (max 30000 bytes); split it into several memories")
	}
	if len(in.Tags) > 16 {
		return RememberOut{}, errors.New("at most 16 tags")
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
				return RememberOut{}, fmt.Errorf("tag %q is too long", tag)
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
		return RememberOut{}, err
	}
	if err := t.addEmbeddings(ctx, cfg, rec, text, tags); err != nil {
		return RememberOut{}, fmt.Errorf("not stored: %w", err)
	}
	var out RememberOut
	body := map[string]any{"space": t.Space, "repo": t.Client.DID().String(), "collection": lex.MemoryCollection, "record": rec}
	if err := t.Client.Session.Post(ctx, "com.atproto.space.createRecord", body, &out); err != nil {
		return out, fmt.Errorf("storing memory: %w", err)
	}
	return out, nil
}

// ---- recall ----

type RecallIn struct {
	Query  string   `json:"query" jsonschema:"what to look for, in natural language"`
	Limit  int      `json:"limit,omitempty" jsonschema:"maximum results, 1-50 (default 10)"`
	Author string   `json:"author,omitempty" jsonschema:"only memories by this agent DID"`
	Tags   []string `json:"tags,omitempty" jsonschema:"only memories carrying all of these tags"`
	Since  string   `json:"since,omitempty" jsonschema:"only memories created at or after this RFC 3339 time"`
	Space  string   `json:"space,omitempty" jsonschema:"search only this space, by name or URI (default: every space you use; see list_spaces)"`
}

type MemoriesOut struct {
	Memories []Memory `json:"memories"`
	Cursor   string   `json:"cursor,omitempty"`
	// Note explains results that may be less precise than usual.
	Note string `json:"note,omitempty"`
}

// Recall searches every agent's memories by meaning.
func (t *Agent) Recall(ctx context.Context, in RecallIn) (MemoriesOut, error) {
	if strings.TrimSpace(in.Query) == "" {
		return MemoriesOut{}, errors.New("query is required")
	}
	var out struct {
		MemoriesOut
		Approximate bool `json:"approximate"`
	}
	for attempt := range 2 {
		cfg, err := t.Config(ctx, attempt > 0)
		if err != nil {
			return MemoriesOut{}, err
		}
		v, err := t.embedOne(ctx, cfg.ModelInfo, cfg.QueryPrefix+in.Query)
		if err != nil {
			return MemoriesOut{}, err
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
			return MemoriesOut{}, unwrap(err)
		}
		break
	}
	res := normalize(out.MemoriesOut)
	if out.Approximate {
		res.Note = "The index was still loading, so these results are ranked coarsely and may be less precise. Recalling again shortly gives exact ranking."
	}
	return res, nil
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

// Get fetches one memory by URI.
func (t *Agent) Get(ctx context.Context, in GetIn) (GetOut, error) {
	var out GetOut
	err := t.query(ctx, "garden.engram.getMemory", url.Values{"space": {t.Space}, "uri": {in.URI}}, &out)
	if out.Memory.Tags == nil {
		out.Memory.Tags = []string{}
	}
	return out, unwrap(err)
}

// ---- list_memories ----

type ListIn struct {
	Limit  int      `json:"limit,omitempty" jsonschema:"maximum results, 1-100 (default 25)"`
	Cursor string   `json:"cursor,omitempty" jsonschema:"cursor from a previous call, for the next page"`
	Author string   `json:"author,omitempty" jsonschema:"only memories by this agent DID"`
	Tags   []string `json:"tags,omitempty" jsonschema:"only memories carrying all of these tags"`
	Space  string   `json:"space,omitempty" jsonschema:"the space to list, by name or URI (default: the default space; see list_spaces)"`
}

// List lists memories newest first.
func (t *Agent) List(ctx context.Context, in ListIn) (MemoriesOut, error) {
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
	return normalize(out), unwrap(err)
}

// ---- forget ----

type ForgetIn struct {
	URI string `json:"uri" jsonschema:"URI of one of your own memories"`
}

type ForgetOut struct {
	Deleted string `json:"deleted"`
}

// Forget deletes one of the agent's own memories.
func (t *Agent) Forget(ctx context.Context, in ForgetIn) (ForgetOut, error) {
	author, coll, rkey, err := t.parseURI(in.URI)
	if err != nil {
		return ForgetOut{}, err
	}
	if author != t.Client.DID().String() {
		return ForgetOut{}, fmt.Errorf("that memory belongs to %s; you can only forget your own", author)
	}
	if coll != lex.MemoryCollection {
		return ForgetOut{}, fmt.Errorf("not a memory: %s", in.URI)
	}
	body := map[string]any{"space": t.Space, "repo": author, "collection": coll, "rkey": rkey}
	if err := t.Client.Session.Post(ctx, "com.atproto.space.deleteRecord", body, nil); err != nil {
		return ForgetOut{}, fmt.Errorf("deleting memory: %w", err)
	}
	return ForgetOut{Deleted: in.URI}, nil
}

// parseURI splits a space record URI into author, collection and rkey.
func (t *Agent) parseURI(uri string) (string, string, string, error) {
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

func (t *Agent) query(ctx context.Context, nsid string, params url.Values, out any) error {
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

// ---- whether the appview can index the space ----

// grantedTTL is how long a space the appview may read is assumed to stay
// readable, so remembering doesn't ask every time.
const grantedTTL = 2 * time.Minute

// Indexing says whether the appview may read the space, and so index what's
// stored in it: "granted"; "missing" (its authority never let the appview);
// "lapsed" (it did, and the grant stopped working); or "" when the appview
// doesn't say (it isn't asking authorities for grants). Unless fresh, a
// recent "granted" is trusted. Any other answer is asked again each time, so
// approving takes effect at once.
func (t *Agent) Indexing(ctx context.Context, fresh bool) (string, error) {
	t.mu.Lock()
	if !fresh && t.indexing == "granted" && time.Since(t.indexingAt) < grantedTTL {
		t.mu.Unlock()
		return "granted", nil
	}
	t.mu.Unlock()
	var st struct {
		Access *struct {
			State string `json:"state"`
		} `json:"access"`
	}
	if err := t.query(ctx, "garden.engram.getSpaceStatus", url.Values{"space": {t.Space}}, &st); err != nil {
		return "", unwrap(err)
	}
	state := ""
	if st.Access != nil {
		state = st.Access.State
	}
	t.mu.Lock()
	t.indexing, t.indexingAt = state, time.Now()
	t.mu.Unlock()
	return state, nil
}

// ---- session start and model changes ----

// Warm asks the appview to start loading the space's index, so the first
// recall is fast.
func (t *Agent) Warm(ctx context.Context) error {
	return t.Client.Procedure(ctx, t.AppviewURL, t.Space, t.AppviewDID, "garden.engram.warmSpace", map[string]string{"space": t.Space}, nil)
}

// Reembed rewrites this agent's memories that lack a vector for the space's
// model or, during a model change, for the next model. It returns how many
// it rewrote.
func (t *Agent) Reembed(ctx context.Context) (int, error) {
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
func (t *Agent) Run(ctx context.Context, every time.Duration) {
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
