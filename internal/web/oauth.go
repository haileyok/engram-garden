package web

import (
	"context"
	"crypto/hmac"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/bluesky-social/indigo/atproto/atclient"
	"github.com/bluesky-social/indigo/atproto/atcrypto"
	"github.com/bluesky-social/indigo/atproto/auth/oauth"
	"github.com/bluesky-social/indigo/atproto/identity"
	"github.com/bluesky-social/indigo/atproto/syntax"

	"github.com/haileyok/engram-garden/internal/lex"
)

// Scopes are the permissions the web app asks for:
//
//   - read every memory space the user is a member of, list their spaces,
//     and delete their own memories in them;
//   - in spaces the user governs: create spaces, manage members, and write
//     the space's model (its garden.engram.config record).
var Scopes = []string{
	"atproto",
	"space:" + lex.SpaceType + "?authority=*&collection=" + lex.MemoryCollection + "&action=read&action=delete",
	"space:" + lex.SpaceType + "?collection=" + lex.ConfigCollection + "&action=read&action=create&action=update&manage=create&manage=update",
}

// OAuth signs people in with ATProto OAuth.
type OAuth struct {
	App       *oauth.ClientApp
	PublicURL string
}

// OAuthConfig configures sign-in.
type OAuthConfig struct {
	// PublicURL is where the web app is served. An http://127.0.0.1 URL
	// makes a development client, which needs no key.
	PublicURL string
	// Key signs the client's assertions (P-256). Required unless developing.
	Key   atcrypto.PrivateKey
	Store oauth.ClientAuthStore
	Dir   identity.Directory
	HTTP  *http.Client
}

// NewOAuth sets up sign-in.
func NewOAuth(cfg OAuthConfig) (*OAuth, error) {
	u, err := url.Parse(cfg.PublicURL)
	if err != nil || u.Host == "" || u.Path != "" && u.Path != "/" {
		return nil, fmt.Errorf("the public URL must be a bare origin like https://engram.garden, not %q", cfg.PublicURL)
	}
	base := u.Scheme + "://" + u.Host
	callback := base + "/oauth/callback"
	var config oauth.ClientConfig
	switch {
	case u.Scheme == "http" && u.Hostname() == "127.0.0.1":
		// Loopback clients are public and need no metadata document.
		config = oauth.NewLocalhostConfig(callback, Scopes)
	case u.Scheme == "https":
		config = oauth.NewPublicConfig(base+"/oauth/client-metadata.json", callback, Scopes)
		if cfg.Key == nil {
			return nil, errors.New("a client key is required (generate one with: goat key generate -t P-256)")
		}
		if err := config.SetClientSecret(cfg.Key, "engram-web-1"); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("the public URL must be https, or http://127.0.0.1:<port> for development, not %q", cfg.PublicURL)
	}
	config.UserAgent = "engram-garden-web"
	app := oauth.NewClientApp(&config, stateCapture{cfg.Store})
	if cfg.Dir != nil {
		app.Dir = cfg.Dir
	}
	if cfg.HTTP != nil {
		app.Client = cfg.HTTP
	}
	return &OAuth{App: app, PublicURL: base}, nil
}

// signinCookie ties a pending sign-in to the browser that started it, so
// nobody can send someone a callback link that signs them in as somebody
// else.
const signinCookie = "engram_signin"

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

// Resume implements Auth.
func (o *OAuth) Resume(ctx context.Context, did syntax.DID, sessionID string) (*atclient.APIClient, error) {
	sess, err := o.App.ResumeSession(ctx, did, sessionID)
	if err != nil {
		return nil, err
	}
	return sess.APIClient(), nil
}

// Logout implements Auth: it revokes the tokens and forgets the session.
func (o *OAuth) Logout(ctx context.Context, did syntax.DID, sessionID string) error {
	return o.App.Logout(ctx, did, sessionID)
}

