// Package web is the Engram Garden web app's backend. People sign in with
// ATProto OAuth; the backend then acts for them in their memory spaces: it
// exchanges their delegation tokens for space credentials, reads the
// appview, deletes their own memories, and lets a space's authority manage
// the space. The React frontend it serves lives in /web.
package web

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"mime"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/bluesky-social/indigo/atproto/atclient"
	"github.com/bluesky-social/indigo/atproto/identity"
	"github.com/bluesky-social/indigo/atproto/syntax"

	"github.com/haileyok/engram-garden/internal/spaceclient"
)

// cookieName holds the signed-in user's DID and session ID.
const cookieName = "engram_session"

// userIdle is how long a signed-in user's PDS session and credentials stay
// cached without requests.
const userIdle = 30 * time.Minute

// maxLivePerUser caps a user's open live-update streams.
const maxLivePerUser = 4

// Auth resumes signed-in users' PDS sessions.
type Auth interface {
	// Resume returns an API client for the user's PDS session.
	Resume(ctx context.Context, did syntax.DID, sessionID string) (*atclient.APIClient, error)
	// Logout ends the session.
	Logout(ctx context.Context, did syntax.DID, sessionID string) error
}

// Server is the web app's HTTP surface.
type Server struct {
	Auth Auth
	// OAuth serves sign-in. Nil disables it (tests sign in with cookies).
	OAuth *OAuth
	Dir   identity.Directory
	// HTTP reaches space authorities and the appview.
	HTTP *http.Client

	AppviewURL string
	AppviewDID string
	// Origin is the web app's own origin, e.g. https://engram.garden.
	// Requests that change anything must come from it.
	Origin string
	// CookieKey signs session cookies.
	CookieKey []byte
	// Static is the built frontend. Nil serves a placeholder page.
	Static fs.FS
	Log    *slog.Logger
	// LiveEvery is how often live updates poll the appview (default 5s).
	LiveEvery time.Duration

	mu        sync.Mutex
	users     map[string]*user
	lastSweep time.Time

	svcMu sync.Mutex
	svc   map[string]any
	svcAt time.Time
}

// user is one signed-in session.
type user struct {
	did   syntax.DID
	api   *atclient.APIClient
	space *spaceclient.Client
	used  time.Time
	live  int
}

func (s *Server) log() *slog.Logger {
	if s.Log == nil {
		return slog.Default()
	}
	return s.Log
}

// Handler routes requests.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	if s.OAuth != nil {
		mux.HandleFunc("GET /oauth/client-metadata.json", s.OAuth.handleMetadata)
		mux.HandleFunc("GET /oauth/jwks.json", s.OAuth.handleJWKS)
		mux.HandleFunc("GET /oauth/callback", s.handleCallback)
		mux.HandleFunc("POST /api/login", s.change(s.handleLogin))
	}
	mux.HandleFunc("POST /api/logout", s.change(s.handleLogout))
	mux.HandleFunc("GET /api/session", s.signedIn(s.handleSession))
	mux.HandleFunc("GET /api/service", s.signedIn(s.handleService))
	mux.HandleFunc("GET /api/profiles", s.signedIn(s.handleProfiles))

	mux.HandleFunc("GET /api/spaces", s.signedIn(s.handleSpaces))
	mux.HandleFunc("GET /api/status", s.signedIn(s.handleStatus))
	mux.HandleFunc("POST /api/register", s.change(s.signedIn(s.handleRegister)))
	mux.HandleFunc("GET /api/memories", s.signedIn(s.handleMemories))
	mux.HandleFunc("GET /api/memory", s.signedIn(s.handleMemory))
	mux.HandleFunc("POST /api/memories/delete", s.change(s.signedIn(s.handleDeleteMemory)))
	mux.HandleFunc("GET /api/live", s.signedIn(s.handleLive))

	mux.HandleFunc("POST /api/spaces/create", s.change(s.signedIn(s.handleCreateSpace)))
	mux.HandleFunc("GET /api/space", s.signedIn(s.handleGetSpace))
	mux.HandleFunc("GET /api/members", s.signedIn(s.handleMembers))
	mux.HandleFunc("POST /api/members/put", s.change(s.signedIn(s.handlePutMember)))
	mux.HandleFunc("POST /api/members/remove", s.change(s.signedIn(s.handleRemoveMember)))
	mux.HandleFunc("GET /api/config", s.signedIn(s.handleGetConfig))
	mux.HandleFunc("POST /api/config", s.change(s.signedIn(s.handlePutConfig)))

	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		writeErr(w, apiErr(http.StatusNotFound, "NotFound", "no such endpoint"))
	})
	mux.HandleFunc("GET /_health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	static := s.static()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		static.ServeHTTP(w, r)
	})
	return securityHeaders(mux)
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self' http://localhost:11434 http://127.0.0.1:11434; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		next.ServeHTTP(w, r)
	})
}

