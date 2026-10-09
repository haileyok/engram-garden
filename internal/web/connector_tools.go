package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/haileyok/cocoon/space"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/haileyok/engram-garden/internal/agent"
	"github.com/haileyok/engram-garden/internal/lex"
	"github.com/haileyok/engram-garden/internal/mcpserver"
)

const connectorInstructions = `Engram Garden memory spaces are shared by groups of people and their agents. You can search and read them; you can't add to them or change them from here. Use recall before starting work that may have been discussed or decided before, and quote what you find rather than paraphrasing it. A memory is one person's or agent's note, written at a time: check its date and author before treating it as current. Don't repeat anything from a space that the person you're talking to hasn't asked about.`

// textQueryMax is the longest query the appview will embed.
const textQueryMax = 1000

// toolTimeout bounds one tool call.
const toolTimeout = 30 * time.Second

// mcpServer is the connector's MCP server. Its tools act as whoever's
// token the call came with.
func (s *Server) mcpServer() *mcp.Server {
	srv := mcp.NewServer(&mcp.Implementation{Name: "engram-garden", Title: "Engram Garden", Version: mcpserver.Version},
		&mcp.ServerOptions{Instructions: connectorInstructions})
	readOnly := &mcp.ToolAnnotations{ReadOnlyHint: true}
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "list_spaces",
		Description: "List the memory spaces you can search: each one's name, URI and whether you run it.",
		Annotations: readOnly,
	}, tool(s.toolListSpaces))
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "recall",
		Description: "Search everyone's memories by meaning and exact words together, in all your spaces unless you pass space. Describe what you're looking for in a sentence or two, and include any identifiers, names, error messages or paths you know: they're matched exactly. Returns the best matches first, each with its space, a similarity from 0 to 1000 when there is one, and a match explaining why it matched.",
		Annotations: readOnly,
	}, tool(s.toolRecall))
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "get_memory",
		Description: "Fetch one memory by its URI.",
		Annotations: readOnly,
	}, tool(s.toolGet))
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "list_memories",
		Description: "List a space's memories newest first, optionally filtered by author or tags. Pass space when you're in more than one. Use the returned cursor for the next page.",
		Annotations: readOnly,
	}, tool(s.toolList))
	return srv
}

// mcpServerFor serves every request from one server: the account is found
// from the request's token when a tool runs.
func (s *Server) mcpServerFor(*http.Request) *mcp.Server {
	s.mcpOnce.Do(func() { s.mcpSrv = s.mcpServer() })
	return s.mcpSrv
}

// tool adapts an operation that acts for a user to an MCP tool handler.
func tool[In, Out any](op func(context.Context, *user, In) (Out, error)) mcp.ToolHandlerFor[In, Out] {
	return func(ctx context.Context, req *mcp.CallToolRequest, in In) (*mcp.CallToolResult, Out, error) {
		var zero Out
		var u *user
		if req != nil && req.Extra != nil && req.Extra.TokenInfo != nil {
			u, _ = req.Extra.TokenInfo.Extra["user"].(*user)
		}
		if u == nil {
			return nil, zero, errors.New("not signed in")
		}
		ctx, cancel := context.WithTimeout(ctx, toolTimeout)
		defer cancel()
		out, err := op(ctx, u, in)
		if err != nil {
			e := upstreamErr(err)
			return nil, zero, fmt.Errorf("%s: %s", e.Name, e.Message)
		}
		return nil, out, nil
	}
}

// ---- the user's spaces ----

type spaceInfo struct {
	Name        string `json:"name"`
	URI         string `json:"uri"`
	Authority   string `json:"authority"`
	IsAuthority bool   `json:"isAuthority"`
}

// listSpaces lists the memory spaces the user's PDS knows them in: the ones
// they govern and the ones they've written to.
func (s *Server) listSpaces(ctx context.Context, u *user) ([]spaceInfo, error) {
	var out struct {
		Spaces []struct {
			URI string `json:"uri"`
		} `json:"spaces"`
	}
	if err := u.api.Get(ctx, "com.atproto.space.listSpaces", map[string]any{"spaceType": lex.SpaceType, "limit": 100}, &out); err != nil {
		return nil, err
	}
	spaces := []spaceInfo{}
	for _, sp := range out.Spaces {
		ref, err := space.ParseRef(sp.URI)
		if err != nil || ref.Type != lex.SpaceType {
			continue
		}
		spaces = append(spaces, spaceInfo{Name: ref.Skey, URI: ref.String(), Authority: ref.Authority, IsAuthority: ref.Authority == u.did.String()})
	}
	return spaces, nil
}

