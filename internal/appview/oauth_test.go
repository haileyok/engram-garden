package appview

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bluesky-social/indigo/atproto/atcrypto"
	"github.com/bluesky-social/indigo/atproto/auth/oauth"
	"github.com/bluesky-social/indigo/atproto/syntax"

	"github.com/haileyok/engram-garden/internal/blob"
)

func TestOAuthClientMetadata(t *testing.T) {
	t.Parallel()
	key, err := atcrypto.GeneratePrivateKeyP256()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewOAuthClient(OAuthConfig{PublicURL: "https://api.engram.test", Store: BlobAuthStore{blob.Dir{Root: t.TempDir()}}}); err == nil {
		t.Fatal("an https client without a key")
	}
	c, err := NewOAuthClient(OAuthConfig{PublicURL: "https://api.engram.test", Key: key, Store: BlobAuthStore{blob.Dir{Root: t.TempDir()}}})
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{ServiceDID: serviceDID, PublicURL: "https://api.engram.test", Grants: &Grants{Blob: blob.Dir{Root: t.TempDir()}, Auth: c}}
	hs := httptest.NewServer(s.Handler())
	t.Cleanup(hs.Close)

	resp, err := http.Get(hs.URL + "/oauth/client-metadata.json")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var meta map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&meta); err != nil {
		t.Fatal(err)
	}
	scope, _ := meta["scope"].(string)
	if meta["client_id"] != "https://api.engram.test/oauth/client-metadata.json" ||
		meta["token_endpoint_auth_method"] != "private_key_jwt" || meta["dpop_bound_access_tokens"] != true ||
		meta["jwks_uri"] != "https://api.engram.test/oauth/jwks.json" ||
		!strings.Contains(scope, "space:garden.engram.space?action=read") {
		t.Fatalf("metadata: %v", meta)
	}
	if uris, _ := meta["redirect_uris"].([]any); len(uris) != 1 || uris[0] != "https://api.engram.test/oauth/callback" {
		t.Fatalf("redirect URIs: %v", meta["redirect_uris"])
	}

	resp2, err := http.Get(hs.URL + "/oauth/jwks.json")
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	var jwks struct {
		Keys []map[string]any `json:"keys"`
	}
	if err := json.NewDecoder(resp2.Body).Decode(&jwks); err != nil || len(jwks.Keys) != 1 || jwks.Keys[0]["crv"] != "P-256" || jwks.Keys[0]["d"] != nil {
		t.Fatalf("jwks: %+v %v", jwks, err)
	}
}

func TestBlobAuthStore(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	bs := blob.Dir{Root: t.TempDir()}
	st := BlobAuthStore{bs}
	did := syntax.DID("did:plc:authority")

	sess := oauth.ClientSessionData{AccountDID: did, SessionID: "s1", RefreshToken: "r1"}
	if err := st.SaveSession(ctx, sess); err != nil {
		t.Fatal(err)
	}
	sess.RefreshToken = "r2" // a refresh overwrites it
	if err := st.SaveSession(ctx, sess); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetSession(ctx, did, "s1")
	if err != nil || got.RefreshToken != "r2" {
		t.Fatalf("session: %+v %v", got, err)
	}
	if _, err := st.GetSession(ctx, "did:plc:other", "s1"); err == nil {
		t.Fatal("found a session under another DID")
	}
	if err := st.DeleteSession(ctx, did, "s1"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetSession(ctx, did, "s1"); err == nil {
		t.Fatal("deleted session still there")
	}

	if err := st.SaveAuthRequestInfo(ctx, oauth.AuthRequestData{State: "st"}); err != nil {
		t.Fatal(err)
	}
	if info, err := st.GetAuthRequestInfo(ctx, "st"); err != nil || info.State != "st" {
		t.Fatalf("sign-in: %+v %v", info, err)
	}
	// An old sign-in has expired.
	raw, _ := json.Marshal(storedRequest{Info: oauth.AuthRequestData{State: "old"}, Created: time.Now().Add(-pendingTTL - time.Minute)})
	if err := blob.PutBytes(ctx, bs, requestKey("old"), raw, false); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetAuthRequestInfo(ctx, "old"); err == nil {
		t.Fatal("an expired sign-in was accepted")
	}
}
