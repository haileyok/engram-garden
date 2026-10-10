package appview

import (
	"math"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/haileyok/engram-garden/internal/spacestore"
	"github.com/haileyok/engram-garden/internal/text"
)

// matchView explains why a search result matched (garden.engram.defs
// #matchView). Lexicons have no floating point, so scores are scaled
// integers.
type matchView struct {
	Fusion  string        `json:"fusion,omitempty"`
	Vector  *vectorMatch  `json:"vector,omitempty"`
	Keyword *keywordMatch `json:"keyword,omitempty"`
	Snippet *snippetView  `json:"snippet,omitempty"`
}

type vectorMatch struct {
	// Rank is the result's rank by similarity among the results.
	Rank       int `json:"rank"`
	Similarity int `json:"similarity"`
}

type keywordMatch struct {
	// Rank is the result's rank by keyword score among the results.
	Rank int `json:"rank"`
	// Score is the BM25 keyword score × 100.
	Score int         `json:"score"`
	Terms []termMatch `json:"terms"`
}

type termMatch struct {
	Term  string `json:"term"`
	Kind  string `json:"kind"`
	Field string `json:"field"`
}

type snippetView struct {
	Field      string      `json:"field"`
	Text       string      `json:"text"`
	Highlights []byteRange `json:"highlights"`
}

// byteRange is a UTF-8 byte range in a snippet, like a rich-text facet's.
type byteRange struct {
	ByteStart int `json:"byteStart"`
	ByteEnd   int `json:"byteEnd"`
}

const snippetBytes = 200

// explain sets each result's match: its ranks by each score among the
// results, the query terms it contains (per query token, the exact form if
// present, else the stem, else the identifier's parts), and a snippet
// around them.
func explain(out []memoryView, hits []spacestore.Hit, query, mode string, idf []float64) {
	pq := text.ParseQuery(query)
	vrank := ranksBy(hits, func(h spacestore.Hit) (float64, bool) { return h.Similarity, h.HasSimilarity })
	krank := ranksBy(hits, func(h spacestore.Hit) (float64, bool) { return h.Keyword, h.Keyword > 0 })
	for i, h := range hits {
		m := &matchView{}
		if mode == "hybrid" {
			m.Fusion = "convex"
		}
		if h.HasSimilarity {
			m.Vector = &vectorMatch{Rank: vrank[i], Similarity: int(max(0, min(1, h.Similarity)) * 1000)}
		}
		terms, spans := matchedTerms(&pq, h, idf)
		if h.Keyword > 0 {
			m.Keyword = &keywordMatch{Rank: krank[i], Score: int(math.Round(h.Keyword * 100)), Terms: terms}
			m.Snippet = snippet(h, spans)
		}
		out[i].Match = m
	}
}

func ranksBy(hits []spacestore.Hit, score func(spacestore.Hit) (float64, bool)) []int {
	idx := make([]int, 0, len(hits))
	for i, h := range hits {
		if _, ok := score(h); ok {
			idx = append(idx, i)
		}
	}
	sort.SliceStable(idx, func(a, b int) bool {
		sa, _ := score(hits[idx[a]])
		sb, _ := score(hits[idx[b]])
		return sa > sb
	})
	out := make([]int, len(hits))
	for r, i := range idx {
		out[i] = r + 1
	}
	return out
}

// fieldSpan is where a matched term occurs.
type fieldSpan struct {
	field      text.Field
	tag        int
	start, end int
}

