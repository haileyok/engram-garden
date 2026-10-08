package appview

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bluesky-social/indigo/atproto/atclient"
	"github.com/bluesky-social/indigo/atproto/auth/oauth"
	"github.com/bluesky-social/indigo/atproto/syntax"

	"github.com/haileyok/engram-garden/internal/control"
	"github.com/haileyok/engram-garden/internal/control/controltest"
	"github.com/haileyok/engram-garden/internal/indexer"
	"github.com/haileyok/engram-garden/internal/spaceclient"
	"github.com/haileyok/engram-garden/internal/spacetest"
)

const webOrigin = "https://web.test"

// fakeAuth stands in for OAuth: every sign-in succeeds as the account it was
// started for (unless a test says otherwise), and sessions are spacetest
// sessions.
type fakeAuth struct {
	net *spacetest.Net

	mu       sync.Mutex
	n        int
	starts   map[string]fakeStart // state -> sign-in in progress
	signInAs map[string]string    // state -> the DID that actually signs in
	scopes   map[string][]string  // state -> scopes granted instead of those asked for
	sessions map[string]string    // session ID -> DID
	revoked  map[string]bool      // session ID -> revoked by the appview
	refused  map[string]bool      // session ID -> the authorization server refuses it
}

type fakeStart struct {
	did  string
	mode string
}

func newFakeAuth(n *spacetest.Net) *fakeAuth {
	return &fakeAuth{net: n, starts: map[string]fakeStart{}, signInAs: map[string]string{}, scopes: map[string][]string{},
		sessions: map[string]string{}, revoked: map[string]bool{}, refused: map[string]bool{}}
}

func (a *fakeAuth) Start(_ context.Context, did syntax.DID, mode string) (string, string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.n++
	state := fmt.Sprintf("state%d", a.n)
	a.starts[state] = fakeStart{did: did.String(), mode: mode}
	return "https://pds.test/authorize?state=" + state, state, nil
}

func (a *fakeAuth) Finish(_ context.Context, _ string, q url.Values) (*AuthResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	state := q.Get("state")
	st, ok := a.starts[state]
	if !ok {
		return nil, errors.New("unknown state")
	}
	delete(a.starts, state)
	if q.Get("error") == "access_denied" {
		return nil, ErrDeclined
	}
	did := st.did
	if as := a.signInAs[state]; as != "" {
		did = as
	}
	scopes := GrantScopes
	if st.mode == modeStop {
		scopes = StopScopes
	}
	if s, ok := a.scopes[state]; ok {
		scopes = s
	}
	sid := "session-" + state
	a.sessions[sid] = did
	return &AuthResult{DID: syntax.DID(did), SessionID: sid, Scopes: scopes}, nil
}

func (a *fakeAuth) Resume(_ context.Context, did syntax.DID, sid string) (*atclient.APIClient, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.sessions[sid] != did.String() || a.revoked[sid] {
		return nil, fmt.Errorf("loading OAuth session: %w", control.ErrNotFound)
	}
	c := a.net.Session(a.net.AccountByDID(did.String()))
	if a.refused[sid] {
		c.Auth = refusedAuth{}
	}
	return c, nil
}

func (a *fakeAuth) Revoke(_ context.Context, _ string, _ syntax.DID, sid string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.revoked[sid] = true
	return nil
}

func (a *fakeAuth) isRevoked(sid string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.revoked[sid]
}

// refuse makes the authorization server refuse a session, as when the
// person revokes the app at their PDS.
func (a *fakeAuth) refuse(sid string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.refused[sid] = true
}

// refusedAuth fails every request the way indigo does when the
// authorization server refuses a token refresh.
type refusedAuth struct{}

func (refusedAuth) DoWithAuth(*http.Client, *http.Request, syntax.NSID) (*http.Response, error) {
	return nil, errors.New("token refresh failed (HTTP 400): invalid_grant")
}