// ---- errors ----

// Error is an API error: {"error": name, "message": message}.
type Error struct {
	Status  int
	Name    string
	Message string
}

func (e *Error) Error() string { return e.Name + ": " + e.Message }

func apiErr(status int, name, format string, args ...any) *Error {
	return &Error{Status: status, Name: name, Message: fmt.Sprintf(format, args...)}
}

// upstreamErr turns an error from a PDS, space authority or the appview
// into an API error.
func upstreamErr(err error) *Error {
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	var se *spaceclient.Error
	if errors.As(err, &se) {
		return passthrough(se.Status, se.Name, se.Message)
	}
	var ae *atclient.APIError
	if errors.As(err, &ae) {
		if ae.StatusCode == http.StatusUnauthorized {
			// The user's PDS refused the session or its scopes.
			return apiErr(http.StatusUnauthorized, "SessionExpired", "your sign-in expired or lacks a permission; sign in again")
		}
		return passthrough(ae.StatusCode, ae.Name, ae.Message)
	}
	if strings.Contains(err.Error(), "ran out of request retries") {
		return apiErr(http.StatusUnauthorized, "SessionExpired", "your sign-in expired; sign in again")
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return apiErr(http.StatusGatewayTimeout, "Timeout", "the request took too long")
	}
	return apiErr(http.StatusBadGateway, "UpstreamFailed", "%v", err)
}

func passthrough(status int, name, msg string) *Error {
	if name == "" {
		name = http.StatusText(status)
	}
	switch {
	case status == http.StatusUnauthorized:
		// A host rejected a space credential even after a fresh one: the
		// user can't read the space. Our own 401 means "sign in".
		return &Error{Status: http.StatusForbidden, Name: name, Message: msg}
	case status >= 400 && status < 500:
		return &Error{Status: status, Name: name, Message: msg}
	}
	return &Error{Status: http.StatusBadGateway, Name: name, Message: msg}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, e *Error) {
	writeJSON(w, e.Status, map[string]string{"error": e.Name, "message": e.Message})
}

func (s *Server) fail(w http.ResponseWriter, err error) {
	e := upstreamErr(err)
	if e.Status >= 500 {
		s.log().Warn("request failed", "err", err)
	}
	writeErr(w, e)
}

// ---- request guards ----

// change guards requests that change anything: they must come from the web
// app's own pages, as JSON.
func (s *Server) change(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.sameOrigin(r) {
			writeErr(w, apiErr(http.StatusForbidden, "CrossSiteRequest", "requests must come from %s", s.Origin))
			return
		}
		if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt != "application/json" {
			writeErr(w, apiErr(http.StatusUnsupportedMediaType, "InvalidRequest", "send JSON"))
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
		h(w, r)
	}
}

func (s *Server) sameOrigin(r *http.Request) bool {
	if o := r.Header.Get("Origin"); o != "" {
		return s.Origin != "" && o == s.Origin
	}
	return r.Header.Get("Sec-Fetch-Site") == "same-origin"
}

type ctxKey struct{}

// signedIn resolves the session cookie to a user.
func (s *Server) signedIn(h func(http.ResponseWriter, *http.Request, *user)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u, err := s.user(r)
		if err != nil {
			s.clearCookie(w)
			writeErr(w, apiErr(http.StatusUnauthorized, "AuthRequired", "sign in first"))
			return
		}
		h(w, r, u)
	}
}

func decode(r *http.Request, v any) *Error {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil && !errors.Is(err, io.EOF) {
		return apiErr(http.StatusBadRequest, "InvalidRequest", "bad JSON body")
	}
	return nil
}

