package appview

import (
	"context"
	"errors"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/haileyok/engram-garden/internal/embed"
	"github.com/haileyok/engram-garden/internal/indexer"
	"github.com/haileyok/engram-garden/internal/lex"
)

// textQuery is a search with no vector: the appview embeds the text.
func textQuery(spaceURI, q string) url.Values {
	return url.Values{"space": {spaceURI}, "q": {q}}
}

// checkKeywordFallback checks a keyword search's answer for "deploy": the
// mode it ran, no similarity, and a match naming the term with a
// highlighted snippet.
func checkKeywordFallback(t *testing.T, status int, body map[string]any) {
	t.Helper()
	ms, _ := body["memories"].([]any)
	if status != 200 || body["mode"] != "keyword" || len(ms) == 0 {
		t.Fatalf("keyword fallback: %d %v", status, body)
	}
	m := ms[0].(map[string]any)
	match, _ := m["match"].(map[string]any)
	kw, _ := match["keyword"].(map[string]any)
	sn, _ := match["snippet"].(map[string]any)
	if _, ok := m["similarity"]; ok || match["vector"] != nil || kw == nil || sn == nil {
		t.Fatalf("keyword fallback memory: %v", m)
	}
	terms, _ := kw["terms"].([]any)
	if len(terms) == 0 || terms[0].(map[string]any)["term"] != "deploy" {
		t.Fatalf("matched terms: %v", kw)
	}
	hl, _ := sn["highlights"].([]any)
	txt, _ := sn["text"].(string)
	if len(hl) == 0 {
		t.Fatalf("no highlights: %v", sn)
	}
	h := hl[0].(map[string]any)
	if s, e := int(h["byteStart"].(float64)), int(h["byteEnd"].(float64)); txt[s:e] != "deploy" && txt[s:e] != "deploys" {
		t.Fatalf("highlight %d-%d is %q in %q", s, e, txt[s:e], txt)
	}
}

