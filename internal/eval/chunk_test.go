package eval

import (
	"strings"
	"testing"
)

func TestChunkShortStaysWhole(t *testing.T) {
	t.Parallel()
	p := ChunkParams{Threshold: 100, Target: 50, Overlap: 10}
	s := strings.Repeat("word ", 20)
	if got := Chunk(s, p); len(got) != 1 || got[0] != s {
		t.Fatalf("got %d chunks, want the text whole", len(got))
	}
	if got := Chunk("", p); got != nil {
		t.Fatalf("empty text gave %v", got)
	}
}

func TestChunkWindowsOverlapAndCover(t *testing.T) {
	t.Parallel()
	p := ChunkParams{Threshold: 100, Target: 50, Overlap: 10}
	var words []string
	for i := range 200 {
		words = append(words, "w"+strings.Repeat("x", i%7))
	}
	s := strings.Join(words, " ")
	cs := Chunk(s, p)
	if len(cs) < 5 {
		t.Fatalf("got %d chunks for %d runes", len(cs), len(s))
	}
	for i, c := range cs {
		if len([]rune(c)) > p.Target {
			t.Errorf("chunk %d has %d runes, over the target %d", i, len([]rune(c)), p.Target)
		}
		if i > 0 {
			// Each window starts inside the previous one: no gap.
			if !strings.Contains(s, cs[i-1][len(cs[i-1])-5:]+c[:5]) && !overlaps(cs[i-1], c) {
				t.Errorf("chunk %d doesn't overlap chunk %d", i, i-1)
			}
		}
	}
	if !strings.HasSuffix(s, cs[len(cs)-1]) {
		t.Error("the last chunk isn't the end of the text")
	}
	if !strings.HasPrefix(s, cs[0]) {
		t.Error("the first chunk isn't the start of the text")
	}
}

func overlaps(a, b string) bool {
	for n := min(len(a), len(b)); n > 0; n-- {
		if strings.HasSuffix(a, b[:n]) {
			return true
		}
	}
	return false
}

func TestChunkMultibyteNeverSplitsARune(t *testing.T) {
	t.Parallel()
	s := strings.Repeat("héllo wörld 🙂 ", 300)
	for i, c := range Chunk(s, ChunkParams{Threshold: 100, Target: 77, Overlap: 13}) {
		if !strings.ContainsRune(s, []rune(c)[0]) || strings.ToValidUTF8(c, "") != c {
			t.Fatalf("chunk %d isn't valid UTF-8", i)
		}
	}
}

func TestIndexRanksByBestVector(t *testing.T) {
	t.Parallel()
	mems := []Memory{{ID: "a", Text: "alpha"}, {ID: "b", Text: "beta"}}
	// Memory a's second vector matches the query; b's only one is orthogonal.
	rows := [][][]float32{
		{{0, 1, 0, 0}, {1, 0, 0, 0}},
		{{0, 0, 1, 0}},
	}
	ix := NewIndexRows(mems, rows)
	hits := ix.Vector([]float32{1, 0, 0, 0}, 10)
	if len(hits) != 2 || hits[0].Doc != 0 || hits[0].Sim < 0.99 {
		t.Fatalf("hits = %+v, want memory a first with its best vector's similarity", hits)
	}
}
