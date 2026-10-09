package segment

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"os"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/haileyok/engram-garden/internal/text"
)

// TestKeywordScale measures a large version 2 segment: section sizes,
// build time and query latency, pruned and exhaustive. It runs only with
// ENGRAM_KEYWORD_SCALE set to a memory count. ENGRAM_KEYWORD_CORPUS may
// name a JSONL file of {"text": ...} memories to grow from (kept outside
// the repository); each copy replaces about a tenth of its words with new
// ones so the vocabulary keeps growing like real text. Without it the text
// is synthetic.
func TestKeywordScale(t *testing.T) {
	n, _ := strconv.Atoi(os.Getenv("ENGRAM_KEYWORD_SCALE"))
	if n == 0 {
		t.Skip("set ENGRAM_KEYWORD_SCALE to a memory count")
	}
	ctx := context.Background()
	base := scaleBase(t)
	rng := rand.New(rand.NewPCG(1, 2))
	// Every word occurrence in the base, so sampling follows real word
	// frequencies.
	var pool []string
	for _, s := range base {
		pool = append(pool, strings.Fields(s)...)
	}
	docs := make([]Doc, n)
	var textBytes int64
	for i := range docs {
		words := strings.Fields(base[rng.IntN(len(base))])
		for j := range words {
			if rng.IntN(10) == 0 {
				words[j] = pool[rng.IntN(len(pool))]
			}
		}
		// One new token per memory, standing in for fresh identifiers.
		words = append(words, fmt.Sprintf("id%07x", i))
		docs[i] = Doc{ID: uint32(i), Author: "did:plc:a", Rkey: fmt.Sprintf("r%d", i), CID: "c", Text: strings.Join(words, " "), Bits: make([]byte, 8), Int8: make([]byte, 12)}
		textBytes += int64(len(docs[i].Text))
	}
	base, pool = nil, nil
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	start := time.Now()
	b, _, err := Bytes(docs, WriteOptions{Dims: 8, Keyword: true})
	if err != nil {
		t.Fatal(err)
	}
	build := time.Since(start)
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	docs = nil
	r, err := Open(ctx, BytesReader(b))
	if err != nil {
		t.Fatal(err)
	}
	k, err := r.LoadKeyword(ctx)
	if err != nil {
		t.Fatal(err)
	}
	sec := func(s int) int64 { return int64(r.h.sections[s].len) }
	kwBytes := sec(secNorms) + sec(secTermIndex) + sec(secTerms) + sec(secPostings)
	// A real segment at 768 dimensions: 96 + 772 bytes of vectors and 40
	// of metadata per memory, plus its compressed text.
	real768 := int64(n)*(96+772+40) + sec(secDocs) + sec(secDocIndex) + sec(secStrings)
	t.Logf("%d memories, %.0f bytes of text each; %d distinct terms", n, float64(textBytes)/float64(n), k.Terms)
	t.Logf("build %v (%.1f µs/memory), allocated %.0f MB", build.Round(time.Millisecond), float64(build.Microseconds())/float64(n), float64(after.TotalAlloc-before.TotalAlloc)/1e6)
	t.Logf("norms %.1f MB, term index %.1f MB (RAM %.1f MB), terms %.1f MB, postings %.1f MB: keyword %.1f MB, %.1f%% of a 768-dimension segment (%.0f MB)",
		mb(sec(secNorms)), mb(sec(secTermIndex)), mb(k.RAMBytes()), mb(sec(secTerms)), mb(sec(secPostings)), mb(kwBytes), 100*float64(kwBytes)/float64(real768), mb(real768))

	// Where the postings bytes go, by list length.
	termsSec, err := r.section(ctx, secTerms)
	if err != nil {
		t.Fatal(err)
	}
	type bucket struct {
		lists, postings int
		bytes           uint64
	}
	buckets := map[string]*bucket{}
	for b := range k.firsts {
		if err := parseTermBlock(termsSec[k.offs[b]:k.offs[b]+k.lens[b]], func(_ string, ti TermInfo) bool {
			name := "df 1 (inline)"
			switch {
			case ti.DF >= 1024:
				name = "df ≥ 1024"
			case ti.DF >= 128:
				name = "df 128–1023"
			case ti.DF >= 8:
				name = "df 8–127"
			case ti.DF >= 2:
				name = "df 2–7"
			}
			bk := buckets[name]
			if bk == nil {
				bk = &bucket{}
				buckets[name] = bk
			}
			bk.lists++
			bk.postings += int(ti.DF)
			bk.bytes += ti.len
			return true
		}); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"df 1 (inline)", "df 2–7", "df 8–127", "df 128–1023", "df ≥ 1024"} {
		if bk := buckets[name]; bk != nil {
			t.Logf("%-14s %8d lists %10d postings %8.1f MB  %.2f bytes/posting", name, bk.lists, bk.postings, mb(int64(bk.bytes)), float64(bk.bytes)/float64(max(bk.postings, 1)))
		}
	}

	for _, q := range scaleQueries() {
		kq := query(t, r, k, q)
		var pruned, exh []time.Duration
		var hits []KeywordHit
		for range 7 {
			s := time.Now()
			hits, err = TopK(ctx, kq, k, 200, nil)
			if err != nil {
				t.Fatal(err)
			}
			pruned = append(pruned, time.Since(s))
			s = time.Now()
			if _, err := TopKExhaustive(kq, k, 200, nil); err != nil {
				t.Fatal(err)
			}
			exh = append(exh, time.Since(s))
		}
		df := 0
		for _, p := range kq.Postings {
			if p != nil {
				df += p.DF
			}
		}
		t.Logf("%-40q postings %9d  hits %3d  pruned %8v  exhaustive %8v", q, df, len(hits), median(pruned), median(exh))
	}
}

func mb(n int64) float64 { return float64(n) / 1e6 }

func median(d []time.Duration) time.Duration {
	slices.Sort(d)
	return d[len(d)/2].Round(10 * time.Microsecond)
}

func scaleQueries() []string {
	if q := os.Getenv("ENGRAM_KEYWORD_QUERIES"); q != "" {
		return strings.Split(q, "|")
	}
	return []string{"the", "the memory agent", "deploy", "engram_space_uri", "space uri", "why did the deploy fail", "ModelMismatch search.go", "rare1234"}
}

func scaleBase(t *testing.T) []string {
	p := os.Getenv("ENGRAM_KEYWORD_CORPUS")
	if p == "" {
		docs := keywordDocs(rand.New(rand.NewPCG(1, 1)), 2000, 8)
		out := make([]string, len(docs))
		for i, d := range docs {
			out[i] = d.Text
		}
		return out
	}
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for sc.Scan() {
		var m struct {
			Text string `json:"text"`
		}
		if json.Unmarshal(sc.Bytes(), &m) == nil && m.Text != "" {
			out = append(out, m.Text)
		}
	}
	return out
}

var _ = text.Version
