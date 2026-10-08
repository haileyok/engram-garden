package control

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

// migrationLock is the advisory lock nodes take while migrating, so several
// starting at once don't race to create the same tables.
const migrationLock int64 = 0x656e6772616d // "engram"

// setupTimeout bounds connecting and migrating at startup, so a database
// that accepts connections and never answers fails the start instead of
// hanging it.
const setupTimeout = time.Minute

// Postgres is a Store in a Postgres database. Every node of an appview
// points at the same one.
type Postgres struct {
	pool *pgxpool.Pool
}

var _ Store = (*Postgres)(nil)

// OpenPostgres connects to the database at url (a postgres:// URL) and
// brings its tables up to date.
func OpenPostgres(ctx context.Context, url string) (*Postgres, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("database URL: %w", err)
	}
	return openPostgres(ctx, cfg)
}

func openPostgres(ctx context.Context, cfg *pgxpool.Config) (*Postgres, error) {
	setup, cancel := context.WithTimeout(ctx, setupTimeout)
	defer cancel()
	pool, err := pgxpool.NewWithConfig(setup, cfg)
	if err != nil {
		return nil, fmt.Errorf("connecting to the database: %w", err)
	}
	if err := pool.Ping(setup); err != nil {
		pool.Close()
		return nil, fmt.Errorf("connecting to the database: %w", err)
	}
	if err := migrate(setup, pool); err != nil {
		pool.Close()
		return nil, fmt.Errorf("migrating the database: %w", err)
	}
	return &Postgres{pool: pool}, nil
}

// Close releases the connections.
func (p *Postgres) Close() { p.pool.Close() }

type migration struct {
	version int
	name    string
}

