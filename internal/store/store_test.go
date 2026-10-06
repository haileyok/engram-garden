package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/haileyok/engram-garden/internal/embed"
)

const testDims = 256

const testSpace = "at://did:plc:auth/space/garden.engram.space/memory"

// newTestStore opens a store in a fresh schema of ENGRAM_TEST_DATABASE_URL,
// skipping when it is unset.
func newTestStore(t *testing.T, dims int) *Store {
	t.Helper()
	dsn := os.Getenv("ENGRAM_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ENGRAM_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	schema := "t_" + hex.EncodeToString(b)
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	if err := EnsureExtension(ctx, conn.Config()); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = conn.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
		conn.Close(context.Background())
	})
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema + ",public"
	s, err := OpenConfig(ctx, cfg, dims)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

func mem(t *testing.T, author, rkey, text string, tags []string, created time.Time) Memory {
	t.Helper()
	v, _ := embed.Hashing{Dims: testDims}.Embed(context.Background(), []string{text})
	return Memory{
		URI:   testSpace + "/" + author + "/garden.engram.memory/" + rkey,
		Space: testSpace, Author: author, Rkey: rkey, CID: "bafy" + rkey,
		Text: text, Tags: tags, CreatedAt: created, Model: "hashing", Embedding: v[0],
	}
}

func query(t *testing.T, text string) []float32 {
	v, _ := embed.Hashing{Dims: testDims}.Embed(context.Background(), []string{text})
	return v[0]
}

