package spacestore

import (
	"context"
	"math"
	"sort"
	"sync"

	"github.com/RoaringBitmap/roaring/v2"

	"github.com/haileyok/engram-garden/internal/segment"
	"github.com/haileyok/engram-garden/internal/text"
	"github.com/haileyok/engram-garden/internal/vec"
)

// Hybrid search, as in docs/design/keyword-search.md: the keyword top
// candidates join the vector ones, every candidate gets both scores, and
// the two are fused.

// fusionAlpha is the vector score's weight, tuned with engram-eval.
const fusionAlpha = 0.5

// bufSnapshot is what a search needs of the buffer's keyword index, taken
// under the space's read lock.
type bufSnapshot struct {
	// terms holds, for each query term, the buffered memories with it.
	terms  []map[*bufEntry]uint32
	count  int
	length uint64
}

func (s *Space) snapshotBufKW(q *text.Query) bufSnapshot {
	snap := bufSnapshot{count: len(s.buf), length: s.bufKW.length, terms: make([]map[*bufEntry]uint32, len(q.Terms))}
	for i, t := range q.Terms {
		if m := s.bufKW.terms[t]; len(m) > 0 {
			c := make(map[*bufEntry]uint32, len(m))
			for e, tf := range m {
				c[e] = tf
			}
			snap.terms[i] = c
		}
	}
	return snap
}

// keywordCovered reports whether every segment has a keyword index from
// this analyzer. Scores never mix analyzer versions, and a space whose
// index isn't complete searches by vector only.
func keywordCovered(segs []*seg) bool {
	for _, sg := range segs {
		if sg.kw == nil || sg.kw.Analyzer != text.Version {
			return false
		}
	}
	return true
}

type candKey struct {
	seg *seg
	row int
	buf *bufEntry
}

func (c cand) key() candKey { return candKey{c.seg, c.row, c.buf} }

// keyID is a candidate's memory id, which grows with arrival order.
func keyID(k candKey) uint32 {
	if k.buf != nil {
		return k.buf.id
	}
	return k.seg.ix.ID(k.row)
}

