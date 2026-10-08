package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/bluesky-social/indigo/atproto/atclient"
	"github.com/bluesky-social/indigo/atproto/atdata"
	"github.com/bluesky-social/indigo/atproto/syntax"
	"github.com/haileyok/cocoon/space"

	"github.com/haileyok/engram-garden/internal/embed"
	"github.com/haileyok/engram-garden/internal/lex"
)

// Running spaces: creating them, their members, their model and whether the
// appview indexes them. These act as the space's authority, so they need the
// account to be the one that created the space.

// manageErr adds what to do when the account's server refuses for lack of
// permission: sign-ins from before engram could run spaces don't have it.
func manageErr(what string, err error) error {
	var ae *atclient.APIError
	if errors.As(err, &ae) && (ae.StatusCode == http.StatusForbidden || ae.StatusCode == http.StatusUnauthorized ||
		strings.Contains(strings.ToLower(ae.Name+ae.Message), "scope")) {
		return fmt.Errorf("%s: %w (if this sign-in is from before engram could run spaces, run `engram login` again to grant it)", what, err)
	}
	return fmt.Errorf("%s: %w", what, Explain(err))
}

// owned resolves a space the account governs.
func (s *Spaces) owned(nameOrURI string) (SpaceEntry, space.Ref, error) {
	e, err := s.settings().Resolve(nameOrURI)
	if err != nil {
		return e, space.Ref{}, err
	}
	ref, err := space.ParseRef(e.URI)
	if err != nil {
		return e, ref, err
	}
	if me := s.Client.DID().String(); ref.Authority != me {
		return e, ref, fmt.Errorf("only the space's authority (%s) can do that; this account is %s", ref.Authority, me)
	}
	return e, ref, nil
}

// ---- create_space ----

type CreateSpaceIn struct {
	Name  string `json:"name" jsonschema:"the space's key, the last part of its URI: letters, digits and . _ ~ : -"`
	Model string `json:"model,omitempty" jsonschema:"declare this embedding model right away (an Ollama model name, e.g. nomic-embed-text); set_model can do it later"`
	// Dims is for the offline hashing provider, which has no model to probe.
	Dims int `json:"dims,omitempty" jsonschema:"vector size; only for the offline hashing provider"`
}

type CreateSpaceOut struct {
	Space SpaceEntry `json:"space"`
	// Model is the declared model, if one was.
	Model *lex.ModelInfo `json:"model,omitempty"`
	// Next says what's left before agents can use the space.
	Next string `json:"next"`
}

// CreateSpace creates a memory space governed by this account, adds it to
// the settings (the default, if it's the first), and optionally declares
// its model. The caller saves the settings.
func (s *Spaces) CreateSpace(ctx context.Context, in CreateSpaceIn) (CreateSpaceOut, error) {
	if _, err := syntax.ParseRecordKey(in.Name); err != nil || in.Name == "." || in.Name == ".." {
		return CreateSpaceOut{}, errors.New("names use letters, digits and . _ ~ : - (up to 512)")
	}
	member := map[string]string{"$type": "com.atproto.simplespace.defs#memberListPolicy"}
	body := map[string]any{"spaceType": lex.SpaceType, "skey": in.Name, "readPolicy": member, "writePolicy": member}
	var out struct {
		URI string `json:"uri"`
	}
	if err := s.Client.Session.Post(ctx, "com.atproto.simplespace.createSpace", body, &out); err != nil {
		return CreateSpaceOut{}, manageErr("creating the space", err)
	}
	e, err := s.addSpace(out.URI)
	if err != nil {
		return CreateSpaceOut{}, err
	}
	res := CreateSpaceOut{Space: e}
	if in.Model != "" {
		cfg, err := s.SetModel(ctx, SetModelIn{Space: e.URI, Action: string(lex.Declare), Model: in.Model, Dims: in.Dims})
		if err != nil {
			res.Next = fmt.Sprintf("The space exists, but declaring its model failed (%v): run set_model / engram model --set.", err)
			return res, nil
		}
		res.Model = cfg.Model
	}
	switch {
	case res.Model == nil:
		res.Next = "Declare its embedding model (set_model / engram model --set <model>), add members, and let the appview index it (index_space / engram index)."
	default:
		res.Next = "Add members (add_member / engram members add) and let the appview index it (index_space / engram index)."
	}
	return res, nil
}

// ---- members ----

type MembersIn struct {
	Space string `json:"space,omitempty" jsonschema:"a space you govern, by name or URI (default: the default space)"`
}

