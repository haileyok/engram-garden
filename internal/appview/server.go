// Package appview serves memory search over XRPC and receives the write
// notifications that keep the index current.
package appview

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bluesky-social/indigo/atproto/auth"
	"github.com/bluesky-social/indigo/atproto/identity"
	"github.com/bluesky-social/indigo/atproto/syntax"
	"github.com/haileyok/cocoon/space"

	"github.com/haileyok/engram-garden/internal/embed"
	"github.com/haileyok/engram-garden/internal/indexer"
	"github.com/haileyok/engram-garden/internal/spaceclient"
	"github.com/haileyok/engram-garden/internal/store"
)

// SyncerFragment names the DID document service that receives notifications.
const SyncerFragment = "atproto_space_syncer"

// Server is the appview's HTTP surface.
type Server struct {
	Store    *store.Store
	Embedder embed.Embedder
	Indexer  *indexer.Indexer
	Dir      identity.Directory
	Log      *slog.Logger

	// ServiceDID is this service's DID. Readers address their requests to it.
	ServiceDID string
	// PublicURL is where this service is reachable, for its did:web document.
	PublicURL string
	// Spaces are the spaces this service indexes.
	Spaces []string

	// Background work, so tests can wait for it.
	Jobs sync.WaitGroup

	syncing sync.Map // space -> struct{}: a space sync is running
}

// ServiceID is the identifier notifications are addressed to.
func (s *Server) ServiceID() string { return s.ServiceDID + "#" + SyncerFragment }

func (s *Server) log() *slog.Logger {
	if s.Log == nil {
		return slog.Default()
	}
	return s.Log
}

// Handler routes requests.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /xrpc/garden.engram.searchMemories", s.handleSearch)
	mux.HandleFunc("GET /xrpc/garden.engram.getMemory", s.handleGet)
	mux.HandleFunc("GET /xrpc/garden.engram.listMemories", s.handleList)
	mux.HandleFunc("POST /xrpc/com.atproto.space.notifyWrite", s.handleNotifyWrite)
	mux.HandleFunc("POST /xrpc/com.atproto.space.notifySpaceDeleted", s.handleNotifySpaceDeleted)
	mux.HandleFunc("GET /.well-known/did.json", s.handleDIDDoc)
	mux.HandleFunc("GET /_health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	return mux
}

// ---- errors ----

type xrpcError struct {
	status int
	name   string
	msg    string
}

func (e *xrpcError) Error() string { return e.name + ": " + e.msg }

func errf(status int, name, format string, args ...any) *xrpcError {
	return &xrpcError{status: status, name: name, msg: fmt.Sprintf(format, args...)}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) writeErr(w http.ResponseWriter, err error) {
	var xe *xrpcError
	if !errors.As(err, &xe) {
		s.log().Error("request failed", "err", err)
		xe = errf(http.StatusInternalServerError, "InternalServerError", "internal error")
	}
	writeJSON(w, xe.status, map[string]string{"error": xe.name, "message": xe.msg})
}

// ---- reader auth ----

// authorizeReader checks the request presents a valid credential for the
// space, signed for this service. A credential only proves its holder may
// read the space; it doesn't say who they are.
func (s *Server) authorizeReader(r *http.Request, spaceURI string) error {
	if spaceURI == "" {
		return errf(http.StatusBadRequest, "InvalidRequest", "space is required")
	}
	if !s.indexes(spaceURI) {
		return errf(http.StatusBadRequest, "UnknownSpace", "this service does not index %s", spaceURI)
	}
	h := r.Header
	for _, name := range []string{"Authorization", space.HeaderSpaceAudience} {
		if len(h.Values(name)) != 1 {
			return errf(http.StatusUnauthorized, "AuthRequired", "request requires exactly one %s header", strings.ToLower(name))
		}
	}
	jwt, ok := strings.CutPrefix(h.Get("Authorization"), "Atproto-Space ")
	if !ok || jwt == "" {
		return errf(http.StatusUnauthorized, "AuthRequired", "requires a space credential (Authorization: Atproto-Space ...)")
	}
	tok, err := space.VerifySpaceToken(space.TokenCredential, jwt, space.VerifyTokenOpts{
		GetSigningKey: spaceclient.KeyResolver(r.Context(), s.Dir),
		Sub:           spaceURI,
	})
	if err != nil {
		return errf(http.StatusUnauthorized, "InvalidToken", "bad space credential: %v", err)
	}
	ref, err := space.ParseRef(tok.Payload.Sub)
	if err != nil || tok.Payload.Iss != ref.Authority {
		return errf(http.StatusUnauthorized, "InvalidToken", "credential was not issued by the space authority")
	}
	if aud := h.Get(space.HeaderSpaceAudience); aud != s.ServiceDID {
		return errf(http.StatusUnauthorized, "BadSpaceSignature", "request is addressed to %q, not this service (%s)", aud, s.ServiceDID)
	}
	if _, err := space.VerifySpaceSignature(h, tok.Payload.Cnf.Kid); err != nil {
		return errf(http.StatusUnauthorized, "BadSpaceSignature", "%v", err)
	}
	if deleted, err := s.Store.SpaceDeleted(r.Context(), spaceURI); err != nil {
		return err
	} else if deleted {
		return errf(http.StatusBadRequest, "UnknownSpace", "space %s was deleted", spaceURI)
	}
	return nil
}

