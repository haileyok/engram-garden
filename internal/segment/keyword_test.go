package segment

import (
	"context"
	"fmt"
	"math/rand/v2"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/haileyok/engram-garden/internal/text"
)

// keywordDocs makes memories with a skewed vocabulary (so some terms have
// many full posting blocks), identifiers, and one memory repeating a word
// more than 255 times.
func keywordDocs(r *rand.Rand, n, dims int) []Doc {
	docs := makeDocs(r, n, dims)
	common := []string{"the", "memory", "deploy", "agent", "space", "search", "index", "running", "runs", "fixed"}
	idents := []string{"engram_space_uri", "ModelMismatch", "internal/spacestore/search.go", "did:plc:ewvi7nxzyoun6zhxrhs64oiz", "3mdj4meipr722"}
	for i := range docs {
		var words []string
		for range 5 + r.IntN(60) {
			switch x := r.Float64(); {
			case x < 0.6:
				words = append(words, common[int(float64(len(common))*r.Float64()*r.Float64())])
			case x < 0.75:
				words = append(words, idents[r.IntN(len(idents))])
			default:
				words = append(words, fmt.Sprintf("rare%d", r.IntN(n)))
			}
		}
		docs[i].Text = strings.Join(words, " ")
	}
	docs[n/2].Text += strings.Repeat(" spam", 300)
	return docs
}

type refPosting struct{ row, tf uint32 }

// reference analyzes the docs as read back (in final row order).
func reference(docs []Doc) (map[string][]refPosting, []uint32) {
	ref := map[string][]refPosting{}
	var lengths []uint32
	for row, d := range docs {
		a := text.AnalyzeMemory(d.Text, d.Tags, d.Source)
		lengths = append(lengths, a.Length)
		for t, tf := range a.TF {
			ref[t] = append(ref[t], refPosting{uint32(row), tf})
		}
	}
	return ref, lengths
}

func openV2(t *testing.T, docs []Doc, opt WriteOptions) (*Reader, *Keyword, []Doc, *countingReader) {
	t.Helper()
	ctx := context.Background()
	opt.Keyword = true
	b, info, err := Bytes(docs, opt)
	if err != nil {
		t.Fatal(err)
	}
	if info.Version != 2 {
		t.Fatalf("wrote version %d", info.Version)
	}
	cr := &countingReader{BytesReader: b}
	r, err := Open(ctx, cr)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Verify(ctx); err != nil {
		t.Fatal(err)
	}
	ix, err := r.LoadIndex(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got, err := r.ReadAll(ctx, ix)
	if err != nil {
		t.Fatal(err)
	}
	k, err := r.LoadKeyword(ctx)
	if err != nil || k == nil {
		t.Fatalf("keyword %v, err %v", k, err)
	}
	return r, k, got, cr
}

func TestKeywordRoundTrip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for _, cluster := range []int{0, 500} {
		docs := keywordDocs(rand.New(rand.NewPCG(3, uint64(cluster))), 1500, 32)
		r, k, got, _ := openV2(t, docs, WriteOptions{Dims: 32, ClusterThreshold: cluster})
		if r.Version() != 2 || !r.HasKeyword() || r.Analyzer() != text.Version {
			t.Fatalf("version %d, keyword %v, analyzer %d", r.Version(), r.HasKeyword(), r.Analyzer())
		}
		ref, lengths := reference(got)
		var total uint64
		for row, l := range lengths {
			if k.Norm(row) != text.EncodeLength(l) {
				t.Fatalf("row %d norm %d, want %d", row, k.Norm(row), text.EncodeLength(l))
			}
			total += uint64(text.DecodeLength(text.EncodeLength(l)))
		}
		if k.TotalLength != total || k.Terms != len(ref) {
			t.Fatalf("total %d (want %d), terms %d (want %d)", k.TotalLength, total, k.Terms, len(ref))
		}
		terms := make([]string, 0, len(ref)+2)
		for term := range ref {
			terms = append(terms, term)
		}
		slices.Sort(terms)
		terms = append(terms, "absent-term", "")
		infos, err := r.LookupTerms(ctx, k, terms)
		if err != nil {
			t.Fatal(err)
		}
		rng := rand.New(rand.NewPCG(9, 9))
		for i, term := range terms {
			want := ref[term]
			if len(want) == 0 {
				if infos[i].DF != 0 {
					t.Errorf("%q found with df %d", term, infos[i].DF)
				}
				continue
			}
			if int(infos[i].DF) != len(want) {
				t.Fatalf("%q df %d, want %d", term, infos[i].DF, len(want))
			}
			p, err := r.Postings(ctx, k, infos[i])
			if err != nil {
				t.Fatal(err)
			}
			var gotPs []refPosting
			it := p.Iter()
			for ; it.Row != NoRow; it.Next() {
				gotPs = append(gotPs, refPosting{it.Row, it.TF})
			}
			if it.Err() != nil || !slices.Equal(gotPs, want) {
				t.Fatalf("%q postings differ (err %v)", term, it.Err())
			}
			// Advance lands on the first posting at or after the target.
			it = p.Iter()
			target := uint32(0)
			for it.Row != NoRow {
				target += uint32(rng.IntN(200))
				it.Advance(target)
				j := sort.Search(len(want), func(j int) bool { return want[j].row >= target })
				if j == len(want) {
					if it.Row != NoRow {
						t.Fatalf("%q advance(%d) = %d, want end", term, target, it.Row)
					}
					break
				}
				if it.Row != want[j].row || it.TF != want[j].tf {
					t.Fatalf("%q advance(%d) = %d, want %d", term, target, it.Row, want[j].row)
				}
			}
		}
	}
}

