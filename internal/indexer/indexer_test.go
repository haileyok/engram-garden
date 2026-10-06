package indexer

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/haileyok/engram-garden/internal/embed"
	"github.com/haileyok/engram-garden/internal/spaceclient"
	"github.com/haileyok/engram-garden/internal/spacetest"
	"github.com/haileyok/engram-garden/internal/store"
	"github.com/haileyok/engram-garden/internal/store/storetest"
)

const dims = 256

type fixture struct {
	net   *spacetest.Net
	ix    *Indexer
	store *store.Store
	alice *spacetest.Account
	bob   *spacetest.Account
}

func setup(t *testing.T) *fixture {
	t.Helper()
	st := storetest.New(t, dims)
	n := spacetest.New(t)
	appview := n.NewAccount("did:plc:appview")
	alice := n.NewAccount("did:plc:alice")
	bob := n.NewAccount("did:plc:bob")
	for _, a := range []*spacetest.Account{appview, alice, bob} {
		n.AddMember(a.DID)
	}
	client, err := spaceclient.New(n.Session(appview), n.Dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{
		net:   n,
		store: st,
		alice: alice,
		bob:   bob,
		ix:    &Indexer{Store: st, Embedder: embed.Hashing{Dims: dims}, Client: client, Dir: n.Dir},
	}
}

func memory(text string, tags ...string) map[string]any {
	m := map[string]any{"$type": Collection, "text": text, "createdAt": "2026-10-06T12:00:00.000Z"}
	if len(tags) > 0 {
		m["tags"] = tags
	}
	return m
}

func (f *fixture) search(t *testing.T, q string, filter store.Filter) []store.Memory {
	t.Helper()
	v, _ := embed.Hashing{Dims: dims}.Embed(context.Background(), []string{q})
	got, err := f.store.Search(context.Background(), f.net.Space, v[0], 10, filter)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func (f *fixture) count(t *testing.T) int {
	t.Helper()
	ms, _, err := f.store.List(context.Background(), f.net.Space, 100, "", store.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	return len(ms)
}

func TestFullThenIncrementalSync(t *testing.T) {
	t.Parallel()
	f := setup(t)
	ctx := context.Background()

	uri1, _ := f.net.Put(f.alice, Collection, "a1", memory("pop1 deploys go through the deploy repo", "infra"))
	f.net.Put(f.alice, Collection, "a2", memory("hailey likes short replies", "prefs"))
	f.net.Put(f.bob, Collection, "b1", memory("the deploy workflow takes a SHA", "infra", "ci"))
	f.net.Put(f.bob, "app.example.other", "x", map[string]any{"text": "not a memory"})
	f.net.Put(f.bob, Collection, "b2", map[string]any{"text": "   ", "createdAt": "2026-10-06T12:00:00Z"})

	if err := f.ix.SyncSpace(ctx, f.net.Space); err != nil {
		t.Fatal(err)
	}
	if got := f.count(t); got != 3 {
		t.Fatalf("indexed %d memories, want 3 (other collections and blank text skipped)", got)
	}
	if f.net.Calls("com.atproto.space.getRepo") != 2 || f.net.Calls("com.atproto.space.listRepoOps") != 0 {
		t.Fatalf("first sync should export each repo once: getRepo=%d listRepoOps=%d",
			f.net.Calls("com.atproto.space.getRepo"), f.net.Calls("com.atproto.space.listRepoOps"))
	}
	m, err := f.store.Get(ctx, f.net.Space, uri1)
	if err != nil || m.Author != f.alice.DID || len(m.Tags) != 1 || !m.CreatedAt.Equal(time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)) {
		t.Fatalf("memory a1 = %+v, %v", m, err)
	}

	// Changes since: update, delete, create.
	f.net.Put(f.alice, Collection, "a1", memory("pop1 deploys now go through argo", "infra"))
	f.net.Delete(f.alice, Collection, "a2")
	_, spaceRev := f.net.Put(f.alice, Collection, "a3", memory("embedding model is text-embedding-3-small"))

	if _, err := f.ix.HandleWrite(ctx, Notification{Space: f.net.Space, Repo: f.alice.DID, SpaceRev: spaceRev}); err != nil {
		t.Fatal(err)
	}
	if f.net.Calls("com.atproto.space.getRepo") != 2 {
		t.Fatalf("incremental sync re-exported the repo")
	}
	if f.net.Calls("com.atproto.space.listRepoOps") != 1 {
		t.Fatalf("listRepoOps calls = %d, want 1", f.net.Calls("com.atproto.space.listRepoOps"))
	}
	if got := f.count(t); got != 3 {
		t.Fatalf("after changes: %d memories, want 3", got)
	}
	m, _ = f.store.Get(ctx, f.net.Space, uri1)
	if m.Text != "pop1 deploys now go through argo" {
		t.Fatalf("update not applied: %q", m.Text)
	}
	if got := f.search(t, "short replies", store.Filter{Author: f.alice.DID}); len(got) != 2 || got[0].Text == "hailey likes short replies" {
		t.Fatalf("deleted memory still searchable: %+v", got)
	}

	// Nothing new: a space sync touches no repo.
	before := f.net.Calls("com.atproto.space.listRepoOps")
	if err := f.ix.SyncSpace(ctx, f.net.Space); err != nil {
		t.Fatal(err)
	}
	if f.net.Calls("com.atproto.space.listRepoOps") != before {
		t.Fatal("up-to-date repos were re-synced")
	}
}

func TestIncrementalSyncPages(t *testing.T) {
	t.Parallel()
	f := setup(t)
	ctx := context.Background()
	f.ix.PageSize = 2
	f.net.Put(f.alice, Collection, "a0", memory("first"))
	if err := f.ix.SyncRepo(ctx, f.net.Space, f.alice.DID, ""); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"a1", "a2", "a3", "a4", "a5"} {
		f.net.Put(f.alice, Collection, k, memory("memory "+k))
	}
	if err := f.ix.SyncRepo(ctx, f.net.Space, f.alice.DID, ""); err != nil {
		t.Fatal(err)
	}
	if got := f.count(t); got != 6 {
		t.Fatalf("got %d memories, want 6", got)
	}
	if n := f.net.Calls("com.atproto.space.listRepoOps"); n != 3 {
		t.Fatalf("listRepoOps calls = %d, want 3 pages", n)
	}
}

func TestUnverifiableOpsFallBackToExport(t *testing.T) {
	t.Parallel()
	f := setup(t)
	ctx := context.Background()
	f.net.Put(f.alice, Collection, "a1", memory("one"))
	if err := f.ix.SyncRepo(ctx, f.net.Space, f.alice.DID, ""); err != nil {
		t.Fatal(err)
	}
	f.net.OmitValues = true
	f.net.Put(f.alice, Collection, "a2", memory("two"))
	if err := f.ix.SyncRepo(ctx, f.net.Space, f.alice.DID, ""); err != nil {
		t.Fatal(err)
	}
	if f.net.Calls("com.atproto.space.getRepo") != 2 {
		t.Fatal("missing values should trigger a full export")
	}
	if got := f.count(t); got != 2 {
		t.Fatalf("got %d memories, want 2", got)
	}
}

func TestBadCommitIndexesNothing(t *testing.T) {
	t.Parallel()
	f := setup(t)
	ctx := context.Background()
	f.net.Put(f.alice, Collection, "a1", memory("one"))
	if err := f.ix.SyncRepo(ctx, f.net.Space, f.alice.DID, ""); err != nil {
		t.Fatal(err)
	}
	f.net.CorruptCommits = true
	f.net.Put(f.alice, Collection, "a2", memory("forged"))
	if err := f.ix.SyncRepo(ctx, f.net.Space, f.alice.DID, ""); err == nil {
		t.Fatal("expected verification failure")
	}
	if got := f.count(t); got != 1 {
		t.Fatalf("unverified changes were indexed: %d memories", got)
	}
	// Once the host behaves again, the repo catches up.
	f.net.CorruptCommits = false
	if err := f.ix.SyncRepo(ctx, f.net.Space, f.alice.DID, ""); err != nil {
		t.Fatal(err)
	}
	if got := f.count(t); got != 2 {
		t.Fatalf("got %d memories after recovery, want 2", got)
	}
}

func TestKeyRotation(t *testing.T) {
	t.Parallel()
	f := setup(t)
	ctx := context.Background()
	f.net.Put(f.alice, Collection, "a1", memory("one"))
	if err := f.ix.SyncRepo(ctx, f.net.Space, f.alice.DID, ""); err != nil {
		t.Fatal(err)
	}
	f.net.RotateKey(f.alice)
	f.net.Put(f.alice, Collection, "a2", memory("two"))
	if err := f.ix.SyncRepo(ctx, f.net.Space, f.alice.DID, ""); err != nil {
		t.Fatal(err)
	}
	if got := f.count(t); got != 2 {
		t.Fatalf("got %d, want 2", got)
	}
}

func TestGapDetection(t *testing.T) {
	t.Parallel()
	f := setup(t)
	ctx := context.Background()
	_, r1 := f.net.Put(f.alice, Collection, "a1", memory("one"))
	gap, err := f.ix.HandleWrite(ctx, Notification{Space: f.net.Space, Repo: f.alice.DID, SpaceRev: r1})
	if err != nil || gap {
		t.Fatalf("first notification: gap=%v err=%v", gap, err)
	}
	_, r2 := f.net.Put(f.bob, Collection, "b1", memory("missed"))
	_, r3 := f.net.Put(f.alice, Collection, "a2", memory("two"))
	gap, err = f.ix.HandleWrite(ctx, Notification{Space: f.net.Space, Repo: f.alice.DID, SpaceRev: r3, PrevSpaceRev: r2})
	if err != nil || !gap {
		t.Fatalf("missed notification not detected: gap=%v err=%v", gap, err)
	}
	if err := f.ix.SyncSpace(ctx, f.net.Space); err != nil {
		t.Fatal(err)
	}
	if got := f.count(t); got != 3 {
		t.Fatalf("gap recovery: %d memories, want 3", got)
	}
}

func TestNonMemberCannotSync(t *testing.T) {
	t.Parallel()
	f := setup(t)
	f.net.RemoveMember("did:plc:appview")
	f.net.Put(f.alice, Collection, "a1", memory("one"))
	err := f.ix.SyncSpace(context.Background(), f.net.Space)
	var xe *spaceclient.Error
	if !errors.As(err, &xe) || xe.Status != 403 {
		t.Fatalf("err = %v, want a 403 from getSpaceCredential", err)
	}
}
