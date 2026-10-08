package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/bluesky-social/indigo/atproto/atclient"
	"github.com/bluesky-social/indigo/atproto/auth/oauth"
	"github.com/bluesky-social/indigo/atproto/identity"
	"github.com/bluesky-social/indigo/atproto/syntax"

	"github.com/haileyok/cocoon/oauth/scopes"
	"github.com/haileyok/engram-garden/internal/embed"
	"github.com/haileyok/engram-garden/internal/lex"
	"github.com/haileyok/engram-garden/internal/spaceclient"
)

// Scopes are what an agent's OAuth sign-in asks for: read the memory spaces
// it belongs to, and write and delete its own memories in them.
var Scopes = []string{
	"atproto",
	"space:" + lex.SpaceType + "?authority=*&collection=" + lex.MemoryCollection + "&action=read&action=create&action=update&action=delete",
}

// Options are what opening an agent needs besides its settings.
type Options struct {
	Dir identity.Directory
	// Store keeps OAuth sessions and sign-ins in progress.
	Store oauth.ClientAuthStore
	// LockDir holds the locks that let several processes share an OAuth
	// session. Empty means only this process uses it.
	LockDir string
	HTTP    *http.Client
	Log     *slog.Logger
}

// hashName names a file after some strings, without revealing them.
func hashName(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

func (o Options) dir() identity.Directory {
	if o.Dir == nil {
		return identity.DefaultDirectory()
	}
	return o.Dir
}

// OAuthApp is the agent's OAuth client: a public loopback client for one
// callback address. Refreshing a session needs the address it signed in
// with, since the client's ID names it.
func OAuthApp(callback string, o Options) *oauth.ClientApp {
	cfg := oauth.NewLocalhostConfig(callback, Scopes)
	cfg.UserAgent = "engram-cli"
	app := oauth.NewClientApp(&cfg, o.Store)
	app.Dir = o.dir()
	if o.HTTP != nil {
		app.Client = o.HTTP
	}
	return app
}

// Session opens the account's session at its PDS.
func (s Settings) Session(ctx context.Context, o Options) (*atclient.APIClient, error) {
	a := s.Account
	switch a.SignIn {
	case SignInPassword:
		// indigo signs in with http.DefaultClient, which never times out:
		// bound signing in by the client's timeout, then use the client.
		if o.HTTP != nil && o.HTTP.Timeout > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, o.HTTP.Timeout)
			defer cancel()
		}
		var api *atclient.APIClient
		var err error
		if a.PDSHost != "" {
			api, err = atclient.LoginWithPasswordHost(ctx, a.PDSHost, a.Handle, a.Password, "", nil)
		} else {
			id, perr := syntax.ParseAtIdentifier(a.Handle)
			if perr != nil {
				return nil, fmt.Errorf("handle %q: %w", a.Handle, perr)
			}
			api, err = atclient.LoginWithPassword(ctx, o.dir(), id, a.Password, "", nil)
		}
		if err != nil {
			return nil, err
		}
		if o.HTTP != nil {
			api.Client = o.HTTP
		}
		return api, nil
	case SignInOAuth:
		if o.Store == nil {
			return nil, errors.New("no OAuth session store")
		}
		did, err := syntax.ParseDID(a.DID)
		if err != nil {
			return nil, fmt.Errorf("account DID %q: %w", a.DID, err)
		}
		app := OAuthApp(a.Callback, o)
		sess, err := app.ResumeSession(ctx, did, a.SessionID)
		if err != nil {
			return nil, Explain(fmt.Errorf("resuming the sign-in: %w", err))
		}
		api := sess.APIClient()
		api.Auth = &sharedSession{app: app, did: did, sessionID: a.SessionID, lock: lockPath(o.LockDir, did, a.SessionID)}
		return api, nil
	}
	return nil, s.Check()
}

// Revoke ends an OAuth sign-in: the server revokes its tokens, and the
// session is forgotten. It waits for requests other processes are making
// with the session, which could otherwise save refreshed tokens after it's
// deleted.
func Revoke(ctx context.Context, a Account, o Options) error {
	if a.SignIn != SignInOAuth || a.SessionID == "" {
		return nil
	}
	did, err := syntax.ParseDID(a.DID)
	if err != nil {
		return err
	}
	if path := lockPath(o.LockDir, did, a.SessionID); path != "" {
		unlock, err := lockFile(path)
		if err != nil {
			return err
		}
		defer unlock()
	}
	return OAuthApp(a.Callback, o).Logout(ctx, did, a.SessionID)
}

// Provider builds the embedding provider.
func (s Settings) Provider() (embed.Provider, error) {
	switch p := s.Embed.Provider; p {
	case "hashing":
		return embed.HashingProvider{}, nil
	case "", "openai":
		base := s.Embed.URL
		if base == "" {
			base = "http://localhost:11434/v1"
		}
		return &embed.OpenAIProvider{
			BaseURL: base, APIKey: s.Embed.APIKey, ModelName: s.Embed.Model, Digest: s.Embed.Digest,
			HTTP: &http.Client{Timeout: 60 * time.Second},
		}, nil
	default:
		return nil, fmt.Errorf("unknown embedding provider %q (openai or hashing)", p)
	}
}

