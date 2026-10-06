// Package store keeps indexed memories and sync state in Postgres, with
// pgvector for similarity search.
package store

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pgvector/pgvector-go"
	pgxvec "github.com/pgvector/pgvector-go/pgx"
)

// ErrNotFound reports a missing memory.
var ErrNotFound = errors.New("not found")

// Memory is one indexed garden.engram.memory record.
type Memory struct {
	URI       string
	Space     string
	Author    string
	Rkey      string
	CID       string
	Text      string
	Tags      []string
	Source    string
	CreatedAt time.Time
	IndexedAt time.Time
	// Model and Embedding are set on writes.
	Model     string
	Embedding []float32
	// Similarity is set on search results: cosine similarity to the query.
	Similarity float64
}

// RepoState is how far the index has synced one member's repo in a space.
type RepoState struct {
	Space string
	DID   string
	// Rev is the last oplog rev applied; listRepoOps resumes after it.
	Rev string
	// SetHash is the LtHash state over every record applied so far. It is
	// checked against the repo's signed commit.
	SetHash []byte
	// SpaceRev is the authority's rev for the repo's latest known write.
	SpaceRev string
}

// Store is the Postgres-backed index.
type Store struct {
	pool *pgxpool.Pool
	dims int
}

// Open connects and migrates. dims fixes the vector column's size; opening an
// existing database with a different size fails rather than mixing vectors.
func Open(ctx context.Context, dsn string, dims int) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	return OpenConfig(ctx, cfg, dims)
}

