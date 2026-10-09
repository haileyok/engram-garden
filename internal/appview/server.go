// Package appview serves memory search over XRPC and receives the write
// notifications that keep the index current. Each space is owned by one
// node; other nodes forward its requests there.
package appview

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bluesky-social/indigo/atproto/auth"
	"github.com/bluesky-social/indigo/atproto/identity"
	"github.com/bluesky-social/indigo/atproto/syntax"
	"github.com/haileyok/cocoon/space"

	"github.com/haileyok/engram-garden/internal/control"
	"github.com/haileyok/engram-garden/internal/indexer"
	"github.com/haileyok/engram-garden/internal/lex"
	"github.com/haileyok/engram-garden/internal/metrics"
	"github.com/haileyok/engram-garden/internal/routing"
	"github.com/haileyok/engram-garden/internal/spaceclient"
	"github.com/haileyok/engram-garden/internal/spacestore"
)

// SyncerFragment names the DID document service that receives notifications.
const SyncerFragment = "atproto_space_syncer"

// ForwardedHeader marks a request one node forwarded to a space's owner.
const ForwardedHeader = "X-Engram-Forwarded-By"

// Server is the appview's HTTP surface.
type Server struct {
	Store   *spacestore.Node
	Indexer *indexer.Indexer
	Dir     identity.Directory
	Log     *slog.Logger

	// ServiceDID is this service's DID. Readers address their requests to it.
	ServiceDID string
	// PublicURL is where this service is reachable, for its did:web document.
	PublicURL string
	// Spaces are the spaces the operator configured. When OpenRegistration
	// is set, a space's authority can have more indexed by granting access.
	Spaces           []string
	OpenRegistration bool
	// Grants hold the authorities' grants that let the appview read their
	// spaces, and run the sign-ins that make them.
	Grants *Grants
	// ReturnOrigins are where a grant may send the browser back to.
	ReturnOrigins []string
	// CookieKey ties grant sign-ins to the browser that started them. It
	// must be the same on every node.
	CookieKey []byte
	// Ring decides which node owns each space. Nil means a single node.
	Ring *routing.Ring
	// DB, when set, holds the registered spaces. Grants have their own
	// handle to it, in Grants.
	DB control.Store
	// HTTP forwards requests to other nodes.
	HTTP *http.Client
	// TextSearch, when set, embeds the text of searches that come without
	// a vector. Nil requires callers to embed their own queries.
	TextSearch *QueryEmbedder

	// MaxSyncs caps notified syncs running at once across the node
	// (default 32), and MaxSpaceSyncs per space (default 2), so one busy
	// space can't take every slot.
	MaxSyncs      int
	MaxSpaceSyncs int

	// Background work, so tests can wait for it.
	Jobs sync.WaitGroup

	syncing  sync.Map // space -> struct{}: a space sync is running
	semOnce  sync.Once
	nodeSem  chan struct{}
	spaceSem sync.Map // space -> chan struct{}
	followUp sync.Map // space -> true: notifications arrived while at the cap
	// ungrantedLogged is when each space's missing grant was last logged.
	ungrantedLogged sync.Map // space -> time.Time

	regMu       sync.RWMutex
	registered  map[string]bool // spaces registered with registerSpace
	lastRefresh time.Time       // when registrations were last reread for an unknown space
	wakeOnce    sync.Once
	wakeCh      chan struct{} // wakes the background loop early
}

// wake is the channel that starts a background tick early.
func (s *Server) wake() chan struct{} {
	s.wakeOnce.Do(func() { s.wakeCh = make(chan struct{}, 1) })
	return s.wakeCh
}

// trySpaceSlot takes one of the space's notified-sync slots without
// waiting. When the space is at its cap, the caller records a follow-up
// instead of queueing, so waiting work is bounded by the number of spaces
// times the per-space cap, not by the notification rate.
func (s *Server) trySpaceSlot(spaceURI string) (func(), bool) {
	per := s.MaxSpaceSyncs
	if per <= 0 {
		per = 2
	}
	v, _ := s.spaceSem.LoadOrStore(spaceURI, make(chan struct{}, per))
	sp := v.(chan struct{})
	select {
	case sp <- struct{}{}:
		return func() { <-sp }, true
	default:
		return nil, false
	}
}

