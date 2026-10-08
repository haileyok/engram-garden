package appview

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/bluesky-social/indigo/atproto/atclient"
	"github.com/bluesky-social/indigo/atproto/syntax"

	"github.com/haileyok/engram-garden/internal/control"
	"github.com/haileyok/engram-garden/internal/lex"
	"github.com/haileyok/engram-garden/internal/spaceclient"
)

// The appview reads a space with its authority's OAuth grant: read-only
// access to the spaces they govern. It keeps the session and asks the
// authority's PDS for delegation tokens with it. See
// docs/design/indexing-access.md.

// GrantScopes are the OAuth scopes a grant asks for: read the memory spaces
// the person governs. With no authority, a space: scope covers only those.
var GrantScopes = []string{"atproto", "space:" + lex.SpaceType + "?action=read"}

// StopScopes are asked for when stopping indexing, only to prove who the
// person is.
var StopScopes = []string{"atproto"}

const (
	modeGrant = "grant"
	modeStop  = "stop"
)

// ErrDeclined is returned by Authorizer.Finish when the person declined.
var ErrDeclined = errors.New("the person declined")

// ErrNoGrant means nobody has granted the appview access to the space.
var ErrNoGrant = errors.New("no grant for this space: its authority hasn't let the appview index it")

// AuthResult is a finished sign-in.
type AuthResult struct {
	DID       syntax.DID
	SessionID string
	Scopes    []string
}

// Authorizer runs OAuth sign-ins and keeps their sessions.
type Authorizer interface {
	// Start begins signing in as did, asking for GrantScopes (modeGrant) or
	// StopScopes (modeStop). It returns where to send the browser, and the
	// OAuth state that will come back to the callback.
	Start(ctx context.Context, did syntax.DID, mode string) (redirect, state string, err error)
	// Finish completes the callback of a sign-in started with mode.
	Finish(ctx context.Context, mode string, query url.Values) (*AuthResult, error)
	// Resume returns an API client for a grant's session. It refreshes
	// tokens as needed.
	Resume(ctx context.Context, did syntax.DID, sessionID string) (*atclient.APIClient, error)
	// Revoke revokes the tokens of a session made with mode and forgets it.
	Revoke(ctx context.Context, mode string, did syntax.DID, sessionID string) error
}

// Grant lets the appview read one space as its authority.
type Grant = control.Grant

// Grants keeps grants in the control-plane database and mints delegation
// tokens with them. It implements spaceclient.Delegator.
type Grants struct {
	DB   control.Store
	Auth Authorizer

	mu     sync.Mutex
	lapsed map[string]string // space -> why the grant last failed
	locks  map[string]*sync.Mutex
}

var _ spaceclient.Delegator = (*Grants)(nil)

// Get returns the space's grant, or nil if there is none.
func (g *Grants) Get(ctx context.Context, spaceURI string) (*Grant, error) {
	return g.DB.GetGrant(ctx, spaceURI)
}

// Put saves a grant, replacing any earlier one.
func (g *Grants) Put(ctx context.Context, gr Grant) error {
	if err := g.DB.PutGrant(ctx, gr); err != nil {
		return err
	}
	g.setLapsed(gr.Space, "")
	return nil
}

// Delete removes the space's grant.
func (g *Grants) Delete(ctx context.Context, spaceURI string) error {
	if err := g.DB.DeleteGrant(ctx, spaceURI); err != nil {
		return err
	}
	g.setLapsed(spaceURI, "")
	return nil
}

func (g *Grants) setLapsed(spaceURI, why string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.lapsed == nil {
		g.lapsed = map[string]string{}
	}
	if why == "" {
		delete(g.lapsed, spaceURI)
	} else {
		g.lapsed[spaceURI] = why
	}
}

// sessionLock serializes uses of one OAuth session on this node: a refresh
// replaces the refresh token, so two at once would spend the same one.
func (g *Grants) sessionLock(sessionID string) *sync.Mutex {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.locks == nil {
		g.locks = map[string]*sync.Mutex{}
	}
	l, ok := g.locks[sessionID]
	if !ok {
		l = &sync.Mutex{}
		g.locks[sessionID] = l
	}
	return l
}