func TestApplySearchListDelete(t *testing.T) {
	t.Parallel()
	s := newTestStore(t, testDims)
	ctx := context.Background()
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)

	a := RepoState{Space: testSpace, DID: "did:plc:alice", Rev: "r1", SetHash: []byte{1}, SpaceRev: "s1"}
	err := s.ApplyRepoChanges(ctx, a, []Memory{
		mem(t, "did:plc:alice", "1", "pop1 deploys go through the deploy repo workflow", []string{"infra"}, base),
		mem(t, "did:plc:alice", "2", "hailey prefers tabs and short replies", []string{"prefs"}, base.Add(time.Hour)),
	}, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	b := RepoState{Space: testSpace, DID: "did:plc:bob", Rev: "r1", SpaceRev: "s2"}
	if err := s.ApplyRepoChanges(ctx, b, []Memory{
		mem(t, "did:plc:bob", "1", "the deploy workflow needs a SHA input", []string{"infra", "ci"}, base.Add(2*time.Hour)),
	}, nil, false); err != nil {
		t.Fatal(err)
	}

	got, err := s.Search(ctx, testSpace, query(t, "deploy workflow"), 10, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[2].Rkey != "2" || got[2].Author != "did:plc:alice" {
		t.Fatalf("search order wrong: %+v", got)
	}
	if got[0].Similarity <= got[2].Similarity {
		t.Errorf("similarity not descending: %v vs %v", got[0].Similarity, got[2].Similarity)
	}

	got, _ = s.Search(ctx, testSpace, query(t, "deploy"), 10, Filter{Tags: []string{"infra", "ci"}})
	if len(got) != 1 || got[0].Author != "did:plc:bob" {
		t.Fatalf("tag filter: %+v", got)
	}
	got, _ = s.Search(ctx, testSpace, query(t, "deploy"), 10, Filter{Author: "did:plc:alice", Since: base.Add(30 * time.Minute)})
	if len(got) != 1 || got[0].Rkey != "2" {
		t.Fatalf("author+since filter: %+v", got)
	}
	got, _ = s.Search(ctx, "at://did:plc:other/space/x/y", query(t, "deploy"), 10, Filter{})
	if len(got) != 0 {
		t.Fatalf("other space leaked: %+v", got)
	}

	page, cursor, err := s.List(ctx, testSpace, 2, "", Filter{})
	if err != nil || len(page) != 2 || page[0].Author != "did:plc:bob" || cursor == "" {
		t.Fatalf("list page 1: %+v %q %v", page, cursor, err)
	}
	page, cursor, err = s.List(ctx, testSpace, 2, cursor, Filter{})
	if err != nil || len(page) != 1 || page[0].Rkey != "1" || cursor != "" {
		t.Fatalf("list page 2: %+v %q %v", page, cursor, err)
	}
	if _, _, err := s.List(ctx, testSpace, 2, "!!", Filter{}); !errors.Is(err, ErrBadCursor) {
		t.Errorf("bad cursor err = %v", err)
	}

	// Update one memory, delete another.
	upd := mem(t, "did:plc:alice", "1", "pop1 deploys now use argo", nil, base)
	a.Rev, a.SpaceRev = "r2", "s3"
	if err := s.ApplyRepoChanges(ctx, a, []Memory{upd}, []string{testSpace + "/did:plc:alice/garden.engram.memory/2"}, false); err != nil {
		t.Fatal(err)
	}
	m, err := s.Get(ctx, testSpace, upd.URI)
	if err != nil || m.Text != "pop1 deploys now use argo" || len(m.Tags) != 0 {
		t.Fatalf("get after update: %+v %v", m, err)
	}
	if _, err := s.Get(ctx, testSpace, testSpace+"/did:plc:alice/garden.engram.memory/2"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted memory still present: %v", err)
	}
	st, err := s.RepoState(ctx, testSpace, "did:plc:alice")
	if err != nil || st.Rev != "r2" || st.SpaceRev != "s3" {
		t.Fatalf("repo state: %+v %v", st, err)
	}

	// A replace drops anything not resupplied.
	a.Rev = "r3"
	if err := s.ApplyRepoChanges(ctx, a, nil, nil, true); err != nil {
		t.Fatal(err)
	}
	got, _ = s.Search(ctx, testSpace, query(t, "deploy"), 10, Filter{Author: "did:plc:alice"})
	if len(got) != 0 {
		t.Fatalf("replace left memories: %+v", got)
	}

	if err := s.MarkSpaceDeleted(ctx, testSpace); err != nil {
		t.Fatal(err)
	}
	if del, _ := s.SpaceDeleted(ctx, testSpace); !del {
		t.Error("space not marked deleted")
	}
	if repos, _ := s.Repos(ctx, testSpace); len(repos) != 0 {
		t.Errorf("repos survived deletion: %+v", repos)
	}
}

func TestSpaceRevNeverGoesBackwards(t *testing.T) {
	t.Parallel()
	s := newTestStore(t, testDims)
	ctx := context.Background()
	st := RepoState{Space: testSpace, DID: "did:plc:a", Rev: "r1", SpaceRev: "s5"}
	if err := s.ApplyRepoChanges(ctx, st, nil, nil, false); err != nil {
		t.Fatal(err)
	}
	st.Rev, st.SpaceRev = "r2", "s3"
	if err := s.ApplyRepoChanges(ctx, st, nil, nil, false); err != nil {
		t.Fatal(err)
	}
	got, _ := s.RepoState(ctx, testSpace, "did:plc:a")
	if got.SpaceRev != "s5" || got.Rev != "r2" {
		t.Fatalf("got %+v", got)
	}
}

func TestRejectsMemoriesFromAnotherRepo(t *testing.T) {
	t.Parallel()
	s := newTestStore(t, testDims)
	st := RepoState{Space: testSpace, DID: "did:plc:a", Rev: "r1"}
	err := s.ApplyRepoChanges(context.Background(), st, []Memory{mem(t, "did:plc:b", "1", "x", nil, time.Now())}, nil, false)
	if err == nil {
		t.Fatal("expected an error")
	}
}

func TestDimensionMismatchRefusesToOpen(t *testing.T) {
	t.Parallel()
	s := newTestStore(t, testDims)
	// Reopen the same schema with another size.
	cfg := s.pool.Config().Copy()
	cfg.AfterConnect = nil
	if _, err := OpenConfig(context.Background(), cfg, testDims*2); err == nil {
		t.Fatal("expected a dimensions mismatch")
	}
}