// Open signs in and returns the agent's spaces. It needs no space set up:
// ListSpaces then lists the ones the account belongs to.
func Open(ctx context.Context, s Settings, o Options) (*Spaces, error) {
	if err := s.CheckAccount(); err != nil {
		return nil, err
	}
	p, err := s.Provider()
	if err != nil {
		return nil, err
	}
	sess, err := s.Session(ctx, o)
	if err != nil {
		return nil, err
	}
	c, err := spaceclient.New(sess, o.dir(), o.HTTP)
	if err != nil {
		return nil, err
	}
	return &Spaces{Client: c, AppviewURL: s.AppviewURL, AppviewDID: s.AppviewDID, Provider: p, Log: o.Log, Settings: s}, nil
}

// ErrSignInExpired means the account's server no longer accepts the
// agent's sign-in.
var ErrSignInExpired = errors.New("the agent's sign-in has expired or was revoked: run `engram login` (OAuth sign-ins last two weeks)")

// Explain turns a refused or missing OAuth session into ErrSignInExpired,
// keeping the original error.
func Explain(err error) error {
	if err == nil || errors.Is(err, ErrSignInExpired) {
		return err
	}
	msg := err.Error()
	if strings.Contains(msg, "token refresh failed") || strings.Contains(msg, "session: not found") {
		return fmt.Errorf("%w (%v)", ErrSignInExpired, err)
	}
	return err
}

// ---- signing in ----

// Prompt is how signing in talks to the person at the terminal.
type Prompt struct {
	Out io.Writer
	// Lines are lines of input, which may be an address pasted from the
	// browser, for machines whose browser can't reach this one's loopback
	// address. A line is only taken while waiting for the sign-in, so input
	// after it is left for whoever reads next. Nil means no input.
	Lines <-chan string
	// Browser opens a URL; nil, or a failure, prints it instead.
	Browser func(string) error
}

// LoginOAuth signs in as handle through the account's authorization server,
// returning the account to save.
func LoginOAuth(ctx context.Context, handle string, o Options, p Prompt) (Account, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return Account{}, err
	}
	defer ln.Close()
	callback := fmt.Sprintf("http://127.0.0.1:%d/callback", ln.Addr().(*net.TCPAddr).Port)
	app := OAuthApp(callback, o)
	redirect, err := app.StartAuthFlow(ctx, handle)
	if err != nil {
		return Account{}, fmt.Errorf("starting to sign in as %s: %w", handle, err)
	}
	fmt.Fprintf(p.Out, "Sign in as %s and approve access:\n\n  %s\n\n", handle, redirect)
	if p.Browser == nil || p.Browser(redirect) != nil {
		fmt.Fprintln(p.Out, "Open that address in a browser.")
	}
	fmt.Fprintln(p.Out, "If the browser is on another machine, it ends on a page that won't load; paste that page's address here.")
	q, err := waitForCallback(ctx, ln, p.Lines)
	if err != nil {
		return Account{}, err
	}
	sess, err := app.ProcessCallback(ctx, q)
	if err != nil {
		var ce *oauth.AuthRequestCallbackError
		if errors.As(err, &ce) && ce.ErrorCode == "access_denied" {
			return Account{}, errors.New("you declined, so the agent isn't signed in")
		}
		return Account{}, fmt.Errorf("finishing sign-in: %w", err)
	}
	if !grantsMemories(sess.Scopes) {
		_ = app.Logout(ctx, sess.AccountDID, sess.SessionID)
		return Account{}, fmt.Errorf("the account's server didn't grant access to memory spaces (granted: %s)", strings.Join(sess.Scopes, " "))
	}
	return Account{
		Handle: handle, DID: sess.AccountDID.String(), SignIn: SignInOAuth,
		SessionID: sess.SessionID, Callback: callback, SignedInAt: time.Now().UTC(),
	}, nil
}

// grantsMemories reports whether the granted scopes let the agent read
// memory spaces and write its own memories. Servers rewrite scopes when they
// issue them, so this reads what each scope means.
func grantsMemories(granted []string) bool {
	for _, g := range granted {
		p := scopes.ParseSpacePermission(g)
		if p == nil || (p.Type != lex.SpaceType && p.Type != "*") {
			continue
		}
		want := scopes.SpaceMatch{Type: lex.SpaceType, Authority: p.Authority, Skey: p.Skey, Collection: lex.MemoryCollection}
		ok := true
		for _, action := range []string{"read", "create"} {
			want.Action = action
			ok = ok && p.Matches(want)
		}
		if ok {
			return true
		}
	}
	return false
}

// waitForCallback returns the callback's query: from the browser reaching
// ln, or from an address pasted as one of lines.
func waitForCallback(ctx context.Context, ln net.Listener, lines <-chan string) (url.Values, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	got := make(chan url.Values, 2)
	srv := &http.Server{ReadHeaderTimeout: 10 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/callback" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintln(w, "Signed in. You can close this tab and go back to the terminal.")
		select {
		case got <- r.URL.Query():
		default:
		}
	})}
	go func() { _ = srv.Serve(ln) }()
	defer srv.Close()
	for {
		select {
		case q := <-got:
			return q, nil
		case line, ok := <-lines:
			if !ok {
				lines = nil // input ended; the browser can still finish
				continue
			}
			if u, err := url.Parse(strings.TrimSpace(line)); err == nil && u.Query().Get("state") != "" {
				return u.Query(), nil
			}
		case <-ctx.Done():
			return nil, errors.New("gave up waiting for the sign-in to finish")
		}
	}
}
