package web

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bluesky-social/indigo/atproto/atclient"
	"github.com/bluesky-social/indigo/atproto/syntax"

	"github.com/haileyok/engram-garden/internal/appview"
	"github.com/haileyok/engram-garden/internal/blob"
	"github.com/haileyok/engram-garden/internal/embed"
	"github.com/haileyok/engram-garden/internal/indexer"
	"github.com/haileyok/engram-garden/internal/lex"
	"github.com/haileyok/engram-garden/internal/spaceclient"
	"github.com/haileyok/engram-garden/internal/spacestore"
	"github.com/haileyok/engram-garden/internal/spacetest"
)

const (
	appviewDID = "did:web:engram.test"
	origin     = "https://web.engram.test"
)

var model = lex.ModelInfo{Model: "hashing-256", ModelDigest: embed.HashingDigest, Dims: 256}

// netAuth stands in for OAuth: a signed-in account's PDS session is its
// session on the test network.
type netAuth struct{ net *spacetest.Net }

func (a netAuth) Resume(_ context.Context, did syntax.DID, sessionID string) (*atclient.APIClient, error) {
	if sessionID != "s-"+did.String() {
		return nil, errors.New("no such session")
	}
	return a.net.Session(a.net.AccountByDID(did.String())), nil
}

func (a netAuth) Logout(context.Context, syntax.DID, string) error { return nil }

// autoGrant stands in for OAuth at the appview: every sign-in is approved
// at once, as the account it was started for, and goes straight back to
// the appview's callback.
type autoGrant struct {
	net      *spacetest.Net
	callback string // the appview's callback URL, once it's serving

	mu      sync.Mutex
	n       int
	pending map[string][2]string // state -> DID, mode
}

func (a *autoGrant) Start(_ context.Context, did syntax.DID, mode string) (string, string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.n++
	state := fmt.Sprintf("st%d", a.n)
	a.pending[state] = [2]string{did.String(), mode}
	return a.callback + "?" + url.Values{"state": {state}, "iss": {"https://pds.test"}, "code": {"c"}}.Encode(), state, nil
}

func (a *autoGrant) Finish(_ context.Context, _ string, q url.Values) (*appview.AuthResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	p, ok := a.pending[q.Get("state")]
	if !ok {
		return nil, errors.New("unknown state")
	}
	delete(a.pending, q.Get("state"))
	scopes := appview.GrantScopes
	if p[1] == "stop" {
		scopes = appview.StopScopes
	}
	return &appview.AuthResult{DID: syntax.DID(p[0]), SessionID: "grant-" + q.Get("state"), Scopes: scopes}, nil
}

func (a *autoGrant) Resume(_ context.Context, did syntax.DID, _ string) (*atclient.APIClient, error) {
	return a.net.Session(a.net.AccountByDID(did.String())), nil
}

func (a *autoGrant) Revoke(context.Context, string, syntax.DID, string) error { return nil }

type fixture struct {
	net     *spacetest.Net
	av      *appview.Server
	web     *Server
	url     string
	alice   *spacetest.Account
	bob     *spacetest.Account
	mallory *spacetest.Account
}