// OpenConfig is Open with a parsed pool config.
func OpenConfig(ctx context.Context, cfg *pgxpool.Config, dims int) (*Store, error) {
	if dims <= 0 || dims > 16000 {
		return nil, fmt.Errorf("invalid embedding dimensions %d", dims)
	}
	// The vector type must exist before connections register it.
	if err := ensureExtension(ctx, cfg.ConnConfig); err != nil {
		return nil, err
	}
	cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		return pgxvec.RegisterTypes(ctx, conn)
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	s := &Store{pool: pool, dims: dims}
	if err := s.migrate(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return s, nil
}

func ensureExtension(ctx context.Context, cc *pgx.ConnConfig) error {
	conn, err := pgx.ConnectConfig(ctx, cc.Copy())
	if err != nil {
		return err
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, `CREATE EXTENSION IF NOT EXISTS vector`); err != nil {
		return fmt.Errorf("enabling pgvector (is it installed?): %w", err)
	}
	return nil
}

func (s *Store) Close() { s.pool.Close() }

// Dimensions is the configured vector size.
func (s *Store) Dimensions() int { return s.dims }

func (s *Store) migrate(ctx context.Context) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	// Serialize concurrent starts.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(727172)`); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `CREATE TABLE IF NOT EXISTS engram_meta (key text PRIMARY KEY, value text NOT NULL)`); err != nil {
		return err
	}
	var dimsStr string
	err = tx.QueryRow(ctx, `SELECT value FROM engram_meta WHERE key = 'dimensions'`).Scan(&dimsStr)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		if _, err := tx.Exec(ctx, `INSERT INTO engram_meta (key, value) VALUES ('dimensions', $1)`, strconv.Itoa(s.dims)); err != nil {
			return err
		}
	case err != nil:
		return err
	case dimsStr != strconv.Itoa(s.dims):
		return fmt.Errorf("database holds %s-dimension vectors but the embedder is configured for %d; use a fresh database or re-index", dimsStr, s.dims)
	}
	stmts := []string{
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS memories (
			uri text PRIMARY KEY,
			space text NOT NULL,
			author text NOT NULL,
			rkey text NOT NULL,
			cid text NOT NULL,
			text text NOT NULL,
			tags text[] NOT NULL DEFAULT '{}',
			source text NOT NULL DEFAULT '',
			created_at timestamptz NOT NULL,
			indexed_at timestamptz NOT NULL DEFAULT now(),
			embedding_model text NOT NULL,
			embedding vector(%d) NOT NULL
		)`, s.dims),
		`CREATE INDEX IF NOT EXISTS memories_space_created ON memories (space, created_at DESC, uri DESC)`,
		`CREATE INDEX IF NOT EXISTS memories_space_author ON memories (space, author)`,
		`CREATE INDEX IF NOT EXISTS memories_tags ON memories USING gin (tags)`,
		`CREATE INDEX IF NOT EXISTS memories_embedding ON memories USING hnsw (embedding vector_cosine_ops)`,
		`CREATE TABLE IF NOT EXISTS repos (
			space text NOT NULL,
			did text NOT NULL,
			rev text NOT NULL DEFAULT '',
			set_hash bytea,
			space_rev text NOT NULL DEFAULT '',
			updated_at timestamptz NOT NULL DEFAULT now(),
			PRIMARY KEY (space, did)
		)`,
		`CREATE TABLE IF NOT EXISTS spaces (
			space text PRIMARY KEY,
			deleted_at timestamptz
		)`,
	}
	for _, q := range stmts {
		if _, err := tx.Exec(ctx, q); err != nil {
			return fmt.Errorf("migrate: %w", err)
		}
	}
	return tx.Commit(ctx)
}

const memoryCols = `uri, space, author, rkey, cid, text, tags, source, created_at, indexed_at`

func scanMemory(row pgx.Row, extra ...any) (Memory, error) {
	var m Memory
	dest := append([]any{&m.URI, &m.Space, &m.Author, &m.Rkey, &m.CID, &m.Text, &m.Tags, &m.Source, &m.CreatedAt, &m.IndexedAt}, extra...)
	err := row.Scan(dest...)
	return m, err
}

// RepoState returns a repo's sync state, or nil if it has never synced.
func (s *Store) RepoState(ctx context.Context, space, did string) (*RepoState, error) {
	st := RepoState{Space: space, DID: did}
	err := s.pool.QueryRow(ctx, `SELECT rev, set_hash, space_rev FROM repos WHERE space = $1 AND did = $2`, space, did).
		Scan(&st.Rev, &st.SetHash, &st.SpaceRev)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &st, nil
}

// ApplyRepoChanges upserts and deletes one repo's memories and advances its
// sync state, atomically. With replace set, every other memory the author has
// in the space is dropped first, as after a full resync.
func (s *Store) ApplyRepoChanges(ctx context.Context, st RepoState, upserts []Memory, deletes []string, replace bool) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if replace {
		if _, err := tx.Exec(ctx, `DELETE FROM memories WHERE space = $1 AND author = $2`, st.Space, st.DID); err != nil {
			return err
		}
	}
	for _, uri := range deletes {
		if _, err := tx.Exec(ctx, `DELETE FROM memories WHERE uri = $1 AND space = $2 AND author = $3`, uri, st.Space, st.DID); err != nil {
			return err
		}
	}
	for _, m := range upserts {
		if m.Space != st.Space || m.Author != st.DID {
			return fmt.Errorf("memory %s is not in repo %s/%s", m.URI, st.Space, st.DID)
		}
		if len(m.Embedding) != s.dims {
			return fmt.Errorf("memory %s: embedding has %d dimensions, want %d", m.URI, len(m.Embedding), s.dims)
		}
		tags := m.Tags
		if tags == nil {
			tags = []string{}
		}
		_, err := tx.Exec(ctx, `INSERT INTO memories (uri, space, author, rkey, cid, text, tags, source, created_at, indexed_at, embedding_model, embedding)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, now(), $10, $11)
			ON CONFLICT (uri) DO UPDATE SET cid = EXCLUDED.cid, text = EXCLUDED.text, tags = EXCLUDED.tags,
				source = EXCLUDED.source, created_at = EXCLUDED.created_at, indexed_at = now(),
				embedding_model = EXCLUDED.embedding_model, embedding = EXCLUDED.embedding`,
			m.URI, m.Space, m.Author, m.Rkey, m.CID, m.Text, tags, m.Source, m.CreatedAt, m.Model, pgvector.NewVector(m.Embedding))
		if err != nil {
			return fmt.Errorf("upsert %s: %w", m.URI, err)
		}
	}
	_, err = tx.Exec(ctx, `INSERT INTO repos (space, did, rev, set_hash, space_rev, updated_at) VALUES ($1, $2, $3, $4, $5, now())
		ON CONFLICT (space, did) DO UPDATE SET rev = EXCLUDED.rev, set_hash = EXCLUDED.set_hash,
			space_rev = GREATEST(repos.space_rev, EXCLUDED.space_rev), updated_at = now()`,
		st.Space, st.DID, st.Rev, st.SetHash, st.SpaceRev)
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// RemoveRepo drops a member's memories and sync state, as when they leave.
func (s *Store) RemoveRepo(ctx context.Context, space, did string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, `DELETE FROM memories WHERE space = $1 AND author = $2`, space, did); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM repos WHERE space = $1 AND did = $2`, space, did); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Repos lists the repos synced for a space.
func (s *Store) Repos(ctx context.Context, space string) ([]RepoState, error) {
	rows, err := s.pool.Query(ctx, `SELECT did, rev, set_hash, space_rev FROM repos WHERE space = $1 ORDER BY did`, space)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RepoState
	for rows.Next() {
		st := RepoState{Space: space}
		if err := rows.Scan(&st.DID, &st.Rev, &st.SetHash, &st.SpaceRev); err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

// MarkSpaceDeleted drops a space's memories and sync state and remembers that
// it is gone.
func (s *Store) MarkSpaceDeleted(ctx context.Context, space string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	for _, q := range []string{`DELETE FROM memories WHERE space = $1`, `DELETE FROM repos WHERE space = $1`} {
		if _, err := tx.Exec(ctx, q, space); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `INSERT INTO spaces (space, deleted_at) VALUES ($1, now()) ON CONFLICT (space) DO UPDATE SET deleted_at = now()`, space); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// SpaceDeleted reports whether a space was marked deleted.
func (s *Store) SpaceDeleted(ctx context.Context, space string) (bool, error) {
	var deleted *time.Time
	err := s.pool.QueryRow(ctx, `SELECT deleted_at FROM spaces WHERE space = $1`, space).Scan(&deleted)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return deleted != nil, err
}

// Get returns one memory.
func (s *Store) Get(ctx context.Context, space, uri string) (Memory, error) {
	m, err := scanMemory(s.pool.QueryRow(ctx, `SELECT `+memoryCols+` FROM memories WHERE space = $1 AND uri = $2`, space, uri))
	if errors.Is(err, pgx.ErrNoRows) {
		return Memory{}, ErrNotFound
	}
	return m, err
}

// Filter narrows searches and listings.
type Filter struct {
	Author string
	// Tags must all be present.
	Tags  []string
	Since time.Time
}

func (f Filter) where(args *[]any, space string) string {
	*args = append(*args, space)
	conds := []string{fmt.Sprintf("space = $%d", len(*args))}
	if f.Author != "" {
		*args = append(*args, f.Author)
		conds = append(conds, fmt.Sprintf("author = $%d", len(*args)))
	}
	if len(f.Tags) > 0 {
		*args = append(*args, f.Tags)
		conds = append(conds, fmt.Sprintf("tags @> $%d", len(*args)))
	}
	if !f.Since.IsZero() {
		*args = append(*args, f.Since)
		conds = append(conds, fmt.Sprintf("created_at >= $%d", len(*args)))
	}
	return strings.Join(conds, " AND ")
}

// Search returns the memories nearest a query vector, most similar first.
func (s *Store) Search(ctx context.Context, space string, query []float32, limit int, f Filter) ([]Memory, error) {
	if len(query) != s.dims {
		return nil, fmt.Errorf("query has %d dimensions, want %d", len(query), s.dims)
	}
	args := []any{pgvector.NewVector(query)}
	where := f.where(&args, space)
	args = append(args, limit)
	q := fmt.Sprintf(`SELECT %s, 1 - (embedding <=> $1) AS similarity FROM memories WHERE %s ORDER BY embedding <=> $1 LIMIT $%d`, memoryCols, where, len(args))

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	// Filters are applied after the HNSW scan; widen it so filtered searches
	// still fill the limit.
	if _, err := tx.Exec(ctx, `SET LOCAL hnsw.ef_search = 200`); err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Memory
	for rows.Next() {
		var sim float64
		m, err := scanMemory(rows, &sim)
		if err != nil {
			return nil, err
		}
		m.Similarity = sim
		out = append(out, m)
	}
	return out, rows.Err()
}

// List returns memories newest first, with a cursor for the next page.
func (s *Store) List(ctx context.Context, space string, limit int, cursor string, f Filter) ([]Memory, string, error) {
	var args []any
	where := f.where(&args, space)
	if cursor != "" {
		t, uri, err := decodeCursor(cursor)
		if err != nil {
			return nil, "", err
		}
		args = append(args, t, uri)
		where += fmt.Sprintf(" AND (created_at, uri) < ($%d, $%d)", len(args)-1, len(args))
	}
	args = append(args, limit)
	rows, err := s.pool.Query(ctx, fmt.Sprintf(`SELECT %s FROM memories WHERE %s ORDER BY created_at DESC, uri DESC LIMIT $%d`, memoryCols, where, len(args)), args...)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	var out []Memory
	for rows.Next() {
		m, err := scanMemory(rows)
		if err != nil {
			return nil, "", err
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	next := ""
	if len(out) == limit {
		last := out[len(out)-1]
		next = encodeCursor(last.CreatedAt, last.URI)
	}
	return out, next, nil
}

// ErrBadCursor reports an unparseable list cursor.
var ErrBadCursor = errors.New("bad cursor")

func encodeCursor(t time.Time, uri string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(t.UnixMicro(), 10) + " " + uri))
}

func decodeCursor(c string) (time.Time, string, error) {
	raw, err := base64.RawURLEncoding.DecodeString(c)
	if err != nil {
		return time.Time{}, "", ErrBadCursor
	}
	ts, uri, ok := strings.Cut(string(raw), " ")
	if !ok {
		return time.Time{}, "", ErrBadCursor
	}
	micros, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return time.Time{}, "", ErrBadCursor
	}
	return time.UnixMicro(micros), uri, nil
}