// acquireNodeSlot waits for one of the node's sync slots.
func (s *Server) acquireNodeSlot(ctx context.Context) (func(), error) {
	s.semOnce.Do(func() {
		n := s.MaxSyncs
		if n <= 0 {
			n = 32
		}
		s.nodeSem = make(chan struct{}, n)
	})
	select {
	case s.nodeSem <- struct{}{}:
		return func() { <-s.nodeSem }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// ServiceID is the identifier notifications are addressed to.
func (s *Server) ServiceID() string { return s.ServiceDID + "#" + SyncerFragment }

func (s *Server) log() *slog.Logger {
	if s.Log == nil {
		return slog.Default()
	}
	return s.Log
}

func (s *Server) ring() *routing.Ring {
	if s.Ring == nil {
		return routing.Single()
	}
	return s.Ring
}

// Handler routes requests.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	q := func(r *http.Request) string { return r.URL.Query().Get("space") }
	mux.HandleFunc("GET /xrpc/garden.engram.searchMemories", s.owned(q, s.handleSearch))
	mux.HandleFunc("GET /xrpc/garden.engram.getMemory", s.owned(q, s.handleGet))
	mux.HandleFunc("GET /xrpc/garden.engram.listMemories", s.owned(q, s.handleList))
	mux.HandleFunc("GET /xrpc/garden.engram.getMemoryGraph", s.owned(q, s.handleGraph))
	mux.HandleFunc("GET /xrpc/garden.engram.getSpaceStatus", s.owned(q, s.handleStatus))
	mux.HandleFunc("GET /xrpc/garden.engram.exportSpace", s.owned(q, s.handleExport))
	mux.HandleFunc("POST /xrpc/garden.engram.warmSpace", s.ownedBody(s.handleWarm))
	mux.HandleFunc("GET /xrpc/garden.engram.describeService", s.handleDescribe)
	mux.HandleFunc("GET /oauth/grant", s.handleGrant)
	mux.HandleFunc("GET /oauth/callback", s.handleCallback)
	if docs, ok := s.oauthDocs(); ok {
		mux.HandleFunc("GET /oauth/client-metadata.json", docs.ServeMetadata)
		mux.HandleFunc("GET /oauth/jwks.json", docs.ServeJWKS)
	}
	mux.HandleFunc("POST /xrpc/com.atproto.space.notifyWrite", s.ownedBody(s.handleNotifyWrite))
	mux.HandleFunc("POST /xrpc/com.atproto.space.notifySpaceDeleted", s.ownedBody(s.handleNotifySpaceDeleted))
	mux.HandleFunc("GET /.well-known/did.json", s.handleDIDDoc)
	mux.HandleFunc("GET /_health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	return mux
}

// ---- routing ----

// owned serves a request on the space's owner, forwarding it there when
// this node isn't the owner.
func (s *Server) owned(spaceOf func(*http.Request) string, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sp := spaceOf(r)
		if sp == "" {
			h(w, r) // the handler reports the error
			return
		}
		if !s.knows(r.Context(), sp) {
			h(w, r) // the handler reports the unknown space
			return
		}
		owner := s.ring().Owner(sp)
		if owner.ID == s.ring().Self {
			h(w, r)
			return
		}
		if r.Header.Get(ForwardedHeader) != "" {
			// Nodes disagree about ownership (mid-change); don't loop.
			s.writeErr(w, errf(http.StatusServiceUnavailable, "NotOwner", "this node doesn't own %s; retry shortly", sp))
			return
		}
		s.forward(w, r, owner)
	}
}

// ownedBody is owned for requests naming the space in a JSON body.
func (s *Server) ownedBody(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<10))
		if err != nil {
			s.writeErr(w, errf(http.StatusBadRequest, "InvalidRequest", "bad body"))
			return
		}
		var body struct {
			Space string `json:"space"`
		}
		_ = json.Unmarshal(raw, &body)
		r.Body = io.NopCloser(bytes.NewReader(raw))
		s.owned(func(*http.Request) string { return body.Space }, h)(w, r)
	}
}

