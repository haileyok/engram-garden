package web

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/haileyok/engram-garden/internal/embed"
	"github.com/haileyok/engram-garden/internal/lex"
)

func queryVector(text string) []float32 {
	v, _ := embed.Hashing{Dims: model.Dims}.Embed(context.Background(), []string{text})
	return v[0]
}

func TestSearchRelaysTheQueryVector(t *testing.T) {
	t.Parallel()
	f := setup(t)
	f.net.Put(f.alice, lex.MemoryCollection, "a1", memory("deploys go through the deploy repo workflow", "infra"))
	f.net.Put(f.bob, lex.MemoryCollection, "b1", memory("hailey prefers short answers", "prefs"))
	f.sync(t)

	in := map[string]any{
		"space": f.net.Space, "vector": queryVector("deploy repo workflow"),
		"model": model.Model, "modelDigest": model.ModelDigest, "limit": 5,
	}
	status, body := f.call(t, "POST", "/api/search", f.bob, in)
	ms := list(body, "memories")
	if status != 200 || len(ms) != 2 || ms[0]["author"] != f.alice.DID || ms[0]["similarity"].(float64) <= ms[1]["similarity"].(float64) {
		t.Fatalf("search: %d %v", status, body)
	}

	// Filters pass through.
	in["tags"] = []string{"prefs"}
	status, body = f.call(t, "POST", "/api/search", f.bob, in)
	if ms := list(body, "memories"); status != 200 || len(ms) != 1 || ms[0]["author"] != f.bob.DID {
		t.Fatalf("filtered search: %d %v", status, body)
	}
	delete(in, "tags")

	// The appview says when the query isn't from the space's model.
	in["modelDigest"] = "sha256:other"
	if status, body := f.call(t, "POST", "/api/search", f.bob, in); status != 400 || body["error"] != "ModelMismatch" {
		t.Fatalf("wrong model: %d %v", status, body)
	}
	in["modelDigest"] = model.ModelDigest

	// A query without a usable vector is the caller's mistake.
	for _, bad := range []any{nil, []float32{}, make([]float32, 16001)} {
		in["vector"] = bad
		if status, body := f.call(t, "POST", "/api/search", f.bob, in); status != 400 {
			t.Fatalf("vector %T len: %d %v", bad, status, body)
		}
	}
	in["vector"] = queryVector("x")

	// Mallory isn't a member.
	if status, body := f.call(t, "POST", "/api/search", f.mallory, in); status != 403 {
		t.Fatalf("non-member: %d %v", status, body)
	}
	// Only memory spaces.
	in["space"] = "at://did:plc:x/space/com.example.other/x"
	if status, _ := f.call(t, "POST", "/api/search", f.bob, in); status != 400 {
		t.Fatalf("other space type: %d", status)
	}
	in["space"] = f.net.Space

	// A request from another site is refused, like every other change.
	req := f.request(t, "POST", "/api/search", f.bob, in)
	req.Header.Set("Origin", "https://evil.example")
	if status, body := f.do(t, req); status != 403 || body["error"] != "CrossSiteRequest" {
		t.Fatalf("cross-site: %d %v", status, body)
	}
	req = f.request(t, "POST", "/api/search", f.bob, nil)
	req.Body = io.NopCloser(strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "text/plain")
	if status, _ := f.do(t, req); status != 415 {
		t.Fatalf("form post: %d", status)
	}
}

func TestGraphRelaysTheSpacesGraph(t *testing.T) {
	t.Parallel()
	f := setup(t)
	f.net.Put(f.alice, lex.MemoryCollection, "a1", memory("pop1 deploys go through the deploy repo workflow", "infra"))
	f.net.Put(f.alice, lex.MemoryCollection, "a2", memory("deploy the sha through the workflow repo", "infra"))
	f.net.Put(f.bob, lex.MemoryCollection, "b1", memory("hailey prefers short answers", "prefs"))
	f.sync(t)

	status, body := f.call(t, "GET", "/api/graph"+q("space", f.net.Space, "minSimilarity", "0.5"), f.bob, nil)
	if status != 200 || len(list(body, "nodes")) != 3 || len(list(body, "edges")) < 1 {
		t.Fatalf("graph: %d %v", status, body)
	}
	status, body = f.call(t, "GET", "/api/graph"+q("space", f.net.Space, "limit", "2"), f.bob, nil)
	if status != 200 || len(list(body, "nodes")) != 2 {
		t.Fatalf("limited graph: %d %v", status, body)
	}
	if status, _ := f.call(t, "GET", "/api/graph"+q("space", f.net.Space, "limit", "0"), f.bob, nil); status != 400 {
		t.Fatalf("bad limit: %d", status)
	}
	if status, _ := f.call(t, "GET", "/api/graph"+q("space", f.net.Space), f.mallory, nil); status != 403 {
		t.Fatalf("non-member: %d", status)
	}
	if status, _ := f.call(t, "GET", "/api/graph"+q("space", "at://did:plc:x/space/com.example.other/x"), f.bob, nil); status != 400 {
		t.Fatalf("other space type: %d", status)
	}
}
