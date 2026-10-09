package appview

import (
	"context"
	"net/url"
	"testing"

	"github.com/haileyok/engram-garden/internal/indexer"
)

func TestMemoryGraph(t *testing.T) {
	t.Parallel()
	f := setup(t)
	ctx := context.Background()
	f.net.Put(f.alice, indexer.Collection, "a1", memory("pop1 deploys go through the deploy repo workflow", "infra"))
	f.net.Put(f.alice, indexer.Collection, "a2", memory("deploy the sha through the workflow repo", "infra"))
	f.net.Put(f.bob, indexer.Collection, "b1", memory("hailey likes short replies that lead with the recommendation", "prefs"))
	f.net.Put(f.bob, indexer.Collection, "b2", memory("short replies should lead with the recommendation", "prefs"))
	if err := f.srv.Indexer.SyncSpace(ctx, f.net.Space); err != nil {
		t.Fatal(err)
	}

	status, body := f.get(t, f.bob, serviceDID, "garden.engram.getMemoryGraph", url.Values{"space": {f.net.Space}, "minSimilarity": {"0.5"}})
	if status != 200 {
		t.Fatalf("graph: %d %v", status, body)
	}
	nodes, _ := body["nodes"].([]any)
	edges, _ := body["edges"].([]any)
	if len(nodes) != 4 {
		t.Fatalf("%d nodes: %v", len(nodes), body)
	}
	text := func(i int) string { return nodes[i].(map[string]any)["text"].(string) }
	topic := func(i int) string {
		if tags, _ := nodes[i].(map[string]any)["tags"].([]any); len(tags) == 1 {
			return tags[0].(string)
		}
		return ""
	}
	if len(edges) < 2 {
		t.Fatalf("want a link inside each pair of memories, got %v", edges)
	}
	for _, raw := range edges {
		e := raw.(map[string]any)
		a, b := int(e["a"].(float64)), int(e["b"].(float64))
		sim := e["similarity"].(float64)
		if a >= b || b >= len(nodes) || sim < 500 || sim > 1000 {
			t.Fatalf("bad edge %v", e)
		}
		if topic(a) != topic(b) {
			t.Fatalf("%q and %q are linked but are about different things", text(a), text(b))
		}
	}
	first := nodes[0].(map[string]any)
	if first["uri"] == nil || first["author"] == nil || first["createdAt"] == nil || first["similarity"] != nil {
		t.Fatalf("node shape: %v", first)
	}

	// The size of the graph is limited.
	_, body = f.get(t, f.bob, serviceDID, "garden.engram.getMemoryGraph", url.Values{"space": {f.net.Space}, "limit": {"2"}})
	if n, _ := body["nodes"].([]any); len(n) != 2 {
		t.Fatalf("limit 2: %v", body)
	}
	for _, p := range []url.Values{
		{"space": {f.net.Space}, "limit": {"0"}},
		{"space": {f.net.Space}, "limit": {"501"}},
		{"space": {f.net.Space}, "neighbors": {"9"}},
		{"space": {f.net.Space}, "minSimilarity": {"2"}},
		{"space": {f.net.Space}, "minSimilarity": {"x"}},
	} {
		if status, body := f.get(t, f.bob, serviceDID, "garden.engram.getMemoryGraph", p); status != 400 {
			t.Fatalf("%v: %d %v", p, status, body)
		}
	}

	// Only members read it.
	if status, _ := f.get(t, nil, "", "garden.engram.getMemoryGraph", url.Values{"space": {f.net.Space}}); status != 401 {
		t.Fatalf("no credential: %d", status)
	}
}
