package lex

// Interoperability with the Python client (python/). Each side makes fixtures
// the other must accept:
//
//   - python/tests/fixtures/go.json is written by this package's test (run with
//     ENGRAM_WRITE_PY_FIXTURES=1) and read by the Python tests.
//   - python/tests/fixtures/python.json is written by
//     python/tests/make_fixtures.py and checked here: its space signatures must
//     verify, and its vectors must decode.

import (
	"encoding/hex"
	"encoding/json"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/bluesky-social/indigo/atproto/atcrypto"
	"github.com/haileyok/cocoon/space"

	"github.com/haileyok/engram-garden/internal/vec"
)

const fixtureDir = "../../python/tests/fixtures"

// testKeyHex is a fixed P-256 private key, so the did:key Python derives can
// be compared with Go's.
const testKeyHex = "1f1e1d1c1b1a191817161514131211100f0e0d0c0b0a09080706050403020100"

type sigFixture struct {
	Authorization string            `json:"authorization"`
	Audience      string            `json:"audience"`
	KeyID         string            `json:"keyId"`
	Headers       map[string]string `json:"headers"`
}

type vectorFixture struct {
	// Bits are float32 bit patterns, so both languages start from the same values.
	Bits        []uint32 `json:"bits"`
	F16Hex      string   `json:"f16Hex"`
	QueryVector string   `json:"queryVector"`
	// Record is the embedding record a memory would carry.
	Record map[string]any `json:"record"`
}

type embedTextFixture struct {
	Prefix string   `json:"prefix"`
	Text   string   `json:"text"`
	Tags   []string `json:"tags"`
	Want   string   `json:"want"`
}

type goFixtures struct {
	PrivateKeyHex string             `json:"privateKeyHex"`
	DIDKey        string             `json:"didKey"`
	Sigs          []sigFixture       `json:"sigs"`
	Vectors       []vectorFixture    `json:"vectors"`
	EmbedText     []embedTextFixture `json:"embedText"`
}

func testVectors() [][]float32 {
	return [][]float32{
		{0.1, -0.5, 3.14159, 1e-5, 65504, 7e-8, 0, float32(math.Copysign(0, -1)), -2.5e-3, 0.333333},
		{0.017, -0.0004, 0.9, 0.0001234, -0.25, 1, -1, 0.5},
	}
}

func bitsOf(v []float32) []uint32 {
	out := make([]uint32, len(v))
	for i, x := range v {
		out[i] = math.Float32bits(x)
	}
	return out
}

func TestWritePythonFixtures(t *testing.T) {
	t.Parallel()
	if os.Getenv("ENGRAM_WRITE_PY_FIXTURES") == "" {
		t.Skip("set ENGRAM_WRITE_PY_FIXTURES=1 to regenerate python/tests/fixtures/go.json")
	}
	raw, err := hex.DecodeString(testKeyHex)
	if err != nil {
		t.Fatal(err)
	}
	key, err := atcrypto.ParsePrivateBytesP256(raw)
	if err != nil {
		t.Fatal(err)
	}
	pub, err := key.PublicKey()
	if err != nil {
		t.Fatal(err)
	}
	fx := goFixtures{PrivateKeyHex: testKeyHex, DIDKey: pub.DIDKey()}
	for _, c := range []struct{ auth, aud string }{
		{"Bearer eyJ.delegation.token", ""},
		{"Atproto-Space eyJ.credential.jwt", "did:web:api.engram.garden"},
		{"Atproto-Space eyJ.credential.jwt", "did:plc:abcdefghijklmnopqrstuvwx"},
	} {
		h, err := space.CreateSpaceSigHeaders(key, c.auth, c.aud)
		if err != nil {
			t.Fatal(err)
		}
		keyID := ""
		if c.aud != "" {
			keyID = pub.DIDKey()
		}
		fx.Sigs = append(fx.Sigs, sigFixture{Authorization: c.auth, Audience: c.aud, KeyID: keyID, Headers: h})
	}
	for _, v := range testVectors() {
		rec := EmbeddingRecord(ModelInfo{Model: nomic.Model, ModelDigest: nomic.ModelDigest, Dims: len(v)}, v)
		fx.Vectors = append(fx.Vectors, vectorFixture{
			Bits:        bitsOf(v),
			F16Hex:      hex.EncodeToString(vec.EncodeF16(v)),
			QueryVector: EncodeQueryVector(v),
			Record:      rec,
		})
	}
	for _, c := range []struct {
		prefix, text string
		tags         []string
	}{
		{"search_document: ", "hello world", nil},
		{"search_document: ", "hello world", []string{"a", "b c"}},
		{"", "ünïcödé text", []string{"x"}},
	} {
		fx.EmbedText = append(fx.EmbedText, embedTextFixture{c.prefix, c.text, c.tags, EmbedText(c.prefix, c.text, c.tags)})
	}
	out, err := json.MarshalIndent(fx, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(fixtureDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixtureDir, "go.json"), append(out, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}

type pyFixtures struct {
	DIDKey  string          `json:"didKey"`
	Sigs    []sigFixture    `json:"sigs"`
	Vectors []vectorFixture `json:"vectors"`
}

func TestPythonFixtures(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(filepath.Join(fixtureDir, "python.json"))
	if err != nil {
		t.Skipf("no Python fixtures (%v): run python/tests/make_fixtures.py", err)
	}
	var fx pyFixtures
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatal(err)
	}
	if _, err := atcrypto.ParsePublicDIDKey(fx.DIDKey); err != nil {
		t.Fatalf("Python's did:key %q doesn't parse: %v", fx.DIDKey, err)
	}
	if len(fx.Sigs) == 0 || len(fx.Vectors) == 0 {
		t.Fatal("empty Python fixtures")
	}
	for i, s := range fx.Sigs {
		h := http.Header{}
		for k, v := range s.Headers {
			h.Set(k, v)
		}
		got, err := space.VerifySpaceSignature(h, s.KeyID)
		if err != nil {
			t.Fatalf("sig %d (audience %q): %v", i, s.Audience, err)
		}
		if s.KeyID == "" && got != fx.DIDKey {
			t.Fatalf("sig %d: signed by %s, want %s", i, got, fx.DIDKey)
		}
	}
	for i, v := range fx.Vectors {
		want := make([]float32, len(v.Bits))
		for j, b := range v.Bits {
			want[j] = math.Float32frombits(b)
		}
		wantF16 := vec.EncodeF16(want)
		if got, _ := hex.DecodeString(v.F16Hex); string(got) != string(wantF16) {
			t.Fatalf("vector %d: Python's f16 %x, Go's %x", i, got, wantF16)
		}
		if goQV := EncodeQueryVector(want); goQV != v.QueryVector {
			t.Fatalf("vector %d: Python's query vector %q, Go's %q", i, v.QueryVector, goQV)
		}
		embs := ParseEmbeddings(map[string]any{"embedding": v.Record})
		if len(embs) != 1 {
			t.Fatalf("vector %d: Go couldn't read Python's embedding record", i)
		}
		round, err := vec.DecodeF16(wantF16)
		if err != nil {
			t.Fatal(err)
		}
		for j := range round {
			if embs[0].Vector[j] != round[j] {
				t.Fatalf("vector %d entry %d: %v != %v", i, j, embs[0].Vector[j], round[j])
			}
		}
		if _, err := DecodeQueryVector(v.QueryVector); err != nil {
			t.Fatalf("vector %d: %v", i, err)
		}
	}
}
