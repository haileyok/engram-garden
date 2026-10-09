package agent

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/haileyok/cocoon/space"

	"github.com/haileyok/engram-garden/internal/embed"
	"github.com/haileyok/engram-garden/internal/lex"
	"github.com/haileyok/engram-garden/internal/spaceclient"
)

// Spaces is an agent's access to the memory spaces it uses: one account,
// embedding provider and appview, and an Agent for each space. Operations
// take a space by name or URI; an empty one is the default space, except
// that Recall searches every space.
type Spaces struct {
	Client     *spaceclient.Client
	AppviewURL string
	AppviewDID string
	Provider   embed.Provider
	Log        *slog.Logger
	// Settings name the spaces and the default.
	Settings Settings
	// NewAgent builds a space's Agent, when set; tests use it to put each
	// space on its own fake network.
	NewAgent func(SpaceEntry) *Agent
	// Save writes the settings back after a space is created; nil doesn't.
	Save func(Settings) error

	mu     sync.Mutex
	agents map[string]*Agent // by space URI
}

// SpacesOf puts agents for different spaces together, each keeping its own
// client (as when each space is on its own test network). The first agent's
// space is the default; the first agent's client lists the account's spaces.
func SpacesOf(agents ...*Agent) *Spaces {
	s := &Spaces{Client: agents[0].Client, AppviewURL: agents[0].AppviewURL, AppviewDID: agents[0].AppviewDID,
		Provider: agents[0].Provider, Log: agents[0].Log}
	byURI := map[string]*Agent{}
	for _, a := range agents {
		if _, err := s.Settings.AddSpace(a.Space, ""); err != nil {
			panic(err)
		}
		byURI[a.Space] = a
	}
	s.NewAgent = func(e SpaceEntry) *Agent {
		if a := byURI[e.URI]; a != nil {
			return a
		}
		return &Agent{Client: s.Client, Space: e.URI, AppviewURL: s.AppviewURL, AppviewDID: s.AppviewDID, Provider: s.Provider, Log: s.Log}
	}
	return s
}

// settings is a snapshot of Settings: CreateSpace may add a space while
// other operations run.
func (s *Spaces) settings() Settings {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.Settings
	c.Spaces = slices.Clone(s.Settings.Spaces)
	return c
}

// SaveSettings calls Save with a snapshot of the settings.
func (s *Spaces) SaveSettings() error {
	if s.Save == nil {
		return nil
	}
	return s.Save(s.settings())
}

// addSpace adds a space to Settings.
func (s *Spaces) addSpace(uri string) (SpaceEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Settings.AddSpace(uri, "")
}

