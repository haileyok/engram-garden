package web

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"time"

	"github.com/haileyok/engram-garden/internal/control"
)

// maxConnectorClients caps how many OAuth clients are remembered. Anyone
// can register one, so the oldest that nobody ever signed in with are
// dropped to make room.
const maxConnectorClients = 1000

// maxGrantsPerUser caps how many apps one account has connected.
const maxGrantsPerUser = 20

var errTooManyClients = errors.New("too many apps are registered")

func randomToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// hashToken is how tokens and codes are stored: they're bearer secrets, so
// the database holds only their hashes.
func hashToken(t string) string {
	h := sha256.Sum256([]byte(t))
	return hex.EncodeToString(h[:])
}

func sameHash(a, b string) bool { return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1 }

// registerClient saves a newly registered app.
func (c *Connector) registerClient(ctx context.Context, name string, redirects []string) (*control.MCPClient, error) {
	id, err := randomToken(16)
	if err != nil {
		return nil, err
	}
	cl := control.MCPClient{ID: id, Name: name, RedirectURIs: redirects, Created: time.Now().UTC()}
	ok, err := c.db.PutMCPClient(ctx, cl, maxConnectorClients)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, errTooManyClients
	}
	return &cl, nil
}

// newGrant records an account's approval of an app, and returns the app's
// first refresh token.
func (c *Connector) newGrant(ctx context.Context, clientID, did, sessionID string) (*control.MCPGrant, string, error) {
	id, err := randomToken(16)
	if err != nil {
		return nil, "", err
	}
	refresh, err := randomToken(32)
	if err != nil {
		return nil, "", err
	}
	now := time.Now().UTC()
	g := control.MCPGrant{ID: id, DID: did, SessionID: sessionID, ClientID: clientID, Created: now, LastUsed: now, RefreshHash: hashToken(refresh)}
	if err := c.db.PutMCPGrant(ctx, g, maxGrantsPerUser); err != nil {
		return nil, "", err
	}
	return &g, refresh, nil
}

// rotate trades a refresh token for the grant's next one. A token that was
// already traded ends the grant.
func (c *Connector) rotate(ctx context.Context, refresh, clientID string) (*control.MCPGrant, string, error) {
	next, err := randomToken(32)
	if err != nil {
		return nil, "", err
	}
	g, err := c.db.RotateMCPRefresh(ctx, hashToken(refresh), clientID, hashToken(next), time.Now().UTC())
	if errors.Is(err, control.ErrNotFound) {
		return nil, "", errInvalidGrant
	}
	if err != nil {
		return nil, "", err
	}
	return g, next, nil
}

var errInvalidGrant = errors.New("the code or token is invalid, expired or was already used")
