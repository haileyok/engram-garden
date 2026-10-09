package control

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"time"
)

// Memory is a Store held in memory: for tests, and for running one appview
// locally. Nothing survives a restart.
type Memory struct {
	mu       sync.Mutex
	grants   map[string]Grant
	sessions map[[2]string]Session // by DID and ID
	requests map[string]Request
	pending  map[string]Pending
	regs     map[string]Registration

	mcpClients map[string]MCPClient
	mcpGrants  map[string]MCPGrant
	mcpCodes   map[string]MCPCode
}

var _ Store = (*Memory)(nil)

// NewMemory returns an empty Memory.
func NewMemory() *Memory {
	return &Memory{
		grants:     map[string]Grant{},
		sessions:   map[[2]string]Session{},
		requests:   map[string]Request{},
		pending:    map[string]Pending{},
		regs:       map[string]Registration{},
		mcpClients: map[string]MCPClient{},
		mcpGrants:  map[string]MCPGrant{},
		mcpCodes:   map[string]MCPCode{},
	}
}

func (m *Memory) GetGrant(ctx context.Context, space string) (*Grant, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	g, ok := m.grants[space]
	if !ok {
		return nil, nil
	}
	return &g, nil
}

func (m *Memory) PutGrant(ctx context.Context, g Grant) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.grants[g.Space] = g
	return nil
}

func (m *Memory) DeleteGrant(ctx context.Context, space string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.grants, space)
	return nil
}

func (m *Memory) ListGrants(ctx context.Context) ([]Grant, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Grant, 0, len(m.grants))
	for _, g := range m.grants {
		out = append(out, g)
	}
	return out, nil
}

func (m *Memory) GetSession(ctx context.Context, did, id string) (*Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[[2]string{did, id}]
	if !ok {
		return nil, fmt.Errorf("session %s %s: %w", did, id, ErrNotFound)
	}
	s.Data = slices.Clone(s.Data)
	return &s, nil
}

func (m *Memory) PutSession(ctx context.Context, s Session) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	s.Data = slices.Clone(s.Data)
	m.sessions[[2]string{s.DID, s.ID}] = s
	return nil
}

func (m *Memory) DeleteSession(ctx context.Context, did, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.sessions, [2]string{did, id})
	return nil
}

func (m *Memory) ListSessions(ctx context.Context) ([]Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		s.Data = slices.Clone(s.Data)
		out = append(out, s)
	}
	return out, nil
}

func (m *Memory) GetRequest(ctx context.Context, state string) (*Request, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.requests[state]
	if !ok {
		return nil, fmt.Errorf("sign-in: %w", ErrNotFound)
	}
	r.Data = slices.Clone(r.Data)
	return &r, nil
}

func (m *Memory) PutRequest(ctx context.Context, r Request) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r.Data = slices.Clone(r.Data)
	m.requests[r.State] = r
	return nil
}

func (m *Memory) DeleteRequest(ctx context.Context, state string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.requests, state)
	return nil
}

func (m *Memory) PutPending(ctx context.Context, p Pending) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pending[p.State] = p
	return nil
}

func (m *Memory) TakePending(ctx context.Context, state string) (*Pending, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.pending[state]
	if !ok {
		return nil, nil
	}
	delete(m.pending, state)
	return &p, nil
}

func (m *Memory) DeleteStale(ctx context.Context, before time.Time) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for k, r := range m.requests {
		if r.Created.Before(before) {
			delete(m.requests, k)
			n++
		}
	}
	for k, p := range m.pending {
		if p.Created.Before(before) {
			delete(m.pending, k)
			n++
		}
	}
	return n, nil
}

func (m *Memory) Register(ctx context.Context, r Registration) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.regs[r.Space]; ok {
		return false, nil
	}
	m.regs[r.Space] = r
	return true, nil
}

func (m *Memory) Registrations(ctx context.Context) ([]Registration, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Registration, 0, len(m.regs))
	for _, r := range m.regs {
		out = append(out, r)
	}
	return out, nil
}
