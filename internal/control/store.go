// Package control keeps the appview's control-plane state: which
// authorities granted it access to their spaces, the OAuth sessions those
// grants use, sign-ins in progress, and which spaces are registered.
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
}
