package appview

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/bluesky-social/indigo/atproto/atclient"
	"github.com/bluesky-social/indigo/atproto/atcrypto"
	"github.com/bluesky-social/indigo/atproto/auth/oauth"
	"github.com/bluesky-social/indigo/atproto/identity"
	"github.com/bluesky-social/indigo/atproto/syntax"

	"github.com/haileyok/engram-garden/internal/control"
)

// OAuthClient is the appview's OAuth client: the Authorizer for real
// authorization servers, built on indigo's client. It's confidential (a
// private_key_jwt client with DPoP), or a loopback client for development.
type OAuthClient struct {
	grant *oauth.ClientApp // asks for GrantScopes
	stop  *oauth.ClientApp // asks for StopScopes; shares the store
	base  string
}

// OAuthConfig configures the appview's OAuth client.
type OAuthConfig struct {
	// PublicURL is where the appview is served. An http://127.0.0.1 URL
	// makes a development client, which needs no key.
	PublicURL string
	// Key signs the client's assertions (P-256). Required unless developing.
	Key   atcrypto.PrivateKey
	Store oauth.ClientAuthStore
	Dir   identity.Directory
	HTTP  *http.Client
}

// NewOAuthClient sets up the appview's OAuth client.
func NewOAuthClient(cfg OAuthConfig) (*OAuthClient, error) {
	u, err := url.Parse(cfg.PublicURL)
	if err != nil || u.Host == "" || u.Path != "" && u.Path != "/" {
		return nil, fmt.Errorf("the public URL must be a bare origin like https://api.engram.garden, not %q", cfg.PublicURL)
	}
	base := u.Scheme + "://" + u.Host
	callback := base + "/oauth/callback"
	o := &OAuthClient{base: base}
	store := stateCapture{cfg.Store}
	for _, c := range []struct {
		app    **oauth.ClientApp
		scopes []string
	}{{&o.grant, GrantScopes}, {&o.stop, StopScopes}} {
		var config oauth.ClientConfig
		switch {
		case u.Scheme == "http" && u.Hostname() == "127.0.0.1":
			config = oauth.NewLocalhostConfig(callback, c.scopes)
		case u.Scheme == "https":
			// Both flows are one client: the metadata lists every scope.
			config = oauth.NewPublicConfig(base+"/oauth/client-metadata.json", callback, c.scopes)
			if cfg.Key == nil {
				return nil, errors.New("an OAuth client key is required (generate one with: goat key generate -t P-256)")
			}
			if err := config.SetClientSecret(cfg.Key, "engram-appview-1"); err != nil {
				return nil, err
			}
		default:
			return nil, fmt.Errorf("the public URL must be https, or http://127.0.0.1:<port> for development, not %q", cfg.PublicURL)
		}
		config.UserAgent = "engram-garden-appview"
		app := oauth.NewClientApp(&config, store)
		if cfg.Dir != nil {
			app.Dir = cfg.Dir
		}
		if cfg.HTTP != nil {
			app.Client = cfg.HTTP
		}
		*c.app = app
	}
	return o, nil
}

type stateKey struct{}

// stateCapture records the state of a sign-in as it's saved, for the
// request that started it.
type stateCapture struct{ oauth.ClientAuthStore }

func (s stateCapture) SaveAuthRequestInfo(ctx context.Context, info oauth.AuthRequestData) error {
	if p, ok := ctx.Value(stateKey{}).(*string); ok {
		*p = info.State
	}
	return s.ClientAuthStore.SaveAuthRequestInfo(ctx, info)
}

// app is the client for a flow. They differ in the scopes they ask for, and
// for a development client in its ID too, so each flow's callback and
// revocation must go through the client that started it.
func (o *OAuthClient) app(mode string) *oauth.ClientApp {
	if mode == modeStop {
		return o.stop
	}
	return o.grant
}

// Start implements Authorizer. Starting from the DID makes indigo check the
// token's subject is that account.
func (o *OAuthClient) Start(ctx context.Context, did syntax.DID, mode string) (string, string, error) {
	var state string
	redirect, err := o.app(mode).StartAuthFlow(context.WithValue(ctx, stateKey{}, &state), did.String())
	if err != nil {
		return "", "", err
	}
	if state == "" {
		return "", "", errors.New("the sign-in wasn't recorded")
	}
	return redirect, state, nil
}

// Finish implements Authorizer.
func (o *OAuthClient) Finish(ctx context.Context, mode string, q url.Values) (*AuthResult, error) {
	sess, err := o.app(mode).ProcessCallback(ctx, q)
	if err != nil {
		var ce *oauth.AuthRequestCallbackError
		if errors.As(err, &ce) && ce.ErrorCode == "access_denied" {
			return nil, ErrDeclined
		}
		return nil, err
	}
	return &AuthResult{DID: sess.AccountDID, SessionID: sess.SessionID, Scopes: sess.Scopes}, nil
}

