package web

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"
)

// maxConnectorClients caps how many OAuth clients are remembered. Anyone
// can register one, so the oldest that nobody ever signed in with are
// dropped to make room.
const maxConnectorClients = 1000

// maxGrantsPerUser caps how many apps one account has connected.
const maxGrantsPerUser = 20

var errInvalidGrant = errors.New("the code or token is invalid, expired or was already used")

// connectorClient is an app that registered itself to connect to the
// service, such as claude.ai.
type connectorClient struct {
	ID           string    `json:"id"`
	Name         string    `json:"name"`
	RedirectURIs []string  `json:"redirectUris"`
	CreatedAt    time.Time `json:"createdAt"`
}

// connectorGrant is one account's approval of one app: it names the web
// app session the app acts through.
type connectorGrant struct {
	ID        string    `json:"id"`
	DID       string    `json:"did"`
	SessionID string    `json:"sessionId"`
	ClientID  string    `json:"clientId"`
	CreatedAt time.Time `json:"createdAt"`
	LastUsed  time.Time `json:"lastUsed"`
	// RefreshHash is the hash of the current refresh token, and
	// PrevRefreshHash of the one before it. A refresh token is used once;
	// seeing the previous one again means a copy exists, so the grant is
	// ended.
	RefreshHash     string `json:"refreshHash"`
	PrevRefreshHash string `json:"prevRefreshHash,omitempty"`
}

// connectorStore keeps the registered apps and the accounts' grants, in a
// file in the web app's data directory (or only in memory with no path).
type connectorStore struct {
	path string

	mu      sync.Mutex
	clients map[string]*connectorClient
	grants  map[string]*connectorGrant
}

type connectorFile struct {
	Clients []*connectorClient `json:"clients"`
	Grants  []*connectorGrant  `json:"grants"`
}

func openConnectorStore(path string) (*connectorStore, error) {
	s := &connectorStore{path: path, clients: map[string]*connectorClient{}, grants: map[string]*connectorGrant{}}
	if path == "" {
		return s, nil
	}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	var f connectorFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	for _, c := range f.Clients {
		s.clients[c.ID] = c
	}
	for _, g := range f.Grants {
		s.grants[g.ID] = g
	}
	return s, nil
}

// saveLocked writes the store out, replacing the file whole.
func (s *connectorStore) saveLocked() error {
	if s.path == "" {
		return nil
	}
	var f connectorFile
	for _, c := range s.clients {
		f.Clients = append(f.Clients, c)
	}
	for _, g := range s.grants {
		f.Grants = append(f.Grants, g)
	}
	slices.SortFunc(f.Clients, func(a, b *connectorClient) int { return a.CreatedAt.Compare(b.CreatedAt) })
	slices.SortFunc(f.Grants, func(a, b *connectorGrant) int { return a.CreatedAt.Compare(b.CreatedAt) })
	raw, err := json.Marshal(f)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".connectors-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), s.path)
}

func randomToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func hashToken(t string) string {
	h := sha256.Sum256([]byte(t))
	return hex.EncodeToString(h[:])
}

func sameHash(a, b string) bool { return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1 }

// ---- clients ----

func (s *connectorStore) addClient(name string, redirects []string) (*connectorClient, error) {
	id, err := randomToken(16)
	if err != nil {
		return nil, err
	}
	c := &connectorClient{ID: id, Name: name, RedirectURIs: redirects, CreatedAt: time.Now().UTC()}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.clients) >= maxConnectorClients {
		used := map[string]bool{}
		for _, g := range s.grants {
			used[g.ClientID] = true
		}
		var oldest *connectorClient
		for _, o := range s.clients {
			if !used[o.ID] && (oldest == nil || o.CreatedAt.Before(oldest.CreatedAt)) {
				oldest = o
			}
		}
		if oldest == nil {
			return nil, errors.New("too many apps are registered")
		}
		delete(s.clients, oldest.ID)
	}
	s.clients[id] = c
	if err := s.saveLocked(); err != nil {
		delete(s.clients, id)
		return nil, err
	}
	return c, nil
}

