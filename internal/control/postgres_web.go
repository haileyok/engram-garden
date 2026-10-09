package control

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

func (p *Postgres) GetWebSession(ctx context.Context, did, id string) (*Session, error) {
	s := Session{DID: did, ID: id}
	err := p.pool.QueryRow(ctx,
		`SELECT data, updated_at FROM web_sessions WHERE did = $1 AND session_id = $2`, did, id).
		Scan(&s.Data, &s.Updated)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("session %s %s: %w", did, id, ErrNotFound)
	}
	if err != nil {
		return nil, err
	}
	s.Updated = s.Updated.UTC()
	return &s, nil
}

func (p *Postgres) PutWebSession(ctx context.Context, s Session) error {
	_, err := p.pool.Exec(ctx,
		`INSERT INTO web_sessions (did, session_id, data, updated_at) VALUES ($1, $2, $3, $4)
		 ON CONFLICT (did, session_id) DO UPDATE SET data = EXCLUDED.data, updated_at = EXCLUDED.updated_at`,
		s.DID, s.ID, s.Data, s.Updated)
	return err
}

func (p *Postgres) ImportWebSession(ctx context.Context, s Session) (bool, error) {
	tag, err := p.pool.Exec(ctx,
		`INSERT INTO web_sessions (did, session_id, data, updated_at) VALUES ($1, $2, $3, $4)
		 ON CONFLICT (did, session_id) DO NOTHING`,
		s.DID, s.ID, s.Data, s.Updated)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

func (p *Postgres) DeleteWebSession(ctx context.Context, did, id string) error {
	_, err := p.pool.Exec(ctx, `DELETE FROM web_sessions WHERE did = $1 AND session_id = $2`, did, id)
	return err
}

func (p *Postgres) GetWebRequest(ctx context.Context, state string) (*Request, error) {
	r := Request{State: state}
	err := p.pool.QueryRow(ctx,
		`SELECT data, created_at FROM web_requests WHERE state_hash = $1`, stateHash(state)).
		Scan(&r.Data, &r.Created)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("sign-in: %w", ErrNotFound)
	}
	if err != nil {
		return nil, err
	}
	r.Created = r.Created.UTC()
	return &r, nil
}

func (p *Postgres) PutWebRequest(ctx context.Context, r Request) error {
	_, err := p.pool.Exec(ctx,
		`INSERT INTO web_requests (state_hash, data, created_at) VALUES ($1, $2, $3)
		 ON CONFLICT (state_hash) DO UPDATE SET data = EXCLUDED.data, created_at = EXCLUDED.created_at`,
		stateHash(r.State), r.Data, r.Created)
	return err
}

func (p *Postgres) DeleteWebRequest(ctx context.Context, state string) error {
	_, err := p.pool.Exec(ctx, `DELETE FROM web_requests WHERE state_hash = $1`, stateHash(state))
	return err
}

func (p *Postgres) DeleteStaleWeb(ctx context.Context, requestsBefore, sessionsBefore time.Time) (int, error) {
	var n int64
	err := p.pool.QueryRow(ctx,
		`WITH r AS (DELETE FROM web_requests WHERE created_at < $1 RETURNING 1),
		      s AS (DELETE FROM web_sessions WHERE updated_at < $2 RETURNING 1)
		 SELECT (SELECT count(*) FROM r) + (SELECT count(*) FROM s)`, requestsBefore, sessionsBefore).Scan(&n)
	return int(n), err
}