// Resume implements Authorizer.
func (o *OAuthClient) Resume(ctx context.Context, did syntax.DID, sessionID string) (*atclient.APIClient, error) {
	sess, err := o.grant.ResumeSession(ctx, did, sessionID)
	if err != nil {
		return nil, err
	}
	return sess.APIClient(), nil
}

// Revoke implements Authorizer.
func (o *OAuthClient) Revoke(ctx context.Context, mode string, did syntax.DID, sessionID string) error {
	return o.app(mode).Logout(ctx, did, sessionID)
}

// ServeMetadata serves the client metadata document.
func (o *OAuthClient) ServeMetadata(w http.ResponseWriter, r *http.Request) {
	meta := o.grant.Config.ClientMetadata()
	if o.grant.Config.IsConfidential() {
		jwks := o.base + "/oauth/jwks.json"
		meta.JWKSURI = &jwks
	}
	name, uri := "Engram Garden appview", o.base
	meta.ClientName, meta.ClientURI = &name, &uri
	writeJSON(w, http.StatusOK, meta)
}

// ServeJWKS serves the client's public key.
func (o *OAuthClient) ServeJWKS(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, o.grant.Config.PublicJWKS())
}

type oauthDocServer interface {
	ServeMetadata(http.ResponseWriter, *http.Request)
	ServeJWKS(http.ResponseWriter, *http.Request)
}

func (s *Server) oauthDocs() (oauthDocServer, bool) {
	if !s.grantsReady() {
		return nil, false
	}
	d, ok := s.Grants.Auth.(oauthDocServer)
	return d, ok
}

// ---- storage ----

// AuthStore keeps OAuth sessions and sign-ins in progress in the
// control-plane database, so every node sees them. Sessions are overwritten
// on each token refresh.
type AuthStore struct{ DB control.Store }

var _ oauth.ClientAuthStore = AuthStore{}

// GetSession implements oauth.ClientAuthStore.
func (a AuthStore) GetSession(ctx context.Context, did syntax.DID, sessionID string) (*oauth.ClientSessionData, error) {
	row, err := a.DB.GetSession(ctx, did.String(), sessionID)
	if err != nil {
		return nil, fmt.Errorf("loading OAuth session: %w", err)
	}
	var sess oauth.ClientSessionData
	if err := json.Unmarshal(row.Data, &sess); err != nil {
		return nil, err
	}
	if sess.AccountDID != did || sess.SessionID != sessionID {
		return nil, errors.New("stored OAuth session doesn't match")
	}
	return &sess, nil
}

// SaveSession implements oauth.ClientAuthStore.
func (a AuthStore) SaveSession(ctx context.Context, sess oauth.ClientSessionData) error {
	raw, err := json.Marshal(sess)
	if err != nil {
		return err
	}
	return a.DB.PutSession(ctx, control.Session{
		DID: sess.AccountDID.String(), ID: sess.SessionID, Data: raw, Updated: time.Now().UTC(),
	})
}

// DeleteSession implements oauth.ClientAuthStore.
func (a AuthStore) DeleteSession(ctx context.Context, did syntax.DID, sessionID string) error {
	return a.DB.DeleteSession(ctx, did.String(), sessionID)
}

// GetAuthRequestInfo implements oauth.ClientAuthStore. Sign-ins older than
// pendingTTL are gone.
func (a AuthStore) GetAuthRequestInfo(ctx context.Context, state string) (*oauth.AuthRequestData, error) {
	row, err := a.DB.GetRequest(ctx, state)
	if err != nil {
		return nil, fmt.Errorf("loading sign-in: %w", err)
	}
	var info oauth.AuthRequestData
	if err := json.Unmarshal(row.Data, &info); err != nil {
		return nil, err
	}
	if time.Since(row.Created) > pendingTTL || info.State != state {
		_ = a.DB.DeleteRequest(ctx, state)
		return nil, errors.New("sign-in expired")
	}
	return &info, nil
}

// SaveAuthRequestInfo implements oauth.ClientAuthStore.
func (a AuthStore) SaveAuthRequestInfo(ctx context.Context, info oauth.AuthRequestData) error {
	raw, err := json.Marshal(info)
	if err != nil {
		return err
	}
	return a.DB.PutRequest(ctx, control.Request{State: info.State, Data: raw, Created: time.Now().UTC()})
}

// DeleteAuthRequestInfo implements oauth.ClientAuthStore.
func (a AuthStore) DeleteAuthRequestInfo(ctx context.Context, state string) error {
	return a.DB.DeleteRequest(ctx, state)
}
