package spacestore

import (
	"context"
	"encoding/binary"
	"errors"
	"math"
	"slices"
	"sort"
	"time"

	"github.com/haileyok/engram-garden/internal/vec"
)

// What a graph may ask for. The graph compares every pair of its memories,
// so the number of memories is bounded.
const (
	MaxGraphNodes             = 500
	MaxGraphNeighbors         = 8
	DefaultGraphNodes         = 200
	DefaultGraphNeighbors     = 3
	DefaultGraphMinSimilarity = 0.4
)

// GraphQuery asks for the newest memories and how they relate.
type GraphQuery struct {
	// Limit is how many of the newest memories to include. Zero means
	// DefaultGraphNodes; it is capped at MaxGraphNodes.
	Limit int
	// Neighbors is how many of its most similar memories each memory links
	// to. Zero means DefaultGraphNeighbors; it is capped at MaxGraphNeighbors.
	Neighbors int
	// MinSimilarity is the cosine similarity a pair needs to be linked. Zero
	// means DefaultGraphMinSimilarity.
	MinSimilarity float64
}

// Edge links two memories of a graph, by their positions in Graph.Nodes
// (A < B). Similarity is their cosine similarity.
type Edge struct {
	A, B       int
	Similarity float64
}

// Graph is the newest memories of a space and the links between memories
// that mean similar things. Nodes are newest first.
type Graph struct {
	Nodes []Hit
	Edges []Edge
}

// Graph returns the space's newest memories, linking each to its nearest
// neighbors among them. Only memories indexed with the space's active model
// are included. It reads the int8 vectors, so the links are as precise as a
// search's re-ranking.
func (n *Node) Graph(ctx context.Context, spaceURI string, q GraphQuery) (*Graph, error) {
	start := time.Now()
	loadCtx, cancel := context.WithDeadline(ctx, start.Add(n.opt.HardLimit))
	defer cancel()
	s, err := n.space(loadCtx, spaceURI)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
			return nil, ErrRetryable
		}
		return nil, err
	}
	// A graph counts as a search: it takes the space's search rate and
	// concurrency allowance.
	if s.limiter != nil && !s.limiter.Allow() {
		return nil, ErrRateLimited
	}
	select {
	case s.searchSem <- struct{}{}:
		defer func() { <-s.searchSem }()
	case <-loadCtx.Done():
		return nil, ErrRetryable
	}
	g, err := s.graph(loadCtx, q)
	if err != nil && errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
		return nil, ErrRetryable
	}
	return g, err
}