func (s *connectorStore) client(id string) (connectorClient, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.clients[id]
	if !ok {
		return connectorClient{}, false
	}
	return *c, true
}

// ---- grants ----

// newGrant records an account's approval of an app, and returns the app's
// first refresh token.
func (s *connectorStore) newGrant(clientID, did, sessionID string) (*connectorGrant, string, error) {
	id, err := randomToken(16)
	if err != nil {
		return nil, "", err
	}
	refresh, err := randomToken(32)
	if err != nil {
		return nil, "", err
	}
	now := time.Now().UTC()
	g := &connectorGrant{ID: id, DID: did, SessionID: sessionID, ClientID: clientID, CreatedAt: now, LastUsed: now, RefreshHash: hashToken(refresh)}
	s.mu.Lock()
	defer s.mu.Unlock()
	// Past the cap, the account's oldest grants make way.
	var mine []*connectorGrant
	for _, o := range s.grants {
		if o.DID == did {
			mine = append(mine, o)
		}
	}
	if len(mine) >= maxGrantsPerUser {
		slices.SortFunc(mine, func(a, b *connectorGrant) int { return a.LastUsed.Compare(b.LastUsed) })
		for _, o := range mine[:len(mine)-maxGrantsPerUser+1] {
			delete(s.grants, o.ID)
		}
	}
	s.grants[id] = g
	if err := s.saveLocked(); err != nil {
		delete(s.grants, id)
		return nil, "", err
	}
	cp := *g
	return &cp, refresh, nil
}

func (s *connectorStore) grant(id string) (connectorGrant, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	g, ok := s.grants[id]
	if !ok {
		return connectorGrant{}, false
	}
	return *g, true
}

// touch notes that a grant was used. It isn't saved: a restart forgetting
// when an app was last seen costs nothing.
func (s *connectorStore) touch(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if g, ok := s.grants[id]; ok {
		g.LastUsed = time.Now().UTC()
	}
}

// rotate trades a refresh token for the grant's next one. A token that was
// already traded ends the grant.
func (s *connectorStore) rotate(refresh, clientID string) (*connectorGrant, string, error) {
	h := hashToken(refresh)
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, g := range s.grants {
		if g.PrevRefreshHash != "" && sameHash(g.PrevRefreshHash, h) {
			delete(s.grants, id)
			_ = s.saveLocked()
			return nil, "", errInvalidGrant
		}
		if !sameHash(g.RefreshHash, h) {
			continue
		}
		if g.ClientID != clientID {
			return nil, "", errInvalidGrant
		}
		next, err := randomToken(32)
		if err != nil {
			return nil, "", err
		}
		prev, prevPrev := g.RefreshHash, g.PrevRefreshHash
		g.PrevRefreshHash, g.RefreshHash = prev, hashToken(next)
		g.LastUsed = time.Now().UTC()
		if err := s.saveLocked(); err != nil {
			g.RefreshHash, g.PrevRefreshHash = prev, prevPrev
			return nil, "", err
		}
		cp := *g
		return &cp, next, nil
	}
	return nil, "", errInvalidGrant
}

// grantsOf lists an account's grants, newest first.
func (s *connectorStore) grantsOf(did string) []connectorGrant {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []connectorGrant
	for _, g := range s.grants {
		if g.DID == did {
			out = append(out, *g)
		}
	}
	slices.SortFunc(out, func(a, b connectorGrant) int { return b.CreatedAt.Compare(a.CreatedAt) })
	return out
}

// revoke ends an account's grant. It reports false when the account has
// no such grant.
func (s *connectorStore) revoke(id, did string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	g, ok := s.grants[id]
	if !ok || g.DID != did {
		return false
	}
	delete(s.grants, id)
	if err := s.saveLocked(); err != nil {
		s.grants[id] = g
		return false
	}
	return true
}

// drop ends a grant whose web app session is gone.
func (s *connectorStore) drop(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.grants[id]; ok {
		delete(s.grants, id)
		_ = s.saveLocked()
	}
}
