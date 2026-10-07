package web

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/bluesky-social/indigo/atproto/atcrypto"
	"github.com/bluesky-social/indigo/atproto/auth/oauth"
)

func TestOAuthClientMetadata(t *testing.T) {
	t.Parallel()
	if _, err := NewOAuth(OAuthConfig{PublicURL: "https://engram.test", Store: &FileStore{Dir: t.TempDir()}}); err == nil {
		t.Fatal("a public client without a key")
	}
	if _, err := NewOAuth(OAuthConfig{PublicURL: "http://engram.test", Store: &FileStore{Dir: t.TempDir()}}); err == nil {
		t.Fatal("plain http outside loopback")
	}
	key, err := atcrypto.GeneratePrivateKeyP256()
	if err != nil {
		t.Fatal(err)
	}
	o, err := NewOAuth(OAuthConfig{PublicURL: "https://engram.test", Key: key, Store: &FileStore{Dir: t.TempDir()}})
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{OAuth: o, Auth: o, Origin: o.PublicURL}
	hs := httptest.NewServer(s.Handler())
	defer hs.Close()

	resp, err := http.Get(hs.URL + "/oauth/client-metadata.json")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var meta oauth.ClientMetadata
	if err := json.NewDecoder(resp.Body).Decode(&meta); err != nil {
		t.Fatal(err)
	}
	if err := meta.Validate("https://engram.test/oauth/client-metadata.json"); err != nil {
		t.Fatalf("metadata invalid: %v", err)
	}
	if meta.JWKSURI == nil || *meta.JWKSURI != "https://engram.test/oauth/jwks.json" || meta.TokenEndpointAuthMethod != "private_key_jwt" {
		t.Fatalf("not a confidential client: %+v", meta)
	}
	for _, sc := range Scopes {
		if !strings.Contains(" "+meta.Scope+" ", " "+sc+" ") {
			t.Fatalf("scope %q missing from %q", sc, meta.Scope)
		}
	}

	resp, err = http.Get(hs.URL + "/oauth/jwks.json")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var jwks oauth.JWKS
	if err := json.NewDecoder(resp.Body).Decode(&jwks); err != nil || len(jwks.Keys) != 1 {
		t.Fatalf("jwks: %+v %v", jwks, err)
	}

	// A loopback development client needs no key.
	dev, err := NewOAuth(OAuthConfig{PublicURL: "http://127.0.0.1:8090", Store: &FileStore{Dir: t.TempDir()}})
	if err != nil || dev.App.Config.IsConfidential() || !strings.HasPrefix(dev.App.Config.ClientID, "http://localhost?") {
		t.Fatalf("dev client: %v", err)
	}
}

// TestCallbackNeedsTheBrowserThatStarted: a callback link from someone
// else's sign-in doesn't sign this browser in.
func TestCallbackNeedsTheBrowserThatStarted(t *testing.T) {
	t.Parallel()
	store := &FileStore{Dir: t.TempDir()}
	o, err := NewOAuth(OAuthConfig{PublicURL: "http://127.0.0.1:8090", Store: store})
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{OAuth: o, Auth: o, Origin: o.PublicURL, CookieKey: []byte("0123456789abcdef0123456789abcdef")}
	hs := httptest.NewServer(s.Handler())
	defer hs.Close()
	noFollow := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	callback := func(cookie string) (string, []*http.Cookie) {
		req, _ := http.NewRequest("GET", hs.URL+"/oauth/callback?state=st1&code=c&iss=https://pds.test", nil)
		if cookie != "" {
			req.AddCookie(&http.Cookie{Name: signinCookie, Value: cookie})
		}
		resp, err := noFollow.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.Header.Get("Location"), resp.Cookies()
	}
	signedIn := func(cs []*http.Cookie) bool {
		for _, c := range cs {
			if c.Name == cookieName && c.Value != "" {
				return true
			}
		}
		return false
	}
	for _, cookie := range []string{"", s.mac("signin.other-state")} {
		loc, cs := callback(cookie)
		if !strings.Contains(loc, "started+from+this+browser") || signedIn(cs) {
			t.Fatalf("cookie %q: %s %v", cookie, loc, cs)
		}
	}
	// With the right cookie the callback gets as far as exchanging the code
	// (which fails here: there's no such sign-in).
	loc, cs := callback(s.mac("signin.st1"))
	if strings.Contains(loc, "started+from+this+browser") || !strings.Contains(loc, "signin_error") || signedIn(cs) {
		t.Fatalf("bound callback: %s", loc)
	}

	// Starting a sign-in records its state for the cookie.
	var state string
	ctx := context.WithValue(context.Background(), stateKey{}, &state)
	if err := (stateCapture{store}).SaveAuthRequestInfo(ctx, oauth.AuthRequestData{State: "st2"}); err != nil || state != "st2" {
		t.Fatalf("captured %q: %v", state, err)
	}
}

func TestMissingScopes(t *testing.T) {
	t.Parallel()
	if m := missingScopes(Scopes); len(m) != 0 {
		t.Fatalf("all granted: %v", m)
	}
	if m := missingScopes(Scopes[:1]); len(m) != len(Scopes)-1 {
		t.Fatalf("partial: %v", m)
	}
}

func TestServesFrontend(t *testing.T) {
	t.Parallel()
	s := &Server{Static: fstest.MapFS{
		"index.html":       {Data: []byte("<html>app</html>")},
		"assets/app-1.js":  {Data: []byte("console.log(1)")},
		"assets/style.css": {Data: []byte("body{}")},
	}}
	hs := httptest.NewServer(s.Handler())
	defer hs.Close()
	get := func(path string) (int, string, http.Header) {
		resp, err := http.Get(hs.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(raw), resp.Header
	}
	for _, p := range []string{"/", "/spaces/x", "/space?uri=y"} {
		if status, body, _ := get(p); status != 200 || body != "<html>app</html>" {
			t.Fatalf("%s: %d %q", p, status, body)
		}
	}
	if status, body, h := get("/assets/app-1.js"); status != 200 || body != "console.log(1)" || !strings.Contains(h.Get("Cache-Control"), "immutable") {
		t.Fatalf("asset: %d %q %v", status, body, h)
	}
	if status, _, _ := get("/assets/missing.js"); status != 404 {
		t.Fatalf("missing asset: %d", status)
	}
	if status, body, _ := get("/api/nothing"); status != 404 || !strings.Contains(body, "NotFound") {
		t.Fatalf("unknown api: %d %q", status, body)
	}
	if _, _, h := get("/"); !strings.Contains(h.Get("Content-Security-Policy"), "frame-ancestors 'none'") {
		t.Fatal("no CSP")
	}
}