func (s *Server) indexes(spaceURI string) bool {
	for _, sp := range s.Spaces {
		if sp == spaceURI {
			return true
		}
	}
	return false
}

// ---- reads ----

type memoryView struct {
	URI        string   `json:"uri"`
	CID        string   `json:"cid"`
	Author     string   `json:"author"`
	Text       string   `json:"text"`
	Tags       []string `json:"tags"`
	Source     string   `json:"source,omitempty"`
	CreatedAt  string   `json:"createdAt"`
	IndexedAt  string   `json:"indexedAt"`
	Similarity *int     `json:"similarity,omitempty"`
}

func view(m store.Memory, withSimilarity bool) memoryView {
	v := memoryView{
		URI: m.URI, CID: m.CID, Author: m.Author, Text: m.Text, Tags: m.Tags, Source: m.Source,
		CreatedAt: m.CreatedAt.UTC().Format("2006-01-02T15:04:05.000Z"),
		IndexedAt: m.IndexedAt.UTC().Format("2006-01-02T15:04:05.000Z"),
	}
	if v.Tags == nil {
		v.Tags = []string{}
	}
	if withSimilarity {
		sim := int(max(0, min(1, m.Similarity)) * 1000)
		v.Similarity = &sim
	}
	return v
}

func parseFilter(r *http.Request) (store.Filter, error) {
	q := r.URL.Query()
	f := store.Filter{Author: q.Get("author"), Tags: q["tags"]}
	if f.Author != "" {
		if _, err := syntax.ParseDID(f.Author); err != nil {
			return f, errf(http.StatusBadRequest, "InvalidRequest", "author must be a DID")
		}
	}
	if len(f.Tags) > 16 {
		return f, errf(http.StatusBadRequest, "InvalidRequest", "at most 16 tags")
	}
	if since := q.Get("since"); since != "" {
		dt, err := syntax.ParseDatetimeLenient(since)
		if err != nil {
			return f, errf(http.StatusBadRequest, "InvalidRequest", "since must be a datetime")
		}
		f.Since = dt.Time()
	}
	return f, nil
}

func parseLimit(r *http.Request, def, maxLimit int) (int, error) {
	s := r.URL.Query().Get("limit")
	if s == "" {
		return def, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 || n > maxLimit {
		return 0, errf(http.StatusBadRequest, "InvalidRequest", "limit must be between 1 and %d", maxLimit)
	}
	return n, nil
}

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	spaceURI := q.Get("space")
	if err := s.authorizeReader(r, spaceURI); err != nil {
		s.writeErr(w, err)
		return
	}
	text := strings.TrimSpace(q.Get("q"))
	if text == "" || len(text) > 4000 {
		s.writeErr(w, errf(http.StatusBadRequest, "InvalidRequest", "q is required and at most 4000 characters"))
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
	vecs, err := s.Embedder.Embed(r.Context(), []string{text})
	if err != nil {
		s.writeErr(w, fmt.Errorf("embedding query: %w", err))
		return
	}
	ms, err := s.Store.Search(r.Context(), spaceURI, vecs[0], limit, f)
	if err != nil {
		s.writeErr(w, err)
		return
	}
	out := make([]memoryView, len(ms))
	for i, m := range ms {
		out[i] = view(m, true)
	}
	writeJSON(w, http.StatusOK, map[string]any{"memories": out})
}

func (s *Server) handleGet(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	spaceURI := q.Get("space")
	if err := s.authorizeReader(r, spaceURI); err != nil {
		s.writeErr(w, err)
		return
	}
	m, err := s.Store.Get(r.Context(), spaceURI, q.Get("uri"))
	if errors.Is(err, store.ErrNotFound) {
		s.writeErr(w, errf(http.StatusNotFound, "MemoryNotFound", "no memory %s", q.Get("uri")))
		return
	}
	if err != nil {
		s.writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"memory": view(m, false)})
}

func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	spaceURI := q.Get("space")
	if err := s.authorizeReader(r, spaceURI); err != nil {
		s.writeErr(w, err)
		return
	}
	limit, err := parseLimit(r, 25, 100)
	if err != nil {
		s.writeErr(w, err)
		return
	}
	f, err := parseFilter(r)
	if err != nil {
		s.writeErr(w, err)
		return
	}
	ms, cursor, err := s.Store.List(r.Context(), spaceURI, limit, q.Get("cursor"), f)
	if errors.Is(err, store.ErrBadCursor) {
		s.writeErr(w, errf(http.StatusBadRequest, "InvalidRequest", "bad cursor"))
		return
	}
	if err != nil {
		s.writeErr(w, err)
		return
	}
	out := make([]memoryView, len(ms))
	for i, m := range ms {
		out[i] = view(m, false)
	}
	res := map[string]any{"memories": out}
	if cursor != "" {
		res["cursor"] = cursor
	}
	writeJSON(w, http.StatusOK, res)
}