// matchedTerms finds which query terms the memory contains, and where.
func matchedTerms(pq *text.Query, h spacestore.Hit, idf []float64) ([]termMatch, []fieldSpan) {
	type occ struct {
		field text.Field
		spans []fieldSpan
	}
	found := map[string]*occ{}
	note := func(term string, f text.Field, tag int, start, end int) {
		o := found[term]
		if o == nil {
			o = &occ{field: f}
			found[term] = o
		}
		o.spans = append(o.spans, fieldSpan{f, tag, start, end})
	}
	scan := func(s string, f text.Field, tag int) {
		text.Tokenize(s, f, tag, func(t *text.Token) {
			note(t.Exact, f, tag, t.Span.Start, t.Span.End)
			if t.Stem != "" {
				note(t.Stem, f, tag, t.Span.Start, t.Span.End)
			}
			for _, p := range t.Parts {
				note(p.Term, f, tag, p.Span.Start, p.Span.End)
				if p.Stem != "" {
					note(p.Stem, f, tag, p.Span.Start, p.Span.End)
				}
			}
		})
	}
	scan(h.Text, text.FieldText, 0)
	for i, tag := range h.Tags {
		scan(tag, text.FieldTag, i)
	}
	scan(h.Source, text.FieldSource, 0)

	type hit struct {
		m     termMatch
		spans []fieldSpan
		w     float64
	}
	var hits []hit
	add := func(i int, kind string) bool {
		o := found[pq.Terms[i]]
		if o == nil {
			return false
		}
		w := 1.0
		if i < len(idf) {
			w = idf[i]
		}
		hits = append(hits, hit{termMatch{Term: pq.Terms[i], Kind: kind, Field: o.field.String()}, o.spans, w})
		return true
	}
	for _, g := range pq.Groups {
		if add(g.Exact, "exact") || g.Stem >= 0 && add(g.Stem, "stem") {
			continue
		}
		for _, p := range g.Parts {
			if !add(p.Term, "part") && p.Stem >= 0 {
				add(p.Stem, "stem")
			}
		}
	}
	// Common words (low IDF) match nearly everything and say little about
	// why this memory was found: show terms below the floor only when
	// nothing else matched, strongest first.
	sort.SliceStable(hits, func(a, b int) bool { return hits[a].w > hits[b].w })
	terms := []termMatch{}
	var spans []fieldSpan
	for i, x := range hits {
		if i == 0 || x.w >= minTermIDF {
			terms = append(terms, x.m)
			spans = append(spans, x.spans...)
		}
	}
	return terms, spans
}

// minTermIDF is the least IDF for a term to be shown as a match: about a
// word that appears in two thirds of a space's memories. It's an absolute
// floor, so one rare word in the query doesn't hide the others.
const minTermIDF = 0.4

// snippet picks the field with the most matches and a window of it around
// the first one, with the matches in the window highlighted.
func snippet(h spacestore.Hit, spans []fieldSpan) *snippetView {
	if len(spans) == 0 {
		return nil
	}
	count := map[[2]int]int{}
	for _, s := range spans {
		count[[2]int{int(s.field), s.tag}]++
	}
	best := [2]int{int(spans[0].field), spans[0].tag}
	for k, n := range count {
		if n > count[best] || n == count[best] && (k[0] < best[0] || k[0] == best[0] && k[1] < best[1]) {
			best = k
		}
	}
	field, tag := text.Field(best[0]), best[1]
	var src string
	switch field {
	case text.FieldText:
		src = h.Text
	case text.FieldTag:
		src = h.Tags[tag]
	default:
		src = h.Source
	}
	var in []fieldSpan
	for _, s := range spans {
		if s.field == field && s.tag == tag {
			in = append(in, s)
		}
	}
	sort.Slice(in, func(a, b int) bool { return in[a].start < in[b].start })
	start := max(0, in[0].start-snippetBytes/4)
	for start > 0 && !utf8.RuneStart(src[start]) {
		start--
	}
	end := min(len(src), start+snippetBytes)
	for end < len(src) && !utf8.RuneStart(src[end]) {
		end++
	}
	prefix, suffix := "", ""
	if start > 0 {
		prefix = "…"
	}
	if end < len(src) {
		suffix = "…"
	}
	sv := &snippetView{Field: field.String(), Text: prefix + strings.TrimSpace(src[start:end]) + suffix, Highlights: []byteRange{}}
	// Offsets shift by the prefix and by any leading space trimmed.
	shift := len(prefix) - (len(src[start:end]) - len(strings.TrimLeftFunc(src[start:end], unicode.IsSpace))) // the same trim as TrimSpace
	seen := map[[2]int]bool{}
	for _, s := range in {
		if s.start < start || s.end > end || seen[[2]int{s.start, s.end}] {
			continue
		}
		seen[[2]int{s.start, s.end}] = true
		sv.Highlights = append(sv.Highlights, byteRange{s.start - start + shift, s.end - start + shift})
	}
	return sv
}
