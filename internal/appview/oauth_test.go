package appview

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/bluesky-social/indigo/atproto/atcrypto"
	"github.com/bluesky-social/indigo/atproto/auth/oauth"
	"github.com/bluesky-social/indigo/atproto/syntax"

	"github.com/haileyok/engram-garden/internal/control"
	"github.com/haileyok/engram-garden/internal/control/controltest"
)

func TestOAuthClientMetadata(t *testing.T) {
	t.Parallel()
	key, err := atcrypto.GeneratePrivateKeyP256()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewOAuthClient(OAuthConfig{PublicURL: "https://api.engram.test", Store: AuthStore{control.NewMemory()}}); err == nil {
		t.Fatal("an https client without a key")
	}
	c, err := NewOAuthClient(OAuthConfig{PublicURL: "https://api.engram.test", Key: key, Store: AuthStore{control.NewMemory()}})
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{ServiceDID: serviceDID, PublicURL: "https://api.engram.test", Grants: &Grants{DB: control.NewMemory(), Auth: c}}
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

func TestAuthStore(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := controltest.New(t)
	st := AuthStore{db}
	did := syntax.DID("did:plc:authority")

	// Every field survives the database, whatever it does to the JSON.
	sess := oauth.ClientSessionData{
		AccountDID: did, SessionID: "s1", HostURL: "https://pds.test", AuthServerURL: "https://auth.test",
		AuthServerTokenEndpoint: "https://auth.test/token", AuthServerRevocationEndpoint: "https://auth.test/revoke",
		Scopes: []string{"atproto", "space:garden.engram.space?action=read"}, AccessToken: "a1", RefreshToken: "r1",
		DPoPAuthServerNonce: "n1", DPoPHostNonce: "n2", DPoPPrivateKeyMultibase: "z42tqExampleKeyMaterial",
	}
	if err := st.SaveSession(ctx, sess); err != nil {
		t.Fatal(err)
	}
	if got, err := st.GetSession(ctx, did, "s1"); err != nil || !reflect.DeepEqual(*got, sess) {
		t.Fatalf("session changed in storage:\n got %+v\nwant %+v\n%v", got, sess, err)
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

	authority := did
	req := oauth.AuthRequestData{
		State: "st", AuthServerURL: "https://auth.test", AccountDID: &authority, Scopes: []string{"atproto"},
		RequestURI: "urn:ietf:params:oauth:request_uri:abc", AuthServerTokenEndpoint: "https://auth.test/token",
		AuthServerRevocationEndpoint: "https://auth.test/revoke", PKCEVerifier: "v", DPoPAuthServerNonce: "n",
		DPoPPrivateKeyMultibase: "z42tqExampleKeyMaterial",
	}
	if err := st.SaveAuthRequestInfo(ctx, req); err != nil {
		t.Fatal(err)
	}
	if info, err := st.GetAuthRequestInfo(ctx, "st"); err != nil || !reflect.DeepEqual(*info, req) {
		t.Fatalf("sign-in changed in storage:\n got %+v\nwant %+v\n%v", info, req, err)
	}
	// An old sign-in has expired.
	raw, _ := json.Marshal(oauth.AuthRequestData{State: "old"})
	if err := db.PutRequest(ctx, control.Request{State: "old", Data: raw, Created: time.Now().Add(-pendingTTL - time.Minute)}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetAuthRequestInfo(ctx, "old"); err == nil {
		t.Fatal("an expired sign-in was accepted")
	}
	if _, err := db.GetRequest(ctx, "old"); !errors.Is(err, control.ErrNotFound) {
		t.Fatalf("an expired sign-in is still stored: %v", err)
	}
	// A session that's gone says so in a way callers can recognize.
	if _, err := st.GetSession(ctx, did, "missing"); !errors.Is(err, control.ErrNotFound) {
		t.Fatalf("a missing session: %v", err)
	}
}
