package control

import (
	"context"
	"fmt"
	"slices"
	"time"
)

func (m *Memory) PutMCPClient(ctx context.Context, c MCPClient, max int) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.mcpClients[c.ID]; !ok && len(m.mcpClients) >= max {
		used := map[string]bool{}
		for _, g := range m.mcpGrants {
			used[g.ClientID] = true
		}
		var oldest *MCPClient
		for _, o := range m.mcpClients {
			if !used[o.ID] && (oldest == nil || o.Created.Before(oldest.Created)) {
				oldest = &o
			}
		}
		if oldest == nil {
			return false, nil
		}
		delete(m.mcpClients, oldest.ID)
	}
	c.RedirectURIs = slices.Clone(c.RedirectURIs)
	m.mcpClients[c.ID] = c
	return true, nil
}

func (m *Memory) GetMCPClient(ctx context.Context, id string) (*MCPClient, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.mcpClients[id]
	if !ok {
		return nil, nil
	}
	c.RedirectURIs = slices.Clone(c.RedirectURIs)
	return &c, nil
}

func (m *Memory) PutMCPGrant(ctx context.Context, g MCPGrant, maxPerDID int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, o := range m.mcpGrants {
		if o.ID != g.ID && o.RefreshHash == g.RefreshHash {
			return fmt.Errorf("a grant with that refresh token exists")
		}
	}
	m.mcpGrants[g.ID] = g
	var mine []MCPGrant
	for _, o := range m.mcpGrants {
		if o.DID == g.DID {
			mine = append(mine, o)
		}
	}
	if len(mine) > maxPerDID {
		slices.SortFunc(mine, func(a, b MCPGrant) int { return a.LastUsed.Compare(b.LastUsed) })
		for _, o := range mine[:len(mine)-maxPerDID] {
			delete(m.mcpGrants, o.ID)
		}
	}
	return nil
}

func (m *Memory) GetMCPGrant(ctx context.Context, id string) (*MCPGrant, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	g, ok := m.mcpGrants[id]
	if !ok {
		return nil, nil
	}
	return &g, nil
}

func (m *Memory) ListMCPGrants(ctx context.Context, did string) ([]MCPGrant, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []MCPGrant
	for _, g := range m.mcpGrants {
		if g.DID == did {
			out = append(out, g)
		}
	}
	slices.SortFunc(out, func(a, b MCPGrant) int { return b.Created.Compare(a.Created) })
	return out, nil
}

func (m *Memory) TouchMCPGrant(ctx context.Context, id string, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if g, ok := m.mcpGrants[id]; ok {
		g.LastUsed = at
		m.mcpGrants[id] = g
	}
	return nil
}

func (m *Memory) DeleteMCPGrant(ctx context.Context, id, did string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	g, ok := m.mcpGrants[id]
	if !ok || g.DID != did {
		return false, nil
	}
	delete(m.mcpGrants, id)
	return true, nil
}

func (m *Memory) RotateMCPRefresh(ctx context.Context, oldHash, clientID, newHash string, at time.Time) (*MCPGrant, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, g := range m.mcpGrants {
		if g.RefreshHash == oldHash && g.ClientID == clientID {
			g.PrevRefreshHash, g.RefreshHash, g.LastUsed = oldHash, newHash, at
			m.mcpGrants[id] = g
			return &g, nil
		}
	}
	for id, g := range m.mcpGrants {
		if g.PrevRefreshHash != "" && g.PrevRefreshHash == oldHash {
			delete(m.mcpGrants, id)
		}
	}
	return nil, fmt.Errorf("refresh token: %w", ErrNotFound)
}

func (m *Memory) PutMCPCode(ctx context.Context, c MCPCode, now time.Time, max int) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for h, o := range m.mcpCodes {
		if !o.Expires.After(now) {
			delete(m.mcpCodes, h)
		}
	}
	if _, ok := m.mcpCodes[c.Hash]; !ok && len(m.mcpCodes) >= max {
		return false, nil
	}
	m.mcpCodes[c.Hash] = c
	return true, nil
}

func (m *Memory) TakeMCPCode(ctx context.Context, hash string, now time.Time) (*MCPCode, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.mcpCodes[hash]
	if !ok {
		return nil, nil
	}
	delete(m.mcpCodes, hash)
	if !c.Expires.After(now) {
		return nil, nil
	}
	return &c, nil
}
