package routing

import (
	"context"
	"fmt"
	"testing"

	"github.com/haileyok/engram-garden/internal/blob"
)

func TestRendezvous(t *testing.T) {
	t.Parallel()
	nodes, err := ParseNodes("a=http://a:8080, b=http://b:8080,c=http://c:8080/")
	if err != nil || len(nodes) != 3 || nodes[2].URL != "http://c:8080" {
		t.Fatalf("%+v %v", nodes, err)
	}
	three := &Ring{Self: "a", Nodes: nodes, Epoch: 1}
	two := &Ring{Self: "a", Nodes: nodes[:2], Epoch: 2}
	counts := map[string]int{}
	moved := 0
	const n = 3000
	for i := range n {
		sp := fmt.Sprintf("at://did:plc:x%d/space/garden.engram.space/memory", i)
		o3 := three.Owner(sp)
		counts[o3.ID]++
		// Same answer on every node.
		if (&Ring{Self: "b", Nodes: nodes}).Owner(sp) != o3 {
			t.Fatal("nodes disagree")
		}
		// Removing c only moves c's spaces.
		if o2 := two.Owner(sp); o2 != o3 {
			moved++
			if o3.ID != "c" {
				t.Fatalf("%s moved from %s to %s", sp, o3.ID, o2.ID)
			}
		}
	}
	for id, c := range counts {
		if c < n/3-n/10 || c > n/3+n/10 {
			t.Fatalf("uneven: %s owns %d of %d", id, c, n)
		}
	}
	if moved != counts["c"] {
		t.Fatalf("moved %d, c owned %d", moved, counts["c"])
	}
	for _, bad := range []string{"", "a", "a=notaurl", "a=http://x,a=http://y"} {
		if _, err := ParseNodes(bad); err == nil {
			t.Fatalf("%q accepted", bad)
		}
	}
}

func TestRegistry(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	bs := blob.Dir{Root: t.TempDir()}
	nodes, _ := ParseNodes("a=http://a,b=http://b")
	var coord *Ring
	for _, id := range []string{"a", "b"} {
		if r := (&Ring{Self: id, Nodes: nodes, Epoch: 1}); r.IsCoordinator() {
			coord = r
		} else if reg, err := r.WriteRegistry(ctx, bs, []string{"x"}); reg != nil || err != nil {
			t.Fatal("non-coordinator wrote the registry")
		}
	}
	reg, err := coord.WriteRegistry(ctx, bs, []string{"at://s1", "at://s2"})
	if err != nil || reg.Generation != 1 || len(reg.Spaces) != 2 {
		t.Fatalf("%+v %v", reg, err)
	}
	// Unchanged: no new generation.
	if reg, _ = coord.WriteRegistry(ctx, bs, []string{"at://s2", "at://s1"}); reg.Generation != 1 {
		t.Fatalf("rewrote an unchanged registry: %d", reg.Generation)
	}
	if reg, _ = coord.WriteRegistry(ctx, bs, []string{"at://s1"}); reg.Generation != 2 {
		t.Fatal("change not written")
	}
	got, err := LoadRegistry(ctx, bs)
	if err != nil || got.Generation != 2 || len(got.Spaces) != 1 || got.Spaces[0].Owner == "" {
		t.Fatalf("%+v %v", got, err)
	}
	// A coordinator with an older epoch refuses.
	if _, err := (&Ring{Self: coord.Self, Nodes: nodes, Epoch: 0}).WriteRegistry(ctx, bs, nil); err == nil {
		t.Fatal("stale epoch wrote the registry")
	}
}
