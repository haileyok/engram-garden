package spacestore

import (
	"container/heap"
	"context"
	"errors"
	"math"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/haileyok/engram-garden/internal/segment"
	"github.com/haileyok/engram-garden/internal/text"
	"github.com/haileyok/engram-garden/internal/vec"
)

// SearchQuery is a vector search, made hybrid by Text.
type SearchQuery struct {
	Vector []float32
	Model  ModelInfo
	Limit  int
	Filter Filter
	// Text is the query's text. When set and every segment has a keyword
	// index, keyword candidates join the vector ones and the two scores
	// are fused; otherwise it's ignored.
	Text string
}

// SearchResult is a search's answer.
type SearchResult struct {
	Hits []Hit
	// Approximate is set when re-ranking missed its deadline and results
	// are ranked by the 1-bit scan alone.
	Approximate bool
	// Hybrid is set when keyword scores took part in the ranking.
	Hybrid bool
}

type cand struct {
	seg *seg
	row int
	buf *bufEntry
	// id is the memory id, which breaks ties: equal distances keep the
	// older memory, so results don't depend on scan or map order.
	id   uint32
	dist int
	sim  float64
	// kw is the keyword score (hybrid searches), and fused the ranking
	// score.
	kw    float64
	hasKW bool
	fused float64
}

// candHeap is a max-heap on distance (then id), keeping the closest
// candidates.
type candHeap []cand

func (h candHeap) Len() int { return len(h) }
func (h candHeap) Less(i, j int) bool {
	if h[i].dist != h[j].dist {
		return h[i].dist > h[j].dist
	}
	return h[i].id > h[j].id
}
func (h candHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *candHeap) Push(x any)   { *h = append(*h, x.(cand)) }
func (h *candHeap) Pop() any {
	old := *h
	x := old[len(old)-1]
	*h = old[:len(old)-1]
	return x
}

func (h *candHeap) offer(c cand, limit int) {
	if h.Len() < limit {
		heap.Push(h, c)
	} else if w := (*h)[0]; c.dist < w.dist || c.dist == w.dist && c.id < w.id {
		(*h)[0] = c
		heap.Fix(h, 0)
	}
}

// segFilter is a filter resolved against one segment's strings.
type segFilter struct {
	author   uint32
	hasAuth  bool
	tags     []uint32
	since    int64
	hasSince bool
}

// resolve returns the filter for a segment, or false when no row of the
// segment can match.
func (f Filter) resolve(sg *seg) (segFilter, bool) {
	var sf segFilter
	if !f.Since.IsZero() {
		if sg.info.MaxCreatedAt.Before(f.Since) {
			return sf, false
		}
		sf.since, sf.hasSince = f.Since.UnixMicro(), true
	}
	if f.Author != "" {
		id, ok := sg.ix.StringID(f.Author)
		if !ok {
			return sf, false
		}
		sf.author, sf.hasAuth = id, true
	}
	for _, t := range f.Tags {
		id, ok := sg.ix.StringID(t)
		if !ok {
			return sf, false
		}
		sf.tags = append(sf.tags, id)
	}
	return sf, true
}

func (sf segFilter) match(ix *segment.Index, row int) bool {
	if sf.hasAuth && ix.AuthorID(row) != sf.author {
		return false
	}
	if sf.hasSince && ix.CreatedAtMicros(row) < sf.since {
		return false
	}
	return len(sf.tags) == 0 || ix.HasTags(row, sf.tags)
}

func (f Filter) matchDoc(d *segment.Doc) bool {
	if f.Author != "" && d.Author != f.Author {
		return false
	}
	if !f.Since.IsZero() && d.CreatedAt.Before(f.Since) {
		return false
	}
	for _, t := range f.Tags {
		if !slices.Contains(d.Tags, t) {
			return false
		}
	}
	return true
}

// streamRows is how many rows' 1-bit vectors a streaming scan reads at once.
const streamRows = 1 << 16