// addKeyword adds the keyword top ncand to the vector candidates and
// completes both scores for all of them: each vector candidate's keyword
// score, and each keyword-only candidate's 1-bit distance (its int8
// cosine comes from the re-rank, like everyone's).
func (s *Space) addKeyword(ctx context.Context, pq *text.Query, f Filter, segs []*seg, buf bufSnapshot,
	pending map[uint32]struct{}, published *roaring.Bitmap, qbits []byte, dims int, cands []cand, ncand int) ([]cand, []float64, error) {
	// Space-wide statistics: every segment and the buffer, filters aside.
	type segTerms struct {
		sg    *seg
		infos []segment.TermInfo
		kq    *segment.KeywordQuery
	}
	per := make([]segTerms, len(segs))
	n := uint64(buf.count)
	total := buf.length
	df := make([]uint64, len(pq.Terms))
	for i, m := range buf.terms {
		df[i] += uint64(len(m))
	}
	var mu sync.Mutex
	var firstErr error
	var wg sync.WaitGroup
	for i, sg := range segs {
		n += uint64(sg.kw.Count)
		total += sg.kw.TotalLength
		wg.Go(func() {
			infos, err := sg.rd.LookupTerms(ctx, sg.kw, pq.Terms)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				firstErr = err
				return
			}
			per[i] = segTerms{sg: sg, infos: infos}
			for t, ti := range infos {
				df[t] += uint64(ti.DF)
			}
		})
	}
	wg.Wait()
	if firstErr != nil {
		return nil, nil, firstErr
	}
	if n == 0 {
		return cands, nil, nil
	}
	avg := float64(total) / float64(n)
	idf := make([]float64, len(pq.Terms))
	for i := range idf {
		idf[i] = text.IDF(n, df[i])
	}

	// Each segment's keyword top ncand, filtered like the vector scan.
	type kwHit struct {
		key   candKey
		score float64
	}
	var hits []kwHit
	for i := range per {
		st := &per[i]
		kq := &segment.KeywordQuery{Q: pq, Weights: text.DefaultWeights, BM25: text.DefaultBM25, IDF: idf, AvgLength: avg,
			Postings: make([]*segment.Postings, len(pq.Terms))}
		for t, ti := range st.infos {
			if ti.DF == 0 {
				continue
			}
			p, err := st.sg.rd.Postings(ctx, st.sg.kw, ti)
			if err != nil {
				return nil, nil, err
			}
			kq.Postings[t] = p
		}
		st.kq = kq
		sf, ok := f.resolve(st.sg)
		if !ok {
			continue
		}
		sg := st.sg
		top, err := segment.TopK(ctx, kq, sg.kw, ncand, func(row int) bool {
			return !s.isDeleted(sg.ix.ID(row), pending, published) && sf.match(sg.ix, row)
		})
		if err != nil {
			return nil, nil, err
		}
		for _, h := range top {
			hits = append(hits, kwHit{candKey{seg: sg, row: h.Row}, h.Score})
		}
	}
	// The buffer, scored directly.
	bufScore := func(e *bufEntry) float64 {
		vals := make([]float64, len(pq.Terms))
		for t, m := range buf.terms {
			if tf := m[e]; tf > 0 {
				vals[t] = idf[t] * text.DefaultBM25.TF(float64(tf), float64(text.DecodeLength(e.norm)), avg)
			}
		}
		return pq.Score(text.DefaultWeights, vals)
	}
	seen := map[*bufEntry]bool{}
	for _, m := range buf.terms {
		for e := range m {
			if seen[e] || e.bits[0] == nil || !f.matchDoc(&e.doc) {
				continue
			}
			seen[e] = true
			if sc := bufScore(e); sc > 0 {
				hits = append(hits, kwHit{candKey{buf: e}, sc})
			}
		}
	}
	// Ties go to the older memory (lower id), so results don't depend on
	// map order.
	sort.Slice(hits, func(a, b int) bool {
		if hits[a].score != hits[b].score {
			return hits[a].score > hits[b].score
		}
		return keyID(hits[a].key) < keyID(hits[b].key)
	})
	hits = hits[:min(ncand, len(hits))]

	// The union, with both scores for everyone.
	at := make(map[candKey]int, len(cands)+len(hits))
	for i, c := range cands {
		at[c.key()] = i
	}
	for _, h := range hits {
		if i, ok := at[h.key]; ok {
			cands[i].kw, cands[i].hasKW = h.score, true
			continue
		}
		c := cand{seg: h.key.seg, row: h.key.row, buf: h.key.buf, id: keyID(h.key), kw: h.score, hasKW: true}
		bl := vec.BitBytes(dims)
		switch {
		case qbits == nil:
			// Keyword-only search: no vector side.
		case c.buf != nil:
			c.dist = vec.Hamming(qbits, c.buf.bits[0])
		case c.seg.bits != nil:
			c.dist = vec.Hamming(qbits, c.seg.bits[c.row*bl:(c.row+1)*bl])
		default:
			b, err := c.seg.rd.BitsRange(ctx, c.row, c.row+1)
			if err != nil {
				return nil, nil, err
			}
			c.dist = vec.Hamming(qbits, b)
		}
		at[h.key] = len(cands)
		cands = append(cands, c)
	}
	// Vector-only candidates: their keyword scores.
	bySeg := map[*seg][]int{}
	for i, c := range cands {
		if c.hasKW {
			continue
		}
		if c.buf != nil {
			cands[i].kw, cands[i].hasKW = bufScore(c.buf), true
			continue
		}
		bySeg[c.seg] = append(bySeg[c.seg], i)
	}
	kqOf := map[*seg]*segment.KeywordQuery{}
	for _, st := range per {
		kqOf[st.sg] = st.kq
	}
	for sg, idxs := range bySeg {
		rows := make([]int, len(idxs))
		for k, i := range idxs {
			rows[k] = cands[i].row
		}
		scores, err := segment.ScoreRows(kqOf[sg], sg.kw, rows)
		if err != nil {
			return nil, nil, err
		}
		for k, i := range idxs {
			cands[i].kw, cands[i].hasKW = scores[k], true
		}
	}
	return cands, idf, nil
}

// fuse orders candidates by a convex combination of their min-max
// normalized cosine and keyword score.
func fuse(cands []cand) {
	minS, maxS, maxK := math.Inf(1), math.Inf(-1), 0.0
	for _, c := range cands {
		minS, maxS, maxK = math.Min(minS, c.sim), math.Max(maxS, c.sim), math.Max(maxK, c.kw)
	}
	for i := range cands {
		var v, k float64
		if maxS > minS {
			v = (cands[i].sim - minS) / (maxS - minS)
		}
		if maxK > 0 {
			k = cands[i].kw / maxK
		}
		cands[i].fused = fusionAlpha*v + (1-fusionAlpha)*k
	}
	sort.Slice(cands, func(a, b int) bool {
		if cands[a].fused != cands[b].fused {
			return cands[a].fused > cands[b].fused
		}
		return keyID(cands[a].key()) < keyID(cands[b].key())
	})
}
