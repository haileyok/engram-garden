// Package embed turns text into vectors for semantic search.
package embed

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"math"
	"net/http"
	"strings"
	"time"
	"unicode"
)

// Embedder embeds text. Every vector it returns has Dimensions() entries.
type Embedder interface {
	Embed(ctx context.Context, texts []string) ([][]float32, error)
	Dimensions() int
	// Model names the model, so stored vectors can be traced to it.
	Model() string
}

// OpenAI calls an OpenAI-compatible /embeddings endpoint. That covers OpenAI
// itself and most local servers (Ollama, llama.cpp, vLLM) through their
// compatibility APIs.
type OpenAI struct {
	BaseURL string // e.g. https://api.openai.com/v1
	APIKey  string
	Name    string // model name
	Dims    int
	// SendDimensions asks the model for Dims-sized vectors. Only models that
	// support shortening (OpenAI's text-embedding-3 family) accept it.
	SendDimensions bool
	BatchSize      int
	HTTP           *http.Client
}

func (o *OpenAI) Dimensions() int { return o.Dims }
func (o *OpenAI) Model() string   { return o.Name }

type openAIRequest struct {
	Model      string   `json:"model"`
	Input      []string `json:"input"`
	Dimensions int      `json:"dimensions,omitempty"`
}

type openAIResponse struct {
	Data []struct {
		Index     int       `json:"index"`
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
}

func (o *OpenAI) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	batch := o.BatchSize
	if batch <= 0 {
		batch = 64
	}
	out := make([][]float32, 0, len(texts))
	for start := 0; start < len(texts); start += batch {
		end := min(start+batch, len(texts))
		vecs, err := o.embedBatch(ctx, texts[start:end])
		if err != nil {
			return nil, err
		}
		out = append(out, vecs...)
	}
	return out, nil
}

func (o *OpenAI) embedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	req := openAIRequest{Model: o.Name, Input: texts}
	if o.SendDimensions {
		req.Dimensions = o.Dims
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(o.BaseURL, "/")+"/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	hreq.Header.Set("Content-Type", "application/json")
	if o.APIKey != "" {
		hreq.Header.Set("Authorization", "Bearer "+o.APIKey)
	}
	client := o.HTTP
	if client == nil {
		client = &http.Client{Timeout: 60 * time.Second}
	}
	resp, err := client.Do(hreq)
	if err != nil {
		return nil, fmt.Errorf("embeddings request: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("embeddings request: %s: %s", resp.Status, truncate(string(raw), 300))
	}
	var parsed openAIResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("embeddings response: %w", err)
	}
	if len(parsed.Data) != len(texts) {
		return nil, fmt.Errorf("embeddings response: got %d vectors for %d inputs", len(parsed.Data), len(texts))
	}
	out := make([][]float32, len(texts))
	for _, d := range parsed.Data {
		if d.Index < 0 || d.Index >= len(texts) || out[d.Index] != nil {
			return nil, errors.New("embeddings response: bad or duplicate index")
		}
		if o.Dims >= 0 && len(d.Embedding) != o.Dims {
			return nil, fmt.Errorf("embeddings response: model returned %d dimensions, configured for %d", len(d.Embedding), o.Dims)
		}
		out[d.Index] = d.Embedding
	}
	return out, nil
}

// ProbeDims embeds one short text and returns the vector size the model
// produces.
func ProbeDims(ctx context.Context, baseURL, apiKey, model string, client *http.Client) (int, error) {
	o := &OpenAI{BaseURL: baseURL, APIKey: apiKey, Name: model, Dims: -1, HTTP: client}
	vecs, err := o.embedBatch(ctx, []string{"dimension probe"})
	if err != nil {
		return 0, err
	}
	return len(vecs[0]), nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// Hashing is a deterministic, offline embedder: it hashes words into buckets
// and normalizes, so texts sharing words land near each other. It has no
// understanding of meaning. Use it for tests and keyless local development.
type Hashing struct{ Dims int }

func (h Hashing) Dimensions() int { return h.Dims }
func (h Hashing) Model() string   { return fmt.Sprintf("hashing-%d", h.Dims) }

func (h Hashing) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		v := make([]float32, h.Dims)
		for _, w := range strings.FieldsFunc(strings.ToLower(t), func(r rune) bool {
			return !unicode.IsLetter(r) && !unicode.IsNumber(r)
		}) {
			f := fnv.New32a()
			_, _ = f.Write([]byte(w))
			v[f.Sum32()%uint32(h.Dims)]++
		}
		normalize(v)
		out[i] = v
	}
	return out, nil
}

func normalize(v []float32) {
	var sum float64
	for _, x := range v {
		sum += float64(x) * float64(x)
	}
	if sum == 0 {
		// pgvector can't take a cosine distance from a zero vector.
		v[0] = 1
		return
	}
	n := float32(math.Sqrt(sum))
	for i := range v {
		v[i] /= n
	}
}
