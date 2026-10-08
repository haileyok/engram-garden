package agent

import (
	"net/http"
	"path/filepath"

	"github.com/bluesky-social/indigo/atproto/auth/oauth"
	"github.com/bluesky-social/indigo/atproto/syntax"
)

// sharedSession authenticates requests with an OAuth session that several
// processes may use at once (engram and engram-mcp on one machine). Each
// refresh replaces the refresh token and spends the old one, so each request
// takes the session's lock, reloads the session from the store (picking up
// tokens another process saved) and lets indigo refresh and save it before
// the lock is released.
type sharedSession struct {
	app       *oauth.ClientApp
	did       syntax.DID
	sessionID string
	// lock is the lock file's path; empty means this process is the only
	// user.
	lock string
}

func lockPath(dir string, did syntax.DID, sessionID string) string {
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, "locks", hashName(did.String(), sessionID)+".lock")
}

// DoWithAuth implements atclient.AuthMethod.
func (s *sharedSession) DoWithAuth(c *http.Client, req *http.Request, endpoint syntax.NSID) (*http.Response, error) {
	if s.lock != "" {
		unlock, err := lockFile(s.lock)
		if err != nil {
			return nil, err
		}
		defer unlock()
	}
	sess, err := s.app.ResumeSession(req.Context(), s.did, s.sessionID)
	if err != nil {
		return nil, Explain(err)
	}
	return sess.DoWithAuth(c, req, endpoint)
}
