package agent

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bluesky-social/indigo/atproto/atcrypto"
	"github.com/bluesky-social/indigo/atproto/auth/oauth"
	"github.com/bluesky-social/indigo/atproto/identity"
	"github.com/bluesky-social/indigo/atproto/syntax"
)

// keyServer is a PDS and its authorization server that sign accounts in
// with the JWT bearer grant, checking what Cocoon checks.
type keyServer struct {
	t      *testing.T
	srv    *httptest.Server
	did    string
	key    atcrypto.PublicKey // the account's sign-in key
	revoke bool               // refuse every assertion, as for a revoked key

	mu       sync.Mutex
	grants   int // jwt-bearer grants issued
	refreshs int
	jtis     map[string]bool
	tokens   map[string]bool // live access tokens
	scopes   []string        // the last grant's scopes
	clientID string
}

func newKeyServer(t *testing.T, did string, key atcrypto.PublicKey) *keyServer {
	t.Helper()
	ks := &keyServer{t: t, did: did, key: key, jtis: map[string]bool{}, tokens: map[string]bool{}}
	ks.srv = httptest.NewServer(http.HandlerFunc(ks.serve))
	t.Cleanup(ks.srv.Close)
	return ks
}

func (ks *keyServer) dir() identity.Directory {
	d := identity.NewMockDirectory()
	d.Insert(identity.Identity{
		DID: syntax.DID(ks.did), Handle: syntax.HandleInvalid,
		Services: map[string]identity.ServiceEndpoint{"atproto_pds": {Type: "AtprotoPersonalDataServer", URL: ks.srv.URL}},
	})
	return d
}

const testNonce = "nonce-1"

func jwtPart(t *testing.T, s string, i int) map[string]any {
	t.Helper()
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		t.Fatalf("not a JWT: %q", s)
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[i])
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func (ks *keyServer) serve(w http.ResponseWriter, r *http.Request) {
	t := ks.t
	fail := func(code int, name string) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": name})
	}
	switch r.URL.Path {
	case "/.well-known/oauth-protected-resource":
		_ = json.NewEncoder(w).Encode(map[string]any{"resource": ks.srv.URL, "authorization_servers": []string{ks.srv.URL}})
	case "/.well-known/oauth-authorization-server":
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer": ks.srv.URL, "token_endpoint": ks.srv.URL + "/oauth/token", "revocation_endpoint": ks.srv.URL + "/oauth/revoke",
		})
	case "/oauth/token":
		dpop := r.Header.Get("DPoP")
		if dpop == "" {
			fail(400, "invalid_dpop_proof")
			return
		}
		if h := jwtPart(t, dpop, 0); h["typ"] != "dpop+jwt" {
			t.Errorf("DPoP typ %v", h["typ"])
		}
		claims := jwtPart(t, dpop, 1)
		if claims["htu"] != ks.srv.URL+"/oauth/token" || claims["htm"] != "POST" {
			t.Errorf("DPoP claims %v", claims)
		}
		if claims["nonce"] != testNonce {
			w.Header().Set("DPoP-Nonce", testNonce)
			fail(400, "use_dpop_nonce")
			return
		}
		_ = r.ParseForm()
		ks.mu.Lock()
		ks.clientID = r.Form.Get("client_id")
		ks.mu.Unlock()
		switch r.Form.Get("grant_type") {
		case "urn:ietf:params:oauth:grant-type:jwt-bearer":
			if !ks.checkAssertion(r.Form.Get("assertion")) {
				fail(400, "invalid_grant")
				return
			}
			ks.mu.Lock()
			ks.grants++
			ks.scopes = strings.Fields(r.Form.Get("scope"))
			ks.mu.Unlock()
		case "refresh_token":
			ks.mu.Lock()
			ks.refreshs++
			ks.mu.Unlock()
		default:
			fail(400, "unsupported_grant_type")
			return
		}
		tok := "access-" + syntax.NewTIDNow(0).String()
		ks.mu.Lock()
		ks.tokens[tok] = true
		ks.mu.Unlock()
		w.Header().Set("DPoP-Nonce", testNonce)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": tok, "token_type": "DPoP", "refresh_token": "refresh-" + tok,
			"scope": r.Form.Get("scope"), "expires_in": 3600, "sub": ks.did,
		})
	case "/oauth/revoke":
		w.WriteHeader(200)
	case "/xrpc/com.atproto.server.getSession":
		tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "DPoP ")
		ks.mu.Lock()
		live := ks.tokens[tok]
		ks.mu.Unlock()
		if !ok || !live || r.Header.Get("DPoP") == "" {
			w.Header().Set("WWW-Authenticate", `DPoP error="invalid_token", error_description="expired"`)
			fail(401, "InvalidToken")
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"did": ks.did, "handle": "child.test"})
	default:
		fail(404, "NotFound")
	}
}

