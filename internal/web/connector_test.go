package web

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"html"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/bluesky-social/indigo/atproto/syntax"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/haileyok/engram-garden/internal/indexer"
	"github.com/haileyok/engram-garden/internal/spacetest"
)

const (
	claudeCallback = "https://claude.ai/api/mcp/auth_callback"
	verifier       = "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"
)

func challenge(v string) string {
	h := sha256.Sum256([]byte(v))
	return base64.RawURLEncoding.EncodeToString(h[:])
}

// noFollow keeps redirects, so the test sees where the server sends the
// browser.
var noFollow = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

func (f *fixture) raw(t *testing.T, req *http.Request) (*http.Response, string) {
	t.Helper()
	resp, err := noFollow.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

// registerClient registers a client the way claude.ai does.
func (f *fixture) registerClient(t *testing.T, redirects ...string) string {
	t.Helper()
	if redirects == nil {
		redirects = []string{claudeCallback}
	}
	body := map[string]any{"redirect_uris": redirects, "client_name": "Claude", "token_endpoint_auth_method": "none",
		"grant_types": []string{"authorization_code", "refresh_token"}, "response_types": []string{"code"}}
	status, out := f.call(t, "POST", "/mcp-oauth/register", nil, body)
	if status != 201 || out["client_id"] == "" || out["client_id"] == nil {
		t.Fatalf("register: %d %v", status, out)
	}
	return out["client_id"].(string)
}

func (f *fixture) authorizeURL(clientID, redirect string) string {
	return f.url + "/mcp-oauth/authorize?" + url.Values{
		"response_type": {"code"}, "client_id": {clientID}, "redirect_uri": {redirect},
		"code_challenge": {challenge(verifier)}, "code_challenge_method": {"S256"},
		"state": {"xyz"}, "resource": {origin + "/mcp"}, "scope": {"memories:read"},
	}.Encode()
}

func (f *fixture) authorizeURLQuery(clientID, redirect string) url.Values {
	u, _ := url.Parse(f.authorizeURL(clientID, redirect))
	return u.Query()
}

var hrefRE = regexp.MustCompile(`href="([^"]+)"`)

// redirectTarget is where a redirect page, or a redirect, sends the browser.
func redirectTarget(t *testing.T, resp *http.Response, body string) *url.URL {
	t.Helper()
	loc := resp.Header.Get("Location")
	if loc == "" {
		m := hrefRE.FindStringSubmatch(body)
		if m == nil {
			t.Fatalf("no redirect: %d %s", resp.StatusCode, body)
		}
		loc = html.UnescapeString(m[1])
	}
	u, err := url.Parse(loc)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// decide submits the consent form as a signed-in account.
func (f *fixture) decide(t *testing.T, a *spacetest.Account, clientID, redirect, decision string) *url.URL {
	t.Helper()
	form := f.authorizeURLQuery(clientID, redirect)
	form.Set("decision", decision)
	req, _ := http.NewRequest("POST", f.url+"/mcp-oauth/authorize", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", origin)
	req.AddCookie(f.web.sessionCookie(syntaxDID(a.DID), "s-"+a.DID))
	resp, body := f.raw(t, req)
	return redirectTarget(t, resp, body)
}

type tokens struct{ Access, Refresh string }

func (f *fixture) token(t *testing.T, form url.Values) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest("POST", f.url+"/mcp-oauth/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return f.do(t, req)
}

// connect runs the whole authorization flow as a, and returns the tokens.
func (f *fixture) connect(t *testing.T, a *spacetest.Account) (string, tokens) {
	t.Helper()
	clientID := f.registerClient(t)
	cb := f.decide(t, a, clientID, claudeCallback, "allow")
	if cb.Query().Get("code") == "" || cb.Query().Get("state") != "xyz" {
		t.Fatalf("callback: %s", cb)
	}
	status, out := f.token(t, url.Values{"grant_type": {"authorization_code"}, "code": {cb.Query().Get("code")},
		"redirect_uri": {claudeCallback}, "client_id": {clientID}, "code_verifier": {verifier}})
	if status != 200 || out["token_type"] != "Bearer" || out["access_token"] == nil || out["refresh_token"] == nil {
		t.Fatalf("token: %d %v", status, out)
	}
	return clientID, tokens{out["access_token"].(string), out["refresh_token"].(string)}
}

type bearer struct{ token string }

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(r)
}

func (f *fixture) mcpSession(t *testing.T, token string) *mcp.ClientSession {
	t.Helper()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil).Connect(context.Background(),
		&mcp.StreamableClientTransport{Endpoint: f.url + "/mcp", HTTPClient: &http.Client{Transport: bearer{token}}, DisableStandaloneSSE: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func callTool(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) (map[string]any, string) {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if res.IsError {
		var msgs []string
		for _, c := range res.Content {
			if tc, ok := c.(*mcp.TextContent); ok {
				msgs = append(msgs, tc.Text)
			}
		}
		return nil, strings.Join(msgs, " ")
	}
	raw, _ := json.Marshal(res.StructuredContent)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return out, ""
}

func TestConnectorMetadata(t *testing.T) {
	t.Parallel()
	f := setup(t)
	for _, p := range []string{"/.well-known/oauth-protected-resource", "/.well-known/oauth-protected-resource/mcp"} {
		status, body := f.call(t, "GET", p, nil, nil)
		servers, _ := body["authorization_servers"].([]any)
		if status != 200 || body["resource"] != origin+"/mcp" || len(servers) != 1 || servers[0] != origin {
			t.Fatalf("%s: %d %v", p, status, body)
		}
	}
	status, body := f.call(t, "GET", "/.well-known/oauth-authorization-server", nil, nil)
	if status != 200 || body["issuer"] != origin || body["authorization_endpoint"] != origin+"/mcp-oauth/authorize" ||
		body["token_endpoint"] != origin+"/mcp-oauth/token" || body["registration_endpoint"] != origin+"/mcp-oauth/register" {
		t.Fatalf("authorization server: %d %v", status, body)
	}
	if m, _ := body["code_challenge_methods_supported"].([]any); len(m) != 1 || m[0] != "S256" {
		t.Fatalf("PKCE methods: %v", body["code_challenge_methods_supported"])
	}
}

func TestConnectorRegistrationRefusesRiskyRedirects(t *testing.T) {
	t.Parallel()
	f := setup(t)
	for _, bad := range []string{"http://evil.example/cb", "javascript:alert(1)", "https://claude.ai/cb#frag", "not a url", ""} {
		status, body := f.call(t, "POST", "/mcp-oauth/register", nil, map[string]any{"redirect_uris": []string{bad}, "client_name": "x"})
		if status != 400 || body["error"] != "invalid_redirect_uri" {
			t.Fatalf("redirect %q: %d %v", bad, status, body)
		}
	}
	if status, body := f.call(t, "POST", "/mcp-oauth/register", nil, map[string]any{"client_name": "x"}); status != 400 {
		t.Fatalf("no redirects: %d %v", status, body)
	}
	// Loopback clients (a desktop app's local callback) are fine.
	f.registerClient(t, "http://127.0.0.1:33418/callback")
	// Anyone can register, so a client can't pass off control characters
	// as its name.
	status, out := f.call(t, "POST", "/mcp-oauth/register", nil, map[string]any{"redirect_uris": []string{claudeCallback}, "client_name": "Cl\x00au\nde"})
	if status != 201 || strings.ContainsAny(out["client_name"].(string), "\x00\n") {
		t.Fatalf("name: %d %v", status, out)
	}
}

func TestConnectorAuthorizeNeedsASignedInUserAndAKnownClient(t *testing.T) {
	t.Parallel()
	f := setup(t)
	clientID := f.registerClient(t)

	// Signed out: asked to sign in, and the page sends the request back
	// here afterwards.
	resp, body := f.raw(t, mustReq(t, "GET", f.authorizeURL(clientID, claudeCallback), nil))
	if resp.StatusCode != 200 || !strings.Contains(body, `action="/mcp-oauth/signin"`) || !strings.Contains(body, `name="handle"`) || !strings.Contains(body, "/mcp-oauth/authorize?") {
		t.Fatalf("signed out: %d %s", resp.StatusCode, body)
	}

	// Signed in: the page says who is asking and what they get.
	req := mustReq(t, "GET", f.authorizeURL(clientID, claudeCallback), nil)
	req.AddCookie(f.web.sessionCookie(syntaxDID(f.alice.DID), "s-"+f.alice.DID))
	resp, body = f.raw(t, req)
	if resp.StatusCode != 200 || !strings.Contains(body, "Claude") || !strings.Contains(body, `name="decision"`) || !strings.Contains(body, "cannot write") {
		t.Fatalf("consent page: %d %s", resp.StatusCode, body)
	}
	if strings.Contains(body, "<script") {
		t.Fatalf("the consent page has a script: %s", body)
	}

	// A redirect the client didn't register gets an error page, never a
	// redirect: sending the browser there would hand over a code.
	req = mustReq(t, "GET", f.authorizeURL(clientID, "https://evil.example/cb"), nil)
	req.AddCookie(f.web.sessionCookie(syntaxDID(f.alice.DID), "s-"+f.alice.DID))
	if resp, body = f.raw(t, req); resp.StatusCode != 400 || resp.Header.Get("Location") != "" {
		t.Fatalf("unregistered redirect: %d %s", resp.StatusCode, body)
	}
	if resp, _ = f.raw(t, mustReq(t, "GET", f.authorizeURL("nobody", claudeCallback), nil)); resp.StatusCode != 400 {
		t.Fatalf("unknown client: %d", resp.StatusCode)
	}

	// PKCE is required.
	q := f.authorizeURLQuery(clientID, claudeCallback)
	q.Del("code_challenge")
	req = mustReq(t, "GET", f.url+"/mcp-oauth/authorize?"+q.Encode(), nil)
	req.AddCookie(f.web.sessionCookie(syntaxDID(f.alice.DID), "s-"+f.alice.DID))
	resp, body = f.raw(t, req)
	if u := redirectTarget(t, resp, body); u.Query().Get("error") != "invalid_request" || u.Query().Get("state") != "xyz" {
		t.Fatalf("no challenge: %s", u)
	}
}

func TestConnectorDecisionNeedsSameOriginAndASession(t *testing.T) {
	t.Parallel()
	f := setup(t)
	clientID := f.registerClient(t)
	form := f.authorizeURLQuery(clientID, claudeCallback)
	form.Set("decision", "allow")
	post := func(originHeader string, a *spacetest.Account) int {
		req, _ := http.NewRequest("POST", f.url+"/mcp-oauth/authorize", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if originHeader != "" {
			req.Header.Set("Origin", originHeader)
		}
		if a != nil {
			req.AddCookie(f.web.sessionCookie(syntaxDID(a.DID), "s-"+a.DID))
		}
		resp, _ := f.raw(t, req)
		return resp.StatusCode
	}
	if status := post("https://evil.example", f.alice); status != 403 {
		t.Fatalf("cross-site decision: %d", status)
	}
	if status := post("", f.alice); status != 403 {
		t.Fatalf("decision with no origin: %d", status)
	}
	if status := post(origin, nil); status != 401 {
		t.Fatalf("decision signed out: %d", status)
	}
}

func TestConnectorDenied(t *testing.T) {
	t.Parallel()
	f := setup(t)
	clientID := f.registerClient(t)
	cb := f.decide(t, f.alice, clientID, claudeCallback, "deny")
	if cb.Query().Get("error") != "access_denied" || cb.Query().Get("code") != "" || cb.Query().Get("state") != "xyz" || cb.Host != "claude.ai" {
		t.Fatalf("denied: %s", cb)
	}
}

func TestConnectorTokenChecks(t *testing.T) {
	t.Parallel()
	f := setup(t)
	clientID := f.registerClient(t)
	code := func() string { return f.decide(t, f.alice, clientID, claudeCallback, "allow").Query().Get("code") }
	exchange := func(code, redirect, client, ver string) (int, map[string]any) {
		return f.token(t, url.Values{"grant_type": {"authorization_code"}, "code": {code},
			"redirect_uri": {redirect}, "client_id": {client}, "code_verifier": {ver}})
	}

	if status, body := exchange(code(), claudeCallback, clientID, "the-wrong-verifier-the-wrong-verifier-the-wrong-verifier"); status != 400 || body["error"] != "invalid_grant" {
		t.Fatalf("wrong verifier: %d %v", status, body)
	}
	if status, body := exchange(code(), "https://claude.ai/other", clientID, verifier); status != 400 || body["error"] != "invalid_grant" {
		t.Fatalf("other redirect: %d %v", status, body)
	}
	other := f.registerClient(t)
	if status, body := exchange(code(), claudeCallback, other, verifier); status != 400 || body["error"] != "invalid_grant" {
		t.Fatalf("other client: %d %v", status, body)
	}
	// A code works once.
	c := code()
	if status, body := exchange(c, claudeCallback, clientID, verifier); status != 200 {
		t.Fatalf("first use: %d %v", status, body)
	}
	if status, body := exchange(c, claudeCallback, clientID, verifier); status != 400 || body["error"] != "invalid_grant" {
		t.Fatalf("second use: %d %v", status, body)
	}
	if status, body := f.token(t, url.Values{"grant_type": {"password"}}); status != 400 || body["error"] != "unsupported_grant_type" {
		t.Fatalf("grant type: %d %v", status, body)
	}
}

func TestConnectorRefreshRotates(t *testing.T) {
	t.Parallel()
	f := setup(t)
	clientID, tok := f.connect(t, f.alice)
	status, out := f.token(t, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {tok.Refresh}, "client_id": {clientID}})
	if status != 200 || out["refresh_token"] == nil || out["refresh_token"] == tok.Refresh || out["access_token"] == nil {
		t.Fatalf("refresh: %d %v", status, out)
	}
	// The old refresh token is spent.
	if status, body := f.token(t, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {tok.Refresh}, "client_id": {clientID}}); status != 400 || body["error"] != "invalid_grant" {
		t.Fatalf("reused refresh token: %d %v", status, body)
	}
	// And so is the whole grant, once a spent token shows up again: someone
	// may hold a copy.
	if status, body := f.token(t, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {out["refresh_token"].(string)}, "client_id": {clientID}}); status != 400 {
		t.Fatalf("refresh after reuse: %d %v", status, body)
	}
	// Another client can't use it.
	_, tok2 := f.connect(t, f.alice)
	if status, _ := f.token(t, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {tok2.Refresh}, "client_id": {"someone-else"}}); status != 400 {
		t.Fatalf("refresh as another client: %d", status)
	}
}

func TestConnectorMCPRequiresAToken(t *testing.T) {
	t.Parallel()
	f := setup(t)
	req := mustReq(t, "POST", f.url+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, _ := f.raw(t, req)
	if resp.StatusCode != 401 || !strings.Contains(resp.Header.Get("WWW-Authenticate"), origin+"/.well-known/oauth-protected-resource") {
		t.Fatalf("no token: %d %v", resp.StatusCode, resp.Header)
	}
	_, tok := f.connect(t, f.alice)
	for _, bad := range []string{"garbage", tok.Access + "x", strings.Replace(tok.Access, ".", ".A", 1), tok.Refresh} {
		req = mustReq(t, "POST", f.url+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set("Authorization", "Bearer "+bad)
		if resp, _ := f.raw(t, req); resp.StatusCode != 401 {
			t.Fatalf("bad token %q: %d", bad, resp.StatusCode)
		}
	}
}

func TestConnectorToolsAreReadOnlyAndSearchTheUsersSpaces(t *testing.T) {
	t.Parallel()
	f := setup(t)
	f.net.Put(f.alice, indexer.Collection, "a1", memory("pop1 deploys go through the deploy repo workflow", "infra"))
	f.net.Put(f.bob, indexer.Collection, "b1", memory("hailey prefers short answers", "prefs"))
	f.sync(t)

	_, tok := f.connect(t, f.bob)
	cs := f.mcpSession(t, tok.Access)

	tools, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, tl := range tools.Tools {
		names[tl.Name] = true
		if tl.Annotations == nil || !tl.Annotations.ReadOnlyHint {
			t.Fatalf("tool %s isn't marked read-only", tl.Name)
		}
	}
	for _, want := range []string{"list_spaces", "recall", "get_memory", "list_memories"} {
		if !names[want] {
			t.Fatalf("missing tool %s: %v", want, names)
		}
	}
	for _, no := range []string{"remember", "forget", "create_space", "add_member", "set_model"} {
		if names[no] {
			t.Fatalf("the connector offers %s", no)
		}
	}

	out, errText := callTool(t, cs, "list_spaces", nil)
	spaces, _ := out["spaces"].([]any)
	if errText != "" || len(spaces) != 1 || spaces[0].(map[string]any)["uri"] != f.net.Space {
		t.Fatalf("list_spaces: %v %s", out, errText)
	}

	// Bob finds what Alice wrote, by meaning, with no vector of his own.
	out, errText = callTool(t, cs, "recall", map[string]any{"query": "deploy workflow", "limit": 1})
	ms, _ := out["memories"].([]any)
	if errText != "" || len(ms) != 1 {
		t.Fatalf("recall: %v %s", out, errText)
	}
	hit := ms[0].(map[string]any)
	if hit["author"] != f.alice.DID || hit["space"] != f.net.Space || hit["similarity"] == nil {
		t.Fatalf("recall hit: %v", hit)
	}

	out, errText = callTool(t, cs, "get_memory", map[string]any{"uri": hit["uri"]})
	if errText != "" || out["memory"].(map[string]any)["text"] != "pop1 deploys go through the deploy repo workflow" {
		t.Fatalf("get_memory: %v %s", out, errText)
	}
	out, errText = callTool(t, cs, "list_memories", map[string]any{"tags": []string{"prefs"}})
	ms, _ = out["memories"].([]any)
	if errText != "" || len(ms) != 1 || ms[0].(map[string]any)["author"] != f.bob.DID {
		t.Fatalf("list_memories: %v %s", out, errText)
	}

	// A space the account isn't in is out of reach, whatever the model
	// asks for.
	_, errText = callTool(t, cs, "recall", map[string]any{"query": "x", "space": "at://did:plc:other/space/garden.engram.space/secret"})
	if errText == "" {
		t.Fatal("recall in a space the account isn't a member of worked")
	}
}

func TestConnectorTokensActAsTheirOwnUser(t *testing.T) {
	t.Parallel()
	f := setup(t)
	f.net.Put(f.alice, indexer.Collection, "a1", memory("alice only notes live in the deploy repo", "infra"))
	f.sync(t)
	// Mallory isn't a member of the space.
	_, tok := f.connect(t, f.mallory)
	cs := f.mcpSession(t, tok.Access)
	out, errText := callTool(t, cs, "recall", map[string]any{"query": "deploy repo"})
	ms, _ := out["memories"].([]any)
	if len(ms) != 0 {
		t.Fatalf("a non-member's connector found %v (%s)", ms, errText)
	}
	out, _ = callTool(t, cs, "list_spaces", nil)
	if sp, _ := out["spaces"].([]any); len(sp) != 0 {
		t.Fatalf("a non-member's spaces: %v", sp)
	}
}

func TestConnectorRevocation(t *testing.T) {
	t.Parallel()
	f := setup(t)
	clientID, tok := f.connect(t, f.alice)
	cs := f.mcpSession(t, tok.Access)
	if _, errText := callTool(t, cs, "list_spaces", nil); errText != "" {
		t.Fatal(errText)
	}

	status, body := f.call(t, "GET", "/api/connectors", f.alice, nil)
	conns := list(body, "connectors")
	if status != 200 || len(conns) != 1 || conns[0]["clientName"] != "Claude" || conns[0]["id"] == "" {
		t.Fatalf("connectors: %d %v", status, body)
	}
	// Someone else can't revoke it.
	if status, _ := f.call(t, "POST", "/api/connectors/revoke", f.bob, map[string]string{"id": conns[0]["id"].(string)}); status != 404 {
		t.Fatalf("revoke as another account: %d", status)
	}
	if status, _ := f.call(t, "POST", "/api/connectors/revoke", f.alice, map[string]string{"id": conns[0]["id"].(string)}); status != 200 {
		t.Fatalf("revoke: %d", status)
	}

	// The access token and the refresh token both stop working.
	req := mustReq(t, "POST", f.url+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+tok.Access)
	if resp, _ := f.raw(t, req); resp.StatusCode != 401 {
		t.Fatalf("revoked access token: %d", resp.StatusCode)
	}
	if status, _ := f.token(t, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {tok.Refresh}, "client_id": {clientID}}); status != 400 {
		t.Fatalf("revoked refresh token: %d", status)
	}
}

// TestConnectorStateIsInTheDatabase starts a second web node, or a restarted
// one, on the same database: what the first approved is there, and a token
// is traded once across both.
func TestConnectorStateIsInTheDatabase(t *testing.T) {
	t.Parallel()
	f := setup(t)
	// A space is listed for accounts that wrote to it.
	f.net.Put(f.alice, indexer.Collection, "a1", memory("pop1 deploys go through the deploy repo workflow", "infra"))
	clientID, tok := f.connect(t, f.alice)

	second := &Server{Connector: NewConnector(f.web.Connector.db), Auth: f.web.Auth, Dir: f.web.Dir, AppviewURL: f.web.AppviewURL,
		AppviewDID: appviewDID, Origin: origin, CookieKey: f.web.CookieKey}
	ts := httptest.NewServer(second.Handler())
	t.Cleanup(ts.Close)
	g := &fixture{net: f.net, web: second, url: ts.URL, alice: f.alice, bob: f.bob}

	// The second node honors the first's access token...
	cs := g.mcpSession(t, tok.Access)
	out, errText := callTool(t, cs, "list_spaces", nil)
	if sp, _ := out["spaces"].([]any); errText != "" || len(sp) != 1 {
		t.Fatalf("list_spaces on the second node: %v %s", out, errText)
	}
	// ...and sees the account's connection.
	if status, body := g.call(t, "GET", "/api/connectors", f.alice, nil); status != 200 || len(list(body, "connectors")) != 1 {
		t.Fatalf("connectors on the second node: %d %v", status, body)
	}
	// The refresh token is traded on the second node, and is spent on the
	// first.
	status, out := g.token(t, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {tok.Refresh}, "client_id": {clientID}})
	if status != 200 {
		t.Fatalf("refresh on the second node: %d %v", status, out)
	}
	if status, body := f.token(t, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {tok.Refresh}, "client_id": {clientID}}); status != 400 {
		t.Fatalf("the spent refresh token on the first node: %d %v", status, body)
	}
}