type spacesOut struct {
	Spaces []spaceInfo `json:"spaces"`
}

func (s *Server) toolListSpaces(ctx context.Context, u *user, _ struct{}) (spacesOut, error) {
	spaces, err := s.listSpaces(ctx, u)
	return spacesOut{Spaces: spaces}, err
}

// pickSpace finds a space by name or URI among the user's. A URI the list
// doesn't have is still accepted: the space host decides whether the user
// may read it.
func pickSpace(spaces []spaceInfo, nameOrURI string) (string, error) {
	if strings.HasPrefix(nameOrURI, "at://") {
		ref, e := memorySpace(nameOrURI)
		if e != nil {
			return "", e
		}
		return ref.String(), nil
	}
	var match []string
	for _, sp := range spaces {
		if sp.Name == nameOrURI {
			match = append(match, sp.URI)
		}
	}
	switch len(match) {
	case 0:
		return "", fmt.Errorf("no space named %q; list_spaces shows yours", nameOrURI)
	case 1:
		return match[0], nil
	}
	return "", fmt.Errorf("%d of your spaces are named %q; pass the URI of the one you mean", len(match), nameOrURI)
}

// onlySpace is the space to use when none was named: the user's one space.
func onlySpace(spaces []spaceInfo) (string, error) {
	switch len(spaces) {
	case 0:
		return "", errors.New("you aren't in any memory space")
	case 1:
		return spaces[0].URI, nil
	}
	names := make([]string, len(spaces))
	for i, sp := range spaces {
		names[i] = sp.Name
	}
	return "", fmt.Errorf("you're in several spaces (%s): pass space", strings.Join(names, ", "))
}

// ---- recall ----

type memoryResults struct {
	Memories    []agent.Memory `json:"memories"`
	Approximate bool           `json:"approximate"`
	Mode        string         `json:"mode"`
}

func (s *Server) searchSpace(ctx context.Context, u *user, spaceURI string, in agent.RecallIn, limit int) (memoryResults, error) {
	ref, e := memorySpace(spaceURI)
	if e != nil {
		return memoryResults{}, e
	}
	params := url.Values{"space": {ref.String()}, "q": {in.Query}, "limit": {strconv.Itoa(limit)}}
	if in.Author != "" {
		params.Set("author", in.Author)
	}
	if in.Since != "" {
		params.Set("since", in.Since)
	}
	if len(in.Tags) > 0 {
		params["tags"] = in.Tags
	}
	if in.Mode != "" {
		params.Set("mode", in.Mode)
	}
	var out memoryResults
	if err := s.appview(ctx, u, ref, http.MethodGet, "garden.engram.searchMemories", params, nil, &out); err != nil {
		return memoryResults{}, err
	}
	for i := range out.Memories {
		out.Memories[i].Space = ref.String()
	}
	return out, nil
}

