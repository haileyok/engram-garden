package eval

import (
	"math"
	"sort"

	"github.com/haileyok/engram-garden/internal/text"
	"github.com/haileyok/engram-garden/internal/vec"
)

// Index holds a corpus in memory, ranked the way the appview will rank it:
// vectors by a 1-bit scan and an int8 re-rank, keywords by BM25 through
// text's scoring tree, hybrid by completing both scores for the union of
// candidates and fusing them.
type Index struct {
	Mems []Memory
	dims int
	bits [][]byte
	i8   [][]byte

	n     uint64
	avgdl float64
	dl    []float64 // decoded one-byte lengths, as segments will store them
	df    map[string]uint64
	post  map[string][]posting

	BM25    text.BM25
	Weights text.Weights
}

type posting struct {
	doc int
	tf  uint32
}

// NewIndex indexes memories with their normalized document vectors (nil
// vectors skip vector search).
func NewIndex(mems []Memory, vecs [][]float32) *Index {
	ix := &Index{
		Mems: mems, df: map[string]uint64{}, post: map[string][]posting{},
		BM25: text.DefaultBM25, Weights: text.DefaultWeights,
	}
	if len(vecs) == len(mems) && len(vecs) > 0 {
		ix.dims = len(vecs[0])
		for _, v := range vecs {
			ix.bits = append(ix.bits, vec.AppendBits(nil, v))
			ix.i8 = append(ix.i8, vec.AppendInt8(nil, v))
		}
	}
	var total float64
	for i, m := range mems {
		d := text.AnalyzeMemory(m.Text, m.Tags, m.Source)
		l := float64(text.DecodeLength(text.EncodeLength(d.Length)))
		ix.dl = append(ix.dl, l)
		total += l
		for term, tf := range d.TF {
			ix.df[term]++
			ix.post[term] = append(ix.post[term], posting{i, tf})
		}
	}
	ix.n = uint64(len(mems))
	if ix.n > 0 {
		ix.avgdl = total / float64(ix.n)
	}
	return ix
}

// With returns the index scoring keywords with other weights and BM25
// parameters. It shares everything else, so it costs nothing to make.
func (ix *Index) With(w text.Weights, p text.BM25) *Index {
	c := *ix
	c.Weights, c.BM25 = w, p
	return &c
}

// DF is a term's document frequency.
func (ix *Index) DF(term string) uint64 { return ix.df[term] }

// Hit is one ranked memory with the scores that ranked it.
type Hit struct {
	Doc int
	// Sim is the int8 re-ranked cosine; HasSim is false when the memory
	// wasn't scored by vector.
	Sim    float64
	HasSim bool
	// BM25 is the keyword score (0 when no query term occurs).
	BM25  float64
	Score float64
}

// Vector returns the top ncand by a 1-bit scan, re-ranked by int8 cosine.
func (ix *Index) Vector(q []float32, ncand int) []Hit {
	if ix.bits == nil || q == nil {
		return nil
	}
	qb := vec.AppendBits(nil, q)
	type c struct{ doc, dist int }
	cs := make([]c, len(ix.bits))
	for i, b := range ix.bits {
		cs[i] = c{i, vec.Hamming(qb, b)}
	}
	sort.SliceStable(cs, func(a, b int) bool { return cs[a].dist < cs[b].dist })
	cs = cs[:min(ncand, len(cs))]
	out := make([]Hit, len(cs))
	for i, x := range cs {
		s := float64(vec.Int8Dot(q, ix.i8[x.doc]))
		out[i] = Hit{Doc: x.doc, Sim: s, HasSim: true, Score: s}
	}
	sortHits(out)
	return out
}

// leafValues fills vals with each query term's BM25 score for doc.
func (ix *Index) leafValues(q *text.Query, doc int, tfs []map[int]uint32, vals []float64) {
	for i, term := range q.Terms {
		tf := tfs[i][doc]
		if tf == 0 {
			vals[i] = 0
			continue
		}
		vals[i] = text.IDF(ix.n, ix.df[term]) * ix.BM25.TF(float64(tf), ix.dl[doc], ix.avgdl)
	}
}

func (ix *Index) termTFs(q *text.Query) []map[int]uint32 {
	tfs := make([]map[int]uint32, len(q.Terms))
	for i, term := range q.Terms {
		m := map[int]uint32{}
		for _, p := range ix.post[term] {
			m[p.doc] = p.tf
		}
		tfs[i] = m
	}
	return tfs
}

// Keyword returns the top ncand memories by keyword score, exhaustively.
func (ix *Index) Keyword(q *text.Query, ncand int) []Hit {
	tfs := ix.termTFs(q)
	docs := map[int]bool{}
	for _, m := range tfs {
		for d := range m {
			docs[d] = true
		}
	}
	vals := make([]float64, len(q.Terms))
	out := make([]Hit, 0, len(docs))
	for d := range docs {
		ix.leafValues(q, d, tfs, vals)
		if s := q.Score(ix.Weights, vals); s > 0 {
			out = append(out, Hit{Doc: d, BM25: s, Score: s})
		}
	}
	sortHits(out)
	return out[:min(ncand, len(out))]
}

