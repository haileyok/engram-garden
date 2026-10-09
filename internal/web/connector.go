package web

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/bluesky-social/indigo/atproto/syntax"
	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
	"golang.org/x/time/rate"

	"github.com/haileyok/engram-garden/internal/control"
)

// The connector lets an app such as claude.ai read an account's memory
// spaces. The app is an MCP client: it finds this service's authorization
// server from /.well-known documents, registers itself, sends the person
// here to approve it (signing them in first, with the same ATProto sign-in
// as the web app), and trades the approval for tokens that it presents at
// /mcp. A token stands for one account's approval of one app; the tools it
// reaches run as that account, through the web app session the approval
// was made in, so they can read what the account can read.

// mcpScope is the one permission an app can be given: reading.
const mcpScope = "memories:read"

const (
	accessTokenTTL = time.Hour
	authCodeTTL    = 5 * time.Minute
	// maxPendingCodes bounds approvals waiting to be traded for tokens.
	maxPendingCodes = 1000
)

// Connector is the connector's state: the apps and approvals, and the
// approvals waiting to be traded for tokens, all in the control-plane
// database, so every web node sees them and they survive a restart.
type Connector struct {
	db control.Store
	// registrations limits how fast this node registers apps (anyone can).
	registrations *rate.Limiter
}

// NewConnector keeps the connector's state in db.
func NewConnector(db control.Store) *Connector {
	return &Connector{db: db, registrations: rate.NewLimiter(rate.Every(6*time.Second), 20)}
}

// putCode saves an approval, by the hash of its code, and reports false
// when too many are waiting.
func (c *Connector) putCode(ctx context.Context, code string, a control.MCPCode) (bool, error) {
	a.Hash = hashToken(code)
	return c.db.PutMCPCode(ctx, a, time.Now().UTC(), maxPendingCodes)
}

// takeCode returns an approval's details and spends it.
func (c *Connector) takeCode(ctx context.Context, code string) (*control.MCPCode, error) {
	return c.db.TakeMCPCode(ctx, hashToken(code), time.Now().UTC())
}

// ---- routes ----

func (s *Server) mcpURL() string { return s.Origin + "/mcp" }

func (s *Server) connectorRoutes(mux *http.ServeMux) {
	meta := auth.ProtectedResourceMetadataHandler(&oauthex.ProtectedResourceMetadata{
		Resource:               s.mcpURL(),
		AuthorizationServers:   []string{s.Origin},
		ScopesSupported:        []string{mcpScope},
		BearerMethodsSupported: []string{"header"},
		ResourceName:           "Engram Garden",
	})
	mux.Handle("/.well-known/oauth-protected-resource", meta)
	mux.Handle("/.well-known/oauth-protected-resource/mcp", meta)
	mux.HandleFunc("/.well-known/oauth-authorization-server", cors(s.handleAuthServerMetadata))
	mux.HandleFunc("/mcp-oauth/register", cors(s.handleRegister))
	mux.HandleFunc("/mcp-oauth/token", cors(s.handleMCPToken))
	mux.HandleFunc("GET /mcp-oauth/authorize", s.handleAuthorize)
	mux.HandleFunc("POST /mcp-oauth/authorize", s.handleDecision)
	mux.HandleFunc("POST /mcp-oauth/signin", s.handleConnectorSignIn)

	mux.HandleFunc("GET /api/connectors", s.signedIn(s.handleConnectors))
	mux.HandleFunc("POST /api/connectors/revoke", s.change(s.signedIn(s.handleRevokeConnector)))

	bearer := auth.RequireBearerToken(s.verifyAccessToken, &auth.RequireBearerTokenOptions{
		ResourceMetadataURL: s.Origin + "/.well-known/oauth-protected-resource",
	})
	mux.Handle("/mcp", bearer(mcp.NewStreamableHTTPHandler(s.mcpServerFor, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})))
}

// cors lets apps in browsers reach the endpoints that carry no cookies.
func cors(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		h(w, r)
	}
}