func (s *Server) toolRecall(ctx context.Context, u *user, in agent.RecallIn) (agent.MemoriesOut, error) {
	if strings.TrimSpace(in.Query) == "" {
		return agent.MemoriesOut{}, errors.New("query is required")
	}
	if len(in.Query) > textQueryMax {
		return agent.MemoriesOut{}, fmt.Errorf("query is at most %d characters: describe what you're looking for briefly", textQueryMax)
	}
	limit := 10
	if in.Limit != 0 {
		limit = min(max(in.Limit, 1), 50)
	}
	spaces, err := s.listSpaces(ctx, u)
	if err != nil {
		return agent.MemoriesOut{}, err
	}
	targets := make([]string, 0, len(spaces))
	if in.Space != "" {
		t, err := pickSpace(spaces, in.Space)
		if err != nil {
			return agent.MemoriesOut{}, err
		}
		targets = append(targets, t)
	} else {
		for _, sp := range spaces {
			targets = append(targets, sp.URI)
		}
	}
	if len(targets) == 0 {
		return agent.MemoriesOut{Memories: []agent.Memory{}, Note: "You aren't in any memory space."}, nil
	}

	type result struct {
		memories    []agent.Memory
		approximate bool
		mode        string
		err         error
	}
	results := make([]result, len(targets))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 6)
	for i, t := range targets {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			r, err := s.searchSpace(ctx, u, t, in, limit)
			results[i] = result{r.Memories, r.Approximate, r.Mode, err}
		}()
	}
	wg.Wait()

	out := agent.MemoriesOut{Memories: []agent.Memory{}}
	var lists []agent.MemoriesOut
	var notes []string
	var firstErr error
	failed := 0
	approximate, keywordOnly := false, false
	for i, r := range results {
		if r.err != nil {
			failed++
			if firstErr == nil {
				firstErr = r.err
			}
			notes = append(notes, fmt.Sprintf("Couldn't search %s: %s.", targets[i], upstreamErr(r.err).Message))
			continue
		}
		approximate = approximate || r.approximate
		keywordOnly = keywordOnly || r.mode == "keyword" && in.Mode != "keyword"
		for j := range r.memories {
			if r.memories[j].Tags == nil {
				r.memories[j].Tags = []string{}
			}
		}
		lists = append(lists, agent.MemoriesOut{Memories: r.memories, Mode: r.mode})
	}
	if failed == len(targets) {
		return agent.MemoriesOut{}, firstErr
	}
	// Each space's results stay in the order the appview ranked them;
	// several spaces interleave by position (see agent.MergeRanked).
	if merged, mode := agent.MergeRanked(lists); merged != nil {
		out.Memories, out.Mode = merged, mode
	}
	if keywordOnly {
		notes = append(notes, "Some results matched by exact words only, because the appview couldn't embed the query for that space.")
	}
	if len(out.Memories) > limit {
		out.Memories = out.Memories[:limit]
	}
	if approximate {
		notes = append(notes, "The index was still loading, so some results are ranked coarsely and may be less precise. Recalling again shortly gives exact ranking.")
	}
	out.Note = strings.Join(notes, " ")
	return out, nil
}

// ---- get_memory and list_memories ----

// spaceOfMemoryURI is the space a memory's URI is in:
// at://{authority}/space/{type}/{skey}/{author}/{collection}/{rkey}.
func spaceOfMemoryURI(uri string) (string, error) {
	rest, ok := strings.CutPrefix(uri, "at://")
	parts := strings.Split(rest, "/")
	if !ok || len(parts) != 7 || parts[1] != "space" {
		return "", fmt.Errorf("%q is not a memory's URI", uri)
	}
	return "at://" + strings.Join(parts[:4], "/"), nil
}

func (s *Server) toolGet(ctx context.Context, u *user, in agent.GetIn) (agent.GetOut, error) {
	spaceURI, err := spaceOfMemoryURI(in.URI)
	if err != nil {
		return agent.GetOut{}, err
	}
	ref, e := memorySpace(spaceURI)
	if e != nil {
		return agent.GetOut{}, e
	}
	var out agent.GetOut
	if err := s.appview(ctx, u, ref, http.MethodGet, "garden.engram.getMemory", url.Values{"space": {ref.String()}, "uri": {in.URI}}, nil, &out); err != nil {
		return agent.GetOut{}, err
	}
	out.Memory.Space = ref.String()
	if out.Memory.Tags == nil {
		out.Memory.Tags = []string{}
	}
	return out, nil
}

func (s *Server) toolList(ctx context.Context, u *user, in agent.ListIn) (agent.MemoriesOut, error) {
	spaces, err := s.listSpaces(ctx, u)
	if err != nil {
		return agent.MemoriesOut{}, err
	}
	var spaceURI string
	if in.Space != "" {
		spaceURI, err = pickSpace(spaces, in.Space)
	} else {
		spaceURI, err = onlySpace(spaces)
	}
	if err != nil {
		return agent.MemoriesOut{}, err
	}
	ref, e := memorySpace(spaceURI)
	if e != nil {
		return agent.MemoriesOut{}, e
	}
	params := url.Values{"space": {ref.String()}}
	if in.Limit != 0 {
		params.Set("limit", strconv.Itoa(min(max(in.Limit, 1), 100)))
	}
	if in.Cursor != "" {
		params.Set("cursor", in.Cursor)
	}
	if in.Author != "" {
		params.Set("author", in.Author)
	}
	if len(in.Tags) > 0 {
		params["tags"] = in.Tags
	}
	var out agent.MemoriesOut
	if err := s.appview(ctx, u, ref, http.MethodGet, "garden.engram.listMemories", params, nil, &out); err != nil {
		return agent.MemoriesOut{}, err
	}
	if out.Memories == nil {
		out.Memories = []agent.Memory{}
	}
	for i := range out.Memories {
		out.Memories[i].Space = ref.String()
		if out.Memories[i].Tags == nil {
			out.Memories[i].Tags = []string{}
		}
	}
	return out, nil
}
