package appview

import (
	"slices"
	"testing"

	"github.com/haileyok/engram-garden/internal/spacestore"
	"github.com/haileyok/engram-garden/internal/text"
)

// Common words are left out of a result's matched terms, and the rest come
// strongest first.
func TestMatchedTermsSkipCommonWords(t *testing.T) {
	t.Parallel()
	pq := text.ParseQuery("what is the deploy process for engram")
	hit := spacestore.Hit{Text: "What is the deploy process for engram? It is a compose pull."}
	idf := make([]float64, len(pq.Terms))
	weight := map[string]float64{"what": 1.9, "deploy": 0.7, "engram": 0.5, "is": 0.2, "the": 0.01, "for": 0.3}
	for i, term := range pq.Terms {
		idf[i] = weight[term]
	}

	terms := func(idf []float64) []string {
		got, _ := matchedTerms(&pq, hit, idf)
		var out []string
		for _, m := range got {
			out = append(out, m.Term)
		}
		return out
	}
	if got, want := terms(idf), []string{"what", "deploy", "engram"}; !slices.Equal(got, want) {
		t.Fatalf("weighted: got %v, want %v", got, want)
	}
	// Without weights (a vector-only search) every contained term is shown.
	all := terms(nil)
	for _, w := range []string{"what", "is", "the", "for"} {
		if !slices.Contains(all, w) {
			t.Fatalf("unweighted: %q missing from %v", w, all)
		}
	}
}