func (s *Server) handleAuthServerMetadata(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"issuer":                                         s.Origin,
		"authorization_endpoint":                         s.Origin + "/mcp-oauth/authorize",
		"token_endpoint":                                 s.Origin + "/mcp-oauth/token",
		"registration_endpoint":                          s.Origin + "/mcp-oauth/register",
		"response_types_supported":                       []string{"code"},
		"grant_types_supported":                          []string{"authorization_code", "refresh_token"},
		"code_challenge_methods_supported":               []string{"S256"},
		"token_endpoint_auth_methods_supported":          []string{"none"},
		"scopes_supported":                               []string{mcpScope},
		"authorization_response_iss_parameter_supported": true,
	})
}

// ---- registration ----

// oauthErr writes an OAuth error response.
func oauthErr(w http.ResponseWriter, status int, code, desc string) {
	writeJSON(w, status, map[string]string{"error": code, "error_description": desc})
}

// cleanName makes a client's name safe to show.
func cleanName(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > 60 {
		s = string(r[:60])
	}
	return s
}

// validRedirect says whether an app may be sent back to a URL: https, or
// http on the loopback interface for an app on the same machine.
func validRedirect(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.Fragment != "" || u.User != nil || len(raw) > 500 {
		return false
	}
	switch u.Scheme {
	case "https":
		return true
	case "http":
		h := u.Hostname()
		return h == "127.0.0.1" || h == "localhost" || h == "::1"
	}
	return false
}

// handleRegister registers an app (RFC 7591). Apps are public clients; they
// prove they started a sign-in with PKCE rather than with a secret.
func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.Connector.registrations.Allow() {
		oauthErr(w, http.StatusTooManyRequests, "temporarily_unavailable", "too many registrations; try again shortly")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	var in struct {
		RedirectURIs []string `json:"redirect_uris"`
		ClientName   string   `json:"client_name"`
	}
	if e := decode(r, &in); e != nil {
		oauthErr(w, http.StatusBadRequest, "invalid_client_metadata", "bad JSON body")
		return
	}
	if n := len(in.RedirectURIs); n == 0 || n > 5 {
		oauthErr(w, http.StatusBadRequest, "invalid_redirect_uri", "send between 1 and 5 redirect_uris")
		return
	}
	for _, u := range in.RedirectURIs {
		if !validRedirect(u) {
			oauthErr(w, http.StatusBadRequest, "invalid_redirect_uri", "redirect URIs must be https, or http on localhost, without a fragment")
			return
		}
	}
	name := cleanName(in.ClientName)
	if name == "" {
		name = "An app"
	}
	c, err := s.Connector.registerClient(r.Context(), name, in.RedirectURIs)
	if err != nil {
		if err != errTooManyClients {
			s.log().Warn("couldn't register an app", "err", err)
		}
		oauthErr(w, http.StatusServiceUnavailable, "temporarily_unavailable", "couldn't register the app")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"client_id": c.ID, "client_id_issued_at": c.Created.Unix(), "client_name": c.Name, "redirect_uris": c.RedirectURIs,
		"grant_types": []string{"authorization_code", "refresh_token"}, "response_types": []string{"code"},
		"token_endpoint_auth_method": "none",
	})
}

// ---- pages ----

// connectorPage writes a small page. body is HTML the caller has escaped.
func connectorPage(w http.ResponseWriter, status int, title, body string) {
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	fmt.Fprintf(w, `<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>%s</title>
<style>body{font:16px/1.5 Georgia,serif;margin:0;background:#faf8f4;color:#1d1b17}main{max-width:30rem;margin:12vh auto;padding:0 1.25rem}
h1{font-size:1.5rem;font-weight:600;margin:0 0 .5rem}p{margin:.75rem 0}.muted{color:#6b665c;font-size:.9rem}ul{padding-left:1.2rem}
form{margin-top:1.25rem}input[type=text]{font:inherit;padding:.5rem;width:100%%;box-sizing:border-box;margin:.25rem 0 .75rem}
button{font:inherit;padding:.5rem 1rem;border:1px solid #1d1b17;background:#fff;color:inherit;border-radius:4px;cursor:pointer;margin-right:.5rem}
button.primary{background:#1d1b17;color:#fff}code{font-family:ui-monospace,monospace;font-size:.9em}</style></head><body><main>%s</main></body></html>`,
		html.EscapeString(title), body)
}