// ---- notifications ----

// authorizeAuthority checks a notification carries service auth from the
// space's authority, addressed to this service for the method.
func (s *Server) authorizeAuthority(r *http.Request, spaceURI, lxm string) error {
	if !s.indexes(spaceURI) {
		return errf(http.StatusBadRequest, "UnknownSpace", "this service does not index %s", spaceURI)
	}
	ref, err := space.ParseRef(spaceURI)
	if err != nil {
		return errf(http.StatusBadRequest, "InvalidRequest", "bad space: %v", err)
	}
	jwt, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return errf(http.StatusUnauthorized, "AuthRequired", "requires service auth")
	}
	nsid := syntax.NSID(lxm)
	v := auth.ServiceAuthValidator{Audience: s.ServiceID(), Dir: s.Dir}
	iss, err := v.Validate(r.Context(), jwt, &nsid)
	if err != nil {
		return errf(http.StatusUnauthorized, "InvalidToken", "%v", err)
	}
	if iss.String() != ref.Authority {
		return errf(http.StatusForbidden, "Forbidden", "only the space authority may notify")
	}
	return nil
}

func (s *Server) handleNotifyWrite(w http.ResponseWriter, r *http.Request) {
	var n indexer.Notification
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&n); err != nil || n.Space == "" || n.Repo == "" {
		s.writeErr(w, errf(http.StatusBadRequest, "InvalidRequest", "bad notification body"))
		return
	}
	if err := s.authorizeAuthority(r, n.Space, "com.atproto.space.notifyWrite"); err != nil {
		s.writeErr(w, err)
		return
	}
	// Sync in the background: embedding can be slow, and the periodic space
	// sync catches anything that fails here.
	s.Jobs.Add(1)
	go func() {
		defer s.Jobs.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		gap, err := s.Indexer.HandleWrite(ctx, n)
		if err != nil {
			s.log().Warn("notified sync failed", "space", n.Space, "repo", n.Repo, "err", err)
		}
		if gap {
			s.log().Info("missed notifications, syncing space", "space", n.Space)
			s.syncSpaceOnce(ctx, n.Space)
		}
	}()
	w.WriteHeader(http.StatusOK)
}

// syncSpaceOnce runs a space sync unless one is already running.
func (s *Server) syncSpaceOnce(ctx context.Context, spaceURI string) {
	if _, running := s.syncing.LoadOrStore(spaceURI, struct{}{}); running {
		return
	}
	defer s.syncing.Delete(spaceURI)
	if err := s.Indexer.SyncSpace(ctx, spaceURI); err != nil {
		s.log().Warn("space sync failed", "space", spaceURI, "err", err)
	}
}

func (s *Server) handleNotifySpaceDeleted(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Space string `json:"space"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&body); err != nil || body.Space == "" {
		s.writeErr(w, errf(http.StatusBadRequest, "InvalidRequest", "bad body"))
		return
	}
	if err := s.authorizeAuthority(r, body.Space, "com.atproto.space.notifySpaceDeleted"); err != nil {
		s.writeErr(w, err)
		return
	}
	if err := s.Store.MarkSpaceDeleted(r.Context(), body.Space); err != nil {
		s.writeErr(w, err)
		return
	}
	s.log().Info("space deleted", "space", body.Space)
	w.WriteHeader(http.StatusOK)
}

// ---- did:web ----

func (s *Server) handleDIDDoc(w http.ResponseWriter, r *http.Request) {
	if !strings.HasPrefix(s.ServiceDID, "did:web:") || s.PublicURL == "" {
		http.NotFound(w, r)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"@context": []string{"https://www.w3.org/ns/did/v1"},
		"id":       s.ServiceDID,
		"service": []map[string]string{{
			"id":              "#" + SyncerFragment,
			"type":            "AtprotoSpaceSyncer",
			"serviceEndpoint": s.PublicURL,
		}},
	})
}

// ---- background loops ----

// Run keeps the index current until ctx ends: it syncs every space at start
// and on each poll tick, and renews notification registrations when the
// service is publicly reachable.
func (s *Server) Run(ctx context.Context, poll time.Duration, register bool) {
	renewAt := map[string]time.Time{}
	tick := func() {
		for _, sp := range s.Spaces {
			if register && time.Now().After(renewAt[sp]) {
				exp, err := s.Indexer.Register(ctx, sp, s.ServiceID())
				if err != nil {
					s.log().Warn("registering for notifications failed", "space", sp, "err", err)
					renewAt[sp] = time.Now().Add(5 * time.Minute)
				} else {
					// Renew well before the registration lapses.
					renewAt[sp] = time.Now().Add(max(time.Minute, time.Until(exp)/2))
					s.log().Info("registered for notifications", "space", sp, "service", s.ServiceID(), "expires", exp)
				}
			}
			s.syncSpaceOnce(ctx, sp)
		}
	}
	tick()
	t := time.NewTicker(poll)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			tick()
		}
	}
}