// ---- sessions ----

func base64DID(did string) string { return base64.RawURLEncoding.EncodeToString([]byte(did)) }

func (s *Server) mac(payload string) string {
	m := hmac.New(sha256.New, s.CookieKey)
	m.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

// sessionCookie names a signed-in session: the DID, the session ID and a
// signature over both.
func (s *Server) sessionCookie(did syntax.DID, sessionID string) *http.Cookie {
	payload := base64DID(did.String()) + "." + base64.RawURLEncoding.EncodeToString([]byte(sessionID))
	return &http.Cookie{
		Name:     cookieName,
		Value:    payload + "." + s.mac(payload),
		Path:     "/",
		HttpOnly: true,
		Secure:   strings.HasPrefix(s.Origin, "https://"),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int((180 * 24 * time.Hour).Seconds()),
	}
}

func (s *Server) clearCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", MaxAge: -1, HttpOnly: true,
		Secure: strings.HasPrefix(s.Origin, "https://"), SameSite: http.SameSiteLaxMode})
}

func (s *Server) readCookie(r *http.Request) (syntax.DID, string, bool) {
	c, err := r.Cookie(cookieName)
	if err != nil || len(s.CookieKey) == 0 {
		return "", "", false
	}
	parts := strings.Split(c.Value, ".")
	if len(parts) != 3 {
		return "", "", false
	}
	payload := parts[0] + "." + parts[1]
	if !hmac.Equal([]byte(parts[2]), []byte(s.mac(payload))) {
		return "", "", false
	}
	rawDID, err1 := base64.RawURLEncoding.DecodeString(parts[0])
	sid, err2 := base64.RawURLEncoding.DecodeString(parts[1])
	if err1 != nil || err2 != nil {
		return "", "", false
	}
	did, err := syntax.ParseDID(string(rawDID))
	if err != nil || len(sid) == 0 {
		return "", "", false
	}
	return did, string(sid), true
}

// user returns the signed-in user, resuming their PDS session once and
// keeping it (with its space credentials) while they're active. One client
// per session, so token refreshes don't race.
func (s *Server) user(r *http.Request) (*user, error) {
	did, sid, ok := s.readCookie(r)
	if !ok {
		return nil, errors.New("no session")
	}
	key := did.String() + "\x00" + sid
	s.mu.Lock()
	if s.users == nil {
		s.users = map[string]*user{}
	}
	if time.Since(s.lastSweep) > time.Minute {
		s.lastSweep = time.Now()
		for k, u := range s.users {
			if time.Since(u.used) > userIdle && u.live == 0 {
				delete(s.users, k)
			}
		}
	}
	if u, ok := s.users[key]; ok {
		u.used = time.Now()
		s.mu.Unlock()
		return u, nil
	}
	s.mu.Unlock()

	api, err := s.Auth.Resume(r.Context(), did, sid)
	if err != nil {
		return nil, err
	}
	sc, err := spaceclient.New(api, s.Dir, s.HTTP)
	if err != nil {
		return nil, err
	}
	u := &user{did: did, api: api, space: sc, used: time.Now()}
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.users[key]; ok {
		return existing, nil
	}
	s.users[key] = u
	return u, nil
}

func (s *Server) forget(did syntax.DID, sid string) {
	s.mu.Lock()
	delete(s.users, did.String()+"\x00"+sid)
	s.mu.Unlock()
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if did, sid, ok := s.readCookie(r); ok {
		s.forget(did, sid)
		if err := s.Auth.Logout(r.Context(), did, sid); err != nil {
			s.log().Warn("logout failed", "did", did, "err", err)
		}
	}
	s.clearCookie(w)
	writeJSON(w, http.StatusOK, map[string]any{})
}

func (s *Server) handleSession(w http.ResponseWriter, r *http.Request, u *user) {
	out := map[string]any{"did": u.did.String(), "appviewDid": s.AppviewDID}
	if ident, err := s.Dir.LookupDID(r.Context(), u.did); err == nil && !ident.Handle.IsInvalidHandle() {
		out["handle"] = ident.Handle.String()
	}
	writeJSON(w, http.StatusOK, out)
}
