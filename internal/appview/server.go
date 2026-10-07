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

	"github.com/haileyok/engram-garden/internal/blob"
	"github.com/haileyok/engram-garden/internal/indexer"
	"github.com/haileyok/engram-garden/internal/lex"
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
	// Spaces are the spaces this service indexes.
	Spaces []string
	// Ring decides which node owns each space. Nil means a single node.
	Ring *routing.Ring
	// Blob, when set, holds the registry of indexed spaces.
	Blob blob.Store
	// HTTP forwards requests to other nodes.
	HTTP *http.Client

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
	mux.HandleFunc("GET /xrpc/garden.engram.getSpaceStatus", s.owned(q, s.handleStatus))
	mux.HandleFunc("GET /xrpc/garden.engram.exportSpace", s.owned(q, s.handleExport))
	mux.HandleFunc("POST /xrpc/garden.engram.warmSpace", s.ownedBody(s.handleWarm))
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
		if sp == "" || !s.indexes(sp) {
			h(w, r) // the handler reports the error
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
	if checkDeleted {
		if deleted, err := s.Store.SpaceDeleted(r.Context(), spaceURI); err != nil {
			return err
		} else if deleted {
			return errf(http.StatusBadRequest, "UnknownSpace", "space %s was deleted", spaceURI)
		}
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
	if q.Get("vector") == "" || q.Get("model") == "" || q.Get("modelDigest") == "" {
		s.writeErr(w, errf(http.StatusBadRequest, "InvalidRequest", "vector, model and modelDigest are required: embed the query with the space's model"))
		return
	}
	vector, err := lex.DecodeQueryVector(q.Get("vector"))
	if err != nil || len(vector) == 0 || len(vector) > 16000 {
		s.writeErr(w, errf(http.StatusBadRequest, "InvalidRequest", "vector must be base64url-encoded f16le"))
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
	model := lex.ModelInfo{Model: q.Get("model"), ModelDigest: q.Get("modelDigest"), Dims: len(vector)}
	res, err := s.Store.Search(r.Context(), spaceURI, spacestore.SearchQuery{Vector: vector, Model: model, Limit: limit, Filter: f})
	if err != nil {
		s.writeErr(w, err)
		return
	}
	out := make([]memoryView, len(res.Hits))
	for i, m := range res.Hits {
		out[i] = view(spaceURI, m, true)
	}
	body := map[string]any{"memories": out}
	if res.Approximate {
		body["approximate"] = true
	}
	writeJSON(w, http.StatusOK, body)
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
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleExport(w http.ResponseWriter, r *http.Request) {
	spaceURI := r.URL.Query().Get("space")
	if err := s.authorizeReader(r, spaceURI, true); err != nil {
		s.writeErr(w, err)
		return
	}
	// Errors after the first byte can only cut the stream short; tar
	// readers notice the missing end.
	w.Header().Set("Content-Type", "application/x-tar")
	w.Header().Set("Content-Disposition", `attachment; filename="`+spacestore.SpaceKey(spaceURI)+`.tar"`)
	if err := s.Store.Export(r.Context(), spaceURI, w); err != nil {
		s.log().Warn("export failed", "space", spaceURI, "err", err)
	}
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
		s.writeErr(w, errf(http.StatusBadRequest, "InvalidRequest", "bad notification body"))
		return
	}
	if err := s.authorizeAuthority(r, n.Space, "com.atproto.space.notifyWrite"); err != nil {
		s.writeErr(w, err)
		return
	}
	// A write is a sign the space is in use: start loading it now.
	s.Store.Warm(n.Space)
	// Sync in the background; the periodic space sync catches anything
	// that fails here.
	s.Jobs.Add(1)
	go func() {
		defer s.Jobs.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		needSync, err := s.Indexer.HandleWrite(ctx, n)
		if err != nil {
			s.log().Warn("notified sync failed", "space", n.Space, "repo", n.Repo, "err", err)
		}
		if needSync {
			s.log().Info("syncing the whole space", "space", n.Space)
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
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Space == "" {
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

// Run keeps the index current until ctx ends. For every space this node
// owns, it syncs at start and on each poll tick, and renews notification
// registrations when the service is publicly reachable. The coordinator
// also keeps the registry of indexed spaces.
func (s *Server) Run(ctx context.Context, poll time.Duration, register bool) {
	renewAt := map[string]time.Time{}
	tick := func() {
		if s.Blob != nil {
			if _, err := s.ring().WriteRegistry(ctx, s.Blob, s.Spaces); err != nil {
				s.log().Warn("writing the registry failed", "err", err)
			}
		}
		for _, sp := range s.Spaces {
			if !s.ring().Owns(sp) {
				continue
			}
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
