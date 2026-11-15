package spacestore

import (
	"context"
	"slices"
	"testing"
	"time"
)

// memAt is a memory created at a given time.
func memAt(author, rkey, text string, at time.Time) Memory {
	m := mem(author, rkey, text)
	m.CreatedAt = at
	return m
}

func rkeys[T interface{ ~[]Hit }](hs T) []string {
	var out []string
	for _, h := range hs {
		out = append(out, h.Rkey)
	}
	slices.Sort(out)
	return out
}

// Until, with Since, picks a window of time: from Since up to but not
// including Until. Searches and listings agree, whether the memories are in
// the write buffer, in a segment, or in a segment a fresh node loaded.
func TestFilterUntilWindow(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFixture(t)
	n := f.node()
	configure(t, n, SpaceConfig{ModelInfo: modelA})
	day := func(d int) time.Time { return time.Date(2026, 10, d, 12, 0, 0, 0, time.UTC) }
	err := n.ApplyRepoChanges(ctx, testSpace, "did:plc:alice", pos("r1"), []Memory{
		memAt("did:plc:alice", "d1", "deploy notes one", day(1)),
		memAt("did:plc:alice", "d2", "deploy notes two", day(2)),
		memAt("did:plc:alice", "d3", "deploy notes three", day(3)),
		memAt("did:plc:alice", "d4", "deploy notes four", day(4)),
	}, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		f    Filter
		want []string
	}{
		{"until only", Filter{Until: day(3)}, []string{"d1", "d2"}},
		{"until is exclusive", Filter{Until: day(1)}, nil},
		{"since inclusive, until exclusive", Filter{Since: day(2), Until: day(4)}, []string{"d2", "d3"}},
		{"since only", Filter{Since: day(3)}, []string{"d3", "d4"}},
		{"empty window", Filter{Since: day(3), Until: day(3)}, nil},
		{"after everything", Filter{Until: day(30)}, []string{"d1", "d2", "d3", "d4"}},
	}
	check := func(n *Node, stage string) {
		t.Helper()
		for _, c := range cases {
			res := search(t, n, modelA, "deploy notes", c.f)
			if got := rkeys(res.Hits); !slices.Equal(got, c.want) {
				t.Errorf("%s: search %s = %v, want %v", stage, c.name, got, c.want)
			}
			page, _, err := n.List(ctx, testSpace, 10, "", c.f)
			if err != nil {
				t.Fatal(err)
			}
			if got := rkeys(page); !slices.Equal(got, c.want) {
				t.Errorf("%s: list %s = %v, want %v", stage, c.name, got, c.want)
			}
		}
	}
	check(n, "buffered")
	if err := n.Flush(ctx, testSpace); err != nil {
		t.Fatal(err)
	}
	check(n, "flushed")
	check(f.node(), "reloaded")
}