type Member struct {
	DID    string `json:"did"`
	Handle string `json:"handle,omitempty"`
	Read   bool   `json:"read"`
	Write  bool   `json:"write"`
}

type MembersOut struct {
	Space   string   `json:"space"`
	Members []Member `json:"members"`
}

// ListMembers lists a space's members, with their handles when they resolve.
func (s *Spaces) ListMembers(ctx context.Context, in MembersIn) (MembersOut, error) {
	e, ref, err := s.owned(in.Space)
	if err != nil {
		return MembersOut{}, err
	}
	out := MembersOut{Space: e.Name, Members: []Member{}}
	cursor := ""
	for range 50 {
		params := map[string]any{"space": ref.String(), "limit": 100}
		if cursor != "" {
			params["cursor"] = cursor
		}
		var page struct {
			Members []Member `json:"members"`
			Cursor  string   `json:"cursor"`
		}
		if err := s.Client.Session.Get(ctx, "com.atproto.simplespace.listMembers", params, &page); err != nil {
			return out, manageErr("listing members", err)
		}
		out.Members = append(out.Members, page.Members...)
		if page.Cursor == "" || len(page.Members) == 0 {
			break
		}
		cursor = page.Cursor
	}
	for i, m := range out.Members {
		if did, err := syntax.ParseDID(m.DID); err == nil && s.Client.Dir != nil {
			if ident, err := s.Client.Dir.LookupDID(ctx, did); err == nil && !ident.Handle.IsInvalidHandle() {
				out.Members[i].Handle = ident.Handle.String()
			}
		}
	}
	return out, nil
}

type AddMemberIn struct {
	Space  string `json:"space,omitempty" jsonschema:"a space you govern, by name or URI (default: the default space)"`
	Member string `json:"member" jsonschema:"the account's handle or DID"`
	// ReadOnly members can recall but not remember.
	ReadOnly bool `json:"readOnly,omitempty" jsonschema:"let them read but not write memories"`
}

type MemberOut struct {
	Space  string `json:"space"`
	Member Member `json:"member"`
}

func (s *Spaces) resolveMember(ctx context.Context, raw string) (string, error) {
	raw = strings.TrimPrefix(strings.TrimSpace(raw), "@")
	id, err := syntax.ParseAtIdentifier(raw)
	if err != nil {
		return "", fmt.Errorf("%q isn't a handle or DID", raw)
	}
	if id.IsDID() {
		return id.String(), nil
	}
	ident, err := s.Client.Dir.Lookup(ctx, id)
	if err != nil {
		return "", fmt.Errorf("couldn't resolve %s: %w", raw, err)
	}
	return ident.DID.String(), nil
}

// AddMember adds an account to a space, or changes what it may do there.
func (s *Spaces) AddMember(ctx context.Context, in AddMemberIn) (MemberOut, error) {
	e, ref, err := s.owned(in.Space)
	if err != nil {
		return MemberOut{}, err
	}
	did, err := s.resolveMember(ctx, in.Member)
	if err != nil {
		return MemberOut{}, err
	}
	m := Member{DID: did, Read: true, Write: !in.ReadOnly}
	if !strings.HasPrefix(in.Member, "did:") {
		m.Handle = strings.TrimPrefix(strings.TrimSpace(in.Member), "@")
	}
	body := map[string]any{"space": ref.String(), "did": did, "read": m.Read, "write": m.Write}
	if err := s.Client.Session.Post(ctx, "com.atproto.simplespace.putMember", body, nil); err != nil {
		return MemberOut{}, manageErr("adding the member", err)
	}
	return MemberOut{Space: e.Name, Member: m}, nil
}

type RemoveMemberIn struct {
	Space  string `json:"space,omitempty" jsonschema:"a space you govern, by name or URI (default: the default space)"`
	Member string `json:"member" jsonschema:"the account's handle or DID"`
}

// RemoveMember removes an account from a space. Its memories stay in its
// own repo, but the appview stops indexing them.
func (s *Spaces) RemoveMember(ctx context.Context, in RemoveMemberIn) (MemberOut, error) {
	e, ref, err := s.owned(in.Space)
	if err != nil {
		return MemberOut{}, err
	}
	did, err := s.resolveMember(ctx, in.Member)
	if err != nil {
		return MemberOut{}, err
	}
	if err := s.Client.Session.Post(ctx, "com.atproto.simplespace.removeMember", map[string]any{"space": ref.String(), "did": did}, nil); err != nil {
		return MemberOut{}, manageErr("removing the member", err)
	}
	return MemberOut{Space: e.Name, Member: Member{DID: did}}, nil
}