// query builds a KeywordQuery against one segment, with the segment as the
// whole space.
func query(t *testing.T, r *Reader, k *Keyword, q string) *KeywordQuery {
	t.Helper()
	ctx := context.Background()
	pq := text.ParseQuery(q)
	infos, err := r.LookupTerms(ctx, k, pq.Terms)
	if err != nil {
		t.Fatal(err)
	}
	kq := &KeywordQuery{Q: &pq, Weights: text.DefaultWeights, BM25: text.DefaultBM25, AvgLength: float64(k.TotalLength) / float64(k.Count)}
	for _, ti := range infos {
		kq.IDF = append(kq.IDF, text.IDF(uint64(k.Count), uint64(ti.DF)))
		var p *Postings
		if ti.DF > 0 {
			if p, err = r.Postings(ctx, k, ti); err != nil {
				t.Fatal(err)
			}
		}
		kq.Postings = append(kq.Postings, p)
	}
	return kq
}

func TestPrunedEqualsExhaustive(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	docs := keywordDocs(rand.New(rand.NewPCG(4, 4)), 4000, 32)
	r, k, _, _ := openV2(t, docs, WriteOptions{Dims: 32})
	words := []string{"the", "memory", "deploy", "agent", "space", "search", "index", "run", "running", "fix",
		"engram_space_uri", "space uri", "ModelMismatch", "model", "search.go", "spam", "rare17", "rare3999", "nothing-here"}
	rng := rand.New(rand.NewPCG(5, 5))
	for trial := range 300 {
		var parts []string
		for range 1 + rng.IntN(5) {
			parts = append(parts, words[rng.IntN(len(words))])
		}
		q := strings.Join(parts, " ")
		kq := query(t, r, k, q)
		n := []int{1, 5, 50, 200}[rng.IntN(4)]
		var accept func(int) bool
		if trial%3 == 0 {
			mod := 2 + rng.IntN(5)
			accept = func(row int) bool { return row%mod != 0 }
		}
		want, err := TopKExhaustive(kq, k, n, accept)
		if err != nil {
			t.Fatal(err)
		}
		for _, win := range []int{1, 7, 128, window} {
			got, err := topK(ctx, kq, k, n, accept, win)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got, want) {
				t.Fatalf("%q n=%d window=%d: pruned %v\nexhaustive %v", q, n, win, got[:min(5, len(got))], want[:min(5, len(want))])
			}
		}
		// Scoring single rows agrees with the search.
		rows := make([]int, len(want))
		for i, h := range want {
			rows[i] = h.Row
		}
		rng.Shuffle(len(rows), func(i, j int) { rows[i], rows[j] = rows[j], rows[i] })
		scores, err := ScoreRows(kq, k, append(rows, 0, k.Count-1))
		if err != nil {
			t.Fatal(err)
		}
		byRow := map[int]float64{}
		for _, h := range want {
			byRow[h.Row] = h.Score
		}
		for i, row := range rows {
			if scores[i] != byRow[row] {
				t.Fatalf("%q row %d scored %v, search %v", q, row, scores[i], byRow[row])
			}
		}
	}
}