// redirectPage sends the browser on. A page, not a redirect: the sign-in
// form's CSP doesn't allow redirects to other sites, and the account's
// server is one.
func redirectPage(w http.ResponseWriter, target string) {
	e := html.EscapeString(target)
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, `<!doctype html><html lang="en"><head><meta charset="utf-8"><meta http-equiv="refresh" content="0;url=%s"><title>Continuing…</title></head><body><p><a href="%s">Continue</a></p></body></html>`, e, e)
}

func errorPage(w http.ResponseWriter, status int, msg string) {
	connectorPage(w, status, "Engram Garden", "<h1>That can't be done</h1><p>"+html.EscapeString(msg)+"</p>")
}

// ---- authorize ----

// authRequest is an app's request to be let in, once its client and
// redirect URI check out.
type authRequest struct {
	client    control.MCPClient
	redirect  string
	state     string
	challenge string
	values    url.Values
}

// authRequest reads an authorization request. When the client or its
// redirect URI isn't right it returns a message for an error page, because
// the browser must not be sent to a URL the app didn't register. When
// anything else is wrong it returns the request and an OAuth error code to
// send back to the app.
func (s *Server) authRequest(ctx context.Context, v url.Values) (ar *authRequest, pageErr, code string) {
	c, err := s.Connector.db.GetMCPClient(ctx, v.Get("client_id"))
	if err != nil {
		s.log().Warn("couldn't look up an app", "err", err)
		return nil, "couldn't look this app up; try again", ""
	}
	if c == nil {
		return nil, "this app isn't registered", ""
	}
	redirect := v.Get("redirect_uri")
	found := false
	for _, u := range c.RedirectURIs {
		if u == redirect {
			found = true
		}
	}
	if !found {
		return nil, "this app asked to be sent somewhere it didn't register", ""
	}
	ar = &authRequest{client: *c, redirect: redirect, state: v.Get("state"), challenge: v.Get("code_challenge"), values: v}
	switch {
	case v.Get("response_type") != "code":
		return ar, "", "unsupported_response_type"
	case ar.challenge == "" || v.Get("code_challenge_method") != "S256" || len(ar.challenge) < 43 || len(ar.challenge) > 128:
		return ar, "", "invalid_request"
	}
	if res := strings.TrimSuffix(v.Get("resource"), "/"); res != "" && res != s.mcpURL() {
		return ar, "", "invalid_target"
	}
	return ar, "", ""
}

// callback is the app's redirect URI with the response added.
func (ar *authRequest) callback(s *Server, q url.Values) string {
	u, _ := url.Parse(ar.redirect)
	query := u.Query()
	for k, vs := range q {
		query[k] = vs
	}
	if ar.state != "" {
		query.Set("state", ar.state)
	}
	query.Set("iss", s.Origin)
	u.RawQuery = query.Encode()
	return u.String()
}

// authorizeFields are the request's parameters the consent form carries
// back.
var authorizeFields = []string{"response_type", "client_id", "redirect_uri", "code_challenge", "code_challenge_method", "state", "resource", "scope"}

func (s *Server) handleAuthorize(w http.ResponseWriter, r *http.Request) {
	ar, pageErr, code := s.authRequest(r.Context(), r.URL.Query())
	if pageErr != "" {
		errorPage(w, http.StatusBadRequest, pageErr)
		return
	}
	if code != "" {
		http.Redirect(w, r, ar.callback(s, url.Values{"error": {code}}), http.StatusSeeOther)
		return
	}
	u, err := s.user(r)
	if err != nil {
		s.signInPage(w, ar.client.Name, r.URL.RequestURI())
		return
	}
	who := u.did.String()
	if ident, err := s.Dir.LookupDID(r.Context(), u.did); err == nil && !ident.Handle.IsInvalidHandle() {
		who = ident.Handle.String()
	}
	var hidden strings.Builder
	for _, k := range authorizeFields {
		if v := ar.values.Get(k); v != "" {
			fmt.Fprintf(&hidden, `<input type="hidden" name="%s" value="%s">`, k, html.EscapeString(v))
		}
	}
	redirectHost := ""
	if ru, err := url.Parse(ar.redirect); err == nil {
		redirectHost = ru.Host
	}
	connectorPage(w, http.StatusOK, "Connect "+ar.client.Name, fmt.Sprintf(`<h1>Connect %[1]s to Engram Garden?</h1>
<p><strong>%[1]s</strong> (it will return you to <code>%[2]s</code>) is asking to read the memory spaces you can read, signed in as <strong>%[3]s</strong>.</p>
<ul><li>It can search and read memories in your spaces.</li><li>It cannot write, change or delete anything.</li></ul>
<p class="muted">You can disconnect it from the Engram Garden app at any time.</p>
<form method="post" action="/mcp-oauth/authorize">%[4]s<button class="primary" name="decision" value="allow">Allow</button><button name="decision" value="deny">Cancel</button></form>`,
		html.EscapeString(ar.client.Name), html.EscapeString(redirectHost), html.EscapeString(who), hidden.String()))
}