// ---- set_model ----

type SetModelIn struct {
	Space string `json:"space,omitempty" jsonschema:"a space you govern, by name or URI (default: the default space)"`
	// Action is declare, next, promote or cancel.
	Action string `json:"action" jsonschema:"declare (set the model), next (start moving to another model; agents re-embed in the background), promote (finish the move) or cancel (abandon it)"`
	Model  string `json:"model,omitempty" jsonschema:"the model, for declare and next: an Ollama model name such as nomic-embed-text"`
	Dims   int    `json:"dims,omitempty" jsonschema:"vector size; only for the offline hashing provider"`
	// The prefixes default to the model's convention.
	DocumentPrefix string `json:"documentPrefix,omitempty" jsonschema:"text put before memories when embedding (default: the model's convention)"`
	QueryPrefix    string `json:"queryPrefix,omitempty" jsonschema:"text put before queries when embedding (default: the model's convention)"`
}

type ModelOut struct {
	Space          string         `json:"space"`
	Model          *lex.ModelInfo `json:"model,omitempty"`
	NextModel      *lex.ModelInfo `json:"nextModel,omitempty"`
	DocumentPrefix string         `json:"documentPrefix,omitempty"`
	QueryPrefix    string         `json:"queryPrefix,omitempty"`
}

func modelOut(name string, cfg *lex.Config) ModelOut {
	out := ModelOut{Space: name}
	if cfg != nil {
		m := cfg.ModelInfo
		out.Model, out.NextModel = &m, cfg.Next
		out.DocumentPrefix, out.QueryPrefix = cfg.DocumentPrefix, cfg.QueryPrefix
	}
	return out
}

// DescribeModel identifies a local model exactly: its name, its digest (from
// Ollama, or the configured digest) and its dimensions (from embedding a
// probe). The offline hashing provider takes dims as given.
func (s Settings) DescribeModel(ctx context.Context, name string, dims int) (lex.ModelInfo, error) {
	if strings.TrimSpace(name) == "" {
		return lex.ModelInfo{}, errors.New("which model? e.g. nomic-embed-text")
	}
	if s.Embed.Provider == "hashing" {
		if dims <= 0 {
			return lex.ModelInfo{}, errors.New("dims is required with the hashing provider")
		}
		return lex.ModelInfo{Model: name, ModelDigest: embed.HashingDigest, Dims: dims}, nil
	}
	base := s.Embed.URL
	if base == "" {
		base = "http://localhost:11434/v1"
	}
	client := &http.Client{Timeout: 60 * time.Second}
	digest := s.Embed.Digest
	if digest == "" {
		d, err := embed.OllamaDigest(ctx, client, base, name)
		if err != nil {
			return lex.ModelInfo{}, err
		}
		if d == "" {
			return lex.ModelInfo{}, fmt.Errorf("%s isn't installed in Ollama: run ollama pull %s", name, name)
		}
		digest = d
	}
	n, err := embed.ProbeDims(ctx, base, s.Embed.APIKey, name, client)
	if err != nil {
		return lex.ModelInfo{}, fmt.Errorf("embedding a probe with %s: %w", name, err)
	}
	return lex.ModelInfo{Model: name, ModelDigest: digest, Dims: n}, nil
}

// currentConfig reads the space's config record from the authority's repo,
// or nil when there's none.
func (s *Spaces) currentConfig(ctx context.Context, ref space.Ref) (*lex.Config, error) {
	var out struct {
		Value json.RawMessage `json:"value"`
	}
	params := map[string]any{"space": ref.String(), "repo": ref.Authority, "collection": lex.ConfigCollection, "rkey": lex.ConfigRkey}
	if err := s.Client.Session.Get(ctx, "com.atproto.space.getRecord", params, &out); err != nil {
		var ae *atclient.APIError
		if errors.As(err, &ae) && (ae.Name == "RecordNotFound" || ae.Name == "RepoNotFound") {
			return nil, nil
		}
		return nil, manageErr("reading the space's model", err)
	}
	rec, err := atdata.UnmarshalJSON(out.Value)
	if err != nil {
		return nil, err
	}
	return lex.ParseConfig(rec)
}

// Model shows a space's model, from its authority's record.
func (s *Spaces) Model(ctx context.Context, in MembersIn) (ModelOut, error) {
	a, e, err := s.Agent(in.Space)
	if err != nil {
		return ModelOut{}, err
	}
	cfg, err := a.Config(ctx, true)
	if err != nil {
		return ModelOut{Space: e.Name}, err
	}
	return modelOut(e.Name, cfg), nil
}

