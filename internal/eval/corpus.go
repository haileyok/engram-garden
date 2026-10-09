// Package eval measures search quality: vector-only, keyword-only and
// hybrid ranking over a corpus of memories, with queries and relevance
// judgments from a chat model. See "Evaluation" in
// docs/design/keyword-search.md. cmd/engram-eval drives it.
package eval

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// Memory is one memory in a corpus.
type Memory struct {
	ID        string   `json:"id"`
	Text      string   `json:"text"`
	Tags      []string `json:"tags,omitempty"`
	Source    string   `json:"source,omitempty"`
	CreatedAt string   `json:"createdAt,omitempty"`
}

// Model says how a corpus's memories and queries are embedded.
type Model struct {
	// URL is an OpenAI-compatible embeddings endpoint's base, such as
	// Ollama's http://localhost:11434/v1.
	URL            string `json:"url"`
	Name           string `json:"name"`
	Dims           int    `json:"dims"`
	DocumentPrefix string `json:"documentPrefix,omitempty"`
	QueryPrefix    string `json:"queryPrefix,omitempty"`
}

// ReadJSONL reads one JSON value per line. A missing file is empty.
func ReadJSONL[T any](path string) ([]T, error) {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []T
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for n := 1; sc.Scan(); n++ {
		if len(sc.Bytes()) == 0 {
			continue
		}
		var v T
		if err := json.Unmarshal(sc.Bytes(), &v); err != nil {
			return nil, fmt.Errorf("%s:%d: %w", path, n, err)
		}
		out = append(out, v)
	}
	return out, sc.Err()
}

// WriteJSONL replaces path with one JSON value per line.
func WriteJSONL[T any](path string, items []T) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	w := bufio.NewWriter(f)
	enc := json.NewEncoder(w)
	for _, it := range items {
		if err := enc.Encode(it); err != nil {
			f.Close()
			return err
		}
	}
	if err := w.Flush(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ReadJSON reads a JSON file into v. A missing file leaves v alone and
// reports false.
func ReadJSON(path string, v any) (bool, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, json.Unmarshal(b, v)
}

// WriteJSON replaces path with v as indented JSON.
func WriteJSON(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
