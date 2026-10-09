package spacestore

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func graphTexts() map[string]string {
	return map[string]string{
		"a1": "pop1 deploys go through the deploy repo workflow",
		"a2": "the deploy workflow takes a single sha",
		"a3": "deploy the sha through the workflow repo",
		"b1": "hailey likes short replies that lead with the recommendation",
		"b2": "short replies should lead with the recommendation",
		"c1": "wasabi bills a ninety day minimum per object",
	}
}

func seedGraph(t *testing.T, n *Node) {
	t.Helper()
	ctx := context.Background()
	configure(t, n, SpaceConfig{ModelInfo: model768})
	texts := graphTexts()
	for _, k := range []string{"a1", "a2", "a3", "b1", "b2", "c1"} {
		if err := n.ApplyRepoChanges(ctx, testSpace, "did:plc:alice", pos("r-"+k), []Memory{mem("did:plc:alice", k, texts[k], model768)}, nil, false); err != nil {
			t.Fatal(err)
		}
	}
}

func checkGraph(t *testing.T, g *Graph, stage string) {
	t.Helper()
	if len(g.Nodes) != 6 {
		t.Fatalf("%s: %d nodes", stage, len(g.Nodes))
	}
	byKey := map[string]int{}
	for i, h := range g.Nodes {
		byKey[h.Rkey] = i
		if h.Text != graphTexts()[h.Rkey] || h.Author != "did:plc:alice" || h.CID == "" {
			t.Fatalf("%s: node %d is %+v", stage, i, h)
		}
	}
	linked := func(a, b string) bool {
		for _, e := range g.Edges {
			x, y := byKey[a], byKey[b]
			if (e.A == x && e.B == y) || (e.A == y && e.B == x) {
				return true
			}
		}
		return false
	}
	seen := map[[2]int]bool{}
	for _, e := range g.Edges {
		if e.A >= e.B || e.A < 0 || e.B >= len(g.Nodes) {
			t.Fatalf("%s: edge %+v must have A < B inside the node list", stage, e)
		}
		if seen[[2]int{e.A, e.B}] {
			t.Fatalf("%s: duplicate edge %+v", stage, e)
		}
		seen[[2]int{e.A, e.B}] = true
		if e.Similarity < 0.3 || e.Similarity > 1 {
			t.Fatalf("%s: edge similarity %v", stage, e.Similarity)
		}
	}
	if !linked("a1", "a2") && !linked("a1", "a3") {
		t.Fatalf("%s: deploy memories aren't linked: %+v", stage, g.Edges)
	}
	if !linked("b1", "b2") {
		t.Fatalf("%s: short-replies memories aren't linked: %+v", stage, g.Edges)
	}
	for _, a := range []string{"a1", "a2", "a3"} {
		for _, b := range []string{"b1", "b2", "c1"} {
			if linked(a, b) {
				t.Fatalf("%s: %s and %s are about different things but are linked", stage, a, b)
			}
		}
	}
	if len(g.Edges) > len(g.Nodes)*2 {
		t.Fatalf("%s: %d edges is more than 2 neighbors a node", stage, len(g.Edges))
	}
}

func TestGraph(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFixture(t)
	n := f.node()
	if _, err := n.Graph(ctx, testSpace, GraphQuery{}); !errors.Is(err, ErrNoModel) {
		t.Fatalf("graph before config: %v", err)
	}
	seedGraph(t, n)
	q := GraphQuery{Neighbors: 2, MinSimilarity: 0.3}

	// From the write buffer, then from a published segment.
	g, err := n.Graph(ctx, testSpace, q)
	if err != nil {
		t.Fatal(err)
	}
	checkGraph(t, g, "buffered")
	if err := n.Flush(ctx, testSpace); err != nil {
		t.Fatal(err)
	}
	g, err = n.Graph(ctx, testSpace, q)
	if err != nil {
		t.Fatal(err)
	}
	checkGraph(t, g, "flushed")

	// A fresh node reads the segments from storage.
	g, err = f.node().Graph(ctx, testSpace, q)
	if err != nil {
		t.Fatal(err)
	}
	checkGraph(t, g, "reloaded")

	// The newest memories only, newest first.
	g, err = n.Graph(ctx, testSpace, GraphQuery{Limit: 2, Neighbors: 2, MinSimilarity: 0.3})
	if err != nil {
		t.Fatal(err)
	}
	if len(g.Nodes) != 2 {
		t.Fatalf("limit 2 gave %d nodes", len(g.Nodes))
	}
	if !g.Nodes[0].CreatedAt.After(g.Nodes[1].CreatedAt) && !g.Nodes[0].CreatedAt.Equal(g.Nodes[1].CreatedAt) {
		t.Fatalf("nodes aren't newest first: %+v", g.Nodes)
	}

	// Nothing is similar enough.
	g, err = n.Graph(ctx, testSpace, GraphQuery{Neighbors: 2, MinSimilarity: 0.999})
	if err != nil {
		t.Fatal(err)
	}
	if len(g.Edges) != 0 || len(g.Nodes) != 6 {
		t.Fatalf("threshold 0.999: %d nodes, %d edges", len(g.Nodes), len(g.Edges))
	}
}

func TestGraphIgnoresMemoriesWithoutTheSpacesModel(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFixture(t)
	n := f.node()
	configure(t, n, SpaceConfig{ModelInfo: model768})
	if err := n.ApplyRepoChanges(ctx, testSpace, "did:plc:alice", pos("r1"), []Memory{
		mem("did:plc:alice", "a1", "deploy the sha through the workflow", model768),
		mem("did:plc:alice", "a2", "the deploy workflow takes a sha", model768),
		mem("did:plc:alice", "z1", "written with another model", modelB),
	}, nil, false); err != nil {
		t.Fatal(err)
	}
	g, err := n.Graph(ctx, testSpace, GraphQuery{MinSimilarity: 0.3})
	if err != nil {
		t.Fatal(err)
	}
	if len(g.Nodes) != 2 {
		t.Fatalf("%d nodes; a memory without the space's vectors must not be in the graph: %+v", len(g.Nodes), g.Nodes)
	}
}

func TestGraphLimitsAreCapped(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFixture(t)
	n := f.node()
	configure(t, n, SpaceConfig{ModelInfo: model768})
	var batch []Memory
	for i := range MaxGraphNodes + 20 {
		batch = append(batch, mem("did:plc:alice", fmt.Sprintf("m%04d", i), fmt.Sprintf("memory number %d about deploys", i), model768))
	}
	if err := n.ApplyRepoChanges(ctx, testSpace, "did:plc:alice", pos("r1"), batch, nil, false); err != nil {
		t.Fatal(err)
	}
	g, err := n.Graph(ctx, testSpace, GraphQuery{Limit: 100000, Neighbors: 1000})
	if err != nil {
		t.Fatal(err)
	}
	if len(g.Nodes) != MaxGraphNodes {
		t.Fatalf("%d nodes, want the cap %d", len(g.Nodes), MaxGraphNodes)
	}
	if len(g.Edges) > MaxGraphNodes*MaxGraphNeighbors {
		t.Fatalf("%d edges", len(g.Edges))
	}
}
