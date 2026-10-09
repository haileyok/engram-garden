package segment

import (
	"container/heap"
	"context"
	"sort"

	"github.com/haileyok/engram-garden/internal/text"
)

// KeywordQuery is a query prepared for scoring one segment with space-wide
// statistics.
type KeywordQuery struct {
	Q       *text.Query
	Weights text.Weights
	BM25    text.BM25
	// IDF is each of Q.Terms' inverse document frequency over the space.
	IDF []float64
	// AvgLength is the space's average memory length (decoded lengths).
	AvgLength float64
	// Postings are each of Q.Terms' postings in this segment; nil where the
	// segment doesn't have the term.
	Postings []*Postings
}

// KeywordHit is a scored row.
type KeywordHit struct {
	Row   int
	Score float64
}

// leaf is one term's BM25 score for a memory.
func (kq *KeywordQuery) leaf(i int, tf uint32, norm byte) float64 {
	return kq.IDF[i] * kq.BM25.TF(float64(tf), float64(text.DecodeLength(norm)), kq.AvgLength)
}

// leafBound bounds a term's score over a block from its stored maximum tf
// and minimum length code. A stored tf of 255 means 255 or more, so it
// takes the factor's limit.
func (kq *KeywordQuery) leafBound(i int, maxTF uint8, minNorm byte) float64 {
	if maxTF == 255 {
		return kq.IDF[i] * kq.BM25.TFLimit()
	}
	return kq.leaf(i, uint32(maxTF), minNorm)
}

// worse orders hits for the result heap: the root is the hit to drop
// first (lowest score, then highest row).
type hitHeap []KeywordHit

func (h hitHeap) Len() int { return len(h) }
func (h hitHeap) Less(a, b int) bool {
	if h[a].Score != h[b].Score {
		return h[a].Score < h[b].Score
	}
	return h[a].Row > h[b].Row
}
func (h hitHeap) Swap(a, b int) { h[a], h[b] = h[b], h[a] }
func (h *hitHeap) Push(x any)   { *h = append(*h, x.(KeywordHit)) }
func (h *hitHeap) Pop() any {
	old := *h
	x := old[len(old)-1]
	*h = old[:len(old)-1]
	return x
}

func (h *hitHeap) offer(hit KeywordHit, n int) {
	if h.Len() < n {
		heap.Push(h, hit)
		return
	}
	w := (*h)[0]
	if hit.Score > w.Score || hit.Score == w.Score && hit.Row < w.Row {
		(*h)[0] = hit
		heap.Fix(h, 0)
	}
}

func (h hitHeap) sorted() []KeywordHit {
	out := []KeywordHit(h)
	sort.Slice(out, func(a, b int) bool {
		if out[a].Score != out[b].Score {
			return out[a].Score > out[b].Score
		}
		return out[a].Row < out[b].Row
	})
	return out
}

// TopKExhaustive scores every row with a posting for any query term and
// returns the best n with a positive score, best first (ties by row).
// accept, if set, filters rows (deletions, author, tags, time).
func TopKExhaustive(kq *KeywordQuery, k *Keyword, n int, accept func(row int) bool) ([]KeywordHit, error) {
	vals := map[uint32][]float64{}
	for i, p := range kq.Postings {
		if p == nil {
			continue
		}
		it := p.Iter()
		for ; it.Row != NoRow; it.Next() {
			v := vals[it.Row]
			if v == nil {
				v = make([]float64, len(kq.Q.Terms))
				vals[it.Row] = v
			}
			v[i] = kq.leaf(i, it.TF, k.norms[it.Row])
		}
		if it.Err() != nil {
			return nil, it.Err()
		}
	}
	h := &hitHeap{}
	for row, v := range vals {
		if accept != nil && !accept(int(row)) {
			continue
		}
		if s := kq.Q.Score(kq.Weights, v); s > 0 {
			h.offer(KeywordHit{int(row), s}, n)
		}
	}
	return h.sorted(), nil
}

// window is how many rows TopK bounds and scores at a time.
const window = 1024