func (o *OAuth) handleMetadata(w http.ResponseWriter, r *http.Request) {
	meta := o.App.Config.ClientMetadata()
	if o.App.Config.IsConfidential() {
		jwks := o.PublicURL + "/oauth/jwks.json"
		meta.JWKSURI = &jwks
	}
	name, uri := "Engram Garden", o.PublicURL
	meta.ClientName, meta.ClientURI = &name, &uri
	writeJSON(w, http.StatusOK, meta)
}

func (o *OAuth) handleJWKS(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, o.App.Config.PublicJWKS())
}

// handleLogin starts signing in: it returns the URL of the user's
// authorization server.
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Handle string `json:"handle"`
	}
	if e := decode(r, &in); e != nil {
		writeErr(w, e)
		return
	}
	id := strings.TrimPrefix(strings.TrimSpace(in.Handle), "@")
	if _, err := syntax.ParseAtIdentifier(id); err != nil && !strings.HasPrefix(id, "https://") {
		writeErr(w, apiErr(http.StatusBadRequest, "InvalidHandle", "enter a handle like alice.bsky.social"))
		return
	}
	var state string
	redirect, err := s.OAuth.App.StartAuthFlow(context.WithValue(r.Context(), stateKey{}, &state), id)
	if err != nil {
		s.log().Info("couldn't start sign-in", "handle", id, "err", err)
		writeErr(w, apiErr(http.StatusBadRequest, "SignInFailed", "couldn't start signing in as %s", id))
		return
	}
	if state == "" {
		writeErr(w, apiErr(http.StatusInternalServerError, "SignInFailed", "sign-in wasn't recorded"))
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: signinCookie, Value: s.mac("signin." + state), Path: "/oauth/callback",
		HttpOnly: true, Secure: strings.HasPrefix(s.Origin, "https://"), SameSite: http.SameSiteLaxMode,
		MaxAge: int(authRequestTTL.Seconds()),
	})
	writeJSON(w, http.StatusOK, map[string]string{"redirect": redirect})
}

// handleCallback finishes signing in and returns to the app.
func (s *Server) handleCallback(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: signinCookie, Value: "", Path: "/oauth/callback", MaxAge: -1, HttpOnly: true,
		Secure: strings.HasPrefix(s.Origin, "https://"), SameSite: http.SameSiteLaxMode})
	if !s.startedHere(r) {
		s.log().Info("refused a sign-in callback this browser didn't start")
		http.Redirect(w, r, "/?signin_error="+url.QueryEscape("that sign-in wasn't started from this browser; try again"), http.StatusSeeOther)
		return
	}
	sess, err := s.OAuth.App.ProcessCallback(r.Context(), r.URL.Query())
	if err != nil {
		msg := "signing in failed"
		var ce *oauth.AuthRequestCallbackError
		if errors.As(err, &ce) && ce.ErrorCode == "access_denied" {
			msg = "you declined to sign in"
		}
		s.log().Info("sign-in callback failed", "err", err)
		http.Redirect(w, r, "/?signin_error="+url.QueryEscape(msg), http.StatusSeeOther)
		return
	}
	if missing := missingScopes(sess.Scopes); len(missing) > 0 {
		// The server granted less than asked: some features will fail.
		s.log().Warn("sign-in granted fewer permissions than requested", "did", sess.AccountDID, "missing", missing)
	}
	http.SetCookie(w, s.sessionCookie(sess.AccountDID, sess.SessionID))
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// startedHere checks the callback's state belongs to a sign-in this browser
// started.
func (s *Server) startedHere(r *http.Request) bool {
	state := r.URL.Query().Get("state")
	c, err := r.Cookie(signinCookie)
	if err != nil || state == "" {
		return false
	}
	return hmac.Equal([]byte(c.Value), []byte(s.mac("signin."+state)))
}

func missingScopes(granted []string) []string {
	have := map[string]bool{}
	for _, g := range granted {
		have[g] = true
	}
	var missing []string
	for _, want := range Scopes {
		if !have[want] {
			missing = append(missing, want)
		}
	}
	return missing
}
