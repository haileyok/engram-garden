package embed

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/haileyok/engram-garden/internal/lex"
)

func TestOpenAIBatchesAndReordersByIndex(t *testing.T) {
	t.Parallel()
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/v1/embeddings" || r.Header.Get("Authorization") != "Bearer k" {
			t.Errorf("unexpected request %s auth=%q", r.URL.Path, r.Header.Get("Authorization"))
		}
		var req openAIRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatal(err)
		}
		if req.Dimensions != 2 {
			t.Errorf("dimensions = %d, want 2", req.Dimensions)
		}
		// Answer in reverse order: the client must place vectors by index.
		var resp openAIResponse
		for i := len(req.Input) - 1; i >= 0; i-- {
			resp.Data = append(resp.Data, struct {
				Index     int       `json:"index"`
				Embedding []float32 `json:"embedding"`
			}{i, []float32{float32(len(req.Input[i])), 0}})
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	o := &OpenAI{BaseURL: srv.URL + "/v1/", APIKey: "k", Name: "m", Dims: 2, SendDimensions: true, BatchSize: 2}
	got, err := o.Embed(context.Background(), []string{"a", "bb", "ccc"})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Errorf("calls = %d, want 2 batches", calls)
	}
	for i, want := range []float32{1, 2, 3} {
		if got[i][0] != want {
			t.Errorf("vector %d = %v, want first entry %v", i, got[i], want)
		}
	}
}

func TestOpenAIRejectsWrongDimensions(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"index":0,"embedding":[1,2,3]}]}`))
	}))
	defer srv.Close()
	o := &OpenAI{BaseURL: srv.URL, Name: "m", Dims: 2}
	if _, err := o.Embed(context.Background(), []string{"a"}); err == nil {
		t.Fatal("expected a dimension mismatch error")
	}
}

func TestOpenAIReportsHTTPErrors(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusTooManyRequests)
	}))
	defer srv.Close()
	o := &OpenAI{BaseURL: srv.URL, Name: "m", Dims: 2}
	if _, err := o.Embed(context.Background(), []string{"a"}); err == nil {
		t.Fatal("expected an error")
	}
}

func TestHashingIsDeterministicAndNormalized(t *testing.T) {
	t.Parallel()
	h := Hashing{Dims: 64}
	a, _ := h.Embed(context.Background(), []string{"Deploy the API to pop1", "deploy THE api, to pop1!", "", "lunch menu"})
	if dot(a[0], a[1]) < 0.99 {
		t.Errorf("same words should embed identically, cos=%v", dot(a[0], a[1]))
	}
	if dot(a[0], a[3]) > 0.5 {
		t.Errorf("unrelated texts too similar, cos=%v", dot(a[0], a[3]))
	}
	for i, v := range a {
		if n := dot(v, v); n < 0.999 || n > 1.001 {
			t.Errorf("vector %d not unit length: %v", i, n)
		}
	}
}

func dot(a, b []float32) float32 {
	var s float32
	for i := range a {
		s += a[i] * b[i]
	}
	return s
}

func TestOpenAIProviderChecksOllamaDigest(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			_, _ = w.Write([]byte(`{"models":[{"name":"nomic-embed-text:latest","model":"nomic-embed-text:latest","digest":"0a109f42"}]}`))
		case "/v1/embeddings":
			_, _ = w.Write([]byte(`{"data":[{"index":0,"embedding":[1,0,0]}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	ctx := context.Background()
	p := &OpenAIProvider{BaseURL: srv.URL + "/v1"}
	want := lex.ModelInfo{Model: "nomic-embed-text", ModelDigest: "sha256:0a109f42", Dims: 3}
	e, err := p.For(ctx, want)
	if err != nil {
		t.Fatal(err)
	}
	if v, err := e.Embed(ctx, []string{"x"}); err != nil || len(v[0]) != 3 {
		t.Fatalf("%v %v", v, err)
	}
	other := want
	other.ModelDigest = "sha256:ffff"
	var mm *ModelMismatchError
	if _, err := p.For(ctx, other); !errors.As(err, &mm) || mm.Local != "sha256:0a109f42" {
		t.Fatalf("mismatch: %v", err)
	}
	missing := want
	missing.Model = "not-pulled"
	if _, err := p.For(ctx, missing); !errors.As(err, &mm) || mm.Local != "" {
		t.Fatalf("missing model: %v", err)
	}
	// Non-Ollama servers declare the digest.
	declared := &OpenAIProvider{BaseURL: srv.URL + "/v1", Digest: "sha256:ffff"}
	if _, err := declared.For(ctx, other); err != nil {
		t.Fatal(err)
	}
}