func TestConnectorSignInReturnsToTheAuthorizeRequest(t *testing.T) {
	t.Parallel()
	f := setup(t)
	want := "/mcp-oauth/authorize?client_id=abc&state=xyz"
	c := f.web.returnCookie(want)
	if got, ok := f.web.readReturn(c.Value); !ok || got != want {
		t.Fatalf("return cookie: %q %v", got, ok)
	}
	// Only the authorize endpoint is somewhere to return to, and the
	// cookie can't be forged.
	for _, bad := range []string{"https://evil.example/", "//evil.example/", "/api/session", "/mcp-oauth/authorizex"} {
		if _, ok := f.web.readReturn(f.web.returnCookie(bad).Value); ok {
			t.Fatalf("returning to %q", bad)
		}
	}
	other := f.web.returnCookie("/mcp-oauth/authorize?client_id=evil&state=xyz")
	payload, _, _ := strings.Cut(other.Value, ".")
	_, sig, _ := strings.Cut(c.Value, ".")
	if _, ok := f.web.readReturn(payload + "." + sig); ok {
		t.Fatal("a return cookie with another cookie's signature was accepted")
	}
}

func TestConnectorIsOffWithoutAConnector(t *testing.T) {
	t.Parallel()
	f := setup(t)
	s := &Server{Auth: f.web.Auth, Dir: f.web.Dir, AppviewURL: f.web.AppviewURL, AppviewDID: appviewDID, Origin: origin, CookieKey: f.web.CookieKey}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	for _, p := range []string{"/mcp", "/mcp-oauth/register", "/.well-known/oauth-authorization-server"} {
		req, _ := http.NewRequest("POST", ts.URL+p, strings.NewReader("{}"))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode == 200 || resp.StatusCode == 201 {
			t.Fatalf("%s answers with a connector off: %d", p, resp.StatusCode)
		}
	}
}

func mustReq(t *testing.T, method, u string, body io.Reader) *http.Request {
	t.Helper()
	req, err := http.NewRequest(method, u, body)
	if err != nil {
		t.Fatal(err)
	}
	return req
}

func syntaxDID(s string) syntax.DID { return syntax.DID(s) }
