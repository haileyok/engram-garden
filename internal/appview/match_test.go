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

// Common words and question words are left out of a result's matched terms,
// whatever else the result matched, and the rest come strongest first.
func TestMatchedTermsSkipCommonWords(t *testing.T) {
	t.Parallel()
	weight := map[string]float64{"what": 1.9, "deploy": 0.7, "engram": 0.5, "is": 0.2, "the": 0.01, "for": 0.3}
	const query = "what is the deploy process for engram"

	got := matchedWords(t, query, "What is the deploy process for engram? It is a compose pull.", weight)
	if want := []string{"deploy", "engram"}; !slices.Equal(got, want) {
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

// A question word is left out of the explanation even when it is rare in
// the space, but it is the explanation when it is all that matched.
func TestMatchedTermsFillerWords(t *testing.T) {
	t.Parallel()
	weight := map[string]float64{"we": 2.6, "how": 1.6, "web": 0.9, "deploy": 0.7}
	got := matchedWords(t, "how do we deploy the web image", "how we deploy the web", weight)
	if want := []string{"web", "deploy"}; !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	got = matchedWords(t, "how do we deploy", "we know how", weight)
	if want := []string{"we"}; !slices.Equal(got, want) {
		t.Fatalf("only filler matched: got %v, want %v", got, want)
	}
}

// Without weights (every term weighs the same) only question words are
// left out.
func TestMatchedTermsUnweighted(t *testing.T) {
	t.Parallel()
	got := matchedWords(t, "what is the for", "what is the point of it for", nil)
	if want := []string{"is", "the", "for"}; !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}
