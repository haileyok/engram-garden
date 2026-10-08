package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bluesky-social/indigo/atproto/atcrypto"
	"github.com/bluesky-social/indigo/atproto/auth/oauth"
	"github.com/bluesky-social/indigo/atproto/syntax"

	"github.com/haileyok/engram-garden/internal/oauthfile"
)

// TestLockFileExcludes: a second holder waits for the first, as a second
// process would.
func TestLockFileExcludes(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "s.lock")
	unlock, err := lockFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan struct{})
	go func() {
		u, err := lockFile(path)
		if err == nil {
			u()
		}
		close(got)
	}()
	select {
	case <-got:
		t.Fatal("took a held lock")
	case <-time.After(100 * time.Millisecond):
	}
	unlock()
	select {
	case <-got:
	case <-time.After(5 * time.Second):
		t.Fatal("never got the lock after it was released")
	}
}

// oauthSession saves an OAuth session at host and returns the settings that
// use it.
func oauthSession(t *testing.T, store *oauthfile.FileStore, host, token string) (Settings, oauth.ClientSessionData) {
	t.Helper()
	key, err := atcrypto.GeneratePrivateKeyP256()
	if err != nil {
		t.Fatal(err)
	}
	did := syntax.DID("did:plc:agent")
	sess := oauth.ClientSessionData{AccountDID: did, SessionID: "s", HostURL: host, AccessToken: token, DPoPPrivateKeyMultibase: key.Multibase()}
	if err := store.SaveSession(context.Background(), sess); err != nil {
		t.Fatal(err)
	}
	return Settings{Account: Account{DID: did.String(), SignIn: SignInOAuth, SessionID: "s", Callback: "http://127.0.0.1:1/callback"}}, sess
}

// TestSharedSessionHoldsLockDuringRequest: the lock covers the whole
// request, since a refresh happens in the middle of one and spends the
// refresh token another process would otherwise use.
func TestSharedSessionHoldsLockDuringRequest(t *testing.T) {
	t.Parallel()
	entered, release := make(chan struct{}), make(chan struct{})
	pds := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"token":"t"}`))
	}))
	t.Cleanup(pds.Close)
	dir := t.TempDir()
	store := &oauthfile.FileStore{Dir: dir}
	s, sess := oauthSession(t, store, pds.URL, "first")
	api, err := s.Session(context.Background(), Options{Store: store, LockDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		var out map[string]any
		done <- api.Get(context.Background(), "com.atproto.space.getDelegationToken", map[string]any{"space": "x"}, &out)
	}()
	<-entered

	locked := make(chan func())
	go func() {
		unlock, err := lockFile(lockPath(dir, sess.AccountDID, sess.SessionID))
		if err != nil {
			t.Error(err)
			unlock = func() {}
		}
		locked <- unlock
	}()
	select {
	case unlock := <-locked:
		unlock()
		t.Fatal("the session's lock was free during a request")
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	select {
	case unlock := <-locked:
		unlock()
	case <-time.After(5 * time.Second):
		t.Fatal("the lock wasn't released after the request")
	}
}

// TestRevokeWaitsForLock: ending a sign-in waits for another process's
// request on it, which could otherwise save refreshed tokens after the
// session is deleted, bringing it back.
func TestRevokeWaitsForLock(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store := &oauthfile.FileStore{Dir: dir}
	s, sess := oauthSession(t, store, "http://127.0.0.1:1", "first")
	unlock, err := lockFile(lockPath(dir, sess.AccountDID, sess.SessionID))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- Revoke(context.Background(), s.Account, Options{Store: store, LockDir: dir}) }()
	select {
	case err := <-done:
		unlock()
		t.Fatalf("revoked while another request held the session (err %v)", err)
	case <-time.After(100 * time.Millisecond):
	}
	if _, err := store.GetSession(context.Background(), sess.AccountDID, sess.SessionID); err != nil {
		t.Fatalf("session deleted while locked: %v", err)
	}
	unlock()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("revoke never finished")
	}
	if _, err := store.GetSession(context.Background(), sess.AccountDID, sess.SessionID); err == nil {
		t.Fatal("session kept after revoking")
	}
}

// TestPasswordSignInUsesClient: password sign-ins use the configured HTTP
// client, so a stalled PDS times out instead of hanging.
func TestPasswordSignInUsesClient(t *testing.T) {
	t.Parallel()
	stall := make(chan struct{})
	pds := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/xrpc/com.atproto.server.createSession":
			_, _ = w.Write([]byte(`{"did":"did:plc:agent","handle":"agent.test","accessJwt":"a","refreshJwt":"r"}`))
		default:
			<-stall
		}
	}))
	t.Cleanup(pds.Close)
	t.Cleanup(func() { close(stall) })

	client := &http.Client{Timeout: 200 * time.Millisecond}
	s := Settings{Account: Account{Handle: "agent.test", SignIn: SignInPassword, Password: "p", PDSHost: pds.URL}}
	api, err := s.Session(context.Background(), Options{HTTP: client})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		var out map[string]any
		done <- api.Get(context.Background(), "com.atproto.space.getDelegationToken", map[string]any{"space": "x"}, &out)
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a stalled request succeeded")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a stalled request never timed out")
	}
}

// TestPasswordSignInTimesOut: signing in itself is bounded by the client's
// timeout too.
func TestPasswordSignInTimesOut(t *testing.T) {
	t.Parallel()
	stall := make(chan struct{})
	pds := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-stall }))
	t.Cleanup(pds.Close)
	t.Cleanup(func() { close(stall) })
	s := Settings{Account: Account{Handle: "agent.test", SignIn: SignInPassword, Password: "p", PDSHost: pds.URL}}
	done := make(chan error, 1)
	go func() {
		_, err := s.Session(context.Background(), Options{HTTP: &http.Client{Timeout: 200 * time.Millisecond}})
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("signed in at a stalled PDS")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("signing in at a stalled PDS never timed out")
	}
}

// TestSharedSessionReadsNewTokens: a request uses the tokens another process
// saved since this one opened the session, so two processes sharing a
// sign-in don't spend the same refresh token.
func TestSharedSessionReadsNewTokens(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var seen []string
	pds := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Get("Authorization"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"token":"t"}`))
	}))
	t.Cleanup(pds.Close)

	key, err := atcrypto.GeneratePrivateKeyP256()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	store := &oauthfile.FileStore{Dir: dir}
	did := syntax.DID("did:plc:agent")
	sess := oauth.ClientSessionData{AccountDID: did, SessionID: "s", HostURL: pds.URL, AccessToken: "first", DPoPPrivateKeyMultibase: key.Multibase()}
	if err := store.SaveSession(context.Background(), sess); err != nil {
		t.Fatal(err)
	}
	s := Settings{Account: Account{DID: did.String(), SignIn: SignInOAuth, SessionID: "s", Callback: "http://127.0.0.1:1/callback"}}
	api, err := s.Session(context.Background(), Options{Store: store, LockDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	call := func() {
		var out map[string]any
		if err := api.Get(context.Background(), "com.atproto.space.getDelegationToken", map[string]any{"space": "x"}, &out); err != nil {
			t.Fatal(err)
		}
	}
	call()
	// Another process refreshes and saves new tokens.
	sess.AccessToken = "second"
	if err := store.SaveSession(context.Background(), sess); err != nil {
		t.Fatal(err)
	}
	call()
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 || !strings.HasSuffix(seen[0], " first") || !strings.HasSuffix(seen[1], " second") {
		t.Fatalf("authorization headers: %q", seen)
	}
}
