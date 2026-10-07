package spacestore

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/haileyok/engram-garden/internal/blob"
	"github.com/haileyok/engram-garden/internal/lex"
)

// ManifestFormat is the manifest version this package reads and writes.
const ManifestFormat = 1

// ModelInfo identifies an embedding model exactly.
type ModelInfo = lex.ModelInfo

// SpaceConfig is the space authority's declared model (its
// garden.engram.config record), as last seen by the index.
type SpaceConfig = lex.Config

// SegmentInfo describes one segment in a manifest.
type SegmentInfo struct {
	ID           string    `json:"id"`
	Count        int       `json:"count"`
	Bytes        int64     `json:"bytes"`
	MinCreatedAt time.Time `json:"minCreatedAt"`
	MaxCreatedAt time.Time `json:"maxCreatedAt"`
	// CreatedAt is when the segment was written, which decides when it's
	// past the storage provider's minimum retention.
	CreatedAt time.Time `json:"createdAt"`
	Clustered bool      `json:"clustered,omitempty"`
}

// IndexManifest is one model's index: its segments.
type IndexManifest struct {
	ModelInfo
	Segments []SegmentInfo `json:"segments"`
}

// RepoPosition is how far the index has synced one member's repo.
type RepoPosition struct {
	// Rev is the last oplog rev applied; listRepoOps resumes after it.
	Rev string `json:"rev"`
	// SetHash is the set hash state over every record applied so far.
	SetHash []byte `json:"setHash"`
	// SpaceRev is the authority's rev for the repo's latest known write.
	SpaceRev string `json:"spaceRev"`
}

// Manifest is a space's root: its state is exactly what its newest
// manifest lists.
type Manifest struct {
	Format     int    `json:"format"`
	Space      string `json:"space"`
	Token      uint64 `json:"token"`
	Generation uint64 `json:"generation"`
	// Active serves searches. Building is a second index being filled with
	// the next model's vectors during a model change.
	Active   *IndexManifest `json:"active"`
	Building *IndexManifest `json:"building"`
	// Deleted is a roaring bitmap of deleted memory ids.
	Deleted      []byte                  `json:"deleted"`
	NextMemoryID uint32                  `json:"nextMemoryId"`
	Repos        map[string]RepoPosition `json:"repos"`
	Config       *SpaceConfig            `json:"config,omitempty"`
	// Skipped lists, per author, the record keys of memories not indexed
	// because their vectors don't match the space's model.
	Skipped      map[string][]string `json:"skipped,omitempty"`
	SpaceDeleted bool                `json:"spaceDeleted,omitempty"`
	LastMergeAt  time.Time           `json:"lastMergeAt"`
	LastGCAt     time.Time           `json:"lastGcAt"`
	UpdatedAt    time.Time           `json:"updatedAt"`
}

// SpaceKey is the object-storage directory for a space: the hex SHA-256 of
// its URI.
func SpaceKey(spaceURI string) string {
	h := sha256.Sum256([]byte(spaceURI))
	return hex.EncodeToString(h[:])
}

func spacePrefix(spaceURI string) string { return "spaces/" + SpaceKey(spaceURI) + "/" }

// ManifestKey names a manifest object. Both numbers are zero-padded so keys
// sort by token, then generation.
func ManifestKey(spaceURI string, token, gen uint64) string {
	return fmt.Sprintf("%smanifest-%020d-%020d.json", spacePrefix(spaceURI), token, gen)
}

// SegmentKey names a segment object.
func SegmentKey(spaceURI, id string) string {
	return spacePrefix(spaceURI) + "seg-" + id + ".seg"
}

// newSegmentID returns a time-ordered unique id.
func newSegmentID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%016x%s", time.Now().UnixNano(), hex.EncodeToString(b))
}

func parseManifestKey(key string) (token, gen uint64, ok bool) {
	i := strings.LastIndex(key, "/manifest-")
	if i < 0 || !strings.HasSuffix(key, ".json") {
		return 0, 0, false
	}
	parts := strings.Split(strings.TrimSuffix(key[i+len("/manifest-"):], ".json"), "-")
	if len(parts) != 2 {
		return 0, 0, false
	}
	t, err1 := strconv.ParseUint(parts[0], 10, 64)
	g, err2 := strconv.ParseUint(parts[1], 10, 64)
	return t, g, err1 == nil && err2 == nil
}

// ErrNoManifest reports a space with nothing stored yet.
var ErrNoManifest = errors.New("no manifest")

// LoadLatestManifest reads the current manifest: the one with the highest
// token, then the highest generation.
func LoadLatestManifest(ctx context.Context, bs blob.Store, spaceURI string) (*Manifest, error) {
	objs, err := bs.List(ctx, spacePrefix(spaceURI)+"manifest-")
	if err != nil {
		return nil, err
	}
	var best string
	var bt, bg uint64
	for _, o := range objs {
		t, g, ok := parseManifestKey(o.Key)
		if !ok {
			continue
		}
		if best == "" || t > bt || (t == bt && g > bg) {
			best, bt, bg = o.Key, t, g
		}
	}
	if best == "" {
		return nil, ErrNoManifest
	}
	raw, err := blob.GetBytes(ctx, bs, best)
	if err != nil {
		return nil, err
	}
	m, err := DecodeManifest(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", best, err)
	}
	if m.Token != bt || m.Generation != bg || m.Space != spaceURI {
		return nil, fmt.Errorf("%s: manifest contents don't match its key", best)
	}
	return m, nil
}

// DecodeManifest parses and checks a manifest.
func DecodeManifest(raw []byte) (*Manifest, error) {
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	if m.Format != ManifestFormat {
		return nil, fmt.Errorf("unsupported manifest format %d", m.Format)
	}
	if m.Repos == nil {
		m.Repos = map[string]RepoPosition{}
	}
	for _, ix := range []*IndexManifest{m.Active, m.Building} {
		if ix == nil {
			continue
		}
		if !ix.Valid() {
			return nil, errors.New("manifest index has an incomplete model")
		}
		for _, s := range ix.Segments {
			if s.ID == "" || strings.ContainsAny(s.ID, "/.") {
				return nil, fmt.Errorf("bad segment id %q", s.ID)
			}
		}
	}
	return &m, nil
}

func (m *Manifest) clone() *Manifest {
	raw, _ := json.Marshal(m)
	out, _ := DecodeManifest(raw)
	return out
}