// signInPage asks who's connecting, before sending them back to the
// request.
func (s *Server) signInPage(w http.ResponseWriter, app, back string) {
	connectorPage(w, http.StatusOK, "Sign in to Engram Garden", fmt.Sprintf(`<h1>Sign in to connect %s</h1>
<p>Sign in to Engram Garden first. Your account has to be on a server that supports ATProto spaces.</p>
<form method="post" action="/mcp-oauth/signin"><label for="handle">Your handle</label><input id="handle" name="handle" type="text" autocomplete="username" placeholder="alice.example.com" autofocus required>
<input type="hidden" name="return" value="%s"><button class="primary">Sign in</button></form>`, html.EscapeString(app), html.EscapeString(back)))
}

// handleConnectorSignIn starts the web app's sign-in for the connector's
// consent page, and arranges to come back to it.
func (s *Server) handleConnectorSignIn(w http.ResponseWriter, r *http.Request) {
	if !s.sameOrigin(r) {
		errorPage(w, http.StatusForbidden, "this has to be sent from the Engram Garden page")
		return
	}
	if s.OAuth == nil {
		errorPage(w, http.StatusServiceUnavailable, "signing in isn't available")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if err := r.ParseForm(); err != nil {
		errorPage(w, http.StatusBadRequest, "bad form")
		return
	}
	back := r.PostForm.Get("return")
	if !isReturnPath(back) {
		errorPage(w, http.StatusBadRequest, "start from the app you are connecting")
		return
	}
	redirect, e := s.beginSignIn(w, r, r.PostForm.Get("handle"))
	if e != nil {
		errorPage(w, e.Status, e.Message)
		return
	}
	http.SetCookie(w, s.returnCookie(back))
	redirectPage(w, redirect)
}

func (s *Server) handleDecision(w http.ResponseWriter, r *http.Request) {
	if !s.sameOrigin(r) {
		errorPage(w, http.StatusForbidden, "this has to be sent from the Engram Garden page")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if err := r.ParseForm(); err != nil {
		errorPage(w, http.StatusBadRequest, "bad form")
		return
	}
	ar, pageErr, code := s.authRequest(r.Context(), r.PostForm)
	if pageErr != "" {
		errorPage(w, http.StatusBadRequest, pageErr)
		return
	}
	did, sid, ok := s.readCookie(r)
	if _, err := s.user(r); err != nil || !ok {
		errorPage(w, http.StatusUnauthorized, "sign in first")
		return
	}
	if code != "" {
		redirectPage(w, ar.callback(s, url.Values{"error": {code}}))
		return
	}
	if r.PostForm.Get("decision") != "allow" {
		redirectPage(w, ar.callback(s, url.Values{"error": {"access_denied"}}))
		return
	}
	code, err := randomToken(32)
	if err != nil {
		errorPage(w, http.StatusInternalServerError, "couldn't approve")
		return
	}
	saved, err := s.Connector.putCode(r.Context(), code, control.MCPCode{ClientID: ar.client.ID, RedirectURI: ar.redirect, Challenge: ar.challenge,
		DID: did.String(), SessionID: sid, Expires: time.Now().Add(authCodeTTL).UTC()})
	if err != nil {
		s.log().Warn("couldn't save an approval", "err", err)
	}
	if err != nil || !saved {
		errorPage(w, http.StatusServiceUnavailable, "too many approvals are waiting; try again shortly")
		return
	}
	redirectPage(w, ar.callback(s, url.Values{"code": {code}}))
}

// ---- sign-in return ----

// returnCookieName holds where to go once the web app's sign-in is done.
const returnCookieName = "engram_return"

// isReturnPath says whether a path is one sign-in may return to: the
// connector's authorize request, and nowhere else.
func isReturnPath(p string) bool {
	return strings.HasPrefix(p, "/mcp-oauth/authorize?") && len(p) < 3000 && !strings.ContainsAny(p, "\r\n")
}

func (s *Server) returnCookie(path string) *http.Cookie {
	payload := base64.RawURLEncoding.EncodeToString([]byte(path))
	return &http.Cookie{
		Name: returnCookieName, Value: payload + "." + s.mac("return."+payload), Path: "/oauth/callback",
		HttpOnly: true, Secure: strings.HasPrefix(s.Origin, "https://"), SameSite: http.SameSiteLaxMode, MaxAge: 600,
	}
}

func (s *Server) readReturn(value string) (string, bool) {
	payload, sig, ok := strings.Cut(value, ".")
	if !ok || !hmac.Equal([]byte(sig), []byte(s.mac("return."+payload))) {
		return "", false
	}
	raw, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil || !isReturnPath(string(raw)) {
		return "", false
	}
	return string(raw), true
}

// returnTo is where to send a browser that has just signed in: the
// connector's consent page if that's where it came from, else the app.
func (s *Server) returnTo(w http.ResponseWriter, r *http.Request) string {
	c, err := r.Cookie(returnCookieName)
	if err != nil {
		return "/"
	}
	http.SetCookie(w, &http.Cookie{Name: returnCookieName, Value: "", Path: "/oauth/callback", MaxAge: -1, HttpOnly: true,
		Secure: strings.HasPrefix(s.Origin, "https://"), SameSite: http.SameSiteLaxMode})
	if p, ok := s.readReturn(c.Value); ok {
		return p
	}
	return "/"
}

// ---- tokens ----

func (s *Server) handleMCPToken(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if err := r.ParseForm(); err != nil {
		oauthErr(w, http.StatusBadRequest, "invalid_request", "send a form-encoded body")
		return
	}
	switch r.PostForm.Get("grant_type") {
	case "authorization_code":
		s.exchangeCode(r.Context(), w, r.PostForm)
	case "refresh_token":
		s.refreshToken(r.Context(), w, r.PostForm)
	default:
		oauthErr(w, http.StatusBadRequest, "unsupported_grant_type", "use authorization_code or refresh_token")
	}
}

func (s *Server) exchangeCode(ctx context.Context, w http.ResponseWriter, f url.Values) {
	a, err := s.Connector.takeCode(ctx, f.Get("code"))
	if err != nil {
		s.log().Warn("couldn't look up an approval", "err", err)
		oauthErr(w, http.StatusServiceUnavailable, "temporarily_unavailable", "couldn't issue tokens")
		return
	}
	if a == nil {
		oauthErr(w, http.StatusBadRequest, "invalid_grant", "the code is invalid, expired or was already used")
		return
	}
	v := f.Get("code_verifier")
	sum := sha256.Sum256([]byte(v))
	want := base64.RawURLEncoding.EncodeToString(sum[:])
	if f.Get("client_id") != a.ClientID || f.Get("redirect_uri") != a.RedirectURI || len(v) < 43 || len(v) > 128 ||
		subtle.ConstantTimeCompare([]byte(want), []byte(a.Challenge)) != 1 {
		oauthErr(w, http.StatusBadRequest, "invalid_grant", "the code doesn't match this request")
		return
	}
	g, refresh, err := s.Connector.newGrant(ctx, a.ClientID, a.DID, a.SessionID)
	if err != nil {
		s.log().Warn("couldn't record a connector grant", "err", err)
		oauthErr(w, http.StatusServiceUnavailable, "temporarily_unavailable", "couldn't issue tokens")
		return
	}
	s.writeTokens(w, g.ID, refresh)
}

func (s *Server) refreshToken(ctx context.Context, w http.ResponseWriter, f url.Values) {
	g, refresh, err := s.Connector.rotate(ctx, f.Get("refresh_token"), f.Get("client_id"))
	if err == errInvalidGrant {
		oauthErr(w, http.StatusBadRequest, "invalid_grant", "the refresh token is invalid or was already used")
		return
	}
	if err != nil {
		s.log().Warn("couldn't rotate a connector token", "err", err)
		oauthErr(w, http.StatusServiceUnavailable, "temporarily_unavailable", "couldn't issue tokens")
		return
	}
	s.writeTokens(w, g.ID, refresh)
}

func (s *Server) writeTokens(w http.ResponseWriter, grantID, refresh string) {
	writeJSON(w, http.StatusOK, map[string]any{
		"access_token": s.accessToken(grantID, time.Now().Add(accessTokenTTL)), "token_type": "Bearer",
		"expires_in": int(accessTokenTTL.Seconds()), "refresh_token": refresh, "scope": mcpScope,
	})
}

// accessToken names a grant until a time, signed so it can be checked
// without looking anything up.
func (s *Server) accessToken(grantID string, exp time.Time) string {
	payload := base64.RawURLEncoding.EncodeToString([]byte(grantID + "." + strconv.FormatInt(exp.Unix(), 10)))
	return "egm1_" + payload + "." + s.mac("mcp-access."+payload)
}

// verifyAccessToken checks a bearer token at /mcp. The grant has to still
// exist, and the web app session it acts through has to still work.
func (s *Server) verifyAccessToken(ctx context.Context, token string, _ *http.Request) (*auth.TokenInfo, error) {
	invalid := func(why string) (*auth.TokenInfo, error) { return nil, fmt.Errorf("%w: %s", auth.ErrInvalidToken, why) }
	rest, ok := strings.CutPrefix(token, "egm1_")
	if !ok {
		return invalid("not an access token")
	}
	payload, sig, ok := strings.Cut(rest, ".")
	if !ok || !hmac.Equal([]byte(sig), []byte(s.mac("mcp-access."+payload))) {
		return invalid("bad signature")
	}
	raw, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return invalid("malformed")
	}
	id, expStr, ok := strings.Cut(string(raw), ".")
	exp, err := strconv.ParseInt(expStr, 10, 64)
	if !ok || err != nil {
		return invalid("malformed")
	}
	g, err := s.Connector.db.GetMCPGrant(ctx, id)
	if err != nil {
		// Not the token's fault: don't have the app start over.
		return nil, err
	}
	if g == nil {
		return invalid("this connection was ended")
	}
	u, err := s.userFor(ctx, syntax.DID(g.DID), g.SessionID)
	if err != nil {
		// The account signed out of the web app, or its session lapsed.
		// The grant stays, as the failure may not be permanent; the
		// account can end it from the app.
		return invalid("the sign-in behind this connection ended; connect again")
	}
	if time.Since(g.LastUsed) > time.Minute {
		if err := s.Connector.db.TouchMCPGrant(ctx, g.ID, time.Now().UTC()); err != nil {
			s.log().Warn("couldn't note a connection's use", "err", err)
		}
	}
	return &auth.TokenInfo{Scopes: []string{mcpScope}, Expiration: time.Unix(exp, 0), UserID: g.ID, Extra: map[string]any{"user": u}}, nil
}

// ---- managing connections ----

func (s *Server) handleConnectors(w http.ResponseWriter, r *http.Request, u *user) {
	grants, err := s.Connector.db.ListMCPGrants(r.Context(), u.did.String())
	if err != nil {
		s.fail(w, err)
		return
	}
	out := []map[string]any{}
	for _, g := range grants {
		name := "An app"
		if c, err := s.Connector.db.GetMCPClient(r.Context(), g.ClientID); err == nil && c != nil {
			name = c.Name
		}
		out = append(out, map[string]any{"id": g.ID, "clientName": name, "createdAt": g.Created, "lastUsed": g.LastUsed})
	}
	writeJSON(w, http.StatusOK, map[string]any{"connectors": out, "url": s.mcpURL()})
}

func (s *Server) handleRevokeConnector(w http.ResponseWriter, r *http.Request, u *user) {
	var in struct {
		ID string `json:"id"`
	}
	if e := decode(r, &in); e != nil {
		writeErr(w, e)
		return
	}
	ok, err := s.Connector.db.DeleteMCPGrant(r.Context(), in.ID, u.did.String())
	if err != nil {
		s.fail(w, err)
		return
	}
	if !ok {
		writeErr(w, apiErr(http.StatusNotFound, "NotFound", "no such connection"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"revoked": in.ID})
}
