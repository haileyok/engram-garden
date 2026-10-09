package eval

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/haileyok/engram-garden/internal/embed"
	"github.com/haileyok/engram-garden/internal/text"
)

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestMetrics(t *testing.T) {
	t.Parallel()
	g := Grades{1: 2, 3: 1, 9: 0}
	ranked := []int{5, 3, 1, 7}
	if v := RecallAt(ranked, g, 2); !near(v, 0.5) {
		t.Errorf("recall@2 = %v", v)
	}
	if v := MRRAt(ranked, g, 10); !near(v, 0.5) {
		t.Errorf("mrr = %v", v)
	}
	// DCG: rank 2 grade 1 → 1/log2(3); rank 3 grade 2 → 3/log2(4).
	// Ideal: 3/log2(2) + 1/log2(3).
	want := (1/math.Log2(3) + 3/2.0) / (3 + 1/math.Log2(3))
	if v := NDCGAt(ranked, g, 10); !near(v, want) {
		t.Errorf("ndcg = %v, want %v", v, want)
	}
	if !math.IsNaN(NDCGAt(ranked, Grades{4: 0}, 10)) || !math.IsNaN(MRRAt(ranked, Grades{}, 10)) {
		t.Error("metrics without relevant memories should be NaN")
	}
	if m, n := Mean([]float64{1, math.NaN(), 3}); m != 2 || n != 2 {
		t.Errorf("mean = %v over %d", m, n)
	}
}

func TestBootstrapDiff(t *testing.T) {
	t.Parallel()
	a, b := make([]float64, 200), make([]float64, 200)
	for i := range a {
		a[i] = 0.5 + 0.01*float64(i%7)
		b[i] = 0.4 + 0.01*float64(i%5)
	}
	d, lo, hi, n := BootstrapDiff(a, b, 2000, 1)
	if n != 200 || !(lo < d && d < hi) || lo <= 0 {
		t.Errorf("diff %v [%v, %v] over %d", d, lo, hi, n)
	}
}

func TestCohensKappa(t *testing.T) {
	t.Parallel()
	if k := CohensKappa([]int{0, 1, 2, 2}, []int{0, 1, 2, 2}); !near(k, 1) {
		t.Errorf("perfect agreement κ = %v", k)
	}
	if k := CohensKappa([]int{0, 0, 1, 1}, []int{1, 1, 0, 0}); k >= 0 {
		t.Errorf("total disagreement κ = %v", k)
	}
}

// corpus builds a small index whose vectors come from the hashing embedder
// (similarity is word overlap).
func corpus(t *testing.T, mems []Memory) (*Index, func(string) []float32) {
	t.Helper()
	h := embed.Hashing{Dims: 256}
	texts := make([]string, len(mems))
	for i, m := range mems {
		texts[i] = m.Text
	}
	vs, err := h.Embed(context.Background(), texts)
	if err != nil {
		t.Fatal(err)
	}
	embedQ := func(q string) []float32 {
		v, err := h.Embed(context.Background(), []string{q})
		if err != nil {
			t.Fatal(err)
		}
		return v[0]
	}
	return NewIndex(mems, vs), embedQ
}

var testMems = []Memory{
	{ID: "a", Text: "The harness stores the engram_space_uri in memory_config."},
	{ID: "b", Text: "Deploys go through Argo; watch the rollout and the logs."},
	{ID: "c", Text: "Penny prefers short replies and dislikes emoji."},
	{ID: "d", Text: "The space URI for a memory space is an at:// URI.", Tags: []string{"spaces"}},
	{ID: "e", Text: "Reembed failed with KeyError uri against cocoon listRecords."},
}