// forward replays a request on another node. The Host header and every
// signed header travel unchanged, so the owner verifies the caller's own
// credential and signatures.
func (s *Server) forward(w http.ResponseWriter, r *http.Request, owner routing.Node) {
	var body io.Reader
	if r.Body != nil {
		body = r.Body
	}
	req, err := http.NewRequestWithContext(r.Context(), r.Method, owner.URL+r.URL.RequestURI(), body)
	if err != nil {
		s.writeErr(w, err)
		return
	}
	req.Header = r.Header.Clone()
	req.Host = r.Host
	req.Header.Set(ForwardedHeader, s.ring().Self)
	c := s.HTTP
	if c == nil {
		c = &http.Client{Timeout: 5 * time.Minute}
	}
	resp, err := c.Do(req)
	if err != nil {
		s.writeErr(w, errf(http.StatusBadGateway, "OwnerUnavailable", "forwarding to node %s: %v", owner.ID, err))
		return
	}
	defer resp.Body.Close()
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
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

// storeErr maps index errors to XRPC errors.
func storeErr(err error) error {
	var mm *spacestore.ModelMismatchError
	switch {
	case errors.As(err, &mm):
		return errf(http.StatusBadRequest, "ModelMismatch", "%v", err)
	case errors.Is(err, spacestore.ErrNoModel):
		return errf(http.StatusBadRequest, "NoModel", "%v", err)
	case errors.Is(err, spacestore.ErrKeywordIndexBuilding):
		return errf(http.StatusServiceUnavailable, "KeywordIndexBuilding", "%v; search with a vector meanwhile", err)
	case errors.Is(err, spacestore.ErrRetryable):
		return errf(http.StatusServiceUnavailable, "IndexLoading", "%v", err)
	case errors.Is(err, spacestore.ErrRateLimited):
		return errf(http.StatusTooManyRequests, "RateLimitExceeded", "%v", err)
	case errors.Is(err, spacestore.ErrSpaceDeleted):
		return errf(http.StatusBadRequest, "UnknownSpace", "%v", err)
	case errors.Is(err, spacestore.ErrNotOwner), errors.Is(err, spacestore.ErrStaleOwner):
		return errf(http.StatusServiceUnavailable, "NotOwner", "%v", err)
	}
	return err
}

func (s *Server) writeErr(w http.ResponseWriter, err error) {
	err = storeErr(err)
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
func (s *Server) authorizeReader(r *http.Request, spaceURI string, checkDeleted bool) error {
	if spaceURI == "" {
		return errf(http.StatusBadRequest, "InvalidRequest", "space is required")
	}
	if !s.indexes(spaceURI) {
		return errf(http.StatusBadRequest, "UnknownSpace", "this service does not index %s", spaceURI)
	}
	if err := s.verifyCredential(r, spaceURI); err != nil {
		return err
	}
	if checkDeleted {
		if deleted, err := s.Store.SpaceDeleted(r.Context(), spaceURI); err != nil {
			return err
		} else if deleted {
			return errf(http.StatusBadRequest, "UnknownSpace", "space %s was deleted", spaceURI)
		}
	}
	return nil
}

// verifyCredential checks the request presents a valid credential for the
// space, signed for this service.
func (s *Server) verifyCredential(r *http.Request, spaceURI string) error {
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
	return nil
}

func (s *Server) indexes(spaceURI string) bool {
	for _, sp := range s.Spaces {
		if sp == spaceURI {
			return true
		}
	}
	s.regMu.RLock()
	defer s.regMu.RUnlock()
	return s.registered[spaceURI]
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
	// Match explains a hybrid or keyword search result.
	Match *matchView `json:"match,omitempty"`
}

func view(spaceURI string, m spacestore.Hit, withSimilarity bool) memoryView {
	ref, _ := space.ParseRef(spaceURI)
	v := memoryView{
		URI: ref.RecordURI(m.Author, lex.MemoryCollection, m.Rkey), CID: m.CID, Author: m.Author,
		Text: m.Text, Tags: m.Tags, Source: m.Source,
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

func parseFilter(r *http.Request) (spacestore.Filter, error) {
	q := r.URL.Query()
	f := spacestore.Filter{Author: q.Get("author"), Tags: q["tags"]}
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
	if err := s.authorizeReader(r, spaceURI, true); err != nil {
		s.writeErr(w, err)
		return
	}
	if len(q.Get("q")) > 4000 {
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
	// mode: hybrid (the default with text and a vector), vector, or
	// keyword. An explicit mode never falls back.
	mode := q.Get("mode")
	text := q.Get("q")
	hasText := strings.TrimSpace(text) != ""
	switch mode {
	case "", "hybrid", "vector", "keyword":
	default:
		s.writeErr(w, errf(http.StatusBadRequest, "InvalidRequest", "mode must be hybrid, vector or keyword"))
		return
	}
	if (mode == "keyword" || mode == "hybrid") && !hasText {
		s.writeErr(w, errf(http.StatusBadRequest, "InvalidRequest", "mode %s needs q", mode))
		return
	}
	var (
		vector []float32
		model  lex.ModelInfo
		// embedErr is why a text-only search couldn't be embedded, kept in
		// case keyword search can't run either.
		embedErr    error
		keywordOnly = mode == "keyword"
	)
	switch {
	case keywordOnly:
	case q.Get("vector") == "" && hasText:
		// No vector: embed the text with the space's model, if this
		// service runs it; otherwise fall back to keyword search.
		if s.TextSearch == nil {
			embedErr = errf(http.StatusBadRequest, "InvalidRequest", "vector, model and modelDigest are required: embed the query with the space's model")
		} else if vector, model, err = s.embedQuery(r.Context(), spaceURI, text); err != nil {
			embedErr = err
		}
		if embedErr != nil {
			var xe *xrpcError
			fallback := mode == "" && (!errors.As(embedErr, &xe) || xe.name == "InvalidRequest" && s.TextSearch == nil || xe.name == "ModelNotHosted" || xe.name == "TextSearchNotAllowed")
			if !fallback {
				s.writeErr(w, embedErr)
				return
			}
			keywordOnly = true
		}
	default:
		if q.Get("vector") == "" || q.Get("model") == "" || q.Get("modelDigest") == "" {
			s.writeErr(w, errf(http.StatusBadRequest, "InvalidRequest", "vector, model and modelDigest are required: embed the query with the space's model"))
			return
		}
		if vector, err = lex.DecodeQueryVector(q.Get("vector")); err != nil || len(vector) == 0 || len(vector) > 16000 {
			s.writeErr(w, errf(http.StatusBadRequest, "InvalidRequest", "vector must be base64url-encoded f16le"))
			return
		}
		model = lex.ModelInfo{Model: q.Get("model"), ModelDigest: q.Get("modelDigest"), Dims: len(vector)}
	}
	sq := spacestore.SearchQuery{Vector: vector, Model: model, Limit: limit, Filter: f, KeywordOnly: keywordOnly}
	if mode != "vector" {
		// The text makes the search hybrid once the space's segments all
		// have a keyword index (ENGRAM_KEYWORD_SEARCH); until then the store
		// ignores it.
		sq.Text = text
	}
	res, err := s.Store.Search(r.Context(), spaceURI, sq)
	if err != nil {
		if embedErr != nil && (errors.Is(err, spacestore.ErrKeywordIndexBuilding) || errors.Is(err, spacestore.ErrNoModel)) {
			// Neither embedding nor keyword search can serve this space.
			err = embedErr
		}
		s.writeErr(w, err)
		return
	}
	ran := "vector"
	switch {
	case res.KeywordOnly:
		ran = "keyword"
	case res.Hybrid:
		ran = "hybrid"
	}
	out := make([]memoryView, len(res.Hits))
	for i, m := range res.Hits {
		out[i] = view(spaceURI, m, m.HasSimilarity)
	}
	if ran != "vector" {
		explain(out, res.Hits, text, ran)
	}
	body := map[string]any{"memories": out, "mode": ran}
	if res.Approximate {
		body["approximate"] = true
	}
	writeJSON(w, http.StatusOK, body)
}

// embedQuery turns a search's text into a vector with the space's declared
// model, and returns that model for the search to check against.
func (s *Server) embedQuery(ctx context.Context, spaceURI, text string) ([]float32, lex.ModelInfo, error) {
	if len(text) > MaxTextQueryChars {
		return nil, lex.ModelInfo{}, errf(http.StatusBadRequest, "InvalidRequest", "q is at most %d characters when the service embeds it", MaxTextQueryChars)
	}
	cfg, err := s.Store.Config(ctx, spaceURI)
	if err != nil {
		return nil, lex.ModelInfo{}, err
	}
	if cfg == nil {
		return nil, lex.ModelInfo{}, spacestore.ErrNoModel
	}
	v, err := s.TextSearch.Embed(ctx, spaceURI, cfg, text)
	if err != nil {
		return nil, lex.ModelInfo{}, err
	}
	return v, cfg.ModelInfo, nil
}

// parseMemoryURI splits a memory URI in the space into author and rkey.
func parseMemoryURI(spaceURI, uri string) (string, string, bool) {
	ref, err := space.ParseRef(spaceURI)
	if err != nil {
		return "", "", false
	}
	rest, ok := strings.CutPrefix(uri, ref.String()+"/")
	parts := strings.Split(rest, "/")
	if !ok || len(parts) != 3 || parts[1] != lex.MemoryCollection || parts[0] == "" || parts[2] == "" {
		return "", "", false
	}
	return parts[0], parts[2], true
}

func (s *Server) handleGet(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	spaceURI := q.Get("space")
	if err := s.authorizeReader(r, spaceURI, true); err != nil {
		s.writeErr(w, err)
		return
	}
	author, rkey, ok := parseMemoryURI(spaceURI, q.Get("uri"))
	var m spacestore.Hit
	err := spacestore.ErrNotFound
	if ok {
		m, err = s.Store.Get(r.Context(), spaceURI, author, rkey)
	}
	if errors.Is(err, spacestore.ErrNotFound) {
		s.writeErr(w, errf(http.StatusNotFound, "MemoryNotFound", "no memory %s", q.Get("uri")))
		return
	}
	if err != nil {
		s.writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"memory": view(spaceURI, m, false)})
}

func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	spaceURI := q.Get("space")
	if err := s.authorizeReader(r, spaceURI, true); err != nil {
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
	if errors.Is(err, spacestore.ErrBadCursor) {
		s.writeErr(w, errf(http.StatusBadRequest, "InvalidRequest", "bad cursor"))
		return
	}
	if err != nil {
		s.writeErr(w, err)
		return
	}
	out := make([]memoryView, len(ms))
	for i, m := range ms {
		out[i] = view(spaceURI, m, false)
	}
	res := map[string]any{"memories": out}
	if cursor != "" {
		res["cursor"] = cursor
	}
	writeJSON(w, http.StatusOK, res)
}

// handleGraph answers with the space's newest memories and links between
// the ones that mean similar things, for drawing the space as a graph.
func (s *Server) handleGraph(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	spaceURI := q.Get("space")
	if err := s.authorizeReader(r, spaceURI, true); err != nil {
		s.writeErr(w, err)
		return
	}
	limit, err := parseLimit(r, spacestore.DefaultGraphNodes, spacestore.MaxGraphNodes)
	if err != nil {
		s.writeErr(w, err)
		return
	}
	query := spacestore.GraphQuery{Limit: limit}
	if v := q.Get("neighbors"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > spacestore.MaxGraphNeighbors {
			s.writeErr(w, errf(http.StatusBadRequest, "InvalidRequest", "neighbors must be between 1 and %d", spacestore.MaxGraphNeighbors))
			return
		}
		query.Neighbors = n
	}
	if v := q.Get("minSimilarity"); v != "" {
		m, err := strconv.ParseFloat(v, 64)
		if err != nil || m <= 0 || m > 1 {
			s.writeErr(w, errf(http.StatusBadRequest, "InvalidRequest", "minSimilarity must be above 0 and at most 1"))
			return
		}
		query.MinSimilarity = m
	}
	g, err := s.Store.Graph(r.Context(), spaceURI, query)
	if err != nil {
		s.writeErr(w, err)
		return
	}
	type edgeView struct {
		A          int `json:"a"`
		B          int `json:"b"`
		Similarity int `json:"similarity"`
	}
	nodes := make([]memoryView, len(g.Nodes))
	for i, m := range g.Nodes {
		nodes[i] = view(spaceURI, m, false)
	}
	edges := make([]edgeView, len(g.Edges))
	for i, e := range g.Edges {
		edges[i] = edgeView{A: e.A, B: e.B, Similarity: int(max(0, min(1, e.Similarity)) * 1000)}
	}
	writeJSON(w, http.StatusOK, map[string]any{"nodes": nodes, "edges": edges})
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	spaceURI := r.URL.Query().Get("space")
	if err := s.authorizeReader(r, spaceURI, true); err != nil {
		s.writeErr(w, err)
		return
	}
	st, err := s.Store.Status(r.Context(), spaceURI)
	if err != nil {
		s.writeErr(w, err)
		return
	}
	type skipped struct {
		Author string `json:"author"`
		Count  int    `json:"count"`
	}
	sk := []skipped{}
	for a, n := range st.Skipped {
		sk = append(sk, skipped{a, n})
	}
	sort.Slice(sk, func(i, j int) bool { return sk[i].Author < sk[j].Author })
	out := map[string]any{"memories": st.Memories, "skipped": sk}
	if st.Config != nil {
		out["config"] = st.Config
	}
	if st.Active != nil {
		out["active"] = st.Active
	}
	if st.Building != nil {
		out["building"] = st.Building
		out["buildingMemories"] = st.BuildingMemories
	}
	if s.Grants != nil {
		access, err := s.Grants.Access(r.Context(), spaceURI)
		if err != nil {
			s.writeErr(w, err)
			return
		}
		out["access"] = access
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleExport(w http.ResponseWriter, r *http.Request) {
	spaceURI := r.URL.Query().Get("space")
	if err := s.authorizeReader(r, spaceURI, true); err != nil {
		s.writeErr(w, err)
		return
	}
	tw := &startedWriter{w: w, start: func() {
		w.Header().Set("Content-Type", "application/x-tar")
		w.Header().Set("Content-Disposition", `attachment; filename="`+spacestore.SpaceKey(spaceURI)+`.tar"`)
	}}
	if err := s.Store.Export(r.Context(), spaceURI, tw); err != nil {
		if !tw.started {
			s.writeErr(w, err)
			return
		}
		// Mid-stream, the error can only cut the tar short; tar readers
		// notice the missing end.
		s.log().Warn("export failed", "space", spaceURI, "err", err)
	}
}

// startedWriter sets the response headers on the first write, so an error
// before any data can still be reported as an XRPC error.
type startedWriter struct {
	w       http.ResponseWriter
	start   func()
	started bool
}

func (s *startedWriter) Write(p []byte) (int, error) {
	if !s.started {
		s.started = true
		s.start()
	}
	return s.w.Write(p)
}

func (s *Server) handleWarm(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Space string `json:"space"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		s.writeErr(w, errf(http.StatusBadRequest, "InvalidRequest", "bad body"))
		return
	}
	// Don't check deletion here: that would load the space synchronously.
	if err := s.authorizeReader(r, body.Space, false); err != nil {
		s.writeErr(w, err)
		return
	}
	s.Store.Warm(body.Space)
	writeJSON(w, http.StatusOK, map[string]any{})
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
	if err := json.NewDecoder(r.Body).Decode(&n); err != nil || n.Space == "" || n.Repo == "" {
		// Authorities don't retry a rejected notification, so the write
		// waits for the periodic sync: worth a line in the log.
		s.log().Warn("rejected a write notification", "reason", "bad body", "err", err)
		notifications.WithLabelValues("write", "bad_body").Inc()
		s.writeErr(w, errf(http.StatusBadRequest, "InvalidRequest", "bad notification body"))
		return
	}
	if err := s.authorizeAuthority(r, n.Space, "com.atproto.space.notifyWrite"); err != nil {
		s.log().Warn("rejected a write notification", "space", n.Space, "repo", n.Repo, "err", err)
		notifications.WithLabelValues("write", rejection(err)).Inc()
		s.writeErr(w, err)
		return
	}
	if !s.granted(r.Context(), n.Space) {
		// Nothing can be read until the authority approves, however often
		// this is retried. Say so, since the write will never be searchable.
		notifications.WithLabelValues("write", "not_granted").Inc()
		s.logUngranted(n.Space, n.Repo)
		w.WriteHeader(http.StatusOK)
		return
	}
	// A write is a sign the space is in use: start loading it now.
	s.Store.Warm(n.Space)
	// Sync in the background; the periodic space sync catches anything
	// that fails here. A space already at its cap gets a follow-up space
	// sync from a running job instead of another goroutine.
	releaseSpace, ok := s.trySpaceSlot(n.Space)
	if !ok {
		s.followUp.Store(n.Space, true)
		notifications.WithLabelValues("write", "deferred").Inc()
		w.WriteHeader(http.StatusOK)
		return
	}
	notifications.WithLabelValues("write", "accepted").Inc()
	s.Jobs.Add(1)
	go func() {
		defer s.Jobs.Done()
		defer releaseSpace()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		releaseNode, err := s.acquireNodeSlot(ctx)
		if err != nil {
			notifiedSyncs.WithLabelValues("no_slot").Inc()
			s.log().Warn("notified sync never got a slot; the periodic sync will catch up", "space", n.Space)
			return
		}
		defer releaseNode()
		needSync, err := s.Indexer.HandleWrite(ctx, n)
		notifiedSyncs.WithLabelValues(metrics.Result(err)).Inc()
		if err != nil {
			s.log().Warn("notified sync failed", "space", n.Space, "repo", n.Repo, "err", err)
		}
		if _, more := s.followUp.LoadAndDelete(n.Space); more {
			needSync = true
		}
		if needSync {
			s.log().Info("syncing the whole space", "space", n.Space)
			s.syncSpaceOnce(ctx, n.Space)
		}
	}()
	w.WriteHeader(http.StatusOK)
}

// granted reports whether the appview may read the space: its authority
// has granted access (or, without Grants, always).
func (s *Server) granted(ctx context.Context, spaceURI string) bool {
	if s.Grants == nil {
		return true
	}
	g, err := s.Grants.Get(ctx, spaceURI)
	if err != nil {
		s.log().Warn("reading a grant failed", "space", spaceURI, "err", err)
		return false
	}
	return g != nil
}

// ungrantedLogEvery is how often a space's missing grant is logged, however
// many writes arrive.
const ungrantedLogEvery = 10 * time.Minute

// logUngranted warns that a space is being written to without the appview
// being allowed to read it, at most once in a while per space.
func (s *Server) logUngranted(spaceURI, repo string) {
	now := time.Now()
	if last, ok := s.ungrantedLogged.Load(spaceURI); ok && now.Sub(last.(time.Time)) < ungrantedLogEvery {
		return
	}
	s.ungrantedLogged.Store(spaceURI, now)
	s.log().Warn("a write arrived for a space the appview isn't allowed to read, so it won't be searchable: its authority must approve indexing",
		"space", spaceURI, "repo", repo, "approvalPage", s.GrantURL())
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
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Space == "" {
		notifications.WithLabelValues("space_deleted", "bad_body").Inc()
		s.writeErr(w, errf(http.StatusBadRequest, "InvalidRequest", "bad body"))
		return
	}
	if err := s.authorizeAuthority(r, body.Space, "com.atproto.space.notifySpaceDeleted"); err != nil {
		s.log().Warn("rejected a space deletion notification", "space", body.Space, "err", err)
		notifications.WithLabelValues("space_deleted", rejection(err)).Inc()
		s.writeErr(w, err)
		return
	}
	if err := s.Store.MarkSpaceDeleted(r.Context(), body.Space); err != nil {
		notifications.WithLabelValues("space_deleted", "error").Inc()
		s.writeErr(w, err)
		return
	}
	notifications.WithLabelValues("space_deleted", "accepted").Inc()
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

// Run keeps the index current until ctx ends. For every space this node
// owns, it syncs at start and on each poll tick, and renews notification
// registrations when the service is publicly reachable. The coordinator
// also clears abandoned sign-ins.
func (s *Server) Run(ctx context.Context, poll time.Duration, register bool) {
	renewAt := map[string]time.Time{}
	tick := func() {
		if err := s.LoadRegistrations(ctx); err != nil {
			s.log().Warn("reading space registrations failed", "err", err)
		}
		spaces := s.allSpaces()
		// One node clears abandoned sign-ins: the coordinator.
		if s.Grants != nil && s.ring().IsCoordinator() {
			if n, err := s.Grants.Sweep(ctx, time.Now()); err != nil {
				s.log().Warn("clearing abandoned sign-ins failed", "err", err)
			} else if n > 0 {
				s.log().Info("cleared abandoned sign-ins", "count", n)
			}
		}
		spacesIndexed.Set(float64(len(spaces)))
		owned := 0
		defer func() { spacesOwned.Set(float64(owned)) }()
		for _, sp := range spaces {
			if !s.ring().Owns(sp) || !s.granted(ctx, sp) {
				continue
			}
			owned++
			if register && time.Now().After(renewAt[sp]) {
				exp, err := s.Indexer.Register(ctx, sp, s.ServiceID())
				notifyRegistrations.WithLabelValues(metrics.Result(err)).Inc()
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
		case <-s.wake():
			tick()
		}
	}
}
