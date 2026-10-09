package control

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// ---- apps ----

func (p *Postgres) PutMCPClient(ctx context.Context, c MCPClient, max int) (bool, error) {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM mcp_clients WHERE id = $1)`, c.ID).Scan(&exists); err != nil {
		return false, err
	}
	if !exists {
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM mcp_clients`).Scan(&n); err != nil {
			return false, err
		}
		if n >= max {
			// Make room with the oldest app that no account approved.
			tag, err := tx.Exec(ctx,
				`DELETE FROM mcp_clients WHERE id = (
				   SELECT c.id FROM mcp_clients c
				   WHERE NOT EXISTS (SELECT 1 FROM mcp_grants g WHERE g.client_id = c.id)
				   ORDER BY c.created_at LIMIT 1)`)
			if err != nil {
				return false, err
			}
			if tag.RowsAffected() == 0 {
				return false, nil
			}
		}
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO mcp_clients (id, name, redirect_uris, created_at) VALUES ($1, $2, $3, $4)
		 ON CONFLICT (id) DO UPDATE SET name = EXCLUDED.name, redirect_uris = EXCLUDED.redirect_uris`,
		c.ID, c.Name, c.RedirectURIs, c.Created); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}

func (p *Postgres) GetMCPClient(ctx context.Context, id string) (*MCPClient, error) {
	c := MCPClient{ID: id}
	err := p.pool.QueryRow(ctx, `SELECT name, redirect_uris, created_at FROM mcp_clients WHERE id = $1`, id).
		Scan(&c.Name, &c.RedirectURIs, &c.Created)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	c.Created = c.Created.UTC()
	return &c, nil
}

// ---- approvals ----

const mcpGrantColumns = `id, did, session_id, client_id, created_at, last_used_at, refresh_hash, prev_refresh_hash`

func scanMCPGrant(row pgx.Row) (*MCPGrant, error) {
	var g MCPGrant
	if err := row.Scan(&g.ID, &g.DID, &g.SessionID, &g.ClientID, &g.Created, &g.LastUsed, &g.RefreshHash, &g.PrevRefreshHash); err != nil {
		return nil, err
	}
	g.Created, g.LastUsed = g.Created.UTC(), g.LastUsed.UTC()
	return &g, nil
}

func (p *Postgres) PutMCPGrant(ctx context.Context, g MCPGrant, maxPerDID int) error {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := tx.Exec(ctx,
		`INSERT INTO mcp_grants (`+mcpGrantColumns+`) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		 ON CONFLICT (id) DO UPDATE SET did = EXCLUDED.did, session_id = EXCLUDED.session_id, client_id = EXCLUDED.client_id,
		   created_at = EXCLUDED.created_at, last_used_at = EXCLUDED.last_used_at,
		   refresh_hash = EXCLUDED.refresh_hash, prev_refresh_hash = EXCLUDED.prev_refresh_hash`,
		g.ID, g.DID, g.SessionID, g.ClientID, g.Created, g.LastUsed, g.RefreshHash, g.PrevRefreshHash); err != nil {
		return err
	}
	// Keep the account's most recently used approvals.
	if _, err := tx.Exec(ctx,
		`DELETE FROM mcp_grants WHERE id IN (
		   SELECT id FROM mcp_grants WHERE did = $1 ORDER BY last_used_at DESC, created_at DESC OFFSET $2)`,
		g.DID, maxPerDID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (p *Postgres) GetMCPGrant(ctx context.Context, id string) (*MCPGrant, error) {
	g, err := scanMCPGrant(p.pool.QueryRow(ctx, `SELECT `+mcpGrantColumns+` FROM mcp_grants WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return g, err
}

func (p *Postgres) ListMCPGrants(ctx context.Context, did string) ([]MCPGrant, error) {
	rows, err := p.pool.Query(ctx, `SELECT `+mcpGrantColumns+` FROM mcp_grants WHERE did = $1 ORDER BY created_at DESC, id`, did)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MCPGrant
	for rows.Next() {
		g, err := scanMCPGrant(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *g)
	}
	return out, rows.Err()
}

func (p *Postgres) TouchMCPGrant(ctx context.Context, id string, at time.Time) error {
	_, err := p.pool.Exec(ctx, `UPDATE mcp_grants SET last_used_at = $2 WHERE id = $1`, id, at)
	return err
}

func (p *Postgres) DeleteMCPGrant(ctx context.Context, id, did string) (bool, error) {
	tag, err := p.pool.Exec(ctx, `DELETE FROM mcp_grants WHERE id = $1 AND did = $2`, id, did)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// RotateMCPRefresh swaps the token in one statement, so of several callers
// with the same token, on any node, one gets it: the others find the token
// already replaced, which makes it the previous one, and that ends the
// approval.
func (p *Postgres) RotateMCPRefresh(ctx context.Context, oldHash, clientID, newHash string, at time.Time) (*MCPGrant, error) {
	g, err := scanMCPGrant(p.pool.QueryRow(ctx,
		`UPDATE mcp_grants SET prev_refresh_hash = refresh_hash, refresh_hash = $3, last_used_at = $4
		 WHERE refresh_hash = $1 AND client_id = $2
		 RETURNING `+mcpGrantColumns, oldHash, clientID, newHash, at))
	if err == nil {
		return g, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	if _, err := p.pool.Exec(ctx, `DELETE FROM mcp_grants WHERE prev_refresh_hash = $1 AND $1 <> ''`, oldHash); err != nil {
		return nil, err
	}
	return nil, fmt.Errorf("refresh token: %w", ErrNotFound)
}

// ---- approvals waiting for tokens ----

func (p *Postgres) PutMCPCode(ctx context.Context, c MCPCode, now time.Time, max int) (bool, error) {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := tx.Exec(ctx, `DELETE FROM mcp_codes WHERE expires_at <= $1`, now); err != nil {
		return false, err
	}
	var n int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM mcp_codes WHERE code_hash <> $1`, c.Hash).Scan(&n); err != nil {
		return false, err
	}
	if n >= max {
		return false, nil
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO mcp_codes (code_hash, client_id, redirect_uri, challenge, did, session_id, expires_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)
		 ON CONFLICT (code_hash) DO UPDATE SET client_id = EXCLUDED.client_id, redirect_uri = EXCLUDED.redirect_uri,
		   challenge = EXCLUDED.challenge, did = EXCLUDED.did, session_id = EXCLUDED.session_id, expires_at = EXCLUDED.expires_at`,
		c.Hash, c.ClientID, c.RedirectURI, c.Challenge, c.DID, c.SessionID, c.Expires); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}

// TakeMCPCode deletes and returns the row in one statement, so of several
// callers with the same code, on any node, only one gets it.
func (p *Postgres) TakeMCPCode(ctx context.Context, hash string, now time.Time) (*MCPCode, error) {
	c := MCPCode{Hash: hash}
	err := p.pool.QueryRow(ctx,
		`DELETE FROM mcp_codes WHERE code_hash = $1 RETURNING client_id, redirect_uri, challenge, did, session_id, expires_at`, hash).
		Scan(&c.ClientID, &c.RedirectURI, &c.Challenge, &c.DID, &c.SessionID, &c.Expires)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	c.Expires = c.Expires.UTC()
	if !c.Expires.After(now) {
		return nil, nil
	}
	return &c, nil
}
