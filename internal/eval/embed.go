package eval

import (
	"context"
	"crypto/sha256"
	"encoding/gob"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"time"

	"github.com/haileyok/engram-garden/internal/embed"
	"github.com/haileyok/engram-garden/internal/lex"
	"github.com/haileyok/engram-garden/internal/vec"
)

// Embeddings embeds texts with a corpus's model, caching vectors in a file
// so reruns embed only what's new.
type Embeddings struct {
	Model Model
	Path  string
	e     embed.Embedder
	cache map[string][]float32
	dirty bool
}

// OpenEmbeddings loads the cache at path.
func OpenEmbeddings(m Model, path string) (*Embeddings, error) {
	if m.Name == "" || m.Dims == 0 {
		return nil, errors.New("the corpus has no embedding model (model.json)")
	}
	url := m.URL
	if url == "" {
		url = "http://localhost:11434/v1"
	}
	e := &Embeddings{
		Model: m, Path: path, cache: map[string][]float32{},
		e: &embed.OpenAI{BaseURL: url, Name: m.Name, Dims: m.Dims, BatchSize: 32, HTTP: &http.Client{Timeout: 10 * time.Minute}},
	}
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return e, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if err := gob.NewDecoder(f).Decode(&e.cache); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return e, nil
}

func (e *Embeddings) key(s string) string {
	h := sha256.Sum256([]byte(e.Model.Name + "\x00" + s))
	return hex.EncodeToString(h[:16])
}

// Documents embeds memories as the agents do: document prefix, text, tags.
func (e *Embeddings) Documents(ctx context.Context, mems []Memory) ([][]float32, error) {
	texts := make([]string, len(mems))
	for i, m := range mems {
		texts[i] = lex.EmbedText(e.Model.DocumentPrefix, m.Text, m.Tags)
	}
	return e.texts(ctx, texts)
}

// Queries embeds queries with the query prefix.
func (e *Embeddings) Queries(ctx context.Context, qs []Query) ([][]float32, error) {
	texts := make([]string, len(qs))
	for i, q := range qs {
		texts[i] = e.Model.QueryPrefix + q.Text
	}
	return e.texts(ctx, texts)
}

func (e *Embeddings) texts(ctx context.Context, texts []string) ([][]float32, error) {
	var missing []string
	seen := map[string]bool{}
	for _, t := range texts {
		if _, ok := e.cache[e.key(t)]; !ok && !seen[t] {
			seen[t] = true
			missing = append(missing, t)
		}
	}
	for start := 0; start < len(missing); start += 256 {
		batch := missing[start:min(start+256, len(missing))]
		vs, err := e.e.Embed(ctx, batch)
		if err != nil {
			return nil, fmt.Errorf("embedding with %s: %w", e.Model.Name, err)
		}
		for i, v := range vs {
			if len(v) != e.Model.Dims {
				return nil, fmt.Errorf("%s returned %d dimensions, want %d", e.Model.Name, len(v), e.Model.Dims)
			}
			vec.Normalize(v)
			e.cache[e.key(batch[i])] = v
		}
		e.dirty = true
		if err := e.Save(); err != nil {
			return nil, err
		}
	}
	out := make([][]float32, len(texts))
	for i, t := range texts {
		out[i] = e.cache[e.key(t)]
	}
	return out, nil
}

// Save writes the cache if it changed.
func (e *Embeddings) Save() error {
	if !e.dirty {
		return nil
	}
	tmp := e.Path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if err := gob.NewEncoder(f).Encode(e.cache); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	e.dirty = false
	return os.Rename(tmp, e.Path)
}
