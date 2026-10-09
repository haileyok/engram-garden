// Package control keeps the appview's control-plane state: which
// authorities granted it access to their spaces, the OAuth sessions those
// grants use, sign-ins in progress, and which spaces are registered. It
// also keeps what the web app's connector for apps like claude.ai needs:
// the apps that registered, the accounts' approvals of them, and approvals
// waiting to be traded for tokens.
//
// This is small, mutable, and can't be rebuilt from the members' PDSes
// (losing a grant means its authority has to grant again), so it lives in a
// database rather than next to the index in object storage. Postgres is the
// store for deployments; Memory is for tests and for running one appview
// locally.
//
// Times are always passed in by the caller, never read from a clock here,
// so every implementation behaves the same under test.
package control

import (
	"context"
	"errors"
	"time"
)

// ErrNotFound reports a session or sign-in that isn't there.
var ErrNotFound = errors.New("not found")

// Grant lets the appview read one space as its authority.
type Grant struct {
	Space     string    `json:"space"`
	DID       string    `json:"did"`
	SessionID string    `json:"sessionId"`
	GrantedAt time.Time `json:"grantedAt"`
}

// Session is an OAuth session: Data is indigo's session record as JSON
// (tokens, the DPoP key, authorization server URLs). It is rewritten on
// every token refresh; Updated says when.
type Session struct {
	DID     string
	ID      string
	Data    []byte
	Updated time.Time
}

// Request is indigo's record of an OAuth sign-in in progress, as JSON.
type Request struct {
	State   string
	Data    []byte
	Created time.Time
}

// Pending is a grant or stop waiting for its callback: what the person
// asked for, kept next to indigo's record of the sign-in itself.
type Pending struct {
	State   string
	Space   string
	Mode    string
	Return  string
	Created time.Time
}

// Registration records that a space is indexed.
type Registration struct {
	Space string
	At    time.Time
}

// MCPClient is an app that registered itself to connect to the web app
// over MCP, such as claude.ai.
type MCPClient struct {
	ID           string
	Name         string
	RedirectURIs []string
	Created      time.Time
}

// MCPGrant is one account's approval of one app. It names the web app
// session the app acts through, and holds the hash of the app's current
// refresh token and of the one before it: a refresh token is traded once,
// so seeing the previous one again means someone kept a copy.
type MCPGrant struct {
	ID              string
	DID             string
	SessionID       string
	ClientID        string
	Created         time.Time
	LastUsed        time.Time
	RefreshHash     string
	PrevRefreshHash string
}

// MCPCode is an approval waiting for its app to trade it for tokens. Hash
// is the hash of the code, which is a bearer secret.
type MCPCode struct {
	Hash        string
	ClientID    string
	RedirectURI string
	Challenge   string
	DID         string
	SessionID   string
	Expires     time.Time
}

