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
