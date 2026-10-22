package agent

import (
	"errors"
	"net/http"
	"path/filepath"
	"sync"

	"github.com/bluesky-social/indigo/atproto/auth/oauth"
	"github.com/bluesky-social/indigo/atproto/syntax"
)

// sharedSession authenticates requests with an OAuth session that several
// processes may use at once (engram and engram-mcp on one machine). Each
// refresh replaces the refresh token and spends the old one, so each request
// takes the session's lock, reloads the session from the store (picking up
// tokens another process saved) and lets indigo refresh and save it before
// the lock is released.
//
// If the session is gone because the person signed in again, it follows
// the account's new session (Options.Current).
type sharedSession struct {
	opts Options
	did  syntax.DID

	mu        sync.Mutex
	app       *oauth.ClientApp
	sessionID string
	// lock is the lock file's path; empty means this process is the only
	// user.
	lock string
}

func (s *sharedSession) current() (*oauth.ClientApp, string, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.app, s.sessionID, s.lock
}

// follow switches to the account's current session if the settings name a
// different one for the same account, reporting whether it did.
func (s *sharedSession) follow(was string) bool {
	if s.opts.Current == nil {
		return false
	}
	a, err := s.opts.Current()
	if err != nil || a.SignIn != SignInOAuth || a.DID != s.did.String() || a.SessionID == "" || a.SessionID == was {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sessionID == was {
		s.app = OAuthApp(a.Callback, s.opts)
		s.sessionID = a.SessionID
		s.lock = lockPath(s.opts.LockDir, s.did, a.SessionID)
	}
	return true
}

func lockPath(dir string, did syntax.DID, sessionID string) string {
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, "locks", hashName(did.String(), sessionID)+".lock")
}

// DoWithAuth implements atclient.AuthMethod.
func (s *sharedSession) DoWithAuth(c *http.Client, req *http.Request, endpoint syntax.NSID) (*http.Response, error) {
	resp, was, err := s.do(c, req, endpoint)
	// The session is gone or refused before the request was sent: if the
	// person signed in again since, use that sign-in.
	if errors.Is(err, ErrSignInExpired) && resp == nil && s.follow(was) {
		resp, _, err = s.do(c, req, endpoint)
	}
	return resp, err
}

func (s *sharedSession) do(c *http.Client, req *http.Request, endpoint syntax.NSID) (*http.Response, string, error) {
	app, id, lock := s.current()
	if lock != "" {
		unlock, err := lockFile(lock)
		if err != nil {
			return nil, id, err
		}
		defer unlock()
	}
	sess, err := app.ResumeSession(req.Context(), s.did, id)
	if err != nil {
		return nil, id, Explain(err)
	}
	resp, err := sess.DoWithAuth(c, req, endpoint)
	return resp, id, err
}