func textSearchFixture(t *testing.T, te *QueryEmbedder) *fixture {
	t.Helper()
	f := setup(t)
	f.srv.TextSearch = te
	f.net.Put(f.alice, indexer.Collection, "a1", memory("pop1 deploys go through the deploy repo workflow", "infra"))
	f.net.Put(f.bob, indexer.Collection, "b1", memory("hailey prefers short answers", "prefs"))
	if err := f.srv.Indexer.SyncSpace(context.Background(), f.net.Space); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestTextSearchEmbedsTheQuery(t *testing.T) {
	t.Parallel()
	f := textSearchFixture(t, &QueryEmbedder{Provider: embed.HashingProvider{}})

	// The same answer as a search with a vector the caller made.
	withVector := searchParams(f.net.Space, "deploy workflow")
	withVector.Set("limit", "1")
	status, want := f.get(t, f.bob, serviceDID, "garden.engram.searchMemories", withVector)
	if status != 200 || len(memories(want)) != 1 {
		t.Fatalf("search with a vector: %d %v", status, want)
	}

	p := textQuery(f.net.Space, "deploy workflow")
	p.Set("limit", "1")
	status, got := f.get(t, f.bob, serviceDID, "garden.engram.searchMemories", p)
	if status != 200 || len(memories(got)) != 1 {
		t.Fatalf("search with text: %d %v", status, got)
	}
	if got, want := memories(got)[0], memories(want)[0]; got["uri"] != want["uri"] || got["similarity"] != want["similarity"] {
		t.Fatalf("text search found %v, vector search found %v", got, want)
	}

	// Filters still apply.
	p = textQuery(f.net.Space, "anything")
	p["tags"] = []string{"prefs"}
	status, body := f.get(t, f.bob, serviceDID, "garden.engram.searchMemories", p)
	if status != 200 || len(memories(body)) != 1 || memories(body)[0]["author"] != f.bob.DID {
		t.Fatalf("tag-filtered text search: %d %v", status, body)
	}
}

func TestTextSearchNeedsACredential(t *testing.T) {
	t.Parallel()
	f := textSearchFixture(t, &QueryEmbedder{Provider: embed.HashingProvider{}})
	// The embedder is only reached after the credential checks out.
	if status, body := f.get(t, nil, "", "garden.engram.searchMemories", textQuery(f.net.Space, "deploy")); status != 401 {
		t.Fatalf("no credential: %d %v", status, body)
	}
	if status, body := f.get(t, f.alice, f.alice.DID, "garden.engram.searchMemories", textQuery(f.net.Space, "deploy")); status != 401 {
		t.Fatalf("wrong audience: %d %v", status, body)
	}
}

func TestTextSearchOffByDefault(t *testing.T) {
	t.Parallel()
	f := setup(t)
	status, body := f.get(t, f.alice, serviceDID, "garden.engram.searchMemories", textQuery(f.net.Space, "deploy"))
	if status != 400 || body["error"] != "InvalidRequest" {
		t.Fatalf("text search without an embedder: %d %v", status, body)
	}
}

func TestTextSearchRefusesLongQueries(t *testing.T) {
	t.Parallel()
	f := textSearchFixture(t, &QueryEmbedder{Provider: embed.HashingProvider{}})
	long := make([]byte, MaxTextQueryChars+1)
	for i := range long {
		long[i] = 'a'
	}
	status, body := f.get(t, f.alice, serviceDID, "garden.engram.searchMemories", textQuery(f.net.Space, string(long)))
	if status != 400 || body["error"] != "InvalidRequest" {
		t.Fatalf("long query: %d %v", status, body)
	}
}

// mismatched is a provider that doesn't have the space's model.
type mismatched struct{}

func (mismatched) For(_ context.Context, m lex.ModelInfo) (embed.Embedder, error) {
	return nil, &embed.ModelMismatchError{Want: m, LocalName: m.Model, Local: "sha256:other"}
}

func TestTextSearchModelNotHosted(t *testing.T) {
	t.Parallel()
	f := textSearchFixture(t, &QueryEmbedder{Provider: mismatched{}})
	// Without a mode, a search the service can't embed falls back to
	// keyword search, and says so.
	status, body := f.get(t, f.alice, serviceDID, "garden.engram.searchMemories", textQuery(f.net.Space, "deploy"))
	checkKeywordFallback(t, status, body)
	for _, bad := range []url.Values{
		{"space": {f.net.Space}, "q": {"deploy"}, "mode": {"fuzzy"}},
		{"space": {f.net.Space}, "mode": {"keyword"}},
	} {
		if status, body := f.get(t, f.alice, serviceDID, "garden.engram.searchMemories", bad); status != 400 || body["error"] != "InvalidRequest" {
			t.Fatalf("%v: %d %v", bad, status, body)
		}
	}
	// An explicit mode never falls back.
	p := textQuery(f.net.Space, "deploy")
	p.Set("mode", "hybrid")
	if status, body := f.get(t, f.alice, serviceDID, "garden.engram.searchMemories", p); status != 400 || body["error"] != "ModelNotHosted" {
		t.Fatalf("model the service doesn't have, mode hybrid: %d %v", status, body)
	}
	// A caller with its own vector doesn't need the service's model.
	if status, body := f.get(t, f.alice, serviceDID, "garden.engram.searchMemories", searchParams(f.net.Space, "deploy")); status != 200 {
		t.Fatalf("search with a vector: %d %v", status, body)
	}
}

// broken is a provider whose embedder is failing.
type broken struct{}

func (broken) For(context.Context, lex.ModelInfo) (embed.Embedder, error) {
	return nil, errors.New("embedding service unreachable")
}

func TestTextSearchFailingEmbedderDoesNotFallBack(t *testing.T) {
	t.Parallel()
	f := textSearchFixture(t, &QueryEmbedder{Provider: broken{}})
	// A failure that may pass is an error, not keyword-only results.
	status, body := f.get(t, f.alice, serviceDID, "garden.engram.searchMemories", textQuery(f.net.Space, "deploy"))
	if status < 500 || body["mode"] != nil {
		t.Fatalf("failing embedder: %d %v", status, body)
	}
}

func TestTextSearchRateLimitIsPerAuthority(t *testing.T) {
	t.Parallel()
	// One query in the bucket, refilling too slowly to matter.
	f := textSearchFixture(t, &QueryEmbedder{Provider: embed.HashingProvider{}, PerSecond: 0.001, Burst: 1})
	p := textQuery(f.net.Space, "deploy")
	if status, body := f.get(t, f.alice, serviceDID, "garden.engram.searchMemories", p); status != 200 {
		t.Fatalf("first: %d %v", status, body)
	}
	// Bob is another account, but the space's authority is the same.
	if status, body := f.get(t, f.bob, serviceDID, "garden.engram.searchMemories", p); status != 429 || body["error"] != "RateLimitExceeded" {
		t.Fatalf("second: %d %v", status, body)
	}
	// Searching with a vector of one's own isn't metered here.
	if status, body := f.get(t, f.bob, serviceDID, "garden.engram.searchMemories", searchParams(f.net.Space, "deploy")); status != 200 {
		t.Fatalf("with a vector: %d %v", status, body)
	}
}

func TestTextSearchAuthorityAllowlist(t *testing.T) {
	t.Parallel()
	f := textSearchFixture(t, &QueryEmbedder{Provider: embed.HashingProvider{}, Authorities: []string{"did:plc:someoneelse"}})
	status, body := f.get(t, f.alice, serviceDID, "garden.engram.searchMemories", textQuery(f.net.Space, "deploy"))
	checkKeywordFallback(t, status, body)
	p := textQuery(f.net.Space, "deploy")
	p.Set("mode", "hybrid")
	if status, body := f.get(t, f.alice, serviceDID, "garden.engram.searchMemories", p); status != 403 || body["error"] != "TextSearchNotAllowed" {
		t.Fatalf("authority not on the list, mode hybrid: %d %v", status, body)
	}
	f.srv.TextSearch.Authorities = []string{f.net.Authority.DID}
	if status, body := f.get(t, f.alice, serviceDID, "garden.engram.searchMemories", textQuery(f.net.Space, "deploy")); status != 200 {
		t.Fatalf("authority on the list: %d %v", status, body)
	}
}

// blocking holds every embedding until released.
type blocking struct {
	started chan struct{}
	release chan struct{}
}

func (b blocking) For(_ context.Context, m lex.ModelInfo) (embed.Embedder, error) {
	return blockingEmbedder{b, m.Dims}, nil
}

type blockingEmbedder struct {
	b    blocking
	dims int
}

func (e blockingEmbedder) Dimensions() int { return e.dims }
func (e blockingEmbedder) Model() string   { return "blocking" }
func (e blockingEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	e.b.started <- struct{}{}
	select {
	case <-e.b.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return embed.Hashing{Dims: e.dims}.Embed(ctx, texts)
}

func TestQueryEmbedderCapsConcurrency(t *testing.T) {
	t.Parallel()
	b := blocking{started: make(chan struct{}, 4), release: make(chan struct{})}
	qe := &QueryEmbedder{Provider: b, MaxConcurrent: 1, Burst: 100, PerSecond: 100}
	cfg := &lex.Config{ModelInfo: model}
	space := "at://did:plc:a/space/garden.engram.space/x"

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if _, err := qe.Embed(context.Background(), space, cfg, "first"); err != nil {
			t.Error(err)
		}
	}()
	select {
	case <-b.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the first embedding never started")
	}

	// The only slot is taken: the next caller is turned away, not queued.
	_, err := qe.Embed(context.Background(), space, cfg, "second")
	xe, ok := err.(*xrpcError)
	if !ok || xe.status != 503 || xe.name != "EmbedderBusy" {
		t.Fatalf("second embedding: %v", err)
	}
	close(b.release)
	wg.Wait()
}

func TestQueryEmbedderForgetsIdleAuthorities(t *testing.T) {
	t.Parallel()
	now := time.Now()
	qe := &QueryEmbedder{Provider: embed.HashingProvider{}, Now: func() time.Time { return now }}
	cfg := &lex.Config{ModelInfo: model}
	for _, a := range []string{"did:plc:a", "did:plc:b", "did:plc:c"} {
		if _, err := qe.Embed(context.Background(), "at://"+a+"/space/garden.engram.space/x", cfg, "hello"); err != nil {
			t.Fatal(err)
		}
	}
	if n := qe.tracked(); n != 3 {
		t.Fatalf("tracking %d authorities", n)
	}
	now = now.Add(time.Hour)
	if _, err := qe.Embed(context.Background(), "at://did:plc:d/space/garden.engram.space/x", cfg, "hello"); err != nil {
		t.Fatal(err)
	}
	if n := qe.tracked(); n != 1 {
		t.Fatalf("tracking %d authorities after an hour", n)
	}
}
