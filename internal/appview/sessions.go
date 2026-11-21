package appview

import (
	"net/http"
	"strings"

	"github.com/haileyok/engram-garden/internal/indexer"
	"github.com/haileyok/engram-garden/internal/sessionindex"
	"github.com/haileyok/engram-garden/internal/spacestore"
)

// Archives (garden.engram.archive spaces) are indexed as passages of their
// sessions' transcripts (internal/sessionindex); searchSessions searches
// them. See docs/design/sessions.md.

// passageView is a search hit in an archive.
type passageView struct {
	// Session is the session record's URI, Author the agent's DID.
	Session   string `json:"session"`
	Author    string `json:"author"`
	SessionID string `json:"sessionId"`
	// Kind is turn, step, error, summary or session; Entry the transcript
	// entry it's anchored at, and Offset that entry's byte offset.
	Kind   string `json:"kind"`
	Entry  string `json:"entry,omitempty"`
	Offset int64  `json:"offset"`
	Text   string `json:"text"`
	// Facets: the session's repo, branch, kind (agent) and parent, and the
	// files and commands the passage touched.
	Repo      string   `json:"repo,omitempty"`
	Branch    string   `json:"branch,omitempty"`
	Agent     string   `json:"agent,omitempty"`
	Parent    string   `json:"parent,omitempty"`
	Files     []string `json:"files,omitempty"`
	Commands  []string `json:"commands,omitempty"`
	CreatedAt string   `json:"createdAt"`
	// Similarity is the cosine similarity to the query (0-1000), when a
	// vector ranked it.
	Similarity *int       `json:"similarity,omitempty"`
	Match      *matchView `json:"match,omitempty"`
}

func passage(m spacestore.Hit) passageView {
	uri, entry, off, _ := sessionindex.ParseSource(m.Source)
	session, kind, _ := strings.Cut(m.Rkey, ":")
	kind, _, _ = strings.Cut(kind, ":")
	v := passageView{
		Session: uri, Author: m.Author, SessionID: session, Kind: kind, Entry: entry, Offset: off,
		Text: m.Text, CreatedAt: m.CreatedAt.UTC().Format("2006-01-02T15:04:05.000Z"),
	}
	if entry == "record" {
		v.Entry = ""
	}
	for _, t := range m.Tags {
		switch {
		case strings.HasPrefix(t, sessionindex.TagRepo):
			v.Repo = strings.TrimPrefix(t, sessionindex.TagRepo)
		case strings.HasPrefix(t, sessionindex.TagBranch):
			v.Branch = strings.TrimPrefix(t, sessionindex.TagBranch)
		case strings.HasPrefix(t, sessionindex.TagAgent):
			v.Agent = strings.TrimPrefix(t, sessionindex.TagAgent)
		case strings.HasPrefix(t, sessionindex.TagParent):
			v.Parent = strings.TrimPrefix(t, sessionindex.TagParent)
		case strings.HasPrefix(t, sessionindex.TagFile):
			v.Files = append(v.Files, strings.TrimPrefix(t, sessionindex.TagFile))
		case strings.HasPrefix(t, sessionindex.TagCmd):
			v.Commands = append(v.Commands, strings.TrimPrefix(t, sessionindex.TagCmd))
		}
	}
	if m.HasSimilarity {
		sim := int(m.Similarity*1000 + 0.5)
		v.Similarity = &sim
	}
	return v
}

// handleSearchSessions searches an archive's passages. Filters: author,
// kind, repo, branch, session, file, cmd, agent (session or subagent),
// parent, since and until.
func (s *Server) handleSearchSessions(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	spaceURI := q.Get("space")
	if !indexer.IsArchive(spaceURI) {
		s.writeErr(w, errf(http.StatusBadRequest, "InvalidRequest", "%s isn't an archive", spaceURI))
		return
	}
	if err := s.authorizeReader(r, spaceURI, true); err != nil {
		s.writeErr(w, err)
		return
	}
	text := q.Get("q")
	if len(text) > 4000 {
		s.writeErr(w, errf(http.StatusBadRequest, "InvalidRequest", "q is at most 4000 characters"))
		return
	}
	limit, err := parseLimit(r, 10, 50)
	if err != nil {
		s.writeErr(w, err)
		return
	}
	f, err := parseFilter(r)
	if err != nil {
		s.writeErr(w, err)
		return
	}
	for param, prefix := range map[string]string{
		"kind": sessionindex.TagKind, "repo": sessionindex.TagRepo, "branch": sessionindex.TagBranch,
		"session": sessionindex.TagSession, "file": sessionindex.TagFile, "cmd": sessionindex.TagCmd,
		"agent": sessionindex.TagAgent, "parent": sessionindex.TagParent,
	} {
		v := strings.TrimSpace(q.Get(param))
		if v == "" {
			continue
		}
		if param == "repo" {
			v = sessionindex.NormalizeRepo(v)
		}
		f.Tags = append(f.Tags, prefix+v)
	}
	if len(f.Tags) > 16 {
		s.writeErr(w, errf(http.StatusBadRequest, "InvalidRequest", "at most 16 filters"))
		return
	}
	if strings.TrimSpace(text) == "" {
		// No query: the newest passages matching the filters.
		hits, _, err := s.Store.List(r.Context(), spaceURI, limit, "", f)
		if err != nil {
			s.writeErr(w, err)
			return
		}
		out := make([]passageView, len(hits))
		for i, h := range hits {
			out[i] = passage(h)
		}
		writeJSON(w, http.StatusOK, map[string]any{"passages": out, "mode": "list"})
		return
	}
	res, ran, err := s.runSearch(r, spaceURI, text, q.Get("mode"), limit, f)
	if err != nil {
		s.writeErr(w, err)
		return
	}
	out := make([]passageView, len(res.Hits))
	mv := make([]memoryView, len(res.Hits))
	for i, h := range res.Hits {
		out[i] = passage(h)
		mv[i] = view(spaceURI, h, h.HasSimilarity)
	}
	if ran != "vector" {
		explain(mv, res.Hits, text, ran, res.TermIDF)
		for i := range out {
			out[i].Match = mv[i].Match
		}
	}
	body := map[string]any{"passages": out, "mode": ran}
	if res.Approximate {
		body["approximate"] = true
	}
	writeJSON(w, http.StatusOK, body)
}