// Store is the appview's control-plane state. Methods are safe for
// concurrent use, from several nodes at once for a database.
type Store interface {
	// GetGrant returns the space's grant, or nil if there is none.
	GetGrant(ctx context.Context, space string) (*Grant, error)
	// PutGrant saves a grant, replacing any earlier one for the space.
	PutGrant(ctx context.Context, g Grant) error
	// DeleteGrant removes the space's grant. Deleting a missing one is not
	// an error.
	DeleteGrant(ctx context.Context, space string) error
	// ListGrants returns every grant.
	ListGrants(ctx context.Context) ([]Grant, error)

	// GetSession returns the session, or ErrNotFound.
	GetSession(ctx context.Context, did, id string) (*Session, error)
	// PutSession saves a session, replacing any earlier one with the same
	// DID and ID.
	PutSession(ctx context.Context, s Session) error
	// DeleteSession removes a session. Deleting a missing one is not an
	// error.
	DeleteSession(ctx context.Context, did, id string) error
	// ListSessions returns every session.
	ListSessions(ctx context.Context) ([]Session, error)

	// GetRequest returns the sign-in's record, or ErrNotFound.
	GetRequest(ctx context.Context, state string) (*Request, error)
	// PutRequest saves a sign-in's record, replacing any with the same state.
	PutRequest(ctx context.Context, r Request) error
	// DeleteRequest removes a sign-in's record. Deleting a missing one is
	// not an error.
	DeleteRequest(ctx context.Context, state string) error

	// PutPending saves a pending grant or stop, replacing any with the same
	// state.
	PutPending(ctx context.Context, p Pending) error
	// TakePending returns the pending sign-in and removes it, or returns
	// nil if there is none. Of several callers asking for the same state at
	// once, exactly one gets it.
	TakePending(ctx context.Context, state string) (*Pending, error)

	// DeleteStale removes sign-in records (requests and pending) created
	// before the given time, and returns how many it removed.
	DeleteStale(ctx context.Context, before time.Time) (int, error)

	// Register records a space as indexed. It reports whether this call
	// registered it; a space already registered keeps its first time.
	Register(ctx context.Context, r Registration) (bool, error)
	// Registrations returns every registered space.
	Registrations(ctx context.Context) ([]Registration, error)

	// The web app's own OAuth sessions and sign-ins in progress. They're
	// kept apart from the appview's above, which the appview's sweep
	// deletes unless a grant uses them.

	// GetWebSession returns the session, or ErrNotFound.
	GetWebSession(ctx context.Context, did, id string) (*Session, error)
	// PutWebSession saves a session, replacing any earlier one with the
	// same DID and ID.
	PutWebSession(ctx context.Context, s Session) error
	// ImportWebSession saves a session only if there isn't one with the
	// same DID and ID, and reports whether it did. For copying sessions in
	// from files: a refresh token is used once, so a session the database
	// already has is newer than any copy.
	ImportWebSession(ctx context.Context, s Session) (bool, error)
	// DeleteWebSession removes a session. Deleting a missing one is not an
	// error.
	DeleteWebSession(ctx context.Context, did, id string) error
	// GetWebRequest returns the sign-in's record, or ErrNotFound.
	GetWebRequest(ctx context.Context, state string) (*Request, error)
	// PutWebRequest saves a sign-in's record, replacing any with the same
	// state.
	PutWebRequest(ctx context.Context, r Request) error
	// DeleteWebRequest removes a sign-in's record. Deleting a missing one
	// is not an error.
	DeleteWebRequest(ctx context.Context, state string) error
	// DeleteStaleWeb removes sign-ins created before requestsBefore and
	// sessions not saved since sessionsBefore, and returns how many it
	// removed.
	DeleteStaleWeb(ctx context.Context, requestsBefore, sessionsBefore time.Time) (int, error)

	// PutMCPClient saves an app. When max apps are already saved, the
	// oldest one that no account has approved is dropped to make room; it
	// reports false, saving nothing, when every app is in use.
	PutMCPClient(ctx context.Context, c MCPClient, max int) (bool, error)
	// GetMCPClient returns the app, or nil if there is none.
	GetMCPClient(ctx context.Context, id string) (*MCPClient, error)

	// PutMCPGrant saves an approval. An account keeps at most maxPerDID:
	// past that, its least recently used ones are dropped.
	PutMCPGrant(ctx context.Context, g MCPGrant, maxPerDID int) error
	// GetMCPGrant returns the approval, or nil if there is none.
	GetMCPGrant(ctx context.Context, id string) (*MCPGrant, error)
	// ListMCPGrants returns an account's approvals, newest first.
	ListMCPGrants(ctx context.Context, did string) ([]MCPGrant, error)
	// TouchMCPGrant records that an approval was used. A missing one is
	// not an error.
	TouchMCPGrant(ctx context.Context, id string, at time.Time) error
	// DeleteMCPGrant removes an approval the account holds, and reports
	// whether it did.
	DeleteMCPGrant(ctx context.Context, id, did string) (bool, error)
	// RotateMCPRefresh trades the refresh token with hash oldHash for one
	// with newHash, for the app clientID, and returns the approval. Of
	// several callers with the same token, one gets it. It returns
	// ErrNotFound for a token that isn't current. A token that was the
	// previous one has been traded already, so it ends the approval.
	RotateMCPRefresh(ctx context.Context, oldHash, clientID, newHash string, at time.Time) (*MCPGrant, error)

	// PutMCPCode saves an approval waiting for its tokens. Codes that
	// expired by now are dropped first, and it reports false, saving
	// nothing, when max others are still waiting.
	PutMCPCode(ctx context.Context, c MCPCode, now time.Time, max int) (bool, error)
	// TakeMCPCode returns the code and removes it, or returns nil if there
	// is none or it expired by now. Of several callers asking for the same
	// code at once, exactly one gets it.
	TakeMCPCode(ctx context.Context, hash string, now time.Time) (*MCPCode, error)
}