// Agent returns the Agent for a space, by name or URI ("" is the default).
func (s *Spaces) Agent(nameOrURI string) (*Agent, SpaceEntry, error) {
	e, err := s.settings().Resolve(nameOrURI)
	if err != nil {
		return nil, e, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.agents == nil {
		s.agents = map[string]*Agent{}
	}
	a := s.agents[e.URI]
	if a == nil {
		if s.NewAgent != nil {
			a = s.NewAgent(e)
		} else {
			a = &Agent{Client: s.Client, Space: e.URI, AppviewURL: s.AppviewURL, AppviewDID: s.AppviewDID, Provider: s.Provider, Log: s.Log}
		}
		s.agents[e.URI] = a
	}
	return a, e, nil
}

func label(out MemoriesOut, name string) MemoriesOut {
	for i := range out.Memories {
		out.Memories[i].Space = name
	}
	return out
}

// spaceOfURI is the space a record URI is in:
// at://{authority}/space/{type}/{skey}/{author}/{collection}/{rkey}.
func spaceOfURI(uri string) (string, error) {
	rest, ok := strings.CutPrefix(uri, "at://")
	parts := strings.Split(rest, "/")
	if !ok || len(parts) != 7 || parts[1] != "space" {
		return "", fmt.Errorf("%q is not a record URI in a memory space", uri)
	}
	return "at://" + strings.Join(parts[:4], "/"), nil
}

// Remember stores a memory in a space (default: the default space).
func (s *Spaces) Remember(ctx context.Context, in RememberIn) (RememberOut, error) {
	a, e, err := s.Agent(in.Space)
	if err != nil {
		return RememberOut{}, err
	}
	// While the memory is embedded and stored, check the appview can read
	// the space: if it can't, the memory is stored and never searchable.
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	state := make(chan string, 1)
	go func() {
		st, _ := a.Indexing(cctx, false)
		state <- st
	}()
	out, err := a.Remember(ctx, in)
	if err != nil {
		return out, err
	}
	select {
	case st := <-state:
		if p := indexingProblem(a, e, st); p != "" {
			out.Note = "Stored, but " + p
		}
	case <-time.After(indexingCheckWait):
	}
	return out, nil
}

// indexingCheckWait is how long after a memory is stored to wait for the
// check that the appview can read its space.
const indexingCheckWait = 2 * time.Second

// indexingProblem says what's wrong when the appview can't read a space
// (given its access state), or returns "" when nothing is. a is an agent of
// the space.
func indexingProblem(a *Agent, e SpaceEntry, state string) string {
	if state == "" || state == "granted" {
		return ""
	}
	why := "its authority hasn't let the appview index it"
	if state == "lapsed" {
		why = "the appview's access to it has stopped working"
	}
	todo := "Its authority must approve indexing: in the web app, or with `engram index --space " + e.Name + "`."
	if ref, err := space.ParseRef(e.URI); err == nil {
		if ref.Authority == a.Client.DID().String() {
			todo = "This account governs it: run `engram index --space " + e.Name + "` (or use the index_space tool) and open the link it gives, signed in as this account."
		} else {
			todo = "Its authority, " + ref.Authority + ", must approve indexing: in the web app, or with `engram index --space " + e.Name + "`."
		}
	}
	return fmt.Sprintf("the appview can't read this space (%s: %s), so what's stored in it can't be found by searching. %s", state, why, todo)
}

// emptyNote explains an empty search: it may be that the appview can't read
// the space, rather than there being nothing in it.
func emptyNote(ctx context.Context, a *Agent, e SpaceEntry) string {
	ctx, cancel := context.WithTimeout(ctx, indexingCheckWait)
	defer cancel()
	st, err := a.Indexing(ctx, false)
	if err != nil {
		return ""
	}
	if p := indexingProblem(a, e, st); p != "" {
		return "Nothing came back, and " + p
	}
	return ""
}

// Recall searches one space or, by default (or with AllSpaces), every space
// set up, merging the results by similarity. Each space's query is embedded
// with that space's model.
func (s *Spaces) Recall(ctx context.Context, in RecallIn) (MemoriesOut, error) {
	if in.Space != "" && in.Space != AllSpaces {
		a, e, err := s.Agent(in.Space)
		if err != nil {
			return MemoriesOut{}, err
		}
		out, err := a.Recall(ctx, in)
		if err == nil && len(out.Memories) == 0 {
			out.Note = strings.TrimSpace(out.Note + " " + emptyNote(ctx, a, e))
		}
		return label(out, e.Name), err
	}
	if strings.TrimSpace(in.Query) == "" {
		return MemoriesOut{}, errors.New("query is required")
	}
	spaces := s.settings().Spaces
	if len(spaces) == 0 {
		return MemoriesOut{}, s.settings().Check()
	}
	type result struct {
		name string
		out  MemoriesOut
		err  error
		note string // why an empty result may be wrong
	}
	results := make([]result, len(spaces))
	var wg sync.WaitGroup
	for i, e := range spaces {
		wg.Go(func() {
			a, _, err := s.Agent(e.URI)
			if err == nil {
				results[i].out, err = a.Recall(ctx, in)
				if err == nil && len(results[i].out.Memories) == 0 {
					results[i].note = emptyNote(ctx, a, e)
				}
			}
			results[i].name, results[i].err = e.Name, err
		})
	}
	wg.Wait()
	var merged MemoriesOut
	var lists []MemoriesOut
	var failed, notes []string
	var firstErr error
	for _, r := range results {
		if r.err != nil {
			failed = append(failed, fmt.Sprintf("%s (%v)", r.name, Explain(r.err)))
			firstErr = cmp.Or(firstErr, r.err)
			continue
		}
		lists = append(lists, label(r.out, r.name))
		if r.out.Note != "" {
			notes = append(notes, r.name+": "+r.out.Note)
		}
		if r.note != "" {
			notes = append(notes, r.name+": "+r.note)
		}
	}
	if len(failed) == len(results) {
		if len(results) == 1 {
			return MemoriesOut{}, firstErr
		}
		return MemoriesOut{}, fmt.Errorf("recall failed in every space: %s", strings.Join(failed, "; "))
	}
	merged.Memories, merged.Mode = MergeRanked(lists)
	limit := in.Limit
	if limit <= 0 {
		limit = 10
	}
	if len(merged.Memories) > min(limit, 50) {
		merged.Memories = merged.Memories[:min(limit, 50)]
	}
	if len(failed) > 0 {
		notes = append(notes, "Couldn't search "+strings.Join(failed, "; ")+".")
	}
	merged.Note = strings.Join(notes, " ")
	return normalize(merged), nil
}

// MergeRanked combines several spaces' search results, each in its
// service's order, and reports the mode they ran in (empty if they
// differ). One space keeps its order. When every space ranked by vector
// alone, memories sort by similarity, as before. Otherwise they interleave
// by reciprocal rank fusion of their positions in their own space's
// results: hybrid and keyword scores from different spaces aren't
// comparable, and nor are cosines from different models.
func MergeRanked(lists []MemoriesOut) ([]Memory, string) {
	mode := ""
	allVector := true
	for i, l := range lists {
		if i == 0 {
			mode = l.Mode
		} else if l.Mode != mode {
			mode = ""
		}
		if l.Mode != "" && l.Mode != "vector" {
			allVector = false
		}
	}
	if len(lists) == 1 {
		return lists[0].Memories, mode
	}
	type ranked struct {
		m     Memory
		score float64
	}
	var all []ranked
	for _, l := range lists {
		for pos, m := range l.Memories {
			all = append(all, ranked{m, 1 / (60 + float64(pos+1))})
		}
	}
	sim := func(m Memory) int {
		if m.Similarity == nil {
			return 0
		}
		return *m.Similarity
	}
	slices.SortStableFunc(all, func(a, b ranked) int {
		if allVector {
			return cmp.Compare(sim(b.m), sim(a.m))
		}
		if c := cmp.Compare(b.score, a.score); c != 0 {
			return c
		}
		return cmp.Compare(sim(b.m), sim(a.m))
	})
	out := make([]Memory, len(all))
	for i, r := range all {
		out[i] = r.m
	}
	return out, mode
}

// Get fetches a memory by URI, from whichever space the URI is in.
func (s *Spaces) Get(ctx context.Context, in GetIn) (GetOut, error) {
	sp, err := spaceOfURI(in.URI)
	if err != nil {
		return GetOut{}, err
	}
	a, e, err := s.Agent(sp)
	if err != nil {
		return GetOut{}, err
	}
	out, err := a.Get(ctx, in)
	out.Memory.Space = e.Name
	return out, err
}

// List lists one space's memories, newest first (default: the default
// space).
func (s *Spaces) List(ctx context.Context, in ListIn) (MemoriesOut, error) {
	if in.Space == AllSpaces {
		return MemoriesOut{}, errors.New("list one space at a time; recall searches every space")
	}
	a, e, err := s.Agent(in.Space)
	if err != nil {
		return MemoriesOut{}, err
	}
	out, err := a.List(ctx, in)
	return label(out, e.Name), err
}

// Forget deletes one of the agent's own memories, by URI.
func (s *Spaces) Forget(ctx context.Context, in ForgetIn) (ForgetOut, error) {
	sp, err := spaceOfURI(in.URI)
	if err != nil {
		return ForgetOut{}, err
	}
	a, _, err := s.Agent(sp)
	if err != nil {
		return ForgetOut{}, err
	}
	return a.Forget(ctx, in)
}

// ---- list_spaces ----

// SpaceInfo describes a space for agents: how to refer to it, and the
// embedding model it requires.
type SpaceInfo struct {
	Name string `json:"name"`
	URI  string `json:"uri"`
	// Default marks the space used when none is given.
	Default bool `json:"default,omitempty"`
	// SetUp is false for a space the account belongs to that isn't in the
	// settings (add it with `engram use` or `engram spaces add`).
	SetUp bool `json:"setUp"`
	// Model is the embedding model the space requires: memories and queries
	// must be embedded with exactly this model.
	Model *lex.ModelInfo `json:"model,omitempty"`
	// NextModel is set while the space moves to another model.
	NextModel      *lex.ModelInfo `json:"nextModel,omitempty"`
	DocumentPrefix string         `json:"documentPrefix,omitempty"`
	QueryPrefix    string         `json:"queryPrefix,omitempty"`
	// Indexing is whether the appview may read the space, and so index what
	// agents store in it: granted, missing (its authority never let the
	// appview) or lapsed (it did, and that stopped working). Empty when the
	// appview doesn't say.
	Indexing string `json:"indexing,omitempty"`
	// Warning explains what to do when the appview can't read the space:
	// memories stored in it aren't searchable.
	Warning string `json:"warning,omitempty"`
	// LocalModel says whether this machine's embedding endpoint has the
	// model: "ready", or what's wrong.
	LocalModel string `json:"localModel,omitempty"`
	// Error explains a space whose model couldn't be read.
	Error string `json:"error,omitempty"`
}

type ListSpacesIn struct{}

type ListSpacesOut struct {
	Spaces []SpaceInfo `json:"spaces"`
	// Note explains a partial listing.
	Note string `json:"note,omitempty"`
}

// ListSpaces describes the spaces set up, with each one's model and whether
// this machine can embed with it, then any other spaces the account's PDS
// lists for it.
func (s *Spaces) ListSpaces(ctx context.Context, _ ListSpacesIn) (ListSpacesOut, error) {
	set := s.settings()
	def, _ := set.Default()
	infos := make([]SpaceInfo, len(set.Spaces))
	var wg sync.WaitGroup
	for i, e := range set.Spaces {
		wg.Go(func() {
			infos[i] = s.describe(ctx, e)
			infos[i].Default = e.URI == def.URI
		})
	}
	wg.Wait()
	out := ListSpacesOut{Spaces: infos}
	others, err := s.memberSpaces(ctx)
	if err != nil {
		out.Note = "Couldn't list other spaces this account belongs to: " + Explain(err).Error()
	}
	for _, uri := range others {
		if slices.ContainsFunc(infos, func(i SpaceInfo) bool { return i.URI == uri }) {
			continue
		}
		e, _ := set.Resolve(uri)
		out.Spaces = append(out.Spaces, SpaceInfo{Name: e.Name, URI: uri})
	}
	return out, nil
}

func (s *Spaces) describe(ctx context.Context, e SpaceEntry) SpaceInfo {
	info := SpaceInfo{Name: e.Name, URI: e.URI, SetUp: true}
	a, _, err := s.Agent(e.URI)
	if err != nil {
		info.Error = err.Error()
		return info
	}
	if st, err := a.Indexing(ctx, true); err == nil {
		info.Indexing = st
		if p := indexingProblem(a, e, st); p != "" {
			info.Warning = strings.ToUpper(p[:1]) + p[1:]
		}
	}
	cfg, err := a.Config(ctx, false)
	if err != nil {
		info.Error = Explain(err).Error()
		return info
	}
	m := cfg.ModelInfo
	info.Model, info.NextModel = &m, cfg.Next
	info.DocumentPrefix, info.QueryPrefix = cfg.DocumentPrefix, cfg.QueryPrefix
	if _, err := s.Provider.For(ctx, m); err != nil {
		info.LocalModel = err.Error()
	} else {
		info.LocalModel = "ready"
	}
	return info
}

// memberSpaces lists the memory spaces the account's PDS has for it.
func (s *Spaces) memberSpaces(ctx context.Context) ([]string, error) {
	var uris []string
	cursor := ""
	for range 10 {
		params := map[string]any{"spaceType": lex.SpaceType, "limit": 100}
		if cursor != "" {
			params["cursor"] = cursor
		}
		var out struct {
			Spaces []struct {
				URI string `json:"uri"`
			} `json:"spaces"`
			Cursor string `json:"cursor"`
		}
		if err := s.Client.Session.Get(ctx, "com.atproto.space.listSpaces", params, &out); err != nil {
			return uris, err
		}
		for _, sp := range out.Spaces {
			uris = append(uris, sp.URI)
		}
		if out.Cursor == "" || len(out.Spaces) == 0 {
			break
		}
		cursor = out.Cursor
	}
	return uris, nil
}

// Run warms every space set up and keeps this agent's memories in each
// embedded with the space's model(s), until ctx ends.
func (s *Spaces) Run(ctx context.Context, every time.Duration) {
	var wg sync.WaitGroup
	for _, e := range s.settings().Spaces {
		a, _, err := s.Agent(e.URI)
		if err != nil {
			continue
		}
		wg.Go(func() { a.Run(ctx, every) })
	}
	wg.Wait()
}