// SetModel declares or changes a space's embedding model: declare, start
// moving to the next model, promote it, or cancel the move.
func (s *Spaces) SetModel(ctx context.Context, in SetModelIn) (ModelOut, error) {
	e, ref, err := s.owned(in.Space)
	if err != nil {
		return ModelOut{}, err
	}
	cur, err := s.currentConfig(ctx, ref)
	if err != nil {
		return ModelOut{}, err
	}
	action := lex.ConfigAction(in.Action)
	var m lex.ModelInfo
	switch action {
	case lex.Declare, lex.StartNext:
		if m, err = s.settings().DescribeModel(ctx, in.Model, in.Dims); err != nil {
			return ModelOut{}, err
		}
	case lex.Promote, lex.CancelNext:
	default:
		return ModelOut{}, fmt.Errorf("action must be %s, %s, %s or %s", lex.Declare, lex.StartNext, lex.Promote, lex.CancelNext)
	}
	cfg, err := lex.ChangeConfig(cur, action, m, in.DocumentPrefix, in.QueryPrefix)
	if err != nil {
		return ModelOut{}, err
	}
	body := map[string]any{
		"space": ref.String(), "repo": ref.Authority, "collection": lex.ConfigCollection, "rkey": lex.ConfigRkey,
		"record": cfg.Record(time.Now()),
	}
	if err := s.Client.Session.Post(ctx, "com.atproto.space.putRecord", body, nil); err != nil {
		return ModelOut{}, manageErr("writing the space's model", err)
	}
	// Reread it next time it's needed.
	if a, _, err := s.Agent(e.URI); err == nil {
		a.mu.Lock()
		a.cfg = nil
		a.mu.Unlock()
	}
	return modelOut(e.Name, &cfg), nil
}

// ---- index_space ----

type IndexIn struct {
	Space string `json:"space,omitempty" jsonschema:"a space you govern, by name or URI (default: the default space)"`
	Stop  bool   `json:"stop,omitempty" jsonschema:"stop the appview indexing it instead"`
}

type IndexOut struct {
	Space string `json:"space"`
	// State is the appview's access to the space: granted, missing (never
	// approved), lapsed (approved, and no longer working), or empty when it
	// can't say.
	State string `json:"state"`
	// Link is the appview's page where the authority approves (or stops)
	// indexing, signed in as the authority in a browser.
	Link string `json:"link,omitempty"`
	Note string `json:"note,omitempty"`
}

// IndexState reports whether the appview may read the space: granted,
// missing or lapsed, or empty when it doesn't say.
func (s *Spaces) IndexState(ctx context.Context, nameOrURI string) (string, error) {
	a, _, err := s.Agent(nameOrURI)
	if err != nil {
		return "", err
	}
	return a.Indexing(ctx, true)
}

// IndexSpace returns the appview's page for letting it index the space (or
// stopping it), and its current state. Approving takes the authority's
// browser, so an agent passes the link to its person.
func (s *Spaces) IndexSpace(ctx context.Context, in IndexIn) (IndexOut, error) {
	e, ref, err := s.owned(in.Space)
	if err != nil {
		return IndexOut{}, err
	}
	out := IndexOut{Space: e.Name}
	if out.State, err = s.IndexState(ctx, e.URI); err != nil {
		out.Note = "couldn't read the appview's access: " + err.Error()
	}
	if (out.State == "granted") != in.Stop {
		return out, nil // already as asked
	}
	grant, err := s.grantURL(ctx)
	if err != nil {
		return out, err
	}
	mode := "grant"
	if in.Stop {
		mode = "stop"
	}
	q := url.Values{"space": {ref.String()}, "mode": {mode}}
	out.Link = grant + "?" + q.Encode()
	out.Note = "Open the link in a browser and sign in as " + ref.Authority + " to approve; the state changes once you do."
	return out, nil
}

// grantURL asks the appview where its grant page is.
func (s *Spaces) grantURL(ctx context.Context) (string, error) {
	c := atclient.NewAPIClient(s.AppviewURL)
	if s.Client.HTTP != nil {
		c.Client = s.Client.HTTP
	}
	var out struct {
		GrantURL string `json:"grantUrl"`
	}
	if err := c.Get(ctx, "garden.engram.describeService", nil, &out); err != nil {
		return "", fmt.Errorf("asking the appview at %s: %w", s.AppviewURL, err)
	}
	if out.GrantURL == "" {
		return "", fmt.Errorf("the appview at %s doesn't take indexing grants", s.AppviewURL)
	}
	return out.GrantURL, nil
}