// A memory repeating a word hundreds of times saturates the block bound's
// tf byte; pruning must still find it.
func TestSaturatedTFBound(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	docs := keywordDocs(rand.New(rand.NewPCG(6, 6)), 600, 16)
	r, k, got, _ := openV2(t, docs, WriteOptions{Dims: 16})
	kq := query(t, r, k, "spam")
	hits, err := topK(ctx, kq, k, 1, nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || !strings.Contains(got[hits[0].Row].Text, strings.Repeat(" spam", 300)) {
		t.Fatalf("hits %v", hits)
	}
	maxTF, _ := kq.Postings[slices.Index(kq.Q.Terms, "spam")].BlockMax(0)
	if maxTF != 255 {
		t.Errorf("block max tf %d, want the saturated 255", maxTF)
	}

	// Two memories of the same length: the earlier repeats "spam" 260
	// times, the later 300. If the saturated byte were read as exactly 255,
	// the later one's bound would fall below the earlier one's score and it
	// would be skipped, though it scores higher.
	two := makeDocs(rand.New(rand.NewPCG(1, 1)), 300, 16)
	two[100].Text = strings.Repeat("spam ", 260) + strings.Repeat("filler ", 40)
	two[200].Text = strings.Repeat("spam ", 300)
	r2, k2, got2, _ := openV2(t, two, WriteOptions{Dims: 16})
	kq2 := query(t, r2, k2, "spam")
	want, err := TopKExhaustive(kq2, k2, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	pruned, err := topK(ctx, kq2, k2, 1, nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(want) != 1 || got2[want[0].Row].Text != two[200].Text || !slices.Equal(pruned, want) {
		t.Fatalf("pruned %v, exhaustive %v", pruned, want)
	}
}

func TestKeywordReadsAreSmall(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	docs := keywordDocs(rand.New(rand.NewPCG(7, 7)), 3000, 32)
	r, k, _, cr := openV2(t, docs, WriteOptions{Dims: 32})
	cr.reads, cr.bytes = 0, 0
	infos, err := r.LookupTerms(ctx, k, []string{"engram_space_uri", "deploy"})
	if err != nil {
		t.Fatal(err)
	}
	if cr.reads > 2 {
		t.Errorf("lookup took %d reads", cr.reads)
	}
	cr.reads = 0
	for _, ti := range infos {
		if _, err := r.Postings(ctx, k, ti); err != nil {
			t.Fatal(err)
		}
	}
	if cr.reads != 2 {
		t.Errorf("postings took %d reads, want one per term", cr.reads)
	}
	// LoadKeyword is one read.
	cr.reads = 0
	if _, err := r.LoadKeyword(ctx); err != nil || cr.reads != 1 {
		t.Errorf("LoadKeyword took %d reads, err %v", cr.reads, err)
	}
}

func TestKeywordCorruptionDetected(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	docs := keywordDocs(rand.New(rand.NewPCG(8, 8)), 400, 16)
	b, _, err := Bytes(docs, WriteOptions{Dims: 16, Keyword: true})
	if err != nil {
		t.Fatal(err)
	}
	r, err := Open(ctx, BytesReader(b))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []int{secNorms, secTermIndex, secTerms, secPostings} {
		bad := slices.Clone(b)
		sec := r.h.sections[s]
		bad[sec.off+sec.len/2] ^= 0x40
		br, err := Open(ctx, BytesReader(bad))
		if err != nil {
			continue
		}
		if err := br.Verify(ctx); err == nil {
			t.Errorf("corrupt %s section passed Verify", sectionNames[s])
		}
	}
	if _, err := parsePostings([]byte{1, 2, 3}, 300, 400); err == nil {
		t.Error("truncated postings parsed")
	}
	// A version this package doesn't know is refused.
	future := slices.Clone(b)
	future[8] = 3
	if _, err := Open(ctx, BytesReader(future)); err == nil {
		t.Error("version 3 opened")
	}
}

func TestPacking(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewPCG(1, 1))
	for _, w := range []int{0, 1, 3, 8, 13, 31, 32} {
		vals := make([]uint32, postingBlock)
		for i := range vals {
			if w > 0 {
				vals[i] = uint32(rng.Uint64() & (1<<w - 1))
			}
		}
		vals[0] = uint32(uint64(1)<<w - 1)
		b := appendPacked(nil, vals)
		out := make([]uint32, postingBlock)
		n, err := unpack(b, out)
		if err != nil || n != len(b) || !slices.Equal(out, vals) {
			t.Fatalf("width %d: used %d of %d, err %v", w, n, len(b), err)
		}
	}
}