func (s *Space) graph(ctx context.Context, q GraphQuery) (*Graph, error) {
	limit := q.Limit
	if limit <= 0 {
		limit = DefaultGraphNodes
	}
	limit = min(limit, MaxGraphNodes)
	k := q.Neighbors
	if k <= 0 {
		k = DefaultGraphNeighbors
	}
	k = min(k, MaxGraphNeighbors)
	minSim := q.MinSimilarity
	if minSim <= 0 {
		minSim = DefaultGraphMinSimilarity
	}

	type item struct {
		key listKey
		l   *loc
		ref docRef
		// Where the active model's int8 vector is: in the buffer, or at a
		// row of a segment.
		buf []byte
		sg  *seg
		row int
	}
	s.mu.RLock()
	if s.spaceGone {
		s.mu.RUnlock()
		return nil, ErrSpaceDeleted
	}
	sl := s.slots[0]
	if sl == nil {
		s.mu.RUnlock()
		return nil, ErrNoModel
	}
	dims := sl.model.Dims
	var items []item
	for _, l := range s.locs {
		if l.cover&1 == 0 {
			continue
		}
		it := item{key: listKey{l.createdAt, l.author, l.rkey}, l: l, ref: l.ref()}
		switch {
		case l.buf != nil && l.buf.int8[0] != nil:
			it.buf = l.buf.int8[0]
		case l.seg[0] != nil:
			it.sg, it.row = l.seg[0], l.row[0]
		default:
			continue
		}
		items = append(items, it)
	}
	s.mu.RUnlock()

	// Newest first, and only as many as asked for.
	slices.SortFunc(items, func(a, b item) int {
		switch {
		case b.key.less(a.key):
			return -1
		case a.key.less(b.key):
			return 1
		}
		return 0
	})
	if len(items) > limit {
		items = items[:limit]
	}

	// The vectors, as unit vectors: a dot product is then a cosine.
	raw := make([][]byte, len(items))
	bySeg := map[*seg][]int{}
	for i, it := range items {
		if it.buf != nil {
			raw[i] = it.buf
		} else {
			bySeg[it.sg] = append(bySeg[it.sg], i)
		}
	}
	for sg, idxs := range bySeg {
		rows := make([]int, len(idxs))
		for j, i := range idxs {
			rows[j] = items[i].row
		}
		ivs, err := sg.rd.ReadInt8(ctx, rows)
		if err != nil {
			return nil, err
		}
		for j, i := range idxs {
			raw[i] = ivs[j]
		}
	}
	units := make([][]float32, len(items))
	for i, iv := range raw {
		units[i] = unitFromInt8(iv, dims)
	}

	refs := make([]docRef, len(items))
	for i, it := range items {
		refs[i] = it.ref
	}
	bodies, err := fetchDocs(ctx, refs)
	if err != nil {
		return nil, err
	}
	g := &Graph{Nodes: make([]Hit, len(items))}
	for i, it := range items {
		g.Nodes[i] = Hit{
			Author: it.l.author, Rkey: it.l.rkey, Tags: it.l.tags, CreatedAt: time.UnixMicro(it.l.createdAt).UTC(),
			Text: bodies[i].Text, Source: bodies[i].Source, CID: bodies[i].CID, IndexedAt: bodies[i].IndexedAt,
		}
	}

	// Each memory's nearest neighbors; a pair counts once.
	type pair struct{ a, b int }
	seen := map[pair]bool{}
	type scored struct {
		j   int
		sim float64
	}
	for i, ui := range units {
		if ui == nil {
			continue
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var near []scored
		for j, uj := range units {
			if j == i || uj == nil {
				continue
			}
			if sim := float64(dot(ui, uj)); sim >= minSim {
				near = append(near, scored{j, sim})
			}
		}
		sort.SliceStable(near, func(a, b int) bool { return near[a].sim > near[b].sim })
		if len(near) > k {
			near = near[:k]
		}
		for _, nb := range near {
			p := pair{min(i, nb.j), max(i, nb.j)}
			if seen[p] {
				continue
			}
			seen[p] = true
			g.Edges = append(g.Edges, Edge{A: p.a, B: p.b, Similarity: min(1, nb.sim)})
		}
	}
	sort.SliceStable(g.Edges, func(a, b int) bool {
		if g.Edges[a].A != g.Edges[b].A {
			return g.Edges[a].A < g.Edges[b].A
		}
		return g.Edges[a].B < g.Edges[b].B
	})
	return g, nil
}

// unitFromInt8 rebuilds a vector from its int8 form and scales it to length
// one. It returns nil for a vector that is malformed or all zeros.
func unitFromInt8(iv []byte, dims int) []float32 {
	if len(iv) != vec.Int8Bytes(dims) {
		return nil
	}
	scale := math.Float32frombits(binary.LittleEndian.Uint32(iv[dims:]))
	out := make([]float32, dims)
	for i := range out {
		out[i] = float32(int8(iv[i])) * scale
	}
	if !vec.Normalize(out) {
		return nil
	}
	return out
}

func dot(a, b []float32) float32 {
	var s0, s1, s2, s3 float32
	i := 0
	for ; i+4 <= len(a); i += 4 {
		s0 += a[i] * b[i]
		s1 += a[i+1] * b[i+1]
		s2 += a[i+2] * b[i+2]
		s3 += a[i+3] * b[i+3]
	}
	for ; i < len(a); i++ {
		s0 += a[i] * b[i]
	}
	return s0 + s1 + s2 + s3
}