// setup runs the web app against an appview. By default the appview
// indexes the space with its authority's grant; with spaces given, it
// indexes those, and nothing is granted yet.
func setup(t *testing.T, spaces ...string) *fixture {
	t.Helper()
	n := spacetest.New(t)
	f := &fixture{net: n}
	f.alice = n.NewAccount("did:plc:alice")
	f.bob = n.NewAccount("did:plc:bob")
	f.mallory = n.NewAccount("did:plc:mallory")
	for _, a := range []*spacetest.Account{f.alice, f.bob} {
		n.AddMember(a.DID)
	}
	n.Put(n.Authority, lex.ConfigCollection, lex.ConfigRkey, lex.Config{ModelInfo: model}.Record(time.Now()))

	auth := &autoGrant{net: n, pending: map[string][2]string{}}
	bs := blob.Dir{Root: t.TempDir()}
	grants := &appview.Grants{Blob: bs, Auth: auth}
	if spaces == nil {
		spaces = []string{n.Space}
		if err := grants.Put(context.Background(), appview.Grant{Space: n.Space, DID: n.Authority.DID, SessionID: "seed", GrantedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	client, err := spaceclient.NewDelegated(grants, n.Dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	st, err := spacestore.New(spacestore.Options{Blob: blob.Dir{Root: t.TempDir()}, CacheDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close(context.Background()) })
	f.av = &appview.Server{
		Store:            st,
		Indexer:          &indexer.Indexer{Store: st, Client: client, Dir: n.Dir},
		Dir:              n.Dir,
		ServiceDID:       appviewDID,
		Spaces:           spaces,
		Blob:             bs,
		OpenRegistration: true,
		Grants:           grants,
		ReturnOrigins:    []string{origin},
		CookieKey:        []byte("fedcba9876543210fedcba9876543210"),
	}
	avs := httptest.NewServer(f.av.Handler())
	t.Cleanup(avs.Close)
	f.av.PublicURL = avs.URL
	auth.callback = avs.URL + "/oauth/callback"

	f.web = &Server{
		Auth:       netAuth{n},
		Dir:        n.Dir,
		AppviewURL: avs.URL,
		AppviewDID: appviewDID,
		Origin:     origin,
		CookieKey:  []byte("0123456789abcdef0123456789abcdef"),
		LiveEvery:  20 * time.Millisecond,
	}
	ws := httptest.NewServer(f.web.Handler())
	t.Cleanup(ws.Close)
	f.url = ws.URL
	return f
}

func memory(text string, tags ...string) map[string]any {
	v, _ := embed.Hashing{Dims: model.Dims}.Embed(context.Background(), []string{lex.EmbedText("", text, tags)})
	m := map[string]any{"$type": lex.MemoryCollection, "text": text, "createdAt": time.Now().UTC().Format(time.RFC3339)}
	if len(tags) > 0 {
		m["tags"] = tags
	}
	m["embedding"] = lex.EmbeddingRecord(model, v[0])
	return m
}

func (f *fixture) sync(t *testing.T) {
	t.Helper()
	if err := f.av.Indexer.SyncSpace(context.Background(), f.net.Space); err != nil {
		t.Fatal(err)
	}
}

// request builds a request to the web app, signed in as a when set.
func (f *fixture) request(t *testing.T, method, path string, a *spacetest.Account, body any) *http.Request {
	t.Helper()
	var rd io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, f.url+path, rd)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if method != http.MethodGet {
		req.Header.Set("Origin", origin)
	}
	if a != nil {
		req.AddCookie(f.web.sessionCookie(syntax.DID(a.DID), "s-"+a.DID))
	}
	return req
}

func (f *fixture) do(t *testing.T, req *http.Request) (int, map[string]any) {
	t.Helper()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return resp.StatusCode, out
}

func (f *fixture) call(t *testing.T, method, path string, a *spacetest.Account, body any) (int, map[string]any) {
	t.Helper()
	return f.do(t, f.request(t, method, path, a, body))
}

func list(body map[string]any, key string) []map[string]any {
	raw, _ := body[key].([]any)
	out := make([]map[string]any, len(raw))
	for i, m := range raw {
		out[i], _ = m.(map[string]any)
	}
	return out
}

func q(kv ...string) string {
	v := url.Values{}
	for i := 0; i+1 < len(kv); i += 2 {
		v.Add(kv[i], kv[i+1])
	}
	return "?" + v.Encode()
}

func TestSessionRequiresSignIn(t *testing.T) {
	t.Parallel()
	f := setup(t)
	if status, body := f.call(t, "GET", "/api/session", nil, nil); status != 401 || body["error"] != "AuthRequired" {
		t.Fatalf("signed out: %d %v", status, body)
	}
	// A cookie naming another account fails its signature.
	req := f.request(t, "GET", "/api/session", nil, nil)
	good := f.web.sessionCookie(syntax.DID(f.alice.DID), "s-"+f.alice.DID)
	good.Value = strings.Replace(good.Value, base64DID(f.alice.DID), base64DID(f.bob.DID), 1)
	req.AddCookie(good)
	if status, _ := f.do(t, req); status != 401 {
		t.Fatalf("tampered cookie: %d", status)
	}
	status, body := f.call(t, "GET", "/api/session", f.alice, nil)
	if status != 200 || body["did"] != f.alice.DID {
		t.Fatalf("signed in: %d %v", status, body)
	}
	if status, _ := f.call(t, "POST", "/api/logout", f.alice, map[string]any{}); status != 200 {
		t.Fatalf("logout: %d", status)
	}
}

func TestBrowseMemories(t *testing.T) {
	t.Parallel()
	f := setup(t)
	f.net.Put(f.alice, lex.MemoryCollection, "a1", memory("deploys go through the deploy repo", "infra"))
	f.net.Put(f.bob, lex.MemoryCollection, "b1", memory("hailey prefers short answers", "prefs"))
	f.sync(t)

	status, body := f.call(t, "GET", "/api/memories"+q("space", f.net.Space), f.bob, nil)
	if status != 200 || len(list(body, "memories")) != 2 {
		t.Fatalf("list: %d %v", status, body)
	}
	status, body = f.call(t, "GET", "/api/memories"+q("space", f.net.Space, "author", f.alice.DID), f.bob, nil)
	ms := list(body, "memories")
	if status != 200 || len(ms) != 1 || ms[0]["text"] != "deploys go through the deploy repo" {
		t.Fatalf("by author: %d %v", status, body)
	}
	status, body = f.call(t, "GET", "/api/memories"+q("space", f.net.Space, "tags", "prefs"), f.bob, nil)
	ms = list(body, "memories")
	if status != 200 || len(ms) != 1 || ms[0]["author"] != f.bob.DID {
		t.Fatalf("by tag: %d %v", status, body)
	}
	status, body = f.call(t, "GET", "/api/memory"+q("space", f.net.Space, "uri", ms[0]["uri"].(string)), f.alice, nil)
	if m, _ := body["memory"].(map[string]any); status != 200 || m["text"] != "hailey prefers short answers" {
		t.Fatalf("get: %d %v", status, body)
	}
	status, body = f.call(t, "GET", "/api/status"+q("space", f.net.Space), f.alice, nil)
	if status != 200 || body["memories"] != float64(2) {
		t.Fatalf("status: %d %v", status, body)
	}

	// Mallory isn't a member: the authority won't issue her a credential.
	if status, body := f.call(t, "GET", "/api/memories"+q("space", f.net.Space), f.mallory, nil); status != 403 {
		t.Fatalf("non-member: %d %v", status, body)
	}
	// Only memory spaces.
	if status, _ := f.call(t, "GET", "/api/memories"+q("space", "at://did:plc:x/space/com.example.other/x"), f.bob, nil); status != 400 {
		t.Fatalf("other space type: %d", status)
	}
}

func TestDeleteOwnMemory(t *testing.T) {
	t.Parallel()
	f := setup(t)
	uri, _ := f.net.Put(f.alice, lex.MemoryCollection, "a1", memory("delete me"))
	del := map[string]any{"space": f.net.Space, "uri": uri}
	if status, body := f.call(t, "POST", "/api/memories/delete", f.bob, del); status != 403 || body["error"] != "NotYourMemory" {
		t.Fatalf("someone else's: %d %v", status, body)
	}
	if status, body := f.call(t, "POST", "/api/memories/delete", f.alice, del); status != 200 {
		t.Fatalf("own: %d %v", status, body)
	}
	if f.net.Calls("com.atproto.space.deleteRecord") != 1 {
		t.Fatal("record not deleted")
	}
	f.sync(t)
	if status, body := f.call(t, "GET", "/api/memories"+q("space", f.net.Space), f.alice, nil); status != 200 || len(list(body, "memories")) != 0 {
		t.Fatalf("after delete: %d %v", status, body)
	}
}

func TestCrossSiteChangesRefused(t *testing.T) {
	t.Parallel()
	f := setup(t)
	uri, _ := f.net.Put(f.alice, lex.MemoryCollection, "a1", memory("keep me"))
	req := f.request(t, "POST", "/api/memories/delete", f.alice, map[string]any{"space": f.net.Space, "uri": uri})
	req.Header.Set("Origin", "https://evil.example")
	if status, body := f.do(t, req); status != 403 || body["error"] != "CrossSiteRequest" {
		t.Fatalf("cross-site: %d %v", status, body)
	}
	req = f.request(t, "POST", "/api/memories/delete", f.alice, map[string]any{"space": f.net.Space, "uri": uri})
	req.Header.Del("Origin")
	if status, _ := f.do(t, req); status != 403 {
		t.Fatalf("no origin: %d", status)
	}
	req = f.request(t, "POST", "/api/memories/delete", f.alice, nil)
	req.Body = io.NopCloser(strings.NewReader(`{"space":"` + f.net.Space + `","uri":"` + uri + `"}`))
	req.Header.Set("Content-Type", "text/plain")
	if status, _ := f.do(t, req); status != 415 {
		t.Fatalf("form post: %d", status)
	}
	if f.net.Calls("com.atproto.space.deleteRecord") != 0 {
		t.Fatal("deleted")
	}
}

func TestListSpaces(t *testing.T) {
	t.Parallel()
	f := setup(t)
	f.net.Put(f.alice, lex.MemoryCollection, "a1", memory("hi"))
	status, body := f.call(t, "GET", "/api/spaces", f.alice, nil)
	sp := list(body, "spaces")
	if status != 200 || len(sp) != 1 || sp[0]["uri"] != f.net.Space || sp[0]["isAuthority"] != false {
		t.Fatalf("alice: %d %v", status, body)
	}
	status, body = f.call(t, "GET", "/api/spaces", f.net.Authority, nil)
	if sp := list(body, "spaces"); status != 200 || len(sp) != 1 || sp[0]["isAuthority"] != true {
		t.Fatalf("authority: %d %v", status, body)
	}
}

func TestAuthorityManagesSpace(t *testing.T) {
	t.Parallel()
	f := setup(t)
	auth := f.net.Authority

	status, body := f.call(t, "POST", "/api/spaces/create", auth, map[string]any{"name": "team"})
	created, _ := body["uri"].(string)
	if status != 200 || created != "at://"+auth.DID+"/space/garden.engram.space/team" {
		t.Fatalf("create: %d %v", status, body)
	}
	if status, body := f.call(t, "POST", "/api/spaces/create", auth, map[string]any{"name": "team"}); status != 400 || body["error"] != "SpaceAlreadyExists" {
		t.Fatalf("create twice: %d %v", status, body)
	}
	if status, body := f.call(t, "POST", "/api/spaces/create", auth, map[string]any{"name": "bad name!"}); status != 400 {
		t.Fatalf("bad name: %d %v", status, body)
	}

	put := map[string]any{"space": created, "member": f.alice.DID, "read": true, "write": true}
	if status, body := f.call(t, "POST", "/api/members/put", auth, put); status != 200 {
		t.Fatalf("put member: %d %v", status, body)
	}
	status, body = f.call(t, "GET", "/api/members"+q("space", created), auth, nil)
	if ms := list(body, "members"); status != 200 || len(ms) != 1 || ms[0]["did"] != f.alice.DID {
		t.Fatalf("members: %d %v", status, body)
	}
	if status, body := f.call(t, "POST", "/api/members/remove", auth, map[string]any{"space": created, "did": f.alice.DID}); status != 200 {
		t.Fatalf("remove: %d %v", status, body)
	}
	if got := f.net.Members(created); len(got) != 0 {
		t.Fatalf("still members: %v", got)
	}

	// Only the authority manages a space.
	if status, body := f.call(t, "POST", "/api/members/put", f.alice, map[string]any{"space": f.net.Space, "member": f.mallory.DID, "read": true, "write": true}); status != 403 || body["error"] != "NotSpaceOwner" {
		t.Fatalf("member managing: %d %v", status, body)
	}
	if status, _ := f.call(t, "GET", "/api/members"+q("space", f.net.Space), f.alice, nil); status != 403 {
		t.Fatalf("member listing members: %d", status)
	}
}

func TestConfigChanges(t *testing.T) {
	t.Parallel()
	f := setup(t)
	auth := f.net.Authority
	status, body := f.call(t, "GET", "/api/config"+q("space", f.net.Space), auth, nil)
	if cfg, _ := body["config"].(map[string]any); status != 200 || cfg["model"] != model.Model {
		t.Fatalf("read: %d %v", status, body)
	}
	next := map[string]any{"model": "hashing-512", "modelDigest": embed.HashingDigest, "dims": 512}
	status, body = f.call(t, "POST", "/api/config", auth, map[string]any{"space": f.net.Space, "action": "next", "model": next})
	if cfg, _ := body["config"].(map[string]any); status != 200 || cfg["next"] == nil {
		t.Fatalf("next: %d %v", status, body)
	}
	status, body = f.call(t, "POST", "/api/config", auth, map[string]any{"space": f.net.Space, "action": "promote"})
	if cfg, _ := body["config"].(map[string]any); status != 200 || cfg["model"] != "hashing-512" || cfg["next"] != nil {
		t.Fatalf("promote: %d %v", status, body)
	}
	if status, body := f.call(t, "POST", "/api/config", auth, map[string]any{"space": f.net.Space, "action": "promote"}); status != 400 {
		t.Fatalf("promote again: %d %v", status, body)
	}
	if status, _ := f.call(t, "POST", "/api/config", f.alice, map[string]any{"space": f.net.Space, "action": "cancel"}); status != 403 {
		t.Fatalf("member changing config: %d", status)
	}
}

// TestGrantThroughWeb follows what the frontend does: it reads the
// appview's grant page from /api/service and sends the browser there,
// which comes back to the web app.
func TestGrantThroughWeb(t *testing.T) {
	t.Parallel()
	f := setup(t, []string{}...)
	status, body := f.call(t, "GET", "/api/status"+q("space", f.net.Space), f.net.Authority, nil)
	if status != 400 || body["error"] != "UnknownSpace" {
		t.Fatalf("before: %d %v", status, body)
	}
	status, body = f.call(t, "GET", "/api/service", f.net.Authority, nil)
	grantURL, _ := body["grantUrl"].(string)
	if status != 200 || grantURL != f.av.PublicURL+"/oauth/grant" || body["registration"] != "open" || body["account"] != nil {
		t.Fatalf("service: %d %v", status, body)
	}

	jar, _ := cookiejar.New(nil)
	b := &http.Client{Jar: jar, CheckRedirect: func(req *http.Request, _ []*http.Request) error {
		if strings.HasPrefix(req.URL.String(), origin) {
			return http.ErrUseLastResponse // back at the web app
		}
		return nil
	}}
	back := origin + "/space/x"
	resp, err := b.Get(grantURL + "?" + url.Values{"space": {f.net.Space}, "mode": {"grant"}, "return": {back}}.Encode())
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if loc := resp.Header.Get("Location"); resp.StatusCode != http.StatusSeeOther || loc != back+"?indexing=granted" {
		t.Fatalf("came back with %d %q", resp.StatusCode, loc)
	}
	f.av.Jobs.Wait()
	status, body = f.call(t, "GET", "/api/status"+q("space", f.net.Space), f.net.Authority, nil)
	if access, _ := body["access"].(map[string]any); status != 200 || access["state"] != "granted" {
		t.Fatalf("after: %d %v", status, body)
	}
}

func TestLiveUpdates(t *testing.T) {
	t.Parallel()
	f := setup(t)
	f.net.Put(f.alice, lex.MemoryCollection, "a1", memory("already here"))
	f.sync(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req := f.request(t, "GET", "/api/live"+q("space", f.net.Space), f.bob, nil).WithContext(ctx)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("live: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	sc := bufio.NewScanner(resp.Body)
	// Wait for the stream to be ready, then write.
	for sc.Scan() && sc.Text() != "event: ready" {
	}
	f.net.Put(f.alice, lex.MemoryCollection, "a2", memory("brand new memory"))
	f.sync(t)
	event := ""
	for sc.Scan() {
		line := sc.Text()
		if v, ok := strings.CutPrefix(line, "event: "); ok {
			event = v
			continue
		}
		data, ok := strings.CutPrefix(line, "data: ")
		if !ok || event != "memories" {
			continue
		}
		var got struct {
			Memories []map[string]any `json:"memories"`
		}
		if err := json.Unmarshal([]byte(data), &got); err != nil {
			t.Fatal(err)
		}
		if len(got.Memories) != 1 || got.Memories[0]["text"] != "brand new memory" {
			t.Fatalf("live event: %s", data)
		}
		return
	}
	t.Fatalf("stream ended: %v", sc.Err())
}
