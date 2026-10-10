package appview

import (
	"slices"
	"testing"

	"github.com/haileyok/engram-garden/internal/spacestore"
	"github.com/haileyok/engram-garden/internal/text"
)

func matchedWords(t *testing.T, query, body string, weight map[string]float64) []string {
	t.Helper()
	pq := text.ParseQuery(query)
	var idf []float64
	if weight != nil {
		idf = make([]float64, len(pq.Terms))
		for i, term := range pq.Terms {
			idf[i] = weight[term]
		}
	}
	got, _ := matchedTerms(&pq, spacestore.Hit{Text: body}, idf)
	out := []string{}
	for _, m := range got {
		out = append(out, m.Term)
	}
	return out
}

// Common words are left out of a result's matched terms, whatever else the
// result matched, and the rest come strongest first.
func TestMatchedTermsSkipCommonWords(t *testing.T) {
	t.Parallel()
	weight := map[string]float64{"what": 1.9, "deploy": 0.7, "engram": 0.5, "is": 0.2, "the": 0.01, "for": 0.3}
	const query = "what is the deploy process for engram"

	got := matchedWords(t, query, "What is the deploy process for engram? It is a compose pull.", weight)
	if want := []string{"what", "deploy", "engram"}; !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	// A result without the strongest word has the same floor.
	got = matchedWords(t, query, "The deploy of engram is for later.", weight)
	if want := []string{"deploy", "engram"}; !slices.Equal(got, want) {
		t.Fatalf("without the strongest word: got %v, want %v", got, want)
	}
	// One rare query word doesn't hide the others.
	weight["penny"] = 3
	got = matchedWords(t, query+" penny", "penny: deploy engram", weight)
	if want := []string{"penny", "deploy", "engram"}; !slices.Equal(got, want) {
		t.Fatalf("with a rare word: got %v, want %v", got, want)
	}
}

// When only common words matched, the strongest one still explains the hit.
func TestMatchedTermsKeepStrongestCommonWord(t *testing.T) {
	t.Parallel()
	weight := map[string]float64{"is": 0.2, "the": 0.01, "for": 0.3}
	got := matchedWords(t, "is the for", "it is for the best", weight)
	if want := []string{"for"}; !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// Without weights (a vector-only search) every contained term is shown.
func TestMatchedTermsUnweighted(t *testing.T) {
	t.Parallel()
	got := matchedWords(t, "what is the for", "what is the point of it for", nil)
	for _, w := range []string{"what", "is", "the", "for"} {
		if !slices.Contains(got, w) {
			t.Fatalf("%q missing from %v", w, got)
		}
	}
}
