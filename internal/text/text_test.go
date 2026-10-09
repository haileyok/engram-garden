package text

import (
	"fmt"
	"math"
	"math/rand/v2"
	"slices"
	"strings"
	"sync"
	"testing"
)

// render describes a field's tokens compactly: kind, exact term, stem, and
// parts with their stems.
func render(s string) []string {
	var out []string
	Tokenize(s, FieldText, 0, func(t *Token) {
		var b strings.Builder
		fmt.Fprintf(&b, "%s %s", t.Kind, t.Exact)
		if t.Stem != "" {
			fmt.Fprintf(&b, " %s", t.Stem)
		}
		if len(t.Parts) > 0 {
			b.WriteString(" [")
			for i, p := range t.Parts {
				if i > 0 {
					b.WriteString(" ")
				}
				b.WriteString(p.Term)
				if p.Stem != "" {
					b.WriteString("/" + p.Stem)
				}
			}
			b.WriteString("]")
		}
		out = append(out, b.String())
	})
	return out
}

func TestTokenize(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want []string
	}{
		{"Running tests", []string{"word running ~run", "word tests ~test"}},
		{"engram_space_uri", []string{"compound engram_space_uri [engram/~engram space/~space uri/~uri]"}},
		{"ModelMismatch", []string{"compound modelmismatch [model/~model mismatch/~mismatch]"}},
		{"HTTPServer", []string{"compound httpserver [http/~http server/~server]"}},
		// A plural s after an acronym isn't a new word.
		{"URLs", []string{"word urls ~url"}},
		{"v2beta", []string{"compound v2beta [beta/~beta]"}},
		{"sha256", []string{"compound sha256 [sha/~sha 256]"}},
		{"memory.add.", []string{"compound memory.add [memory/~memori add/~add]"}},
		{"see internal/spacestore/search.go", []string{
			"word see ~see",
			"compound internal/spacestore/search.go [internal/~intern spacestore/~spacestor search/~search go/~go]",
		}},
		{"hailey@blueskyweb.xyz", []string{"compound hailey@blueskyweb.xyz [hailey/~hailey blueskyweb/~blueskyweb xyz/~xyz]"}},
		{"#hashtag _private_", []string{"word hashtag ~hashtag", "word private ~privat"}},
		{"foo--bar", []string{"compound foo--bar [foo/~foo bar/~bar]"}},
		{"Hailey's notes", []string{"word hailey's ~hailey", "word notes ~note"}},
		{"Hailey’s", []string{"word hailey's ~hailey"}},
		{"3.14 and 1,000", []string{"compound 3.14 [14]", "word and ~and", "word 1,000"}},
		// Opaque identifiers stay whole, with no fragments.
		{"3fa9c2e1b07d4c5e8a6f", []string{"opaque 3fa9c2e1b07d4c5e8a6f"}},
		{"bafyreib2rxk3rybk3aobmv5cjuql3bm2twh4jo5uxgf", []string{"opaque bafyreib2rxk3rybk3aobmv5cjuql3bm2twh4jo5uxgf"}},
		{"1234567890123456", []string{"opaque 1234567890123456"}},
		{"did:plc:ewvi7nxzyoun6zhxrhs64oiz", []string{"compound did:plc:ewvi7nxzyoun6zhxrhs64oiz [did/~did plc/~plc ewvi7nxzyoun6zhxrhs64oiz]"}},
		// Record keys (TIDs) are 13 characters of base32-sortable.
		{"3mdiyhbljem22 at://did:plc:ewvi7nxzyoun6zhxrhs64oiz/app.bsky.feed.post/3mdj4meipr722", []string{
			"opaque 3mdiyhbljem22",
			"compound at://did:plc:ewvi7nxzyoun6zhxrhs64oiz/app.bsky.f#fe003590ab0a [at/~at did/~did plc/~plc ewvi7nxzyoun6zhxrhs64oiz app/~app bsky/~bski feed/~feed post/~post 3mdj4meipr722]",
		}},
		// A 13-letter word isn't a record key.
		{"Understanding", []string{"word understanding ~understand"}},
		// A long all-letter word isn't opaque.
		{"internationalization", []string{"word internationalization ~internation"}},
		{"記憶検索API", []string{"cjk 記憶", "cjk 憶検", "cjk 検索", "word api ~api"}},
		{"字", []string{"cjk 字"}},
		{"한국어 검색", []string{"cjk 한국", "cjk 국어", "cjk 검색"}},
		{"e.g. a", []string{"compound e.g", "word a ~a"}},
		{"🙂 — …", nil},
	}
	for _, c := range cases {
		if got := render(c.in); !slices.Equal(got, c.want) {
			t.Errorf("%q:\n got %q\nwant %q", c.in, got, c.want)
		}
	}
}

