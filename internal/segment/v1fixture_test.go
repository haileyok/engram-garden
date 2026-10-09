package segment

import (
	"bytes"
	"context"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/haileyok/engram-garden/internal/vec"
)

// Version 1 fixtures were written by the version 1 writer (before keyword
// search) and are kept byte for byte, so every later reader is checked
// against the real old format. ENGRAM_WRITE_V1_FIXTURES=1 rewrites them,
// which only makes sense with a version 1 writer.
var v1Fixtures = []struct {
	name             string
	seed             uint64
	n, dims, cluster int
}{
	{"v1.seg", 7, 50, 64, 0},
	{"v1-clustered.seg", 8, 300, 64, 200},
}

func fixtureDocs(seed uint64, n, dims int) []Doc {
	return makeDocs(rand.New(rand.NewPCG(seed, 0)), n, dims)
}

func TestReadsVersion1Fixtures(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for _, f := range v1Fixtures {
		p := filepath.Join("testdata", f.name)
		if os.Getenv("ENGRAM_WRITE_V1_FIXTURES") == "1" {
			b, _, err := Bytes(fixtureDocs(f.seed, f.n, f.dims), WriteOptions{Dims: f.dims, ClusterThreshold: f.cluster, BlockSize: 2048})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll("testdata", 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, b, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		r, err := Open(ctx, BytesReader(b))
		if err != nil {
			t.Fatalf("%s: %v", f.name, err)
		}
		if err := r.Verify(ctx); err != nil {
			t.Fatalf("%s: %v", f.name, err)
		}
		ix, err := r.LoadIndex(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if (ix.Clusters != nil) != (f.cluster > 0) {
			t.Errorf("%s: clustered %v", f.name, ix.Clusters != nil)
		}
		got, err := r.ReadAll(ctx, ix)
		if err != nil {
			t.Fatal(err)
		}
		want := fixtureDocs(f.seed, f.n, f.dims)
		if len(got) != len(want) {
			t.Fatalf("%s: %d docs, want %d", f.name, len(got), len(want))
		}
		byID := map[uint32]Doc{}
		for _, d := range want {
			byID[d.ID] = d
		}
		for _, g := range got {
			w := byID[g.ID]
			if g.Text != w.Text || g.Source != w.Source || g.CID != w.CID || g.Author != w.Author || g.Rkey != w.Rkey ||
				!slices.Equal(g.Tags, w.Tags) || !g.CreatedAt.Equal(w.CreatedAt) || !g.IndexedAt.Equal(w.IndexedAt) {
				t.Fatalf("%s: doc %d = %+v, want %+v", f.name, g.ID, g, w)
			}
			if !bytes.Equal(g.Bits, vec.AppendBits(nil, w.Vector)) || !bytes.Equal(g.Int8, vec.AppendInt8(nil, w.Vector)) {
				t.Fatalf("%s: doc %d vectors differ", f.name, g.ID)
			}
		}
	}
}
