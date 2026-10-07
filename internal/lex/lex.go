// Package lex holds Engram Garden's record formats: the space's declared
// embedding model (garden.engram.config) and the vectors memories carry
// (garden.engram.memory's embedding fields).
package lex

import (
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/bluesky-social/indigo/atproto/atdata"

	"github.com/haileyok/engram-garden/internal/vec"
)

const (
	SpaceType        = "garden.engram.space"
	MemoryCollection = "garden.engram.memory"
	ConfigCollection = "garden.engram.config"
	// ConfigRkey is the config record's key in the authority's repo.
	ConfigRkey = "self"
	// MaxEmbedChars bounds the text sent to an embedding model, to stay
	// inside common models' input limits.
	MaxEmbedChars = 24000
)

// ModelInfo identifies an embedding model exactly. Vectors are comparable
// only when all three fields match.
type ModelInfo struct {
	Model       string `json:"model"`
	ModelDigest string `json:"modelDigest"`
	Dims        int    `json:"dims"`
}

// Key is a stable identifier for the model.
func (m ModelInfo) Key() string { return fmt.Sprintf("%s@%s/%d", m.Model, m.ModelDigest, m.Dims) }

func (m ModelInfo) String() string {
	return fmt.Sprintf("%s (%s, %d dims)", m.Model, m.ModelDigest, m.Dims)
}

// Valid reports whether the model is fully specified.
func (m ModelInfo) Valid() bool {
	return m.Model != "" && m.ModelDigest != "" && m.Dims > 0 && m.Dims <= 16000
}

// Config is the space authority's garden.engram.config record: the model
// every vector in the space must come from, and how to use it.
type Config struct {
	ModelInfo
	// DocumentPrefix goes before stored text and QueryPrefix before
	// queries, for models trained with task prefixes.
	DocumentPrefix string `json:"documentPrefix,omitempty"`
	QueryPrefix    string `json:"queryPrefix,omitempty"`
	// Next is the model a change is moving to. Agents embed new and
	// existing memories with both while it's set.
	Next *ModelInfo `json:"next,omitempty"`
}

func parseModel(rec map[string]any) (ModelInfo, error) {
	m := ModelInfo{}
	m.Model, _ = rec["model"].(string)
	m.ModelDigest, _ = rec["modelDigest"].(string)
	m.Dims = intOf(rec["dims"])
	if !m.Valid() {
		return m, errors.New("model, modelDigest and dims (1-16000) are required")
	}
	return m, nil
}

func intOf(v any) int {
	switch n := v.(type) {
	case int64:
		return int(n)
	case int:
		return n
	case float64:
		return int(n)
	}
	return 0
}

// ParseConfig reads a garden.engram.config record.
func ParseConfig(rec map[string]any) (*Config, error) {
	m, err := parseModel(rec)
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	c := &Config{ModelInfo: m}
	c.DocumentPrefix, _ = rec["documentPrefix"].(string)
	c.QueryPrefix, _ = rec["queryPrefix"].(string)
	if n, ok := rec["next"].(map[string]any); ok {
		nm, err := parseModel(n)
		if err != nil {
			return nil, fmt.Errorf("config next: %w", err)
		}
		if nm != m {
			c.Next = &nm
		}
	}
	return c, nil
}

func modelRecord(m ModelInfo) map[string]any {
	return map[string]any{"model": m.Model, "modelDigest": m.ModelDigest, "dims": m.Dims}
}

// Record returns the config as a record value.
func (c Config) Record(createdAt time.Time) map[string]any {
	rec := modelRecord(c.ModelInfo)
	rec["$type"] = ConfigCollection
	rec["createdAt"] = createdAt.UTC().Format("2006-01-02T15:04:05.000Z")
	if c.DocumentPrefix != "" {
		rec["documentPrefix"] = c.DocumentPrefix
	}
	if c.QueryPrefix != "" {
		rec["queryPrefix"] = c.QueryPrefix
	}
	if c.Next != nil {
		rec["next"] = modelRecord(*c.Next)
	}
	return rec
}

// Embedding is one vector a memory carries.
type Embedding struct {
	ModelInfo
	Vector []float32
}

// EmbeddingRecord encodes a vector for a memory record's embedding field.
func EmbeddingRecord(m ModelInfo, v []float32) map[string]any {
	rec := modelRecord(m)
	rec["encoding"] = vec.EncodingF16LE
	// The atproto data model encodes $bytes as unpadded base64.
	rec["vector"] = map[string]any{"$bytes": base64.RawStdEncoding.EncodeToString(vec.EncodeF16(v))}
	return rec
}

// MemoryEmbeddingFields are the memory record fields that may hold vectors:
// embedding (the space's model when written) and nextEmbedding (written
// during a model change).
var MemoryEmbeddingFields = []string{"embedding", "nextEmbedding"}

// ParseEmbeddings reads a memory record's vectors. Malformed entries are
// skipped; the caller treats a memory without a usable vector as not
// matching the space's model.
func ParseEmbeddings(rec map[string]any) []Embedding {
	var out []Embedding
	for _, f := range MemoryEmbeddingFields {
		e, ok := rec[f].(map[string]any)
		if !ok {
			continue
		}
		m, err := parseModel(e)
		if err != nil {
			continue
		}
		if enc, _ := e["encoding"].(string); enc != vec.EncodingF16LE {
			continue
		}
		raw, ok := bytesOf(e["vector"])
		if !ok {
			continue
		}
		v, err := vec.DecodeF16(raw)
		if err != nil || len(v) != m.Dims {
			continue
		}
		out = append(out, Embedding{ModelInfo: m, Vector: v})
	}
	return out
}

func bytesOf(v any) ([]byte, bool) {
	switch b := v.(type) {
	case atdata.Bytes:
		return b, true
	case []byte:
		return b, true
	case map[string]any:
		s, ok := b["$bytes"].(string)
		if !ok {
			return nil, false
		}
		raw, err := base64.RawStdEncoding.DecodeString(strings.TrimRight(s, "="))
		return raw, err == nil
	}
	return nil, false
}

// EncodeQueryVector encodes a query vector for searchMemories' vector
// parameter: base64url (unpadded) little-endian half precision.
func EncodeQueryVector(v []float32) string {
	return base64.RawURLEncoding.EncodeToString(vec.EncodeF16(v))
}

// DecodeQueryVector decodes searchMemories' vector parameter.
func DecodeQueryVector(s string) ([]float32, error) {
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(s, "="))
	if err != nil {
		return nil, errors.New("vector must be base64url-encoded f16le")
	}
	return vec.DecodeF16(raw)
}

// EmbedText is what gets embedded for a memory: its text, plus its tags so
// they help retrieval. The space's document prefix goes in front.
func EmbedText(prefix, text string, tags []string) string {
	if len(text) > MaxEmbedChars {
		text = text[:MaxEmbedChars]
	}
	if len(tags) > 0 {
		text += "\n\nTags: " + strings.Join(tags, ", ")
	}
	return prefix + text
}
