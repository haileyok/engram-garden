package spacestore

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/haileyok/engram-garden/internal/blob"
	"github.com/haileyok/engram-garden/internal/embed"
)

const testSpace = "at://did:plc:authority/space/garden.engram.space/memory"

var (
	modelA   = ModelInfo{Model: "hashing-a", ModelDigest: "sha256:aaaa", Dims: 64}
	modelB   = ModelInfo{Model: "hashing-b", ModelDigest: "sha256:bbbb", Dims: 96}
	model768 = ModelInfo{Model: "hashing-768", ModelDigest: "sha256:cccc", Dims: 768}
)

func embedFor(m ModelInfo, text string) []float32 {
	v, _ := embed.Hashing{Dims: m.Dims}.Embed(context.Background(), []string{text})
	return v[0]
}

func mem(author, rkey, text string, models ...ModelInfo) Memory {
	m := Memory{
		Author: author, Rkey: rkey, CID: "cid-" + rkey + "-" + fmt.Sprint(len(text)), Text: text,
		Tags: []string{"t-" + rkey[:1]}, CreatedAt: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(len(rkey)+int(rkey[len(rkey)-1])) * time.Minute),
		Vectors: map[string][]float32{},
	}
	if len(models) == 0 {
		models = []ModelInfo{modelA}
	}
	for _, md := range models {
		m.Vectors[md.Key()] = embedFor(md, text)
	}
	return m
}

type fixture struct {
	t     *testing.T
	blob  blob.Store
	cache string
	opt   Options
}

func newFixture(t *testing.T) *fixture {
	return &fixture{t: t, blob: blob.Dir{Root: t.TempDir()}, cache: t.TempDir()}
}

func (f *fixture) node(mod ...func(*Options)) *Node {
	f.t.Helper()
	opt := f.opt
	opt.Blob = f.blob
	opt.CacheDir = f.cache
	for _, m := range mod {
		m(&opt)
	}
	n, err := New(opt)
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(func() { n.bg.Wait() })
	return n
}

func configure(t *testing.T, n *Node, cfg SpaceConfig) bool {
	t.Helper()
	resync, err := n.SetConfig(context.Background(), testSpace, &cfg)
	if err != nil {
		t.Fatal(err)
	}
	n.bg.Wait()
	return resync
}