// DelegationToken asks the authority's PDS for a delegation token with the
// space's grant.
func (g *Grants) DelegationToken(ctx context.Context, spaceURI string) (string, error) {
	gr, err := g.Get(ctx, spaceURI)
	if err != nil {
		return "", err
	}
	if gr == nil {
		return "", fmt.Errorf("%s: %w", spaceURI, ErrNoGrant)
	}
	l := g.sessionLock(gr.SessionID)
	l.Lock()
	defer l.Unlock()
	api, err := g.Auth.Resume(ctx, syntax.DID(gr.DID), gr.SessionID)
	if err != nil {
		// A missing session won't come back; other failures (storage,
		// network) may be passing.
		if errors.Is(err, control.ErrNotFound) {
			g.setLapsed(spaceURI, "the grant's sign-in is gone")
		}
		return "", fmt.Errorf("resuming the grant for %s: %w", spaceURI, err)
	}
	tok, err := spaceclient.SessionDelegator{Session: api}.DelegationToken(ctx, spaceURI)
	if err != nil {
		if refused(err) {
			g.setLapsed(spaceURI, err.Error())
		}
		return "", err
	}
	g.setLapsed(spaceURI, "")
	return tok, nil
}

// Revoke revokes a session and forgets it. It waits for any use of the
// session on this node, so a token refresh can't save it again after it's
// gone. (Another node's refresh can; Sweep catches that.)
func (g *Grants) Revoke(ctx context.Context, mode string, did syntax.DID, sessionID string) error {
	l := g.sessionLock(sessionID)
	l.Lock()
	defer l.Unlock()
	return g.Auth.Revoke(ctx, mode, did, sessionID)
}

// refused reports whether the authority's servers refused the grant itself,
// rather than failing in passing.
func refused(err error) bool {
	if strings.Contains(err.Error(), "token refresh failed (HTTP 4") {
		return true
	}
	var ae *atclient.APIError
	return errors.As(err, &ae) && (ae.StatusCode == http.StatusUnauthorized || ae.StatusCode == http.StatusForbidden)
}

// Access describes the space's grant for getSpaceStatus.
func (g *Grants) Access(ctx context.Context, spaceURI string) (map[string]any, error) {
	gr, err := g.Get(ctx, spaceURI)
	if err != nil {
		return nil, err
	}
	if gr == nil {
		return map[string]any{"state": "missing"}, nil
	}
	out := map[string]any{"state": "granted", "grantedBy": gr.DID, "grantedAt": gr.GrantedAt.UTC().Format(time.RFC3339)}
	g.mu.Lock()
	why := g.lapsed[spaceURI]
	g.mu.Unlock()
	if why != "" {
		out["state"] = "lapsed"
		out["error"] = why
	}
	return out, nil
}

// ---- sign-ins in progress ----

// pendingTTL is how long a grant or stop may take at the authorization
// server.
const pendingTTL = 10 * time.Minute

// pending is a grant or stop waiting for its callback.
type pending = control.Pending

func (g *Grants) savePending(ctx context.Context, p pending) error {
	return g.DB.PutPending(ctx, p)
}

// Sweep clears what sign-ins leave behind, older than pendingTTL at now:
//   - sign-ins nobody finished (abandoned or refused at the authorization
//     server; a callback deletes its own);
//   - OAuth sessions no grant uses, such as one a token refresh on another
//     node saved again after it was revoked. They're revoked, then deleted.
//
// It returns how many it cleared.
func (g *Grants) Sweep(ctx context.Context, now time.Time) (int, error) {
	n, err := g.DB.DeleteStale(ctx, now.Add(-pendingTTL))
	if err != nil {
		return n, err
	}
	m, err := g.sweepSessions(ctx, now)
	return n + m, err
}

func (g *Grants) sweepSessions(ctx context.Context, now time.Time) (int, error) {
	sessions, err := g.DB.ListSessions(ctx)
	if err != nil || len(sessions) == 0 {
		return 0, err
	}
	grants, err := g.DB.ListGrants(ctx)
	if err != nil {
		return 0, err
	}
	used := map[[2]string]bool{}
	for _, gr := range grants {
		used[[2]string{gr.DID, gr.SessionID}] = true
	}
	n := 0
	for _, s := range sessions {
		if used[[2]string{s.DID, s.ID}] || now.Sub(s.Updated) <= pendingTTL {
			continue
		}
		_ = g.Revoke(ctx, modeGrant, syntax.DID(s.DID), s.ID)
		if err := g.DB.DeleteSession(ctx, s.DID, s.ID); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// takePending returns and forgets a sign-in in progress, or nil if there
// is none or it expired.
func (g *Grants) takePending(ctx context.Context, state string) (*pending, error) {
	p, err := g.DB.TakePending(ctx, state)
	if err != nil || p == nil {
		return nil, err
	}
	if time.Since(p.Created) > pendingTTL {
		return nil, nil
	}
	return p, nil
}