func (ks *keyServer) checkAssertion(a string) bool {
	t := ks.t
	if ks.revoke {
		return false
	}
	parts := strings.Split(a, ".")
	if len(parts) != 3 {
		return false
	}
	h, c := jwtPart(t, a, 0), jwtPart(t, a, 1)
	if h["alg"] != "ES256K" {
		t.Errorf("assertion alg %v", h["alg"])
		return false
	}
	iat, _ := c["iat"].(float64)
	exp, _ := c["exp"].(float64)
	jti, _ := c["jti"].(string)
	now := float64(time.Now().Unix())
	if c["iss"] != ks.did || c["sub"] != ks.did || c["aud"] != ks.srv.URL || jti == "" ||
		iat > now+60 || exp <= now || exp > iat+300 {
		t.Errorf("assertion claims %v", c)
		return false
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || ks.key.HashAndVerify([]byte(parts[0]+"."+parts[1]), sig) != nil {
		return false
	}
	ks.mu.Lock()
	defer ks.mu.Unlock()
	if ks.jtis[jti] {
		t.Error("assertion jti reused")
		return false
	}
	ks.jtis[jti] = true
	return true
}

func keyAccount(t *testing.T) (Account, *keyServer) {
	t.Helper()
	key, err := NewSignInKey()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "keys", "child.key")
	if err := SaveSignInKey(path, key); err != nil {
		t.Fatal(err)
	}
	pub, _ := key.PublicKey()
	ks := newKeyServer(t, "did:plc:child", pub)
	return Account{Handle: "child.test", DID: "did:plc:child", SignIn: SignInKey, KeyFile: path}, ks
}

func TestKeySignIn(t *testing.T) {
	t.Parallel()
	acct, ks := keyAccount(t)
	o := Options{Dir: ks.dir(), Store: oauth.NewMemStore()}
	s := Settings{Account: acct, AppviewURL: "https://api.engram.test"}
	if err := s.CheckAccount(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	api, err := s.Session(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		DID string `json:"did"`
	}
	if err := api.Get(ctx, "com.atproto.server.getSession", nil, &out); err != nil || out.DID != "did:plc:child" {
		t.Fatalf("authenticated request: %v %+v", err, out)
	}
	if ks.grants != 1 || strings.Join(ks.scopes, " ") != strings.Join(Scopes, " ") {
		t.Fatalf("grants %d scopes %v", ks.grants, ks.scopes)
	}
	if !strings.HasPrefix(ks.clientID, "http://localhost?") {
		t.Fatalf("client_id %q isn't a loopback client", ks.clientID)
	}
	// The next run resumes the saved session instead of signing in again.
	api, err = s.Session(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	if ks.grants != 1 {
		t.Fatalf("signed in again: %d grants", ks.grants)
	}
	// An expired access token is refreshed, with the client it was issued to.
	granted := ks.clientID
	ks.mu.Lock()
	clear(ks.tokens)
	ks.mu.Unlock()
	if err := api.Get(ctx, "com.atproto.server.getSession", nil, &out); err != nil {
		t.Fatalf("request after the token expired: %v", err)
	}
	if ks.refreshs != 1 || ks.grants != 1 || ks.clientID != granted {
		t.Fatalf("refreshes %d, grants %d, client %q (signed in as %q)", ks.refreshs, ks.grants, ks.clientID, granted)
	}
	// The key file is the owner's alone.
	if err := checkKeyFile(acct.KeyFile); err != nil {
		t.Fatal(err)
	}
}

func TestKeySignInFor(t *testing.T) {
	t.Parallel()
	acct, ks := keyAccount(t)
	o := Options{Dir: ks.dir(), Store: oauth.NewMemStore()}
	ctx := context.Background()
	want := []string{"atproto", "repo:garden.engram.agents.declaration?action=create&action=update"}
	api, granted, end, err := KeySignInFor(ctx, acct, want, o)
	if err != nil {
		t.Fatal(err)
	}
	defer end()
	if strings.Join(granted, " ") != strings.Join(want, " ") || api.AccountDID.String() != "did:plc:child" {
		t.Fatalf("granted %v as %v", granted, api.AccountDID)
	}
	// A one-time sign-in is a separate session from the agent's own.
	if _, _, end2, err := KeySignInFor(ctx, acct, want, o); err != nil {
		t.Fatal(err)
	} else {
		end2()
	}
	if ks.grants != 2 {
		t.Fatalf("grants %d", ks.grants)
	}
}

func TestKeySignInRefused(t *testing.T) {
	t.Parallel()
	acct, ks := keyAccount(t)
	ks.revoke = true
	o := Options{Dir: ks.dir(), Store: oauth.NewMemStore()}
	_, err := Settings{Account: acct, AppviewURL: "https://api.engram.test"}.Session(context.Background(), o)
	if err == nil || !strings.Contains(err.Error(), "invalid_grant") {
		t.Fatalf("revoked key signed in: %v", err)
	}
	// A key file that isn't there.
	acct.KeyFile = filepath.Join(t.TempDir(), "missing")
	if _, err := (Settings{Account: acct, AppviewURL: "https://api.engram.test"}).Session(context.Background(), o); err == nil {
		t.Fatal("signed in without a key file")
	}
}

func TestKeySignInNeedsHTTPS(t *testing.T) {
	t.Parallel()
	if err := checkServerURL("http://pds.example.com"); err == nil {
		t.Fatal("plain http to a remote server allowed")
	}
	for _, ok := range []string{"https://pds.example.com", "http://127.0.0.1:8080", "http://localhost:2583"} {
		if err := checkServerURL(ok); err != nil {
			t.Fatalf("%s: %v", ok, err)
		}
	}
}
