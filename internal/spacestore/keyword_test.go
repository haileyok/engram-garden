package spacestore

import (
	"context"
	"testing"

	"github.com/haileyok/engram-garden/internal/text"
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
