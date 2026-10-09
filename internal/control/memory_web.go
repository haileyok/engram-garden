package control

import (
	"context"
	"fmt"
	"time"
)

func (m *Memory) GetWebSession(ctx context.Context, did, id string) (*Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.webSessions[[2]string{did, id}]
	if !ok {
		return nil, fmt.Errorf("session %s %s: %w", did, id, ErrNotFound)
	}
	return &s, nil
}

func (m *Memory) PutWebSession(ctx context.Context, s Session) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.webSessions[[2]string{s.DID, s.ID}] = s
	return nil
}

func (m *Memory) ImportWebSession(ctx context.Context, s Session) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := [2]string{s.DID, s.ID}
	if _, ok := m.webSessions[key]; ok {
		return false, nil
	}
	m.webSessions[key] = s
	return true, nil
}

func (m *Memory) DeleteWebSession(ctx context.Context, did, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.webSessions, [2]string{did, id})
	return nil
}

func (m *Memory) GetWebRequest(ctx context.Context, state string) (*Request, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.webRequests[state]
	if !ok {
		return nil, fmt.Errorf("sign-in: %w", ErrNotFound)
	}
	return &r, nil
}

func (m *Memory) PutWebRequest(ctx context.Context, r Request) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.webRequests[r.State] = r
	return nil
}

func (m *Memory) DeleteWebRequest(ctx context.Context, state string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.webRequests, state)
	return nil
}

func (m *Memory) DeleteStaleWeb(ctx context.Context, requestsBefore, sessionsBefore time.Time) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for k, r := range m.webRequests {
		if r.Created.Before(requestsBefore) {
			delete(m.webRequests, k)
			n++
		}
	}
	for k, s := range m.webSessions {
		if s.Updated.Before(sessionsBefore) {
			delete(m.webSessions, k)
			n++
		}
	}
	return n, nil
}