func TestKeywordFindsIdentifiers(t *testing.T) {
	t.Parallel()
	ix, _ := corpus(t, testMems)
	q := text.ParseQuery("engram_space_uri")
	hits := ix.Keyword(&q, 10)
	if len(hits) == 0 || ix.Mems[hits[0].Doc].ID != "a" {
		t.Fatalf("hits = %+v", hits)
	}
	// "space uri" finds the identifier through its parts, and the memory
	// that says it in words.
	q = text.ParseQuery("space uri")
	got := map[string]bool{}
	for _, h := range ix.Keyword(&q, 10) {
		got[ix.Mems[h.Doc].ID] = true
	}
	if !got["a"] || !got["d"] {
		t.Errorf("space uri found %v", got)
	}
}

func TestHybridCompletesBothScores(t *testing.T) {
	t.Parallel()
	ix, embedQ := corpus(t, testMems)
	q := text.ParseQuery("KeyError listRecords")
	// Only one vector candidate, so e is keyword-only unless it's also the
	// vector top hit; either way every hit must carry both scores.
	hits := ix.Hybrid(embedQ("argo rollout logs"), &q, 1, Fusion{Kind: "rrf"})
	if len(hits) != 2 {
		t.Fatalf("hits = %+v", hits)
	}
	for _, h := range hits {
		if !h.HasSim {
			t.Errorf("%s has no vector score", ix.Mems[h.Doc].ID)
		}
		if ix.Mems[h.Doc].ID == "e" && h.BM25 <= 0 {
			t.Error("keyword match lost its keyword score")
		}
	}
}

func TestFuse(t *testing.T) {
	t.Parallel()
	hits := []Hit{
		{Doc: 0, Sim: 0.9, HasSim: true},
		{Doc: 1, Sim: 0.5, HasSim: true, BM25: 7},
		{Doc: 2, BM25: 3},
	}
	Fuse(hits, Fusion{Kind: "rrf", K: 60})
	if !near(hits[0].Score, 1.0/61) || !near(hits[1].Score, 1.0/62+1.0/61) || !near(hits[2].Score, 1.0/62) {
		t.Errorf("rrf = %v %v %v", hits[0].Score, hits[1].Score, hits[2].Score)
	}
	Fuse(hits, Fusion{Kind: "convex", Alpha: 0.5})
	if !near(hits[0].Score, 0.5) || !near(hits[1].Score, 0.5) || !near(hits[2].Score, 0.5*3/7) {
		t.Errorf("convex = %v %v %v", hits[0].Score, hits[1].Score, hits[2].Score)
	}
}

func TestGradesAndPool(t *testing.T) {
	t.Parallel()
	ix, embedQ := corpus(t, testMems)
	qs := []Query{
		{ID: "q1", Category: CatIdentifier, Text: "engram_space_uri", Target: "a"},
		{ID: "q2", Category: CatNoAnswer, Text: "kubernetes ingress certificates"},
	}
	e := NewExperiment(ix, qs, [][]float32{embedQ(qs[0].Text), embedQ(qs[1].Text)})
	r := e.Run([]System{VectorSystem, KeywordSystem, HybridSystem(Fusion{Kind: "rrf"})}, 20)
	pool := e.Pool(r, 20)
	if len(pool[0]) == 0 {
		t.Fatal("empty pool")
	}
	g := e.Grades([]Judgment{
		{Query: "q1", Memory: "d", Grade: 1, By: "model"},
		{Query: "q1", Memory: "a", Grade: 0, By: "model"}, // the target stays 2
		{Query: "q1", Memory: "b", Grade: 2, By: "human"},
		{Query: "q1", Memory: "b", Grade: 0, By: "model"}, // human wins
	})
	if g[0][0] != 2 || g[0][3] != 1 || g[0][1] != 2 {
		t.Errorf("grades = %v", g[0])
	}
	if Answerable(g[1]) {
		t.Error("no-answer query has relevant memories")
	}
}

