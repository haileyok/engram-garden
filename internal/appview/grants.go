package appview

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/bluesky-social/indigo/atproto/atclient"
	"github.com/bluesky-social/indigo/atproto/syntax"

	"github.com/haileyok/engram-garden/internal/blob"
	"github.com/haileyok/engram-garden/internal/lex"
	"github.com/haileyok/engram-garden/internal/spaceclient"
	"github.com/haileyok/engram-garden/internal/spacestore"
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
	// Finish completes a callback.
	Finish(ctx context.Context, query url.Values) (*AuthResult, error)
	// Resume returns an API client for a kept session. It refreshes tokens
	// as needed.
	Resume(ctx context.Context, did syntax.DID, sessionID string) (*atclient.APIClient, error)
	// Revoke revokes a session's tokens and forgets it.
	Revoke(ctx context.Context, did syntax.DID, sessionID string) error
}

// Grant lets the appview read one space as its authority.
type Grant struct {
	Space     string    `json:"space"`
	DID       string    `json:"did"`
	SessionID string    `json:"sessionId"`
	GrantedAt time.Time `json:"grantedAt"`
}

const grantPrefix = "grants/"

func grantKey(spaceURI string) string {
	return grantPrefix + spacestore.SpaceKey(spaceURI) + ".json"
}

// Grants keeps grants in object storage and mints delegation tokens with
// them. It implements spaceclient.Delegator.
type Grants struct {
	Blob blob.Store
	Auth Authorizer

	mu     sync.Mutex
	lapsed map[string]string // space -> why the grant last failed
	locks  map[string]*sync.Mutex
}

var _ spaceclient.Delegator = (*Grants)(nil)

// Get returns the space's grant, or nil if there is none.
func (g *Grants) Get(ctx context.Context, spaceURI string) (*Grant, error) {
	raw, err := blob.GetBytes(ctx, g.Blob, grantKey(spaceURI))
	if errors.Is(err, blob.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var gr Grant
	if err := json.Unmarshal(raw, &gr); err != nil {
		return nil, fmt.Errorf("reading the grant for %s: %w", spaceURI, err)
	}
	if gr.Space != spaceURI {
		return nil, fmt.Errorf("the grant stored for %s names %s", spaceURI, gr.Space)
	}
	return &gr, nil
}

// Put saves a grant, replacing any earlier one.
func (g *Grants) Put(ctx context.Context, gr Grant) error {
	raw, err := json.Marshal(gr)
	if err != nil {
		return err
	}
	if err := blob.PutBytes(ctx, g.Blob, grantKey(gr.Space), raw, false); err != nil {
		return err
	}
	g.setLapsed(gr.Space, "")
	return nil
}

// Delete removes the space's grant.
func (g *Grants) Delete(ctx context.Context, spaceURI string) error {
	if err := g.Blob.Delete(ctx, grantKey(spaceURI)); err != nil {
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
		g.setLapsed(spaceURI, "the grant's sign-in is gone: "+err.Error())
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

const pendingPrefix = "oauth/pending/"

// pending is a grant or stop waiting for its callback.
type pending struct {
	State   string    `json:"state"`
	Space   string    `json:"space"`
	Mode    string    `json:"mode"`
	Return  string    `json:"return,omitempty"`
	Created time.Time `json:"created"`
}

func pendingKey(state string) string {
	h := sha256.Sum256([]byte(state))
	return pendingPrefix + hex.EncodeToString(h[:]) + ".json"
}

func (g *Grants) savePending(ctx context.Context, p pending) error {
	raw, err := json.Marshal(p)
	if err != nil {
		return err
	}
	return blob.PutBytes(ctx, g.Blob, pendingKey(p.State), raw, false)
}

// takePending returns and forgets a sign-in in progress, or nil if there
// is none or it expired.
func (g *Grants) takePending(ctx context.Context, state string) (*pending, error) {
	key := pendingKey(state)
	raw, err := blob.GetBytes(ctx, g.Blob, key)
	if errors.Is(err, blob.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err := g.Blob.Delete(ctx, key); err != nil {
		return nil, err
	}
	var p pending
	if err := json.Unmarshal(raw, &p); err != nil || p.State != state || time.Since(p.Created) > pendingTTL {
		return nil, nil
	}
	return &p, nil
}