// TopK returns the same hits as TopKExhaustive, skipping every window of
// rows whose score bound can't beat the n-th best hit so far. A window's
// bound is the query's scoring tree evaluated on each term's largest
// block bound among the blocks that overlap the window. Every leaf rises
// with tf and falls with length, so that bounds every row in the window.
// Windows go in row order, so a row tying the n-th score loses to it, and
// a bound equal to it is enough to skip.
func TopK(ctx context.Context, kq *KeywordQuery, k *Keyword, n int, accept func(row int) bool) ([]KeywordHit, error) {
	return topK(ctx, kq, k, n, accept, window)
}

func topK(ctx context.Context, kq *KeywordQuery, k *Keyword, n int, accept func(row int) bool, win int) ([]KeywordHit, error) {
	nt := len(kq.Q.Terms)
	its := make([]*Iter, nt)
	blk := make([]int, nt) // each term's first block that may overlap the window
	for i, p := range kq.Postings {
		if p != nil {
			its[i] = p.Iter()
		}
	}
	bounds := make([]float64, nt)
	vals := make([]float64, win*nt)
	touched := make([]bool, win)
	h := &hitHeap{}
	for lo := 0; lo < k.Count; lo += win {
		if lo/win%64 == 0 && ctx.Err() != nil {
			return nil, ctx.Err()
		}
		hi := min(lo+win, k.Count)
		any := false
		for i, p := range kq.Postings {
			bounds[i] = 0
			if p == nil {
				continue
			}
			for blk[i] < p.Blocks() && p.BlockLast(blk[i]) < uint32(lo) {
				blk[i]++
			}
			for b := blk[i]; b < p.Blocks() && p.BlockFirstBound(b) < uint32(hi); b++ {
				maxTF, minNorm := p.BlockMax(b)
				bounds[i] = max(bounds[i], kq.leafBound(i, maxTF, minNorm))
				any = true
			}
		}
		if !any {
			continue
		}
		if b := kq.Q.Score(kq.Weights, bounds); b <= 0 || h.Len() == n && b <= (*h)[0].Score {
			continue
		}
		clear(vals)
		clear(touched)
		for i, it := range its {
			if it == nil {
				continue
			}
			it.Advance(uint32(lo))
			for ; it.Row < uint32(hi); it.Next() {
				r := int(it.Row) - lo
				vals[r*nt+i] = kq.leaf(i, it.TF, k.norms[it.Row])
				touched[r] = true
			}
			if it.Err() != nil {
				return nil, it.Err()
			}
		}
		for r := range hi - lo {
			if !touched[r] || accept != nil && !accept(lo+r) {
				continue
			}
			if s := kq.Q.Score(kq.Weights, vals[r*nt:(r+1)*nt]); s > 0 {
				h.offer(KeywordHit{lo + r, s}, n)
			}
		}
	}
	return h.sorted(), nil
}

// ScoreRows scores specific rows (in any order), for candidates found by
// vector search. Rows without any query term score 0.
func ScoreRows(kq *KeywordQuery, k *Keyword, rows []int) ([]float64, error) {
	order := make([]int, len(rows))
	for i := range order {
		order[i] = i
	}
	sort.Slice(order, func(a, b int) bool { return rows[order[a]] < rows[order[b]] })
	nt := len(kq.Q.Terms)
	its := make([]*Iter, nt)
	for i, p := range kq.Postings {
		if p != nil {
			its[i] = p.Iter()
		}
	}
	out := make([]float64, len(rows))
	vals := make([]float64, nt)
	for _, oi := range order {
		row := uint32(rows[oi])
		clear(vals)
		for i, it := range its {
			if it == nil {
				continue
			}
			it.Advance(row)
			if it.Err() != nil {
				return nil, it.Err()
			}
			if it.Row == row {
				vals[i] = kq.leaf(i, it.TF, k.norms[row])
			}
		}
		out[oi] = kq.Q.Score(kq.Weights, vals)
	}
	return out, nil
}