// fakeChat answers chat completions with answer(prompt) and counts calls.
func fakeChat(t *testing.T, answer func(user string) any) (*LLM, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/v1/chat/completions" || r.Header.Get("X-Test") != "yes" {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Messages []chatMessage `json:"messages"`
		}
		_ = json.Unmarshal(body, &req)
		content, _ := json.Marshal(answer(req.Messages[1].Content))
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": map[string]string{"content": "```json\n" + string(content) + "\n```"}}},
		})
	}))
	t.Cleanup(srv.Close)
	env := map[string]string{
		"ENGRAM_EVAL_LLM_URL":     srv.URL + "/v1",
		"ENGRAM_EVAL_LLM_MODEL":   "test",
		"ENGRAM_EVAL_LLM_HEADERS": "X-Test: yes",
	}
	l, err := LLMFromEnv(func(k string) string { return env[k] }, t.TempDir(), 4)
	if err != nil {
		t.Fatal(err)
	}
	return l, &calls
}

func TestLLMCachesAnswers(t *testing.T) {
	t.Parallel()
	l, calls := fakeChat(t, func(string) any { return map[string]int{"n": 7} })
	for range 3 {
		var out struct{ N int }
		if err := l.JSON(context.Background(), "sys", "user", &out); err != nil || out.N != 7 {
			t.Fatalf("out %+v, err %v", out, err)
		}
	}
	if calls.Load() != 1 {
		t.Errorf("%d calls, want 1", calls.Load())
	}
	if _, err := LLMFromEnv(func(string) string { return "" }, "", 1); err == nil {
		t.Error("missing configuration accepted")
	}
}

func TestGenerateValidatesCategories(t *testing.T) {
	t.Parallel()
	ix, _ := corpus(t, testMems)
	l, _ := fakeChat(t, func(user string) any {
		ans := map[string]string{CatParaphrase: "where is the space address kept"}
		if strings.Contains(user, `"engram_space_uri"`) {
			ans[CatIdentifier] = "engram_space_uri"
			// A part query that copies the identifier is dropped.
			ans[CatPart] = "engram_space_uri config"
		}
		return ans
	})
	qs, err := Generate(context.Background(), l, ix, len(ix.Mems), 1)
	if err != nil {
		t.Fatal(err)
	}
	cats := map[string]int{}
	for _, q := range qs {
		cats[q.Category]++
		if q.Target == "" || q.ID != QueryID(q.Category, q.Text, q.Target) {
			t.Errorf("bad query %+v", q)
		}
	}
	if cats[CatParaphrase] != len(ix.Mems) || cats[CatIdentifier] != 1 || cats[CatPart] != 0 {
		t.Errorf("categories = %v", cats)
	}
}

func TestIdentifierLike(t *testing.T) {
	t.Parallel()
	for s, want := range map[string]bool{
		"engram_space_uri": true, "memory.add": true, "ModelMismatch": true, "v2beta": true,
		"self-experiment": false, "coherence-preserving": false, "Well-known": false,
	} {
		if identifierLike(s) != want {
			t.Errorf("identifierLike(%q) = %v", s, !want)
		}
	}
}

func TestJudgeBatchesAndChecksCounts(t *testing.T) {
	t.Parallel()
	mems := make([]Memory, 45)
	for i := range mems {
		mems[i] = Memory{ID: string(rune('a' + i%26)), Text: strings.Repeat("memory ", i+1)}
	}
	l, calls := fakeChat(t, func(user string) any {
		n := strings.Count(user, "Memory ")
		g := make([]int, n)
		for i := range g {
			g[i] = 5 // clamped to 2
		}
		return map[string][]int{"grades": g}
	})
	grades, err := Judge(context.Background(), l, "q", mems)
	if err != nil || len(grades) != 45 || grades[0] != 2 {
		t.Fatalf("grades %v, err %v", grades, err)
	}
	if calls.Load() != 3 {
		t.Errorf("%d calls, want 3", calls.Load())
	}
	bad, _ := fakeChat(t, func(string) any { return map[string][]int{"grades": {1}} })
	if _, err := Judge(context.Background(), bad, "q2", mems[:3]); err == nil {
		t.Error("a wrong number of grades was accepted")
	}
}
