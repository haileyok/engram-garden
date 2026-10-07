package lex

import (
	"encoding/json"
	"math"
	"testing"
	"time"

	"github.com/bluesky-social/indigo/atproto/atdata"
	"github.com/haileyok/cocoon/space"
)

var nomic = ModelInfo{Model: "nomic-embed-text", ModelDigest: "sha256:0a109f42", Dims: 4}

// roundTrip passes a record value through JSON and DAG-CBOR, as a PDS and
// a CAR export would.
func roundTrip(t *testing.T, rec map[string]any) (fromJSON, fromCBOR map[string]any) {
	t.Helper()
	raw, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	fromJSON, err = atdata.UnmarshalJSON(raw)
	if err != nil {
		t.Fatal(err)
	}
	ser, err := space.SerializeRecord(MemoryCollection, "k", fromJSON)
	if err != nil {
		t.Fatal(err)
	}
	fromCBOR, err = atdata.UnmarshalCBOR(ser.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return fromJSON, fromCBOR
}

func TestConfigRoundTrip(t *testing.T) {
	t.Parallel()
	next := ModelInfo{Model: "other", ModelDigest: "sha256:ff", Dims: 8}
	c := Config{ModelInfo: nomic, DocumentPrefix: "search_document: ", QueryPrefix: "search_query: ", Next: &next}
	j, cb := roundTrip(t, c.Record(time.Now()))
	for _, rec := range []map[string]any{j, cb} {
		got, err := ParseConfig(rec)
		if err != nil {
			t.Fatal(err)
		}
		if got.ModelInfo != nomic || got.QueryPrefix != "search_query: " || got.Next == nil || *got.Next != next {
			t.Fatalf("got %+v", got)
		}
	}
	if _, err := ParseConfig(map[string]any{"model": "x", "dims": int64(3)}); err == nil {
		t.Fatal("config without a digest accepted")
	}
}

func TestEmbeddingRoundTrip(t *testing.T) {
	t.Parallel()
	v := []float32{0.5, -0.25, 0.125, 0}
	other := ModelInfo{Model: "other", ModelDigest: "sha256:ff", Dims: 4}
	rec := map[string]any{
		"$type": MemoryCollection, "text": "hi", "createdAt": "2026-10-06T00:00:00Z",
		"embedding":     EmbeddingRecord(nomic, v),
		"nextEmbedding": EmbeddingRecord(other, v),
	}
	j, cb := roundTrip(t, rec)
	for _, r := range []map[string]any{rec, j, cb} {
		es := ParseEmbeddings(r)
		if len(es) != 2 || es[0].ModelInfo != nomic || es[1].ModelInfo != other {
			t.Fatalf("got %+v", es)
		}
		for i := range v {
			if math.Abs(float64(es[0].Vector[i]-v[i])) > 1e-6 {
				t.Fatalf("vector %v", es[0].Vector)
			}
		}
	}
	// Wrong dimension count or encoding: skipped.
	bad := EmbeddingRecord(nomic, v[:3])
	if es := ParseEmbeddings(map[string]any{"embedding": bad}); len(es) != 0 {
		t.Fatal("short vector accepted")
	}
	bad = EmbeddingRecord(nomic, v)
	bad["encoding"] = "f32le"
	if es := ParseEmbeddings(map[string]any{"embedding": bad}); len(es) != 0 {
		t.Fatal("unknown encoding accepted")
	}
}

func TestQueryVector(t *testing.T) {
	t.Parallel()
	v := []float32{1, -2, 0.5}
	got, err := DecodeQueryVector(EncodeQueryVector(v))
	if err != nil || len(got) != 3 || got[1] != -2 {
		t.Fatalf("%v %v", got, err)
	}
	if _, err := DecodeQueryVector("!!"); err == nil {
		t.Fatal("garbage accepted")
	}
}
