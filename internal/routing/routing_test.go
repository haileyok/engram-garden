package routing

import (
	"fmt"
	"testing"
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

// TestCoordinator: of all the nodes, exactly one is the coordinator, they
// all agree which, and a single node is its own.
func TestCoordinator(t *testing.T) {
	t.Parallel()
	nodes, _ := ParseNodes("a=http://a,b=http://b,c=http://c")
	var coordinators []string
	for _, n := range nodes {
		if (&Ring{Self: n.ID, Nodes: nodes, Epoch: 1}).IsCoordinator() {
			coordinators = append(coordinators, n.ID)
		}
	}
	if len(coordinators) != 1 {
		t.Fatalf("coordinators: %v", coordinators)
	}
	if !Single().IsCoordinator() {
		t.Fatal("a single node isn't its own coordinator")
	}
}