func (s *Space) search(ctx context.Context, q SearchQuery, start time.Time) (*SearchResult, error) {
	opt := s.n.opt
	hardCtx, cancel := context.WithDeadline(ctx, start.Add(opt.HardLimit))
	defer cancel()

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
	if sl.model != q.Model {
		s.mu.RUnlock()
		return nil, &ModelMismatchError{Want: sl.model, Got: q.Model}
	}
	segs := slices.Clone(sl.segs)
	published := s.deleted
	pending := make(map[uint32]struct{}, len(s.pendingDel))
	for id := range s.pendingDel {
		pending[id] = struct{}{}
	}
	var bufs []*bufEntry
	for _, e := range s.buf {
		if e.bits[0] != nil {
			bufs = append(bufs, e)
		}
	}
	var pq text.Query
	var bufKW bufSnapshot
	hybrid := false
	if q.Text != "" && keywordCovered(segs) {
		if pq = text.ParseQuery(q.Text); len(pq.Groups) > 0 {
			hybrid = true
			bufKW = s.snapshotBufKW(&pq)
		}
	}
	s.mu.RUnlock()

	dims := sl.model.Dims
	if len(q.Vector) != dims {
		return nil, &ModelMismatchError{Want: sl.model, Got: ModelInfo{Model: q.Model.Model, ModelDigest: q.Model.ModelDigest, Dims: len(q.Vector)}}
	}
	query := slices.Clone(q.Vector)
	if !vec.Normalize(query) {
		return nil, errors.New("query vector is zero")
	}
	qbits := vec.AppendBits(nil, query)
	bl := vec.BitBytes(dims)
	limit := q.Limit
	if limit <= 0 {
		limit = 10
	}
	ncand := max(opt.Candidates, limit)

	// 1. Scan every segment's 1-bit vectors and the buffer. Partial scans
	// are never returned, so a failed read fails the search.
	var mu sync.Mutex
	h := &candHeap{}
	var wg sync.WaitGroup
	var scanErr error
	for _, sg := range segs {
		sf, ok := q.Filter.resolve(sg)
		if !ok {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			local := &candHeap{}
			nprobe := 0
			if sg.ix.Clusters != nil {
				nprobe = max(4, len(sg.ix.Clusters.Centers)/8)
			}
			for _, rg := range sg.ix.ProbeRanges(query, nprobe) {
				for from := rg[0]; from < rg[1]; from += streamRows {
					to := min(rg[1], from+streamRows)
					var bits []byte
					if sg.bits != nil {
						bits = sg.bits[from*bl : to*bl]
					} else {
						b, err := sg.rd.BitsRange(hardCtx, from, to)
						if err != nil {
							mu.Lock()
							scanErr = err
							mu.Unlock()
							return
						}
						bits = b
					}
					for row := from; row < to; row++ {
						id := sg.ix.ID(row)
						if s.isDeleted(id, pending, published) || !sf.match(sg.ix, row) {
							continue
						}
						off := (row - from) * bl
						local.offer(cand{seg: sg, row: row, id: id, dist: vec.Hamming(qbits, bits[off:off+bl])}, ncand)
					}
				}
			}
			mu.Lock()
			for _, c := range *local {
				h.offer(c, ncand)
			}
			mu.Unlock()
		}()
	}
	wg.Wait()
	if scanErr != nil {
		if hardCtx.Err() != nil {
			return nil, ErrRetryable
		}
		return nil, scanErr
	}
	for _, e := range bufs {
		if !q.Filter.matchDoc(&e.doc) {
			continue
		}
		h.offer(cand{buf: e, id: e.id, dist: vec.Hamming(qbits, e.bits[0])}, ncand)
	}
	cands := []cand(*h)

	// 1b. Hybrid: add the keyword candidates and complete both scores.
	if hybrid {
		var err error
		if cands, err = s.addKeyword(hardCtx, &pq, q.Filter, segs, bufKW, pending, published, qbits, dims, cands, ncand); err != nil {
			if hardCtx.Err() != nil {
				return nil, ErrRetryable
			}
			return nil, err
		}
	}

	// 2. Re-rank with the int8 vectors, by the deadline.
	rerankCtx, cancelRerank := context.WithDeadline(hardCtx, start.Add(opt.Deadline))
	defer cancelRerank()
	approximate := false
	bySeg := map[*seg][]int{}
	for i, c := range cands {
		if c.buf != nil {
			cands[i].sim = float64(vec.Int8Dot(query, c.buf.int8[0]))
		} else {
			bySeg[c.seg] = append(bySeg[c.seg], i)
		}
	}
	var rerankErr error
	for sg, idxs := range bySeg {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rows := make([]int, len(idxs))
			for k, i := range idxs {
				rows[k] = cands[i].row
			}
			ivs, err := sg.rd.ReadInt8(rerankCtx, rows)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				rerankErr = err
				return
			}
			for k, i := range idxs {
				cands[i].sim = float64(vec.Int8Dot(query, ivs[k]))
			}
		}()
	}
	wg.Wait()
	if rerankErr != nil {
		if rerankCtx.Err() == nil {
			return nil, rerankErr
		}
		// Late: rank by the complete 1-bit scan instead.
		approximate = true
		for i := range cands {
			cands[i].sim = math.Cos(math.Pi * float64(cands[i].dist) / float64(dims))
		}
		sort.SliceStable(cands, func(a, b int) bool { return cands[a].dist < cands[b].dist })
	} else {
		sort.SliceStable(cands, func(a, b int) bool { return cands[a].sim > cands[b].sim })
	}
	if hybrid {
		fuse(cands)
	}
	if len(cands) > limit {
		cands = cands[:limit]
	}

	// 3. Fetch the results' documents.
	refs := make([]docRef, len(cands))
	for i, c := range cands {
		refs[i] = docRef{seg: c.seg, row: c.row, buf: c.buf}
	}
	bodies, err := fetchDocs(hardCtx, refs)
	if err != nil {
		if hardCtx.Err() != nil {
			return nil, ErrRetryable
		}
		return nil, err
	}
	out := &SearchResult{Approximate: approximate, Hybrid: hybrid, Hits: make([]Hit, len(cands))}
	for i, c := range cands {
		var h Hit
		if c.buf != nil {
			d := c.buf.doc
			h = Hit{Author: d.Author, Rkey: d.Rkey, Tags: d.Tags, CreatedAt: d.CreatedAt}
		} else {
			m := c.seg.ix.Meta(c.row)
			h = Hit{Author: m.Author, Rkey: m.Rkey, Tags: m.Tags, CreatedAt: m.CreatedAt}
		}
		h.Text, h.Source, h.CID, h.IndexedAt = bodies[i].Text, bodies[i].Source, bodies[i].CID, bodies[i].IndexedAt
		h.Similarity = max(-1, min(1, c.sim))
		h.Keyword = c.kw
		out.Hits[i] = h
	}
	return out, nil
}
