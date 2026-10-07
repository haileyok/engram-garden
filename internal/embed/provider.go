package embed

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/haileyok/engram-garden/internal/lex"
)

// Provider returns an embedder for a space's declared model, after checking
// the local model is exactly that model.
type Provider interface {
	For(ctx context.Context, m lex.ModelInfo) (Embedder, error)
}

// ModelMismatchError reports a local model that isn't the space's model.
type ModelMismatchError struct {
	Want      lex.ModelInfo
	LocalName string
	Local     string // the local digest
}

func (e *ModelMismatchError) Error() string {
	if e.Local == "" {
		return fmt.Sprintf("the space uses %s, but its digest couldn't be checked locally: pull the model into Ollama or set ENGRAM_EMBED_MODEL_DIGEST", e.Want)
	}
	return fmt.Sprintf("the space uses %s, but the local %s has digest %s; pull the declared model (for Ollama: ollama pull %s)", e.Want, e.LocalName, e.Local, e.Want.Model)
}

// OpenAIProvider embeds through an OpenAI-compatible endpoint, Ollama by
// default. It learns the local model's digest from Ollama's /api/tags, or
// takes it from Digest for other servers.
type OpenAIProvider struct {
	// BaseURL is the OpenAI-compatible base (default
	// http://localhost:11434/v1, Ollama's).
	BaseURL string
	APIKey  string
	// ModelName overrides the model name sent to the endpoint, when the
	// local name differs from the declared one.
	ModelName string
	// Digest declares the local model's digest, for servers that aren't
	// Ollama.
	Digest string
	HTTP   *http.Client

	mu    sync.Mutex
	cache map[string]Embedder
}

func (p *OpenAIProvider) base() string {
	if p.BaseURL == "" {
		return "http://localhost:11434/v1"
	}
	return strings.TrimSuffix(p.BaseURL, "/")
}

func (p *OpenAIProvider) client() *http.Client {
	if p.HTTP != nil {
		return p.HTTP
	}
	return &http.Client{Timeout: 60 * time.Second}
}

func (p *OpenAIProvider) For(ctx context.Context, m lex.ModelInfo) (Embedder, error) {
	p.mu.Lock()
	if e, ok := p.cache[m.Key()]; ok {
		p.mu.Unlock()
		return e, nil
	}
	p.mu.Unlock()
	name := p.ModelName
	if name == "" {
		name = m.Model
	}
	digest := p.Digest
	if digest == "" {
		d, err := OllamaDigest(ctx, p.client(), p.base(), name)
		if err != nil {
			return nil, fmt.Errorf("checking the local model: %w", err)
		}
		digest = d
	}
	if digest != m.ModelDigest {
		return nil, &ModelMismatchError{Want: m, LocalName: name, Local: digest}
	}
	e := &OpenAI{BaseURL: p.base(), APIKey: p.APIKey, Name: name, Dims: m.Dims, HTTP: p.client()}
	p.mu.Lock()
	if p.cache == nil {
		p.cache = map[string]Embedder{}
	}
	p.cache[m.Key()] = e
	p.mu.Unlock()
	return e, nil
}

// OllamaDigest returns the digest ("sha256:<hex>") of a local Ollama model,
// given Ollama's OpenAI-compatible base URL. It returns "" if the model
// isn't installed.
func OllamaDigest(ctx context.Context, c *http.Client, openAIBase, name string) (string, error) {
	root := strings.TrimSuffix(strings.TrimSuffix(openAIBase, "/"), "/v1")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, root+"/api/tags", nil)
	if err != nil {
		return "", err
	}
	resp, err := c.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return "", fmt.Errorf("%s/api/tags: %s: %s (not Ollama? set ENGRAM_EMBED_MODEL_DIGEST)", root, resp.Status, b)
	}
	var out struct {
		Models []struct {
			Name   string `json:"name"`
			Model  string `json:"model"`
			Digest string `json:"digest"`
		} `json:"models"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&out); err != nil {
		return "", err
	}
	want := name
	if !strings.Contains(want, ":") {
		want += ":latest"
	}
	for _, m := range out.Models {
		if m.Name == want || m.Model == want || m.Name == name || m.Model == name {
			if m.Digest == "" {
				return "", nil
			}
			if strings.HasPrefix(m.Digest, "sha256:") {
				return m.Digest, nil
			}
			return "sha256:" + m.Digest, nil
		}
	}
	return "", nil
}

// HashingDigest is the digest HashingProvider accepts.
const HashingDigest = "sha256:hashing"

// HashingProvider serves the offline Hashing embedder for any model named
// "hashing…" with HashingDigest. For tests and keyless development only.
type HashingProvider struct{}

func (HashingProvider) For(_ context.Context, m lex.ModelInfo) (Embedder, error) {
	if !strings.HasPrefix(m.Model, "hashing") || m.ModelDigest != HashingDigest {
		return nil, &ModelMismatchError{Want: m, LocalName: "hashing", Local: HashingDigest}
	}
	return Hashing{Dims: m.Dims}, nil
}
