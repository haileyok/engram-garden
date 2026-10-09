package agent

import (
	"slices"
	"testing"
)

func mems(mode string, rkeysAndSims ...any) MemoriesOut {
	out := MemoriesOut{Mode: mode}
	for i := 0; i < len(rkeysAndSims); i += 2 {
		sim := rkeysAndSims[i+1].(int)
		out.Memories = append(out.Memories, Memory{URI: rkeysAndSims[i].(string), Similarity: &sim})
	}
	return out
}

func uris(ms []Memory) []string {
	var out []string
	for _, m := range ms {
		out = append(out, m.URI)
	}
	return out
}

func TestMergeRanked(t *testing.T) {
	t.Parallel()
	// One space keeps the service's order, even when similarity disagrees.
	got, mode := MergeRanked([]MemoriesOut{mems("hybrid", "a", 300, "b", 900)})
	if !slices.Equal(uris(got), []string{"a", "b"}) || mode != "hybrid" {
		t.Errorf("one space: %v %q", uris(got), mode)
	}
	// All vector: by similarity, as before.
	got, mode = MergeRanked([]MemoriesOut{mems("vector", "a", 500, "b", 100), mems("vector", "c", 700)})
	if !slices.Equal(uris(got), []string{"c", "a", "b"}) || mode != "vector" {
		t.Errorf("vector: %v %q", uris(got), mode)
	}
	// Otherwise by position in each space's results: firsts, then seconds.
	got, mode = MergeRanked([]MemoriesOut{mems("hybrid", "a", 100, "b", 900), mems("keyword", "c", 0, "d", 0)})
	if uris(got)[0] == "b" || uris(got)[3] == "a" || mode != "" {
		t.Errorf("hybrid: %v %q", uris(got), mode)
	}
	if first := uris(got)[:2]; !slices.Contains(first, "a") || !slices.Contains(first, "c") {
		t.Errorf("each space's first result should lead: %v", uris(got))
	}
}
