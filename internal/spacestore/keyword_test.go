package spacestore

import (
	"context"
	"fmt"
	"math"
	"math/rand/v2"
	"strings"
	"testing"

	"github.com/haileyok/engram-garden/internal/eval"
	"github.com/haileyok/engram-garden/internal/text"
	"github.com/haileyok/engram-garden/internal/vec"
)

// segsOf returns the active index's segments of the test space.
func segsOf(t *testing.T, n *Node) []*seg {
	t.Helper()
	s, err := n.space(context.Background(), testSpace)
	if err != nil {
		t.Fatal(err)
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]*seg(nil), s.slots[0].segs...)
}

// checkBufKW rebuilds the buffer's keyword index from the buffer and
// compares.
func checkBufKW(t *testing.T, n *Node, stage string) {
	t.Helper()
	s, err := n.space(context.Background(), testSpace)
	if err != nil {
		t.Fatal(err)
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	want := map[string]map[*bufEntry]uint32{}
	var length uint64
	for _, e := range s.buf {
		a := text.AnalyzeMemory(e.doc.Text, e.doc.Tags, e.doc.Source)
		length += uint64(text.DecodeLength(text.EncodeLength(a.Length)))
		for term, tf := range a.TF {
			if want[term] == nil {
				want[term] = map[*bufEntry]uint32{}
			}
			want[term][e] = tf
		}
	}
	if length != s.bufKW.length || len(want) != len(s.bufKW.terms) {
		t.Fatalf("%s: length %d (want %d), %d terms (want %d)", stage, s.bufKW.length, length, len(s.bufKW.terms), len(want))
	}
	for term, m := range want {
		got := s.bufKW.terms[term]
		if len(got) != len(m) {
			t.Fatalf("%s: term %q has %d entries, want %d", stage, term, len(got), len(m))
		}
		for e, tf := range m {
			if got[e] != tf {
				t.Fatalf("%s: term %q tf %d, want %d", stage, term, got[e], tf)
			}
		}
	}
}

func TestBufferKeywordIndex(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFixture(t)
	n := f.node()
	configure(t, n, SpaceConfig{ModelInfo: modelA})
	apply := func(rev string, ms []Memory, deletes []string) {
		t.Helper()
		if err := n.ApplyRepoChanges(ctx, testSpace, "did:plc:alice", pos(rev), ms, deletes, false); err != nil {
			t.Fatal(err)
		}
	}
	apply("r1", []Memory{mem("did:plc:alice", "a1", "deploy the engram_space_uri fix"), mem("did:plc:alice", "a2", "running tests")}, nil)
	checkBufKW(t, n, "inserted")
	apply("r2", []Memory{mem("did:plc:alice", "a1", "an updated memory about argo")}, nil)
	checkBufKW(t, n, "updated")
	apply("r3", nil, []string{"a2"})
	checkBufKW(t, n, "deleted")
	if err := n.Flush(ctx, testSpace); err != nil {
		t.Fatal(err)
	}
	checkBufKW(t, n, "flushed")
	s, _ := n.space(ctx, testSpace)
	s.mu.RLock()
	empty := len(s.bufKW.terms) == 0 && s.bufKW.length == 0
	s.mu.RUnlock()
	if !empty {
		t.Fatal("buffer index not empty after flush")
	}
}

// The store's hybrid search must agree with engram-eval's in-memory index,
// which implements the same ranking without segments: the same memories
// in the same order with the same keyword scores, over a space with some
// memories in keyword segments and some in the buffer.
func TestHybridMatchesReference(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFixture(t)
	n := f.node(func(o *Options) { o.KeywordWrite = true; o.Candidates = 8 })
	configure(t, n, SpaceConfig{ModelInfo: model768})
	rng := rand.New(rand.NewPCG(3, 3))
	words := []string{"deploy", "argo", "rollout", "memory", "space", "engram_space_uri", "ModelMismatch", "running", "runs",
		"hailey", "penny", "cocoon", "listRecords", "reembed", "the", "a", "search", "keyword", "vector", "index"}
	var mems []Memory
	for i := range 60 {
		var ws []string
		for range 4 + rng.IntN(12) {
			ws = append(ws, words[rng.IntN(len(words))])
		}
		mems = append(mems, mem("did:plc:alice", fmt.Sprintf("m%03d", i), strings.Join(ws, " "), model768))
	}
	if err := n.ApplyRepoChanges(ctx, testSpace, "did:plc:alice", pos("r1"), mems[:40], nil, false); err != nil {
		t.Fatal(err)
	}
	if err := n.Flush(ctx, testSpace); err != nil {
		t.Fatal(err)
	}
	if err := n.ApplyRepoChanges(ctx, testSpace, "did:plc:alice", pos("r2"), mems[40:], nil, false); err != nil {
		t.Fatal(err)
	}

	evMems := make([]eval.Memory, len(mems))
	vecs := make([][]float32, len(mems))
	for i, m := range mems {
		evMems[i] = eval.Memory{ID: m.Rkey, Text: m.Text, Tags: m.Tags, Source: m.Source}
		vecs[i] = m.Vectors[model768.Key()]
	}
	ref := eval.NewIndex(evMems, vecs)
	for _, q := range []string{"engram_space_uri", "argo rollout", "running reembed", "model mismatch", "the memory space", "cocoon listRecords penny", "the", "a", "index"} {
		qv := embedFor(model768, q)
		res, err := n.Search(ctx, testSpace, SearchQuery{Vector: qv, Model: model768, Limit: 5, Text: q})
		if err != nil {
			t.Fatal(err)
		}
		pq := text.ParseQuery(q)
		want := ref.Hybrid(qv, &pq, 8, eval.Fusion{Kind: "convex", Alpha: fusionAlpha})
		if !res.Hybrid || len(res.Hits) != min(5, len(want)) {
			t.Fatalf("%q: hybrid %v, %d hits", q, res.Hybrid, len(res.Hits))
		}
		for i, h := range res.Hits {
			w := want[i]
			if h.Rkey != evMems[w.Doc].ID || math.Abs(h.Keyword-w.BM25) > 1e-9 {
				t.Fatalf("%q #%d: got %s (keyword %v), want %s (keyword %v)", q, i, h.Rkey, h.Keyword, evMems[w.Doc].ID, w.BM25)
			}
			// Every hit's keyword score is complete, including candidates
			// found only by vector.
			if ks := ref.KeywordScore(&pq, w.Doc); math.Abs(h.Keyword-ks) > 1e-9 {
				t.Fatalf("%q %s: keyword %v, want %v", q, h.Rkey, h.Keyword, ks)
			}
		}
	}
	// Without text, it's the vector search it always was.
	if res := search(t, n, model768, "argo rollout", Filter{}); res.Hybrid {
		t.Fatal("vector search became hybrid")
	}
}

// A memory found only by vector still gets its keyword score, even though
// it's outside the keyword candidates.
func TestHybridCompletesVectorOnlyScores(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFixture(t)
	n := f.node(func(o *Options) { o.KeywordWrite = true; o.Candidates = 4 })
	configure(t, n, SpaceConfig{ModelInfo: modelA})
	rng := rand.New(rand.NewPCG(4, 4))
	unit := func() []float32 {
		v := make([]float32, modelA.Dims)
		for i := range v {
			v[i] = float32(rng.NormFloat64())
		}
		vec.Normalize(v)
		return v
	}
	qv := unit()
	x := mem("did:plc:alice", "x", "one deploy mentioned among many other words about argo rollouts and logs")
	x.Vectors[modelA.Key()] = qv
	ms := []Memory{x}
	for i := range 10 {
		m := mem("did:plc:alice", fmt.Sprintf("k%02d", i), "deploy deploy deploy deploy")
		m.Vectors[modelA.Key()] = unit()
		ms = append(ms, m)
	}
	if err := n.ApplyRepoChanges(ctx, testSpace, "did:plc:alice", pos("r1"), ms, nil, false); err != nil {
		t.Fatal(err)
	}
	if err := n.Flush(ctx, testSpace); err != nil {
		t.Fatal(err)
	}
	res, err := n.Search(ctx, testSpace, SearchQuery{Vector: qv, Model: modelA, Limit: 3, Text: "deploy"})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Hybrid || len(res.Hits) == 0 || res.Hits[0].Rkey != "x" || res.Hits[0].Keyword <= 0 {
		t.Fatalf("hits %+v", res.Hits)
	}
}

// A space with a version 1 segment searches by vector only, even with
// text: scores never mix in memories that have no keyword index.
func TestHybridNeedsFullCoverage(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFixture(t)
	old := f.node()
	configure(t, old, SpaceConfig{ModelInfo: modelA})
	if err := old.ApplyRepoChanges(ctx, testSpace, "did:plc:alice", pos("r1"), []Memory{mem("did:plc:alice", "a1", "engram_space_uri lives in config")}, nil, false); err != nil {
		t.Fatal(err)
	}
	if err := old.Flush(ctx, testSpace); err != nil {
		t.Fatal(err)
	}
	n := f.node(func(o *Options) { o.KeywordWrite = true })
	res, err := n.Search(ctx, testSpace, SearchQuery{Vector: embedFor(modelA, "engram_space_uri"), Model: modelA, Limit: 5, Text: "engram_space_uri"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Hybrid || len(res.Hits) != 1 {
		t.Fatalf("hybrid %v with a version 1 segment, hits %d", res.Hybrid, len(res.Hits))
	}
}

// Deleted memories and filters apply to keyword candidates too.
func TestHybridRespectsDeletesAndFilters(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFixture(t)
	n := f.node(func(o *Options) { o.KeywordWrite = true })
	configure(t, n, SpaceConfig{ModelInfo: modelA})
	if err := n.ApplyRepoChanges(ctx, testSpace, "did:plc:alice", pos("r1"), []Memory{
		mem("did:plc:alice", "a1", "ModelMismatch happens after a model change"),
		mem("did:plc:alice", "a2", "ModelMismatch again, retry with the new config"),
	}, nil, false); err != nil {
		t.Fatal(err)
	}
	if err := n.ApplyRepoChanges(ctx, testSpace, "did:plc:bob", pos("r2"), []Memory{mem("did:plc:bob", "b1", "bob saw ModelMismatch too")}, nil, false); err != nil {
		t.Fatal(err)
	}
	if err := n.Flush(ctx, testSpace); err != nil {
		t.Fatal(err)
	}
	if err := n.ApplyRepoChanges(ctx, testSpace, "did:plc:alice", pos("r3"), nil, []string{"a2"}, false); err != nil {
		t.Fatal(err)
	}
	q := SearchQuery{Vector: embedFor(modelA, "unrelated words entirely"), Model: modelA, Limit: 10, Text: "ModelMismatch"}
	res, err := n.Search(ctx, testSpace, q)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, h := range res.Hits {
		got[h.Rkey] = h.Keyword > 0
	}
	if !res.Hybrid || got["a2"] || !got["a1"] || !got["b1"] {
		t.Fatalf("hits %v (deleted a2 must be gone; a1 and b1 must match the keyword)", got)
	}
	q.Filter = Filter{Author: "did:plc:bob"}
	if res, err = n.Search(ctx, testSpace, q); err != nil || len(res.Hits) != 1 || res.Hits[0].Rkey != "b1" {
		t.Fatalf("author filter: %+v %v", res, err)
	}
}

// The reader release: a node that doesn't write keyword segments opens and
// searches ones another node wrote, and writes version 1 beside them.
func TestKeywordSegmentsMixWithVersion1(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFixture(t)
	writer := f.node(func(o *Options) { o.KeywordWrite = true })
	configure(t, writer, SpaceConfig{ModelInfo: modelA})
	if err := writer.ApplyRepoChanges(ctx, testSpace, "did:plc:alice", pos("r1"), []Memory{
		mem("did:plc:alice", "a1", "the harness stores engram_space_uri in memory_config"),
		mem("did:plc:alice", "a2", "hailey likes short replies"),
	}, nil, false); err != nil {
		t.Fatal(err)
	}
	if err := writer.Flush(ctx, testSpace); err != nil {
		t.Fatal(err)
	}
	segs := segsOf(t, writer)
	if len(segs) != 1 || segs[0].kw == nil || segs[0].info.Format != 2 || segs[0].info.Analyzer != text.Version {
		t.Fatalf("writer's segment: %+v, keyword %v", segs[0].info, segs[0].kw != nil)
	}

	reader := f.node()
	if res := search(t, reader, modelA, "short replies", Filter{}); len(res.Hits) != 2 || res.Hits[0].Rkey != "a2" {
		t.Fatalf("reader search: %+v", res.Hits)
	}
	if err := reader.ApplyRepoChanges(ctx, testSpace, "did:plc:bob", pos("r2"), []Memory{mem("did:plc:bob", "b1", "deploys go through argo")}, nil, false); err != nil {
		t.Fatal(err)
	}
	if err := reader.Flush(ctx, testSpace); err != nil {
		t.Fatal(err)
	}
	var v1, v2 int
	for _, sg := range segsOf(t, reader) {
		switch {
		case sg.kw != nil && sg.info.Format == 2:
			v2++
		case sg.kw == nil && sg.info.Format == 1:
			v1++
		default:
			t.Errorf("segment %+v keyword %v", sg.info, sg.kw != nil)
		}
	}
	if v1 != 1 || v2 != 1 {
		t.Fatalf("%d version 1 and %d version 2 segments", v1, v2)
	}
	if res := search(t, f.node(), modelA, "argo deploys", Filter{}); len(res.Hits) != 3 || res.Hits[0].Rkey != "b1" {
		t.Fatalf("mixed search: %+v", res.Hits)
	}
}