func migrations() ([]migration, error) {
	entries, err := fs.ReadDir(migrationFiles, "migrations")
	if err != nil {
		return nil, err
	}
	var out []migration
	for _, e := range entries {
		num, _, ok := strings.Cut(e.Name(), "_")
		v, err := strconv.Atoi(num)
		if !ok || err != nil || !strings.HasSuffix(e.Name(), ".sql") {
			return nil, fmt.Errorf("migration %q isn't named <number>_<name>.sql", e.Name())
		}
		out = append(out, migration{version: v, name: e.Name()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	for i := 1; i < len(out); i++ {
		if out[i].version == out[i-1].version {
			return nil, fmt.Errorf("migrations %s and %s have the same number", out[i-1].name, out[i].name)
		}
	}
	return out, nil
}

// migrate applies the migrations the database hasn't seen, all in one
// transaction: either every pending one is applied or none is. The advisory
// lock is the transaction's own, so it is released when the transaction
// ends, however it ends, and can't be left held by a pooled connection that
// other nodes would then wait on forever.
//
// A node that waited for the lock reads which migrations are applied only
// after getting it, so it sees what the node ahead of it did.
func migrate(ctx context.Context, pool *pgxpool.Pool) error {
	todo, err := migrations()
	if err != nil {
		return err
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	// A no-op once committed.
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, migrationLock); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version    integer PRIMARY KEY,
		applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return err
	}
	rows, err := tx.Query(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return err
	}
	applied := map[int]bool{}
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			rows.Close()
			return err
		}
		applied[v] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, m := range todo {
		if applied[m.version] {
			continue
		}
		sql, err := migrationFiles.ReadFile("migrations/" + m.name)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, string(sql)); err != nil {
			return fmt.Errorf("%s: %w", m.name, err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations (version) VALUES ($1)`, m.version); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// stateHash is how sign-ins are keyed: the OAuth state is a bearer secret
// for finishing a sign-in, so it isn't stored as is.
func stateHash(state string) string {
	h := sha256.Sum256([]byte(state))
	return hex.EncodeToString(h[:])
}

// ---- grants ----

func (p *Postgres) GetGrant(ctx context.Context, space string) (*Grant, error) {
	var g Grant
	err := p.pool.QueryRow(ctx,
		`SELECT space, did, session_id, granted_at FROM grants WHERE space = $1`, space).
		Scan(&g.Space, &g.DID, &g.SessionID, &g.GrantedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	g.GrantedAt = g.GrantedAt.UTC()
	return &g, nil
}

func (p *Postgres) PutGrant(ctx context.Context, g Grant) error {
	_, err := p.pool.Exec(ctx,
		`INSERT INTO grants (space, did, session_id, granted_at) VALUES ($1, $2, $3, $4)
		 ON CONFLICT (space) DO UPDATE
		 SET did = EXCLUDED.did, session_id = EXCLUDED.session_id, granted_at = EXCLUDED.granted_at`,
		g.Space, g.DID, g.SessionID, g.GrantedAt)
	return err
}

func (p *Postgres) DeleteGrant(ctx context.Context, space string) error {
	_, err := p.pool.Exec(ctx, `DELETE FROM grants WHERE space = $1`, space)
	return err
}

func (p *Postgres) ListGrants(ctx context.Context) ([]Grant, error) {
	rows, err := p.pool.Query(ctx, `SELECT space, did, session_id, granted_at FROM grants ORDER BY space`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Grant
	for rows.Next() {
		var g Grant
		if err := rows.Scan(&g.Space, &g.DID, &g.SessionID, &g.GrantedAt); err != nil {
			return nil, err
		}
		g.GrantedAt = g.GrantedAt.UTC()
		out = append(out, g)
	}
	return out, rows.Err()
}

// ---- sessions ----

func (p *Postgres) GetSession(ctx context.Context, did, id string) (*Session, error) {
	s := Session{DID: did, ID: id}
	err := p.pool.QueryRow(ctx,
		`SELECT data, updated_at FROM oauth_sessions WHERE did = $1 AND session_id = $2`, did, id).
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

func (p *Postgres) PutSession(ctx context.Context, s Session) error {
	_, err := p.pool.Exec(ctx,
		`INSERT INTO oauth_sessions (did, session_id, data, updated_at) VALUES ($1, $2, $3, $4)
		 ON CONFLICT (did, session_id) DO UPDATE SET data = EXCLUDED.data, updated_at = EXCLUDED.updated_at`,
		s.DID, s.ID, s.Data, s.Updated)
	return err
}

func (p *Postgres) DeleteSession(ctx context.Context, did, id string) error {
	_, err := p.pool.Exec(ctx, `DELETE FROM oauth_sessions WHERE did = $1 AND session_id = $2`, did, id)
	return err
}

func (p *Postgres) ListSessions(ctx context.Context) ([]Session, error) {
	rows, err := p.pool.Query(ctx, `SELECT did, session_id, data, updated_at FROM oauth_sessions ORDER BY did, session_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Session
	for rows.Next() {
		var s Session
		if err := rows.Scan(&s.DID, &s.ID, &s.Data, &s.Updated); err != nil {
			return nil, err
		}
		s.Updated = s.Updated.UTC()
		out = append(out, s)
	}
	return out, rows.Err()
}

// ---- sign-ins in progress ----

func (p *Postgres) GetRequest(ctx context.Context, state string) (*Request, error) {
	r := Request{State: state}
	err := p.pool.QueryRow(ctx,
		`SELECT data, created_at FROM oauth_requests WHERE state_hash = $1`, stateHash(state)).
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

func (p *Postgres) PutRequest(ctx context.Context, r Request) error {
	_, err := p.pool.Exec(ctx,
		`INSERT INTO oauth_requests (state_hash, data, created_at) VALUES ($1, $2, $3)
		 ON CONFLICT (state_hash) DO UPDATE SET data = EXCLUDED.data, created_at = EXCLUDED.created_at`,
		stateHash(r.State), r.Data, r.Created)
	return err
}

func (p *Postgres) DeleteRequest(ctx context.Context, state string) error {
	_, err := p.pool.Exec(ctx, `DELETE FROM oauth_requests WHERE state_hash = $1`, stateHash(state))
	return err
}

func (p *Postgres) PutPending(ctx context.Context, pe Pending) error {
	_, err := p.pool.Exec(ctx,
		`INSERT INTO oauth_pending (state_hash, space, mode, return_url, created_at) VALUES ($1, $2, $3, $4, $5)
		 ON CONFLICT (state_hash) DO UPDATE
		 SET space = EXCLUDED.space, mode = EXCLUDED.mode, return_url = EXCLUDED.return_url, created_at = EXCLUDED.created_at`,
		stateHash(pe.State), pe.Space, pe.Mode, pe.Return, pe.Created)
	return err
}

// TakePending deletes and returns the row in one statement, so of several
// callbacks for the same state, on any node, only one gets it.
func (p *Postgres) TakePending(ctx context.Context, state string) (*Pending, error) {
	pe := Pending{State: state}
	err := p.pool.QueryRow(ctx,
		`DELETE FROM oauth_pending WHERE state_hash = $1 RETURNING space, mode, return_url, created_at`,
		stateHash(state)).Scan(&pe.Space, &pe.Mode, &pe.Return, &pe.Created)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	pe.Created = pe.Created.UTC()
	return &pe, nil
}

func (p *Postgres) DeleteStale(ctx context.Context, before time.Time) (int, error) {
	var n int64
	err := p.pool.QueryRow(ctx,
		`WITH r AS (DELETE FROM oauth_requests WHERE created_at < $1 RETURNING 1),
		      p AS (DELETE FROM oauth_pending WHERE created_at < $1 RETURNING 1)
		 SELECT (SELECT count(*) FROM r) + (SELECT count(*) FROM p)`, before).Scan(&n)
	return int(n), err
}

// ---- registered spaces ----

func (p *Postgres) Register(ctx context.Context, r Registration) (bool, error) {
	tag, err := p.pool.Exec(ctx,
		`INSERT INTO registered_spaces (space, registered_at) VALUES ($1, $2) ON CONFLICT (space) DO NOTHING`,
		r.Space, r.At)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

func (p *Postgres) Registrations(ctx context.Context) ([]Registration, error) {
	rows, err := p.pool.Query(ctx, `SELECT space, registered_at FROM registered_spaces ORDER BY space`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Registration
	for rows.Next() {
		var r Registration
		if err := rows.Scan(&r.Space, &r.At); err != nil {
			return nil, err
		}
		r.At = r.At.UTC()
		out = append(out, r)
	}
	return out, rows.Err()
}