func TestNormalize(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"café":                "cafe",
		"cafe\u0301":          "cafe", // decomposed
		"STRASSE straße":      "strasse strasse",
		"ﬁle":                 "file",
		"Ⅻ":                   "xii",
		"İstanbul":            "istanbul",
		"ＡＢＣ":                 "abc", // fullwidth
		"한국어":                 "한국어", // Hangul recomposed after NFKD
		"Hailey\u2019s":       "hailey's",
		"already lower ascii": "already lower ascii",
	}
	for in, want := range cases {
		if got := normalize(in); got != want {
			t.Errorf("normalize(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSpansPointAtTheOriginal(t *testing.T) {
	t.Parallel()
	s := "Ünïcode ModelMismatch in internal/spacestore/search.go, 記憶検索 and did:plc:ewvi7nxzyoun6zhxrhs64oiz."
	Tokenize(s, FieldText, 0, func(tok *Token) {
		if normalize(s[tok.Span.Start:tok.Span.End]) != tok.Exact && !strings.Contains(tok.Exact, "#") {
			t.Errorf("token %q spans %q", tok.Exact, s[tok.Span.Start:tok.Span.End])
		}
		for _, p := range tok.Parts {
			if normalize(s[p.Span.Start:p.Span.End]) != p.Term {
				t.Errorf("part %q spans %q", p.Term, s[p.Span.Start:p.Span.End])
			}
			if p.Span.Start < tok.Span.Start || p.Span.End > tok.Span.End {
				t.Errorf("part %q outside its token", p.Term)
			}
		}
	})
}

func TestBound(t *testing.T) {
	t.Parallel()
	short := strings.Repeat("a", maxTermBytes)
	if bound(short) != short {
		t.Error("a term at the limit changed")
	}
	a := "https://example.com/" + strings.Repeat("x", 60) + "/one"
	b := "https://example.com/" + strings.Repeat("x", 60) + "/two"
	ba, bb := bound(a), bound(b)
	if ba == bb || len(ba) > maxTermBytes || !strings.HasPrefix(ba, a[:boundPrefix]) {
		t.Errorf("bound(%q) = %q, bound(%q) = %q", a, ba, b, bb)
	}
	// Never cut inside a character.
	multi := strings.Repeat("é", 40) // 2 bytes each
	if bm := bound(multi); !strings.HasPrefix(bm, strings.Repeat("é", 24)+"#") {
		t.Errorf("bound cut a character: %q", bm)
	}
	odd := "a" + strings.Repeat("é", 40)
	if bo := bound(odd); !strings.HasPrefix(bo, "a"+strings.Repeat("é", 23)+"#") {
		t.Errorf("bound cut a character: %q", bo)
	}
	// The query side bounds the same way, so exact lookup works.
	q := ParseQuery(a)
	if len(q.Groups) != 1 || q.Terms[q.Groups[0].Exact] != ba {
		t.Errorf("query for a long term = %+v, want exact %q", q, ba)
	}
}

func TestAnalyzeMemoryCounting(t *testing.T) {
	t.Parallel()
	d := AnalyzeMemory("space_space space Running runs", []string{"Infra"}, "https://example.com/space")
	// Tokens: space_space, space, Running, runs, Infra, https://example.com/space.
	if d.Length != 6 {
		t.Errorf("length = %d, want 6", d.Length)
	}
	want := map[string]uint32{
		"space_space": 1,
		// space_space adds 1 to space, not 2; plain space and the URL's
		// last part add one each.
		"space": 3, "~space": 3,
		"running": 1, "runs": 1, "~run": 2,
		"infra": 1, "~infra": 1,
		"https://example.com/space": 1, "https": 1, "~https": 1,
		"example": 1, "~exampl": 1, "com": 1, "~com": 1,
	}
	for term, n := range want {
		if d.TF[term] != n {
			t.Errorf("tf[%q] = %d, want %d", term, d.TF[term], n)
		}
	}
	for term := range d.TF {
		if _, ok := want[term]; !ok {
			t.Errorf("unexpected term %q", term)
		}
	}
}

func TestParseQuery(t *testing.T) {
	t.Parallel()
	q := ParseQuery("Space space SPACE engram_space_uri space_space running")
	var texts []string
	for _, g := range q.Groups {
		texts = append(texts, g.Text)
	}
	if !slices.Equal(texts, []string{"space", "engram_space_uri", "space_space", "running"}) {
		t.Fatalf("groups = %q", texts)
	}
	// Terms are shared: space from the word and the parts is one term.
	n := 0
	for _, term := range q.Terms {
		if term == "space" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("space appears %d times in %q", n, q.Terms)
	}
	if ss := q.Groups[2]; len(ss.Parts) != 1 {
		t.Errorf("space_space has %d parts, want 1", len(ss.Parts))
	}
	if q.Truncated {
		t.Error("truncated")
	}
}

func TestParseQueryLimits(t *testing.T) {
	t.Parallel()
	var words []string
	for i := range 40 {
		words = append(words, fmt.Sprintf("word%c%c", 'a'+i/26, 'a'+i%26))
	}
	q := ParseQuery(strings.Join(words, " "))
	if len(q.Groups) != MaxQueryTokens || !q.Truncated {
		t.Errorf("%d groups, truncated %v", len(q.Groups), q.Truncated)
	}
	// The term limit: identifiers with many parts.
	var ids []string
	for i := range 10 {
		var parts []string
		for j := range 8 {
			parts = append(parts, fmt.Sprintf("p%c%c", 'a'+i, 'a'+j))
		}
		ids = append(ids, strings.Join(parts, "_"))
	}
	q = ParseQuery(strings.Join(ids, " "))
	if len(q.Terms) > MaxQueryTerms || !q.Truncated {
		t.Errorf("%d terms, truncated %v", len(q.Terms), q.Truncated)
	}
	// One compound with too many parts keeps its first MaxQueryParts.
	var many []string
	for j := range 30 {
		many = append(many, fmt.Sprintf("x%c%c", 'a'+j/26, 'a'+j%26))
	}
	q = ParseQuery(strings.Join(many, "_"))
	if len(q.Groups) != 1 || len(q.Groups[0].Parts) != MaxQueryParts {
		t.Errorf("groups %d, parts %d", len(q.Groups), len(q.Groups[0].Parts))
	}
}

func vals(q *Query, m map[string]float64) []float64 {
	v := make([]float64, len(q.Terms))
	for i, term := range q.Terms {
		v[i] = m[term]
	}
	return v
}

func TestScoreTakesTheBestAlternative(t *testing.T) {
	t.Parallel()
	w := Weights{Exact: 1.0, Stem: 0.5, Parts: 0.4, PartStem: 0.5}
	q := ParseQuery("running")
	// Exact 1.0 and stem 0.8: the token contributes 1.0, not 1.8.
	if got := q.Score(w, vals(&q, map[string]float64{"running": 1, "~run": 1.6})); got != 1 {
		t.Errorf("score = %v, want 1", got)
	}
	if got := q.Score(w, vals(&q, map[string]float64{"~run": 4})); got != 2 {
		t.Errorf("stem-only score = %v, want 2", got)
	}

	q = ParseQuery("engram_space_uri")
	// Parts sum within the compound, then compete with the exact form.
	partsOnly := vals(&q, map[string]float64{"space": 2, "~uri": 2})
	if got, want := q.Score(w, partsOnly), 0.4*(2+0.5*2); math.Abs(got-want) > 1e-12 {
		t.Errorf("parts score = %v, want %v", got, want)
	}
	ex := q.Explain(w, partsOnly)
	if len(ex) != 1 || len(ex[0].Terms) != 2 || ex[0].Terms[0].Kind != MatchPart || ex[0].Terms[1].Kind != MatchPartStem {
		t.Errorf("explain = %+v", ex)
	}
	both := vals(&q, map[string]float64{"engram_space_uri": 5, "space": 2, "uri": 2})
	if got := q.Score(w, both); got != 5 {
		t.Errorf("score = %v, want 5", got)
	}
	if ex := q.Explain(w, both); len(ex) != 1 || len(ex[0].Terms) != 1 || q.Terms[ex[0].Terms[0].Term] != "engram_space_uri" {
		t.Errorf("explain = %+v", ex)
	}
	if ex := q.Explain(w, make([]float64, len(q.Terms))); len(ex) != 0 {
		t.Errorf("explain with no matches = %+v", ex)
	}
}

// Scoring per-term upper bounds must bound the score, whatever the values:
// this is what makes pruning safe.
func TestScoreOfBoundsIsABound(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewPCG(1, 2))
	queries := []string{
		"running engram_space_uri space_space did:plc:ewvi7nxzyoun6zhxrhs64oiz 記憶検索",
		"ModelMismatch model mismatch",
		"the the a an of",
	}
	for _, qs := range queries {
		q := ParseQuery(qs)
		for range 2000 {
			actual := make([]float64, len(q.Terms))
			bounds := make([]float64, len(q.Terms))
			for i := range actual {
				if rng.IntN(3) > 0 {
					actual[i] = rng.Float64() * 10
				}
				bounds[i] = actual[i] + rng.Float64()*3*float64(rng.IntN(2))
			}
			if s, b := q.Score(DefaultWeights, actual), q.Score(DefaultWeights, bounds); s > b+1e-9 {
				t.Fatalf("%q: score %v exceeds bound %v", qs, s, b)
			}
		}
	}
}

func TestBM25(t *testing.T) {
	t.Parallel()
	p := DefaultBM25
	for _, df := range []uint64{0, 1, 500, 999, 1000, 2000} {
		if IDF(1000, df) <= 0 {
			t.Errorf("IDF(1000, %d) = %v, want > 0", df, IDF(1000, df))
		}
	}
	if IDF(1000, 1) <= IDF(1000, 10) {
		t.Error("rarer terms should have higher IDF")
	}
	prev := 0.0
	for tf := 1.0; tf < 1e6; tf *= 2 {
		v := p.TF(tf, 50, 50)
		if v <= prev || v >= p.TFLimit() {
			t.Fatalf("TF(%v) = %v (prev %v, limit %v)", tf, v, prev, p.TFLimit())
		}
		prev = v
	}
	if p.TF(3, 10, 50) <= p.TF(3, 100, 50) {
		t.Error("longer memories should score lower")
	}
	if p.TF(0, 10, 50) != 0 {
		t.Error("absent terms should score 0")
	}
}

func TestLengthEncoding(t *testing.T) {
	t.Parallel()
	for n := range uint32(exactLengths) {
		if DecodeLength(EncodeLength(n)) != n {
			t.Errorf("length %d isn't exact", n)
		}
	}
	for b := 1; b < 256; b++ {
		if lengthTable[b] <= lengthTable[b-1] {
			t.Fatalf("table not increasing at %d", b)
		}
		if EncodeLength(lengthTable[b]) != byte(b) {
			t.Errorf("EncodeLength(DecodeLength(%d)) = %d", b, EncodeLength(lengthTable[b]))
		}
	}
	// Memories are at most 30,000 characters plus tags and source; the
	// table must reach past any real length with small steps.
	if top := DecodeLength(255); top < 20000 {
		t.Errorf("largest length %d, want at least 20000", top)
	}
	for n := uint32(0); n < 100000; n += 7 {
		d := DecodeLength(EncodeLength(n))
		if d > n {
			t.Fatalf("length %d decodes to %d: must round down", n, d)
		}
		if n < DecodeLength(255) && float64(n-d) > 0.04*float64(n)+1 {
			t.Fatalf("length %d decodes to %d: step too coarse", n, d)
		}
	}
}

func TestConcurrentUse(t *testing.T) {
	t.Parallel()
	const s = "Straße café ModelMismatch 記憶検索 engram_space_uri"
	want := render(s)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 50 {
				if got := render(s); !slices.Equal(got, want) {
					t.Errorf("concurrent tokenize = %q", got)
					return
				}
			}
		})
	}
	wg.Wait()
}
