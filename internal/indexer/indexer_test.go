package indexer

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/haileyok/engram-garden/internal/blob"
	"github.com/haileyok/engram-garden/internal/embed"
	"github.com/haileyok/engram-garden/internal/lex"
	"github.com/haileyok/engram-garden/internal/spaceclient"
	"github.com/haileyok/engram-garden/internal/spacestore"
	"github.com/haileyok/engram-garden/internal/spacetest"
)

var (
	model     = lex.ModelInfo{Model: "hashing-256", ModelDigest: embed.HashingDigest, Dims: 256}
	nextModel = lex.ModelInfo{Model: "hashing-128", ModelDigest: embed.HashingDigest, Dims: 128}
)

type fixture struct {
	net   *spacetest.Net
	ix    *Indexer
	store *spacestore.Node
	alice *spacetest.Account
	bob   *spacetest.Account
}

func newStore(t *testing.T) *spacestore.Node {
	t.Helper()
	n, err := spacestore.New(spacestore.Options{Blob: blob.Dir{Root: t.TempDir()}, CacheDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = n.Close(context.Background()) })
	return n
}

func setup(t *testing.T) *fixture {
	t.Helper()
	n := spacetest.New(t)
	appview := n.NewAccount("did:plc:appview")
	alice := n.NewAccount("did:plc:alice")
	bob := n.NewAccount("did:plc:bob")
	for _, a := range []*spacetest.Account{n.Authority, appview, alice, bob} {
		n.AddMember(a.DID)
	}
	client, err := spaceclient.New(n.Session(appview), n.Dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	st := newStore(t)
	f := &fixture{net: n, store: st, alice: alice, bob: bob, ix: &Indexer{Store: st, Client: client, Dir: n.Dir}}
	f.declare(lex.Config{ModelInfo: model})
	return f
}

func (f *fixture) declare(c lex.Config) string {
	_, rev := f.net.Put(f.net.Authority, lex.ConfigCollection, lex.ConfigRkey, c.Record(time.Now()))
	return rev
}

func vector(m lex.ModelInfo, text string) []float32 {
	v, _ := embed.Hashing{Dims: m.Dims}.Embed(context.Background(), []string{text})
	return v[0]
}

// memory builds a memory record carrying vectors for the given models
// (default: the space's model).
func memory(text string, tags ...string) map[string]any {
	return memoryWith([]lex.ModelInfo{model}, text, tags...)
}

func memoryWith(models []lex.ModelInfo, text string, tags ...string) map[string]any {
	m := map[string]any{"$type": Collection, "text": text, "createdAt": "2026-10-06T12:00:00.000Z"}
	if len(tags) > 0 {
		m["tags"] = tags
	}
	for i, md := range models {
		m[lex.MemoryEmbeddingFields[i]] = lex.EmbeddingRecord(md, vector(md, lex.EmbedText("", text, tags)))
	}
	return m
}

func (f *fixture) search(t *testing.T, q string, filter spacestore.Filter) []spacestore.Hit {
	t.Helper()
	res, err := f.store.Search(context.Background(), f.net.Space, spacestore.SearchQuery{Vector: vector(model, q), Model: model, Limit: 10, Filter: filter})
	if err != nil {
		t.Fatal(err)
	}
	return res.Hits
}

func (f *fixture) count(t *testing.T) int {
	t.Helper()
	st, err := f.store.Status(context.Background(), f.net.Space)
	if err != nil {
		t.Fatal(err)
	}
	return st.Memories
}

func TestFullThenIncrementalSync(t *testing.T) {
	t.Parallel()
	f := setup(t)
	ctx := context.Background()

	f.net.Put(f.alice, Collection, "a1", memory("pop1 deploys go through the deploy repo", "infra"))
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
	if f.net.Calls("com.atproto.space.getRepo") != 3 || f.net.Calls("com.atproto.space.listRepoOps") != 0 {
		t.Fatalf("first sync should export each repo (authority, alice, bob) once: getRepo=%d listRepoOps=%d",
			f.net.Calls("com.atproto.space.getRepo"), f.net.Calls("com.atproto.space.listRepoOps"))
	}
	m, err := f.store.Get(ctx, f.net.Space, f.alice.DID, "a1")
	if err != nil || m.Author != f.alice.DID || len(m.Tags) != 1 || !m.CreatedAt.Equal(time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)) {
		t.Fatalf("memory a1 = %+v, %v", m, err)
	}

	// Changes since: update, delete, create.
	f.net.Put(f.alice, Collection, "a1", memory("pop1 deploys now go through argo", "infra"))
	f.net.Delete(f.alice, Collection, "a2")
	_, spaceRev := f.net.Put(f.alice, Collection, "a3", memory("embedding happens on the agent"))

	if _, err := f.ix.HandleWrite(ctx, Notification{Space: f.net.Space, Repo: f.alice.DID, SpaceRev: spaceRev}); err != nil {
		t.Fatal(err)
	}
	if f.net.Calls("com.atproto.space.getRepo") != 3 {
		t.Fatalf("incremental sync re-exported the repo")
	}
	if f.net.Calls("com.atproto.space.listRepoOps") != 1 {
		t.Fatalf("listRepoOps calls = %d, want 1", f.net.Calls("com.atproto.space.listRepoOps"))
	}
	if got := f.count(t); got != 3 {
		t.Fatalf("after changes: %d memories, want 3", got)
	}
	m, _ = f.store.Get(ctx, f.net.Space, f.alice.DID, "a1")
	if m.Text != "pop1 deploys now go through argo" {
		t.Fatalf("update not applied: %q", m.Text)
	}
	if got := f.search(t, "short replies", spacestore.Filter{Author: f.alice.DID}); len(got) != 2 || got[0].Text == "hailey likes short replies" {
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
	// Positions survive a flush and a fresh node.
	if err := f.store.Flush(ctx, f.net.Space); err != nil {
		t.Fatal(err)
	}
}

func TestMismatchedVectorsAreSkipped(t *testing.T) {
	t.Parallel()
	f := setup(t)
	ctx := context.Background()
	f.net.Put(f.alice, Collection, "a1", memory("right model"))
	f.net.Put(f.bob, Collection, "b1", memoryWith([]lex.ModelInfo{nextModel}, "wrong model"))
	f.net.Put(f.bob, Collection, "b2", map[string]any{"$type": Collection, "text": "no vector", "createdAt": "2026-10-06T12:00:00Z"})
	if err := f.ix.SyncSpace(ctx, f.net.Space); err != nil {
		t.Fatal(err)
	}
	st, _ := f.store.Status(ctx, f.net.Space)
	if st.Memories != 1 || st.Skipped[f.bob.DID] != 2 {
		t.Fatalf("status %+v", st)
	}
}

func TestNoConfigIndexesNothingUntilDeclared(t *testing.T) {
	t.Parallel()
	f := setup(t)
	ctx := context.Background()
	f.net.Delete(f.net.Authority, lex.ConfigCollection, lex.ConfigRkey)
	f.net.Put(f.alice, Collection, "a1", memory("waiting for a model"))
	if err := f.ix.SyncSpace(ctx, f.net.Space); err != nil {
		t.Fatal(err)
	}
	if got := f.count(t); got != 0 {
		t.Fatalf("indexed %d memories without a declared model", got)
	}
	// A config record from anyone but the authority is ignored.
	f.net.Put(f.alice, lex.ConfigCollection, lex.ConfigRkey, lex.Config{ModelInfo: model}.Record(time.Now()))
	if err := f.ix.SyncSpace(ctx, f.net.Space); err != nil {
		t.Fatal(err)
	}
	if got := f.count(t); got != 0 {
		t.Fatal("a member's config record was honored")
	}
	// The authority declares: the space resyncs and indexes alice's memory.
	rev := f.declare(lex.Config{ModelInfo: model})
	needSync, err := f.ix.HandleWrite(ctx, Notification{Space: f.net.Space, Repo: f.net.Authority.DID, SpaceRev: rev})
	if err != nil || !needSync {
		t.Fatalf("config notification: needSync=%v err=%v", needSync, err)
	}
	if err := f.ix.SyncSpace(ctx, f.net.Space); err != nil {
		t.Fatal(err)
	}
	if got := f.count(t); got != 1 {
		t.Fatalf("after declaring: %d memories", got)
	}
}

func TestModelChange(t *testing.T) {
	t.Parallel()
	f := setup(t)
	ctx := context.Background()
	f.net.Put(f.alice, Collection, "a1", memory("alice remembers apples"))
	f.net.Put(f.bob, Collection, "b1", memory("bob remembers bananas"))
	if err := f.ix.SyncSpace(ctx, f.net.Space); err != nil {
		t.Fatal(err)
	}
	// Announce the next model; alice re-embeds, bob doesn't.
	rev := f.declare(lex.Config{ModelInfo: model, Next: &nextModel})
	f.net.Put(f.alice, Collection, "a1", memoryWith([]lex.ModelInfo{model, nextModel}, "alice remembers apples"))
	if _, err := f.ix.HandleWrite(ctx, Notification{Space: f.net.Space, Repo: f.net.Authority.DID, SpaceRev: rev}); err != nil {
		t.Fatal(err)
	}
	if err := f.ix.SyncSpace(ctx, f.net.Space); err != nil {
		t.Fatal(err)
	}
	st, _ := f.store.Status(ctx, f.net.Space)
	if st.Building == nil || *st.Building != nextModel || st.BuildingMemories != 1 || st.Memories != 2 {
		t.Fatalf("building: %+v", st)
	}
	// Promote.
	rev = f.declare(lex.Config{ModelInfo: nextModel})
	if _, err := f.ix.HandleWrite(ctx, Notification{Space: f.net.Space, Repo: f.net.Authority.DID, SpaceRev: rev}); err != nil {
		t.Fatal(err)
	}
	res, err := f.store.Search(ctx, f.net.Space, spacestore.SearchQuery{Vector: vector(nextModel, "apples"), Model: nextModel, Limit: 5})
	if err != nil || len(res.Hits) != 1 || res.Hits[0].Rkey != "a1" {
		t.Fatalf("after promotion: %+v %v", res, err)
	}
}

func TestIncrementalSyncPages(t *testing.T) {
	t.Parallel()
	f := setup(t)
	ctx := context.Background()
	if err := f.ix.SyncSpace(ctx, f.net.Space); err != nil {
		t.Fatal(err)
	}
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

func (f *fixture) syncConfig(t *testing.T) {
	t.Helper()
	// The first config asks for a full pass, which SyncSpace runs.
	if err := f.ix.SyncSpace(context.Background(), f.net.Space); err != nil {
		t.Fatal(err)
	}
}

func TestUnverifiableOpsFallBackToExport(t *testing.T) {
	t.Parallel()
	f := setup(t)
	ctx := context.Background()
	f.syncConfig(t)
	f.net.Put(f.alice, Collection, "a1", memory("one"))
	if err := f.ix.SyncRepo(ctx, f.net.Space, f.alice.DID, ""); err != nil {
		t.Fatal(err)
	}
	f.net.OmitValues = true
	f.net.Put(f.alice, Collection, "a2", memory("two"))
	if err := f.ix.SyncRepo(ctx, f.net.Space, f.alice.DID, ""); err != nil {
		t.Fatal(err)
	}
	if f.net.Calls("com.atproto.space.getRepo") != 3 {
		t.Fatalf("missing values should trigger a full export (getRepo=%d)", f.net.Calls("com.atproto.space.getRepo"))
	}
	if got := f.count(t); got != 2 {
		t.Fatalf("got %d memories, want 2", got)
	}
}

func TestBadCommitIndexesNothing(t *testing.T) {
	t.Parallel()
	f := setup(t)
	ctx := context.Background()
	f.syncConfig(t)
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
	f.syncConfig(t)
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
	f.syncConfig(t)
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

func TestRepoDroppedFromListingIsRemoved(t *testing.T) {
	t.Parallel()
	f := setup(t)
	ctx := context.Background()
	f.net.Put(f.alice, Collection, "a1", memory("alice stays"))
	f.net.Put(f.bob, Collection, "b1", memory("bob leaves"))
	if err := f.ix.SyncSpace(ctx, f.net.Space); err != nil {
		t.Fatal(err)
	}
	if got := f.count(t); got != 2 {
		t.Fatalf("got %d, want 2", got)
	}
	f.net.DropWriter(f.bob)
	if err := f.ix.SyncSpace(ctx, f.net.Space); err != nil {
		t.Fatal(err)
	}
	if got := f.search(t, "bob leaves", spacestore.Filter{Author: f.bob.DID}); len(got) != 0 {
		t.Fatalf("removed writer's memories still indexed: %+v", got)
	}
	if got := f.count(t); got != 1 {
		t.Fatalf("got %d, want 1", got)
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