// KeywordScore is one memory's keyword score.
func (ix *Index) KeywordScore(q *text.Query, doc int) float64 {
	tfs := ix.termTFs(q)
	vals := make([]float64, len(q.Terms))
	ix.leafValues(q, doc, tfs, vals)
	return q.Score(ix.Weights, vals)
}

// Fusion combines a vector ranking and a keyword ranking.
type Fusion struct {
	// Kind is "rrf" (reciprocal rank fusion) or "convex" (a weighted sum
	// of min-max normalized scores).
	Kind string
	// K is RRF's rank offset (60 by default).
	K float64
	// Alpha is the vector score's weight in a convex combination.
	Alpha float64
}

// Hybrid unions the top ncand by vector and by keyword, completes both
// scores for every candidate, and fuses them.
func (ix *Index) Hybrid(qv []float32, q *text.Query, ncand int, f Fusion) []Hit {
	byDoc := map[int]*Hit{}
	for _, h := range ix.Vector(qv, ncand) {
		h := h
		byDoc[h.Doc] = &h
	}
	for _, h := range ix.Keyword(q, ncand) {
		if have, ok := byDoc[h.Doc]; ok {
			have.BM25 = h.BM25
			continue
		}
		h := h
		byDoc[h.Doc] = &h
	}
	tfs := ix.termTFs(q)
	vals := make([]float64, len(q.Terms))
	hits := make([]Hit, 0, len(byDoc))
	for d, h := range byDoc {
		if h.BM25 == 0 {
			// A vector-only candidate: score it against the query's terms.
			ix.leafValues(q, d, tfs, vals)
			h.BM25 = q.Score(ix.Weights, vals)
		}
		if !h.HasSim && qv != nil && ix.i8 != nil {
			// A keyword-only candidate: its int8 cosine.
			h.Sim, h.HasSim = float64(vec.Int8Dot(qv, ix.i8[d])), true
		}
		hits = append(hits, *h)
	}
	Fuse(hits, f)
	sortHits(hits)
	return hits
}

// Fuse sets each hit's Score from its vector and keyword scores.
func Fuse(hits []Hit, f Fusion) {
	switch f.Kind {
	case "convex":
		minS, maxS := math.Inf(1), math.Inf(-1)
		maxB := 0.0
		for _, h := range hits {
			if h.HasSim {
				minS, maxS = math.Min(minS, h.Sim), math.Max(maxS, h.Sim)
			}
			maxB = math.Max(maxB, h.BM25)
		}
		for i := range hits {
			var v, k float64
			if hits[i].HasSim && maxS > minS {
				v = (hits[i].Sim - minS) / (maxS - minS)
			}
			if maxB > 0 {
				k = hits[i].BM25 / maxB
			}
			hits[i].Score = f.Alpha*v + (1-f.Alpha)*k
		}
	default:
		k := f.K
		if k == 0 {
			k = 60
		}
		vr := ranks(hits, func(h Hit) (float64, bool) { return h.Sim, h.HasSim })
		kr := ranks(hits, func(h Hit) (float64, bool) { return h.BM25, h.BM25 > 0 })
		for i := range hits {
			var s float64
			if vr[i] > 0 {
				s += 1 / (k + float64(vr[i]))
			}
			if kr[i] > 0 {
				s += 1 / (k + float64(kr[i]))
			}
			hits[i].Score = s
		}
	}
}

// ranks returns each hit's 1-based rank by a score, 0 where it has none.
func ranks(hits []Hit, score func(Hit) (float64, bool)) []int {
	idx := make([]int, 0, len(hits))
	for i, h := range hits {
		if _, ok := score(h); ok {
			idx = append(idx, i)
		}
	}
	sort.SliceStable(idx, func(a, b int) bool {
		sa, _ := score(hits[idx[a]])
		sb, _ := score(hits[idx[b]])
		if sa != sb {
			return sa > sb
		}
		return hits[idx[a]].Doc < hits[idx[b]].Doc
	})
	out := make([]int, len(hits))
	for r, i := range idx {
		out[i] = r + 1
	}
	return out
}

// sortHits orders by score, then by document for stable results.
func sortHits(h []Hit) {
	sort.SliceStable(h, func(a, b int) bool {
		if h[a].Score != h[b].Score {
			return h[a].Score > h[b].Score
		}
		return h[a].Doc < h[b].Doc
	})
}

// Docs returns the first k hits' documents.
func Docs(h []Hit, k int) []int {
	out := make([]int, 0, min(k, len(h)))
	for _, x := range h[:min(k, len(h))] {
		out = append(out, x.Doc)
	}
	return out
}