// grantFixture is an appview that indexes no spaces until one is granted.
func grantFixture(t *testing.T, open bool) *fixture {
	t.Helper()
	f := setup(t)
	f.srv.Spaces = nil
	f.srv.DB = controltest.New(t)
	f.srv.OpenRegistration = open
	f.grants = &Grants{DB: f.srv.DB, Auth: f.auth}
	f.srv.Grants = f.grants
	client, err := spaceclient.NewDelegated(f.grants, f.net.Dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	f.srv.Indexer.Client = client
	// A grant starts a sync in the background; let it finish before the
	// store closes (cleanups run last-registered first).
	t.Cleanup(f.srv.Jobs.Wait)
	return f
}

// browser keeps cookies and doesn't follow redirects.
func browser(t *testing.T) *http.Client {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	return &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// start begins a grant or stop in the browser. It returns the response and,
// if it went to the authorization server, the OAuth state.
func (f *fixture) start(t *testing.T, b *http.Client, mode, ret string) (*http.Response, string) {
	t.Helper()
	q := url.Values{"space": {f.net.Space}}
	if mode != "" {
		q.Set("mode", mode)
	}
	if ret != "" {
		q.Set("return", ret)
	}
	resp, err := b.Get(f.url + "/oauth/grant?" + q.Encode())
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	loc, _ := url.Parse(resp.Header.Get("Location"))
	if resp.StatusCode != http.StatusSeeOther || loc == nil || loc.Host != "pds.test" {
		return resp, ""
	}
	return resp, loc.Query().Get("state")
}

// callback returns from the authorization server.
func (f *fixture) callback(t *testing.T, b *http.Client, state string, extra url.Values) (*http.Response, string) {
	t.Helper()
	q := url.Values{"state": {state}, "iss": {"https://pds.test"}, "code": {"code"}}
	for k, v := range extra {
		q[k] = v
	}
	resp, err := b.Get(f.url + "/oauth/callback?" + q.Encode())
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp, string(raw)
}

// flow runs a grant or stop through to the end, signed in as the account
// it starts for.
func (f *fixture) flow(t *testing.T, mode, ret string) (*http.Response, string) {
	t.Helper()
	b := browser(t)
	resp, state := f.start(t, b, mode, ret)
	if state == "" {
		t.Fatalf("%s didn't go to the authorization server: %d %s", mode, resp.StatusCode, resp.Header.Get("Location"))
	}
	return f.callback(t, b, state, nil)
}

func returned(t *testing.T, resp *http.Response) url.Values {
	t.Helper()
	loc, err := url.Parse(resp.Header.Get("Location"))
	if resp.StatusCode != http.StatusSeeOther || err != nil || !strings.HasPrefix(loc.String(), webOrigin+"/") {
		t.Fatalf("didn't return to the web app: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	return loc.Query()
}

func (f *fixture) access(t *testing.T) map[string]any {
	t.Helper()
	status, body := f.get(t, f.alice, serviceDID, "garden.engram.getSpaceStatus", url.Values{"space": {f.net.Space}})
	if status != 200 {
		t.Fatalf("status: %d %v", status, body)
	}
	a, _ := body["access"].(map[string]any)
	return a
}

func TestGrantIndexesSpace(t *testing.T) {
	t.Parallel()
	f := grantFixture(t, true)
	f.net.Put(f.alice, indexer.Collection, "a1", memory("granted spaces get indexed"))
	list := url.Values{"space": {f.net.Space}}
	if status, body := f.get(t, f.bob, serviceDID, "garden.engram.listMemories", list); status != 400 || body["error"] != "UnknownSpace" {
		t.Fatalf("before granting: %d %v", status, body)
	}

	resp, _ := f.flow(t, modeGrant, webOrigin+"/space?uri=x")
	if q := returned(t, resp); q.Get("indexing") != "granted" || q.Get("uri") != "x" {
		t.Fatalf("returned with %v", q)
	}
	f.srv.Jobs.Wait()
	status, body := f.get(t, f.bob, serviceDID, "garden.engram.listMemories", list)
	if status != 200 || len(memories(body)) != 1 {
		t.Fatalf("after granting: %d %v", status, body)
	}
	if a := f.access(t); a["state"] != "granted" || a["grantedBy"] != f.net.Authority.DID || a["grantedAt"] == nil {
		t.Fatalf("access: %v", a)
	}

	// A restarted appview finds the registration and the grant in the
	// database.
	grants := &Grants{DB: f.srv.DB, Auth: f.auth}
	again := &Server{Store: f.srv.Store, Indexer: f.srv.Indexer, Dir: f.srv.Dir, ServiceDID: serviceDID, DB: f.srv.DB, Grants: grants}
	if err := again.LoadRegistrations(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !again.indexes(f.net.Space) {
		t.Fatal("registration didn't survive a restart")
	}
	if tok, err := grants.DelegationToken(context.Background(), f.net.Space); err != nil || tok == "" {
		t.Fatalf("grant didn't survive a restart: %v", err)
	}
}

// TestGrantOnlyFromAuthority: a member who isn't the authority can't have
// the space indexed, and their sign-in is revoked.
func TestGrantOnlyFromAuthority(t *testing.T) {
	t.Parallel()
	f := grantFixture(t, true)
	b := browser(t)
	_, state := f.start(t, b, modeGrant, "")
	f.auth.signInAs[state] = f.alice.DID
	resp, page := f.callback(t, b, state, nil)
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(page, f.net.Authority.DID) {
		t.Fatalf("grant by a member: %d %s", resp.StatusCode, page)
	}
	if f.srv.indexes(f.net.Space) {
		t.Fatal("indexed without the authority's grant")
	}
	if !f.auth.isRevoked("session-" + state) {
		t.Fatal("the member's sign-in wasn't revoked")
	}
}

// TestGrantCallbackFromAnotherBrowser: a callback only counts in the
// browser that started it.
func TestGrantCallbackFromAnotherBrowser(t *testing.T) {
	t.Parallel()
	f := grantFixture(t, true)
	_, state := f.start(t, browser(t), modeGrant, "")
	resp, _ := f.callback(t, browser(t), state, nil)
	if resp.StatusCode != http.StatusBadRequest || f.srv.indexes(f.net.Space) {
		t.Fatalf("callback in another browser: %d", resp.StatusCode)
	}
}

func TestGrantReturnOrigins(t *testing.T) {
	t.Parallel()
	f := grantFixture(t, true)
	for _, ret := range []string{"https://evil.test/", "javascript:alert(1)", "//evil.test/x", "https://web.test.evil.test/"} {
		if resp, state := f.start(t, browser(t), modeGrant, ret); resp.StatusCode != http.StatusBadRequest || state != "" {
			t.Fatalf("return to %q: %d %s", ret, resp.StatusCode, resp.Header.Get("Location"))
		}
	}
	// Without a return URL the appview shows its own page.
	resp, page := f.flow(t, modeGrant, "")
	if resp.StatusCode != http.StatusOK || !strings.Contains(page, "index") {
		t.Fatalf("no return URL: %d %s", resp.StatusCode, page)
	}
}

func TestGrantDeclined(t *testing.T) {
	t.Parallel()
	f := grantFixture(t, true)
	b := browser(t)
	_, state := f.start(t, b, modeGrant, webOrigin+"/")
	resp, _ := f.callback(t, b, state, url.Values{"error": {"access_denied"}})
	if q := returned(t, resp); !strings.Contains(q.Get("indexing_error"), "declined") {
		t.Fatalf("declined: %v", q)
	}
	if f.srv.indexes(f.net.Space) {
		t.Fatal("indexed after declining")
	}
}

// TestGrantNeedsReadScope: an authorization server that grants less than
// read access gets its session revoked, not a grant.
func TestGrantNeedsReadScope(t *testing.T) {
	t.Parallel()
	f := grantFixture(t, true)
	b := browser(t)
	_, state := f.start(t, b, modeGrant, "")
	f.auth.scopes[state] = []string{"atproto"}
	resp, _ := f.callback(t, b, state, nil)
	if resp.StatusCode != http.StatusForbidden || f.srv.indexes(f.net.Space) || !f.auth.isRevoked("session-"+state) {
		t.Fatalf("grant without the read scope: %d", resp.StatusCode)
	}
}

// TestGrantScopesAsIssued: authorization servers rewrite scopes when they
// issue a token (Cocoon resolves authority=self to the user's DID and
// writes the scope canonically), so the check reads what the scopes mean
// rather than comparing strings.
func TestGrantScopesAsIssued(t *testing.T) {
	t.Parallel()
	authority := spacetest.New(t).Authority.DID // same DID in every network
	for _, c := range []struct {
		scopes []string
		ok     bool
	}{
		{[]string{"atproto", "space:garden.engram.space?authority=" + authority + "&action=read"}, true},
		{[]string{"atproto", "space:garden.engram.space?action=read"}, true},
		{[]string{"atproto", "space:garden.engram.space?authority=*&action=read&action=delete&collection=garden.engram.memory"}, true},
		{[]string{"atproto", "space:garden.engram.space?authority=did:plc:someoneelse&action=read"}, false},
		{[]string{"atproto", "space:garden.engram.space?authority=" + authority + "&action=read_self"}, false},
		{[]string{"atproto", "space:other.app.space?authority=" + authority + "&action=read"}, false},
		{[]string{"atproto"}, false},
	} {
		f := grantFixture(t, true)
		b := browser(t)
		_, state := f.start(t, b, modeGrant, "")
		f.auth.scopes[state] = c.scopes
		resp, page := f.callback(t, b, state, nil)
		if got := resp.StatusCode == http.StatusOK; got != c.ok {
			t.Errorf("scopes %q: %d %s", c.scopes, resp.StatusCode, page)
		}
	}
}

func TestGrantClosedRegistration(t *testing.T) {
	t.Parallel()
	f := grantFixture(t, false)
	if resp, state := f.start(t, browser(t), modeGrant, ""); resp.StatusCode != http.StatusForbidden || state != "" {
		t.Fatalf("closed registration: %d", resp.StatusCode)
	}
	// A space the operator configured can still be granted.
	f.srv.Spaces = []string{f.net.Space}
	if resp, page := f.flow(t, modeGrant, ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("configured space: %d %s", resp.StatusCode, page)
	}
}

func TestStopIndexing(t *testing.T) {
	t.Parallel()
	f := grantFixture(t, true)
	resp, _ := f.flow(t, modeGrant, webOrigin+"/")
	returned(t, resp)
	f.srv.Jobs.Wait()
	g, err := f.grants.Get(context.Background(), f.net.Space)
	if err != nil || g == nil {
		t.Fatalf("grant: %v %v", g, err)
	}

	resp, _ = f.flow(t, modeStop, webOrigin+"/")
	if q := returned(t, resp); q.Get("indexing") != "stopped" {
		t.Fatalf("stop: %v", q)
	}
	if g2, _ := f.grants.Get(context.Background(), f.net.Space); g2 != nil {
		t.Fatal("grant still there")
	}
	if !f.auth.isRevoked(g.SessionID) {
		t.Fatal("the grant's session wasn't revoked")
	}
	if a := f.access(t); a["state"] != "missing" {
		t.Fatalf("access after stopping: %v", a)
	}
	// New memories aren't indexed.
	f.net.Put(f.alice, indexer.Collection, "a2", memory("after stopping"))
	f.srv.syncSpaceOnce(context.Background(), f.net.Space)
	_, body := f.get(t, f.bob, serviceDID, "garden.engram.listMemories", url.Values{"space": {f.net.Space}})
	if n := len(memories(body)); n != 0 {
		t.Fatalf("indexed %d memories after stopping", n)
	}
}

// TestStopOnlyFromAuthority: a member can't stop indexing.
func TestStopOnlyFromAuthority(t *testing.T) {
	t.Parallel()
	f := grantFixture(t, true)
	f.flow(t, modeGrant, "")
	b := browser(t)
	_, state := f.start(t, b, modeStop, "")
	f.auth.signInAs[state] = f.alice.DID
	if resp, _ := f.callback(t, b, state, nil); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("stop by a member: %d", resp.StatusCode)
	}
	if g, _ := f.grants.Get(context.Background(), f.net.Space); g == nil {
		t.Fatal("a member stopped indexing")
	}
}

// TestGrantLapsed: when the authorization server refuses the grant, the
// space's status says so, and granting again fixes it.
func TestGrantLapsed(t *testing.T) {
	t.Parallel()
	f := grantFixture(t, true)
	f.flow(t, modeGrant, "")
	f.srv.Jobs.Wait()
	g, _ := f.grants.Get(context.Background(), f.net.Space)
	f.auth.refuse(g.SessionID)
	f.srv.Indexer.Client.Invalidate(f.net.Space)
	f.srv.syncSpaceOnce(context.Background(), f.net.Space)
	if a := f.access(t); a["state"] != "lapsed" || a["error"] == nil {
		t.Fatalf("access after refusal: %v", a)
	}

	f.flow(t, modeGrant, "")
	if a := f.access(t); a["state"] != "granted" {
		t.Fatalf("access after granting again: %v", a)
	}
	if !f.auth.isRevoked(g.SessionID) {
		t.Fatal("the old session wasn't revoked")
	}
}

// TestFailedRegrantKeepsGrant: granting again with a sign-in that can't
// read the space leaves the working grant in place.
func TestFailedRegrantKeepsGrant(t *testing.T) {
	t.Parallel()
	f := grantFixture(t, true)
	f.flow(t, modeGrant, "")
	f.srv.Jobs.Wait()
	g, _ := f.grants.Get(context.Background(), f.net.Space)

	b := browser(t)
	_, state := f.start(t, b, modeGrant, "")
	f.auth.refuse("session-" + state)
	if resp, _ := f.callback(t, b, state, nil); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("a grant that can't read: %d", resp.StatusCode)
	}
	if g2, _ := f.grants.Get(context.Background(), f.net.Space); g2 == nil || g2.SessionID != g.SessionID {
		t.Fatalf("grant replaced: %+v", g2)
	}
	if f.auth.isRevoked(g.SessionID) || !f.auth.isRevoked("session-"+state) {
		t.Fatal("revoked the wrong session")
	}
	if a := f.access(t); a["state"] != "granted" {
		t.Fatalf("access: %v", a)
	}
}

func TestDescribeService(t *testing.T) {
	t.Parallel()
	f := grantFixture(t, true)
	status, body := f.get(t, nil, "", "garden.engram.describeService", nil)
	if status != 200 || body["did"] != serviceDID || body["registration"] != "open" || body["grantUrl"] != f.url+"/oauth/grant" || body["account"] != nil {
		t.Fatalf("describe: %d %v", status, body)
	}
}

// TestGrantSyncWaitsForASlot: a grant's first sync takes one of the node's
// sync slots, like notified syncs.
func TestGrantSyncWaitsForASlot(t *testing.T) {
	t.Parallel()
	f := grantFixture(t, true)
	f.srv.MaxSyncs = 1
	hold, err := f.srv.acquireNodeSlot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if resp, page := f.flow(t, modeGrant, ""); resp.StatusCode != 200 {
		t.Fatalf("grant: %d %s", resp.StatusCode, page)
	}
	time.Sleep(50 * time.Millisecond)
	if n := f.net.Calls("com.atproto.space.listRepos"); n != 0 {
		t.Fatalf("synced while every slot was taken (%d listRepos calls)", n)
	}
	hold()
	f.srv.Jobs.Wait()
	if f.net.Calls("com.atproto.space.listRepos") == 0 {
		t.Fatal("never synced")
	}
}

// TestOtherNodeLearnsRegistration: a node that hasn't seen a registration
// rereads the database when asked about a space it doesn't know.
func TestOtherNodeLearnsRegistration(t *testing.T) {
	t.Parallel()
	f := grantFixture(t, true)
	f.flow(t, modeGrant, "")
	f.srv.Jobs.Wait()
	other := &Server{Store: f.srv.Store, Indexer: f.srv.Indexer, Dir: f.srv.Dir, ServiceDID: serviceDID, DB: f.srv.DB}
	if !other.knows(context.Background(), f.net.Space) {
		t.Fatal("other node didn't find the registration")
	}
}

// TestSweepAbandonedSignIns: sign-ins nobody finished don't pile up in the
// database.
func TestSweepAbandonedSignIns(t *testing.T) {
	t.Parallel()
	f := grantFixture(t, true)
	ctx := context.Background()
	f.start(t, browser(t), modeGrant, "") // abandoned at the authorization server
	st := AuthStore{f.srv.DB}
	if err := st.SaveAuthRequestInfo(ctx, oauth.AuthRequestData{State: "abandoned"}); err != nil {
		t.Fatal(err)
	}
	// Two records are waiting: the grant's pending record, and the request.
	if n, err := f.grants.Sweep(ctx, time.Now()); err != nil || n != 0 {
		t.Fatalf("swept %d fresh sign-ins, %v", n, err)
	}
	if n, err := f.grants.Sweep(ctx, time.Now().Add(pendingTTL+time.Minute)); err != nil || n != 2 {
		t.Fatalf("swept %d abandoned sign-in records, %v; want the 2", n, err)
	}
	if _, err := f.srv.DB.GetRequest(ctx, "abandoned"); !errors.Is(err, control.ErrNotFound) {
		t.Fatalf("abandoned sign-in left: %v", err)
	}
	if n, err := f.grants.Sweep(ctx, time.Now().Add(pendingTTL+time.Minute)); err != nil || n != 0 {
		t.Fatalf("a second sweep cleared %d, %v", n, err)
	}
}

// TestRunClearsAbandonedSignIns: the background loop (on the coordinator,
// which a single node is) is what sweeps, so nothing else has to.
func TestRunClearsAbandonedSignIns(t *testing.T) {
	t.Parallel()
	f := grantFixture(t, true)
	ctx, cancel := context.WithCancel(context.Background())
	if err := f.srv.DB.PutRequest(ctx, control.Request{State: "abandoned", Data: []byte(`{}`), Created: time.Now().Add(-pendingTTL - time.Minute)}); err != nil {
		t.Fatal(err)
	}
	if err := f.srv.DB.PutRequest(ctx, control.Request{State: "recent", Data: []byte(`{}`), Created: time.Now()}); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { f.srv.Run(ctx, time.Hour, false); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := f.srv.DB.GetRequest(ctx, "abandoned"); errors.Is(err, control.ErrNotFound) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Run never cleared the abandoned sign-in")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	if _, err := f.srv.DB.GetRequest(context.Background(), "recent"); err != nil {
		t.Fatalf("Run cleared a recent sign-in: %v", err)
	}
}

// TestSweepUnusedSessions: an OAuth session no grant uses (say, one a
// refresh saved again after it was revoked) is revoked and deleted; the
// grant's own session stays.
func TestSweepUnusedSessions(t *testing.T) {
	t.Parallel()
	f := grantFixture(t, true)
	ctx := context.Background()
	st := AuthStore{f.srv.DB}
	authority := syntax.DID(f.net.Authority.DID)
	for _, sid := range []string{"live", "orphan"} {
		f.auth.sessions[sid] = authority.String()
		if err := st.SaveSession(ctx, oauth.ClientSessionData{AccountDID: authority, SessionID: sid}); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.grants.Put(ctx, Grant{Space: f.net.Space, DID: authority.String(), SessionID: "live", GrantedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	// Recent sessions may belong to a grant still being made.
	if n, err := f.grants.Sweep(ctx, time.Now()); err != nil || n != 0 {
		t.Fatalf("swept %d recent sessions, %v", n, err)
	}
	if _, err := st.GetSession(ctx, authority, "orphan"); err != nil || f.auth.isRevoked("orphan") {
		t.Fatalf("swept a recent session: %v", err)
	}
	if n, err := f.grants.Sweep(ctx, time.Now().Add(pendingTTL+time.Minute)); err != nil || n != 1 {
		t.Fatalf("swept %d unused sessions, %v; want the orphan", n, err)
	}
	if _, err := st.GetSession(ctx, authority, "orphan"); err == nil || !f.auth.isRevoked("orphan") {
		t.Fatalf("unused session left: %v revoked=%v", err, f.auth.isRevoked("orphan"))
	}
	if _, err := st.GetSession(ctx, authority, "live"); err != nil || f.auth.isRevoked("live") {
		t.Fatalf("swept the grant's session: %v", err)
	}
}

// TestGrantObjects: what a grant stores.
func TestGrantObjects(t *testing.T) {
	t.Parallel()
	f := grantFixture(t, true)
	f.flow(t, modeGrant, "")
	g, err := f.srv.DB.GetGrant(context.Background(), f.net.Space)
	if err != nil || g == nil || g.Space != f.net.Space || g.DID != f.net.Authority.DID || g.SessionID == "" || g.GrantedAt.IsZero() {
		t.Fatalf("grant: %+v %v", g, err)
	}
	regs, err := f.srv.DB.Registrations(context.Background())
	if err != nil || len(regs) != 1 || regs[0].Space != f.net.Space {
		t.Fatalf("registrations: %+v %v", regs, err)
	}
}