func search(t *testing.T, n *Node, model ModelInfo, q string, f Filter) *SearchResult {
	t.Helper()
	res, err := n.Search(context.Background(), testSpace, SearchQuery{Vector: embedFor(model, q), Model: model, Limit: 5, Filter: f})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func pos(rev string) RepoPosition { return RepoPosition{Rev: rev, SetHash: []byte(rev), SpaceRev: rev} }

func TestBufferFlushReload(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFixture(t)
	n := f.node()
	if _, err := n.Search(ctx, testSpace, SearchQuery{Vector: embedFor(modelA, "x"), Model: modelA}); !errors.Is(err, ErrNoModel) {
		t.Fatalf("search before config: %v", err)
	}
	configure(t, n, SpaceConfig{ModelInfo: modelA})
	err := n.ApplyRepoChanges(ctx, testSpace, "did:plc:alice", pos("r1"), []Memory{
		mem("did:plc:alice", "a1", "pop1 deploys go through the deploy repo"),
		mem("did:plc:alice", "a2", "hailey likes short replies"),
	}, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := n.ApplyRepoChanges(ctx, testSpace, "did:plc:bob", pos("r2"), []Memory{mem("did:plc:bob", "b1", "the deploy workflow takes a sha")}, nil, false); err != nil {
		t.Fatal(err)
	}
	check := func(n *Node, stage string) {
		t.Helper()
		res := search(t, n, modelA, "short replies", Filter{})
		if len(res.Hits) != 3 || res.Hits[0].Rkey != "a2" || res.Approximate || res.Hits[0].Similarity < 0.5 {
			t.Fatalf("%s: search %+v", stage, res)
		}
		res = search(t, n, modelA, "deploy", Filter{Author: "did:plc:bob"})
		if len(res.Hits) != 1 || res.Hits[0].Rkey != "b1" || res.Hits[0].Text != "the deploy workflow takes a sha" {
			t.Fatalf("%s: author filter %+v", stage, res.Hits)
		}
		if res := search(t, n, modelA, "deploy", Filter{Tags: []string{"t-b"}}); len(res.Hits) != 1 {
			t.Fatalf("%s: tag filter %+v", stage, res.Hits)
		}
		if res := search(t, n, modelA, "deploy", Filter{Author: "did:plc:nobody"}); len(res.Hits) != 0 {
			t.Fatalf("%s: unknown author matched", stage)
		}
		h, err := n.Get(ctx, testSpace, "did:plc:alice", "a1")
		if err != nil || h.Text != "pop1 deploys go through the deploy repo" || h.CID == "" {
			t.Fatalf("%s: get %+v %v", stage, h, err)
		}
		page, cur, err := n.List(ctx, testSpace, 2, "", Filter{})
		if err != nil || len(page) != 2 || cur == "" {
			t.Fatalf("%s: list %v %q %v", stage, len(page), cur, err)
		}
		rest, cur2, err := n.List(ctx, testSpace, 2, cur, Filter{})
		if err != nil || len(rest) != 1 || cur2 != "" || rest[0].Rkey == page[0].Rkey || rest[0].Rkey == page[1].Rkey {
			t.Fatalf("%s: list page 2 %+v %q %v", stage, rest, cur2, err)
		}
		if !page[0].CreatedAt.After(page[1].CreatedAt) && !page[0].CreatedAt.Equal(page[1].CreatedAt) {
			t.Fatalf("%s: list not newest first", stage)
		}
	}
	check(n, "buffered")
	if p, _ := n.RepoState(ctx, testSpace, "did:plc:alice"); p == nil || p.Rev != "r1" {
		t.Fatalf("buffered position: %+v", p)
	}
	// A crash before the flush loses the buffer and its positions together.
	crashed := f.node()
	if p, _ := crashed.RepoState(ctx, testSpace, "did:plc:alice"); p != nil {
		t.Fatalf("unflushed position survived: %+v", p)
	}
	if cfg, _ := crashed.Config(ctx, testSpace); cfg == nil {
		t.Fatal("config not published") // SetConfig flushes promptly
	}

	if err := n.Flush(ctx, testSpace); err != nil {
		t.Fatal(err)
	}
	check(n, "flushed")
	st, _ := n.Status(ctx, testSpace)
	if st.Buffered != 0 || st.Segments != 1 || st.Memories != 3 {
		t.Fatalf("status after flush: %+v", st)
	}
	n2 := f.node()
	check(n2, "reloaded")
	if p, _ := n2.RepoState(ctx, testSpace, "did:plc:bob"); p == nil || p.Rev != "r2" {
		t.Fatalf("reloaded position: %+v", p)
	}

	// Update, delete and an unchanged record on the reloaded node.
	err = n2.ApplyRepoChanges(ctx, testSpace, "did:plc:alice", pos("r3"), []Memory{
		mem("did:plc:alice", "a1", "pop1 deploys now go through argo"),
		mem("did:plc:alice", "a2", "hailey likes short replies"), // same CID: no change
	}, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if st, _ := n2.Status(ctx, testSpace); st.Buffered != 1 {
		t.Fatalf("unchanged record was re-buffered: %+v", st)
	}
	if err := n2.ApplyRepoChanges(ctx, testSpace, "did:plc:bob", pos("r4"), nil, []string{"b1"}, false); err != nil {
		t.Fatal(err)
	}
	for _, stage := range []string{"buffered", "flushed"} {
		if stage == "flushed" {
			if err := n2.Flush(ctx, testSpace); err != nil {
				t.Fatal(err)
			}
			n2 = f.node()
		}
		res := search(t, n2, modelA, "deploys argo workflow", Filter{})
		if len(res.Hits) != 2 || res.Hits[0].Text != "pop1 deploys now go through argo" {
			t.Fatalf("%s update/delete: %+v", stage, res.Hits)
		}
		if _, err := n2.Get(ctx, testSpace, "did:plc:bob", "b1"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("%s: deleted memory: %v", stage, err)
		}
	}
	// Replace (a full export) drops what the export doesn't list.
	if err := n2.ApplyRepoChanges(ctx, testSpace, "did:plc:alice", pos("r5"), []Memory{mem("did:plc:alice", "a2", "hailey likes short replies")}, nil, true); err != nil {
		t.Fatal(err)
	}
	if st, _ := n2.Status(ctx, testSpace); st.Memories != 1 {
		t.Fatalf("replace: %+v", st)
	}
	// Removing a repo.
	if err := n2.RemoveRepo(ctx, testSpace, "did:plc:alice"); err != nil {
		t.Fatal(err)
	}
	if p, _ := n2.RepoState(ctx, testSpace, "did:plc:alice"); p != nil {
		t.Fatal("removed repo kept its position")
	}
	if st, _ := n2.Status(ctx, testSpace); st.Memories != 0 {
		t.Fatalf("remove repo: %+v", st)
	}
}

func TestSkipsMismatchedVectors(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFixture(t)
	n := f.node()
	configure(t, n, SpaceConfig{ModelInfo: modelA})
	bad := mem("did:plc:bob", "b1", "wrong model", modelB)
	short := mem("did:plc:bob", "b2", "wrong size")
	short.Vectors[modelA.Key()] = short.Vectors[modelA.Key()][:10]
	if err := n.ApplyRepoChanges(ctx, testSpace, "did:plc:bob", pos("r1"), []Memory{bad, short, mem("did:plc:bob", "b3", "fine")}, nil, false); err != nil {
		t.Fatal(err)
	}
	st, _ := n.Status(ctx, testSpace)
	if st.Memories != 1 || st.Skipped["did:plc:bob"] != 2 {
		t.Fatalf("status %+v", st)
	}
	_ = n.Flush(ctx, testSpace)
	if st, _ := f.node().Status(ctx, testSpace); st.Skipped["did:plc:bob"] != 2 {
		t.Fatalf("skipped not persisted: %+v", st)
	}
	// Fixing the record clears the report.
	if err := n.ApplyRepoChanges(ctx, testSpace, "did:plc:bob", pos("r2"), []Memory{mem("did:plc:bob", "b1", "right model")}, []string{"b2"}, false); err != nil {
		t.Fatal(err)
	}
	if st, _ := n.Status(ctx, testSpace); st.Skipped["did:plc:bob"] != 0 || st.Memories != 2 {
		t.Fatalf("after fix: %+v", st)
	}
	if _, err := n.Search(ctx, testSpace, SearchQuery{Vector: embedFor(modelB, "x"), Model: modelB}); err == nil {
		t.Fatal("search with the wrong model accepted")
	}
}

func TestModelChange(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFixture(t)
	n := f.node()
	if !configure(t, n, SpaceConfig{ModelInfo: modelA}) {
		t.Fatal("first config should ask for a sync")
	}
	_ = n.ApplyRepoChanges(ctx, testSpace, "did:plc:alice", pos("r1"), []Memory{mem("did:plc:alice", "a1", "alice remembers apples")}, nil, false)
	_ = n.ApplyRepoChanges(ctx, testSpace, "did:plc:bob", pos("r1"), []Memory{mem("did:plc:bob", "b1", "bob remembers bananas")}, nil, false)
	_ = n.Flush(ctx, testSpace)

	// The authority announces the next model: every repo resyncs.
	if !configure(t, n, SpaceConfig{ModelInfo: modelA, Next: &modelB}) {
		t.Fatal("next model should ask for a sync")
	}
	if p, _ := n.RepoState(ctx, testSpace, "did:plc:alice"); p != nil {
		t.Fatal("positions should reset for a full sync")
	}
	// Alice re-embedded; Bob never comes back (his record has one vector).
	_ = n.ApplyRepoChanges(ctx, testSpace, "did:plc:alice", pos("r2"), []Memory{mem("did:plc:alice", "a1", "alice remembers apples", modelA, modelB)}, nil, true)
	_ = n.ApplyRepoChanges(ctx, testSpace, "did:plc:bob", pos("r2"), []Memory{mem("did:plc:bob", "b1", "bob remembers bananas")}, nil, true)
	_ = n.Flush(ctx, testSpace)
	st, _ := n.Status(ctx, testSpace)
	if st.Building == nil || *st.Building != modelB || st.BuildingMemories != 1 || st.Memories != 2 {
		t.Fatalf("building: %+v", st)
	}
	// Searches still use the active model meanwhile.
	if res := search(t, n, modelA, "bananas", Filter{}); len(res.Hits) != 2 {
		t.Fatalf("active search during build: %+v", res.Hits)
	}
	// Promote.
	if configure(t, n, SpaceConfig{ModelInfo: modelB}) {
		t.Fatal("promotion shouldn't need a sync")
	}
	for _, node := range []*Node{n, f.node()} {
		res := search(t, node, modelB, "apples", Filter{})
		if len(res.Hits) != 1 || res.Hits[0].Rkey != "a1" {
			t.Fatalf("after promotion: %+v", res.Hits)
		}
		if _, err := node.Search(ctx, testSpace, SearchQuery{Vector: embedFor(modelA, "x"), Model: modelA}); err == nil {
			t.Fatal("old model still accepted")
		}
	}
}

func TestMergeAndGC(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFixture(t)
	var clock atomic.Int64
	clock.Store(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC).UnixNano())
	now := func() time.Time { return time.Unix(0, clock.Load()) }
	f.opt = Options{MaxSegments: 3, Now: now, MergeInterval: time.Hour, MinRetention: 90 * 24 * time.Hour}
	n := f.node()
	configure(t, n, SpaceConfig{ModelInfo: modelA})
	for i := range 5 {
		ms := []Memory{mem("did:plc:alice", fmt.Sprintf("k%d", i), fmt.Sprintf("memory about topic%d", i))}
		var dels []string
		if i == 4 {
			dels = []string{"k0", "k1"}
		}
		if err := n.ApplyRepoChanges(ctx, testSpace, "did:plc:alice", pos(fmt.Sprint(i)), ms, dels, false); err != nil {
			t.Fatal(err)
		}
		if err := n.Flush(ctx, testSpace); err != nil {
			t.Fatal(err)
		}
	}
	st, _ := n.Status(ctx, testSpace)
	if st.Segments <= 3 {
		t.Fatalf("expected segments to pile up before the merge interval: %+v", st)
	}
	clock.Add(int64(2 * time.Hour))
	_ = n.ApplyRepoChanges(ctx, testSpace, "did:plc:alice", pos("5"), []Memory{mem("did:plc:alice", "k5", "memory about topic5")}, nil, false)
	if err := n.Flush(ctx, testSpace); err != nil {
		t.Fatal(err)
	}
	st, _ = n.Status(ctx, testSpace)
	if st.Segments != 1 || st.Memories != 4 {
		t.Fatalf("after merge: %+v", st)
	}
	for _, node := range []*Node{n, f.node(func(o *Options) { o.Now = now })} {
		res := search(t, node, modelA, "topic3", Filter{})
		if len(res.Hits) != 4 || res.Hits[0].Rkey != "k3" {
			t.Fatalf("search after merge: %+v", res.Hits)
		}
		if _, err := node.Get(ctx, testSpace, "did:plc:alice", "k1"); !errors.Is(err, ErrNotFound) {
			t.Fatal("merged-away deletion came back")
		}
	}
	segs := func() (n int) {
		objs, _ := f.blob.List(ctx, spacePrefix(testSpace))
		for _, o := range objs {
			if len(o.Key) > 0 && contains(o.Key, "/seg-") {
				n++
			}
		}
		return n
	}
	if segs() != 1 {
		t.Fatalf("merge inputs not deleted: %d segment objects", segs())
	}
	// An orphan from a crashed flush is collected after the retention.
	_ = blob.PutBytes(ctx, f.blob, SegmentKey(testSpace, "orphan"), []byte("x"), false)
	clock.Add(int64(91 * 24 * time.Hour))
	n.mu.Lock()
	s := n.spaces[testSpace]
	n.mu.Unlock()
	s.flushMu.Lock()
	err := s.gcLocked(ctx)
	s.flushMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	// Local files are "modified" now, not 91 days ago, so nothing is old
	// enough yet by mtime; the orphan survives until its retention passes.
	if segs() != 2 {
		t.Fatalf("collected too early: %d", segs())
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func TestLimits(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFixture(t)
	f.opt.Limits = Limits{MaxMemories: 2, SearchesPerSecond: 1}
	n := f.node()
	configure(t, n, SpaceConfig{ModelInfo: modelA})
	err := n.ApplyRepoChanges(ctx, testSpace, "did:plc:alice", pos("r1"), []Memory{
		mem("did:plc:alice", "a1", "one"), mem("did:plc:alice", "a2", "two"), mem("did:plc:alice", "a3", "three"),
	}, nil, false)
	if !errors.Is(err, ErrOverLimit) {
		t.Fatalf("err = %v", err)
	}
	if p, _ := n.RepoState(ctx, testSpace, "did:plc:alice"); p != nil {
		t.Fatal("position advanced past held-back memories")
	}
	if st, _ := n.Status(ctx, testSpace); st.Memories != 2 {
		t.Fatalf("memories %d", st.Memories)
	}
	// Deleting one makes room on the next pass.
	err = n.ApplyRepoChanges(ctx, testSpace, "did:plc:alice", pos("r2"), []Memory{
		mem("did:plc:alice", "a1", "one"), mem("did:plc:alice", "a3", "three"),
	}, []string{"a2"}, false)
	if err != nil {
		t.Fatal(err)
	}
	search(t, n, modelA, "one", Filter{})
	if _, err := n.Search(ctx, testSpace, SearchQuery{Vector: embedFor(modelA, "x"), Model: modelA}); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("rate limit: %v", err)
	}
}

func TestFencing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFixture(t)
	f.opt.ConditionalWrites = true
	var tokenA atomic.Uint64
	tokenA.Store(1)
	a := f.node(func(o *Options) { o.Lease = func(string) (uint64, bool) { return tokenA.Load(), true } })
	configure(t, a, SpaceConfig{ModelInfo: modelA})
	_ = a.ApplyRepoChanges(ctx, testSpace, "did:plc:alice", pos("r1"), []Memory{mem("did:plc:alice", "a1", "before handover")}, nil, false)
	if err := a.Flush(ctx, testSpace); err != nil {
		t.Fatal(err)
	}
	// B takes over with a higher token and republishes first.
	b := f.node(func(o *Options) { o.Lease = func(string) (uint64, bool) { return 2, true } })
	if st, err := b.Status(ctx, testSpace); err != nil || st.Token != 2 || st.Memories != 1 {
		t.Fatalf("handover: %+v %v", st, err)
	}
	// A still thinks it owns the space and flushes: its manifest has the
	// lower token, so no reader picks it.
	_ = a.ApplyRepoChanges(ctx, testSpace, "did:plc:alice", pos("r2"), []Memory{mem("did:plc:alice", "a2", "stale owner write")}, nil, false)
	if err := a.Flush(ctx, testSpace); err != nil {
		t.Fatal(err)
	}
	c := f.node(func(o *Options) { o.Lease = func(string) (uint64, bool) { return 2, true } })
	if st, _ := c.Status(ctx, testSpace); st.Memories != 1 || st.Token != 2 {
		t.Fatalf("stale owner's write was chosen: %+v", st)
	}
	// A node with a lower token than the newest manifest refuses to load.
	if _, err := f.node(func(o *Options) { o.Lease = func(string) (uint64, bool) { return 1, true } }).Status(ctx, testSpace); !errors.Is(err, ErrStaleOwner) {
		t.Fatalf("stale load: %v", err)
	}
	// Losing the lease stops writes.
	tokenA.Store(3)
	_ = a.ApplyRepoChanges(ctx, testSpace, "did:plc:alice", pos("r3"), []Memory{mem("did:plc:alice", "a3", "x")}, nil, false)
	if err := a.Flush(ctx, testSpace); !errors.Is(err, ErrNotOwner) {
		t.Fatalf("flush after losing the lease: %v", err)
	}
	// Two writers with the same token: conditional writes stop the second.
	if err := b.ApplyRepoChanges(ctx, testSpace, "did:plc:bob", pos("r1"), []Memory{mem("did:plc:bob", "b1", "from b")}, nil, false); err != nil {
		t.Fatal(err)
	}
	if err := c.ApplyRepoChanges(ctx, testSpace, "did:plc:bob", pos("r1"), []Memory{mem("did:plc:bob", "b1", "from c")}, nil, false); err != nil {
		t.Fatal(err)
	}
	if err := b.Flush(ctx, testSpace); err != nil {
		t.Fatal(err)
	}
	if err := c.Flush(ctx, testSpace); !errors.Is(err, ErrStaleOwner) {
		t.Fatalf("racing writer: %v", err)
	}
}

// slowBlob delays range reads above a size while enabled.
type slowBlob struct {
	blob.Store
	mu    sync.Mutex
	delay time.Duration
	min   int64
}

func (s *slowBlob) set(d time.Duration, min int64) {
	s.mu.Lock()
	s.delay, s.min = d, min
	s.mu.Unlock()
}

func (s *slowBlob) GetRange(ctx context.Context, key string, off, n int64) ([]byte, error) {
	s.mu.Lock()
	d, min := s.delay, s.min
	s.mu.Unlock()
	if d > 0 && n >= min {
		select {
		case <-time.After(d):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return s.Store.GetRange(ctx, key, off, n)
}

func (s *slowBlob) List(ctx context.Context, p string) ([]blob.Object, error) {
	s.mu.Lock()
	d, min := s.delay, s.min
	s.mu.Unlock()
	if d > 0 && min == 0 {
		select {
		case <-time.After(d):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return s.Store.List(ctx, p)
}

func seed(t *testing.T, n *Node, count int, model ModelInfo) {
	t.Helper()
	ctx := context.Background()
	configure(t, n, SpaceConfig{ModelInfo: model})
	var ms []Memory
	for i := range count {
		ms = append(ms, mem("did:plc:alice", fmt.Sprintf("m%04d", i), fmt.Sprintf("memory %d about subject%d", i, i%17), model))
	}
	if err := n.ApplyRepoChanges(ctx, testSpace, "did:plc:alice", pos("r1"), ms, nil, false); err != nil {
		t.Fatal(err)
	}
	if err := n.Flush(ctx, testSpace); err != nil {
		t.Fatal(err)
	}
}

func TestDeadlines(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFixture(t)
	slow := &slowBlob{Store: f.blob}
	f.blob = slow
	seed(t, f.node(), 300, modelA)

	// Re-rank reads (772 bytes per row, grouped) are slow: the complete
	// 1-bit ranking comes back, marked approximate.
	n := f.node(func(o *Options) { o.CacheDir = ""; o.Deadline = 50 * time.Millisecond })
	search(t, n, modelA, "subject3", Filter{}) // load the space first
	slow.set(300*time.Millisecond, 1)
	start := time.Now()
	res := search(t, n, modelA, "subject3", Filter{})
	slow.set(0, 0)
	if !res.Approximate || len(res.Hits) != 5 {
		t.Fatalf("approximate=%v hits=%d", res.Approximate, len(res.Hits))
	}
	_ = start

	// A load that can't finish by the hard limit fails retryably, and
	// carries on in the background.
	cold := f.node(func(o *Options) { o.CacheDir = ""; o.HardLimit = 50 * time.Millisecond })
	slow.set(200*time.Millisecond, 0)
	_, err := cold.Search(ctx, testSpace, SearchQuery{Vector: embedFor(modelA, "x"), Model: modelA})
	if !errors.Is(err, ErrRetryable) {
		t.Fatalf("cold search: %v", err)
	}
	slow.set(0, 0)
	cold.bg.Wait()
	deadline := time.Now().Add(5 * time.Second)
	for {
		cold.mu.Lock()
		loaded := cold.spaces[testSpace] != nil
		cold.mu.Unlock()
		if loaded || time.Now().After(deadline) {
			if !loaded {
				t.Fatal("background load never finished")
			}
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestStreamingAndColdReads(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	seed(t, f.node(), 2000, model768)
	// No disk cache, and a RAM share too small to pin the 1-bit sections.
	n := f.node(func(o *Options) { o.CacheDir = ""; o.SpaceRAMShare = 1 })
	res := search(t, n, model768, "subject5", Filter{})
	if len(res.Hits) != 5 || res.Approximate {
		t.Fatalf("streamed search: %+v", res)
	}
	n.mu.Lock()
	streamed := n.spaces[testSpace].streamed
	n.mu.Unlock()
	if !streamed {
		t.Fatal("space should stream its 1-bit sections")
	}
	// A cold search reads the metadata, the 1-bit vectors, the re-rank
	// candidates' int8 vectors and a few doc blocks. In this small space the
	// 200 candidates are 10% of the rows; in a 100k space they'd be 0.2%.
	cold := f.node(func(o *Options) { o.CacheDir = "" })
	before, _ := cold.RemoteReads()
	_ = before
	search(t, cold, model768, "subject5", Filter{})
	_, readBytes := cold.RemoteReads()
	objs, _ := f.blob.List(context.Background(), spacePrefix(testSpace))
	var total int64
	for _, o := range objs {
		if contains(o.Key, "/seg-") {
			total += o.Size
		}
	}
	if readBytes*3 > total {
		t.Fatalf("cold load and search read %d of %d bytes", readBytes, total)
	}
}

func TestRAMEviction(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFixture(t)
	f.opt.RAMBytes = 1
	n := f.node()
	spaces := []string{testSpace, testSpace + "2"}
	for _, sp := range spaces {
		cfg := SpaceConfig{ModelInfo: modelA}
		if _, err := n.SetConfig(ctx, sp, &cfg); err != nil {
			t.Fatal(err)
		}
		_ = n.ApplyRepoChanges(ctx, sp, "did:plc:alice", pos("r1"), []Memory{mem("did:plc:alice", "a1", "hello")}, nil, false)
		if err := n.Flush(ctx, sp); err != nil {
			t.Fatal(err)
		}
	}
	n.bg.Wait()
	n.noteRAM()
	n.mu.Lock()
	loaded := len(n.spaces)
	n.mu.Unlock()
	if loaded != 1 {
		t.Fatalf("%d spaces loaded, want the most recent only", loaded)
	}
	// An evicted space reloads on demand.
	if _, err := n.Get(ctx, spaces[0], "did:plc:alice", "a1"); err != nil {
		t.Fatal(err)
	}
}

func TestSpaceDeleted(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFixture(t)
	n := f.node()
	seed(t, n, 3, modelA)
	if err := n.MarkSpaceDeleted(ctx, testSpace); err != nil {
		t.Fatal(err)
	}
	for _, node := range []*Node{n, f.node()} {
		if gone, _ := node.SpaceDeleted(ctx, testSpace); !gone {
			t.Fatal("not marked deleted")
		}
		if _, err := node.Search(ctx, testSpace, SearchQuery{Vector: embedFor(modelA, "x"), Model: modelA}); !errors.Is(err, ErrSpaceDeleted) {
			t.Fatalf("search: %v", err)
		}
	}
}
