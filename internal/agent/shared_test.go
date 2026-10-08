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
