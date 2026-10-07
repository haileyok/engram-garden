package segment

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/haileyok/engram-garden/internal/vec"
)

func randVec(r *rand.Rand, dims int) []float32 {
	v := make([]float32, dims)
	for i := range v {
		v[i] = float32(r.NormFloat64())
	}
	vec.Normalize(v)
	return v
}

func makeDocs(r *rand.Rand, n, dims int) []Doc {
	docs := make([]Doc, n)
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	for i := range docs {
		docs[i] = Doc{
			ID:        uint32(100 + i),
			Author:    fmt.Sprintf("did:plc:author%d", i%3),
			Rkey:      fmt.Sprintf("rk%05d", i),
			CID:       fmt.Sprintf("bafy%d", i),
			Text:      fmt.Sprintf("memory number %d %s", i, strings.Repeat("x", i%50)),
			Source:    fmt.Sprintf("src%d", i%2),
			Tags:      []string{fmt.Sprintf("t%d", i%4), "all"},
			CreatedAt: base.Add(time.Duration(i) * time.Minute),
			IndexedAt: base.Add(time.Hour),
			Vector:    randVec(r, dims),
		}
	}
	return docs
}

// countingReader records the reads a reader makes.
type countingReader struct {
	BytesReader
	reads int
	bytes int64
}

func (c *countingReader) ReadRange(ctx context.Context, off, n int64) ([]byte, error) {
	c.reads++
	c.bytes += n
	return c.BytesReader.ReadRange(ctx, off, n)
}

func TestRoundTrip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := rand.New(rand.NewPCG(1, 1))
	const dims = 64
	docs := makeDocs(r, 500, dims)
	want := slices.Clone(docs)
	data, info, err := Bytes(docs, WriteOptions{Dims: dims, BlockSize: 4096})
	if err != nil {
		t.Fatal(err)
	}
	if info.Count != 500 || info.Bytes != int64(len(data)) || !info.MinCreatedAt.Equal(want[0].CreatedAt) || !info.MaxCreatedAt.Equal(want[499].CreatedAt) {
		t.Fatalf("info %+v", info)
	}
	rd, err := Open(ctx, BytesReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if err := rd.Verify(ctx); err != nil {
		t.Fatal(err)
	}
	ix, err := rd.LoadIndex(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if ix.Clusters != nil || ix.Count != 500 {
		t.Fatal("unexpected index shape")
	}
	m := ix.Meta(7)
	if m.ID != 107 || m.Author != "did:plc:author1" || m.Rkey != "rk00007" || !slices.Equal(m.Tags, []string{"t3", "all"}) || !m.CreatedAt.Equal(want[7].CreatedAt) {
		t.Fatalf("meta %+v", m)
	}
	tid, _ := ix.StringID("t3")
	all, _ := ix.StringID("all")
	if !ix.HasTags(7, []uint32{tid, all}) || ix.HasTags(6, []uint32{tid}) {
		t.Fatal("HasTags")
	}
	bits, err := rd.LoadBits(ctx)
	if err != nil {
		t.Fatal(err)
	}
	bl := vec.BitBytes(dims)
	if !slices.Equal(bits[7*bl:8*bl], vec.AppendBits(nil, want[7].Vector)) {
		t.Fatal("bits mismatch")
	}
	part, _ := rd.BitsRange(ctx, 7, 9)
	if !slices.Equal(part, bits[7*bl:9*bl]) {
		t.Fatal("BitsRange")
	}
	rows := []int{499, 3, 4, 5, 250}
	iv, err := rd.ReadInt8(ctx, rows)
	if err != nil {
		t.Fatal(err)
	}
	for i, row := range rows {
		if !slices.Equal(iv[i], vec.AppendInt8(nil, want[row].Vector)) {
			t.Fatalf("int8 row %d", row)
		}
	}
	bodies, err := rd.ReadDocs(ctx, ix, rows)
	if err != nil {
		t.Fatal(err)
	}
	for i, row := range rows {
		if bodies[i].Text != want[row].Text || bodies[i].CID != want[row].CID || bodies[i].Source != want[row].Source || !bodies[i].IndexedAt.Equal(want[row].IndexedAt) {
			t.Fatalf("doc row %d: %+v", row, bodies[i])
		}
	}
	all2, err := rd.ReadAll(ctx, ix)
	if err != nil || len(all2) != 500 || all2[42].Text != want[42].Text {
		t.Fatalf("ReadAll: %v", err)
	}
	// Rewriting from quantized forms (as a merge does) gives the same vectors.
	data2, _, err := Bytes(all2, WriteOptions{Dims: dims})
	if err != nil {
		t.Fatal(err)
	}
	rd2, _ := Open(ctx, BytesReader(data2))
	bits2, _ := rd2.LoadBits(ctx)
	if !slices.Equal(bits, bits2) {
		t.Fatal("rewrite changed bits")
	}
}

func TestReadsAreSmall(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := rand.New(rand.NewPCG(2, 2))
	data, _, err := Bytes(makeDocs(r, 2000, 768), WriteOptions{Dims: 768})
	if err != nil {
		t.Fatal(err)
	}
	cr := &countingReader{BytesReader: BytesReader(data)}
	rd, err := Open(ctx, cr)
	if err != nil {
		t.Fatal(err)
	}
	ix, _ := rd.LoadIndex(ctx)
	if cr.reads != 3 {
		t.Fatalf("header + index took %d reads, want 3", cr.reads)
	}
	before := cr.reads
	if _, err := rd.ReadInt8(ctx, []int{10, 11, 12, 900}); err != nil {
		t.Fatal(err)
	}
	if cr.reads-before != 2 {
		t.Fatalf("adjacent rows not merged: %d reads", cr.reads-before)
	}
	before, b := cr.reads, cr.bytes
	if _, err := rd.ReadDocs(ctx, ix, []int{1999}); err != nil {
		t.Fatal(err)
	}
	if cr.reads-before != 1 || cr.bytes-b > 64<<10 {
		t.Fatalf("one doc read %d bytes in %d reads", cr.bytes-b, cr.reads-before)
	}
}

func TestCorruptionDetected(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := rand.New(rand.NewPCG(3, 3))
	data, _, err := Bytes(makeDocs(r, 50, 32), WriteOptions{Dims: 32})
	if err != nil {
		t.Fatal(err)
	}
	// Flip a byte in each section and expect Verify to catch it.
	rd, _ := Open(ctx, BytesReader(data))
	for s := range numSections {
		sec := rd.h.sections[s]
		if sec.len == 0 {
			continue
		}
		bad := slices.Clone(data)
		bad[sec.off+sec.len/2] ^= 0xff
		brd, err := Open(ctx, BytesReader(bad))
		if err != nil {
			t.Fatal(err)
		}
		if err := brd.Verify(ctx); !errors.Is(err, ErrFormat) {
			t.Fatalf("%s corruption: %v", sectionNames[s], err)
		}
	}
	// Unknown versions are rejected.
	bad := slices.Clone(data)
	bad[8] = 9
	if _, err := Open(ctx, BytesReader(bad)); !errors.Is(err, ErrFormat) {
		t.Fatalf("version: %v", err)
	}
	if _, err := Open(ctx, BytesReader(data[:len(data)-1])); !errors.Is(err, ErrFormat) {
		t.Fatalf("truncation: %v", err)
	}
}

func TestClusteredProbeFindsNeighbors(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := rand.New(rand.NewPCG(4, 4))
	const dims, n = 64, 3000
	// Points around 20 topics, so clusters mean something.
	topics := make([][]float32, 20)
	for i := range topics {
		topics[i] = randVec(r, dims)
	}
	docs := makeDocs(r, n, dims)
	for i := range docs {
		v := docs[i].Vector
		tp := topics[i%20]
		for j := range v {
			v[j] = v[j]*0.3 + tp[j]
		}
		vec.Normalize(v)
	}
	byID := map[uint32][]float32{}
	for _, d := range docs {
		byID[d.ID] = d.Vector
	}
	data, info, err := Bytes(docs, WriteOptions{Dims: dims, ClusterThreshold: 1000})
	if err != nil || !info.Clustered {
		t.Fatalf("clustered=%v err=%v", info.Clustered, err)
	}
	rd, _ := Open(ctx, BytesReader(data))
	if err := rd.Verify(ctx); err != nil {
		t.Fatal(err)
	}
	ix, _ := rd.LoadIndex(ctx)
	if ix.Clusters == nil || len(ix.Clusters.Centers) < 20 {
		t.Fatalf("clusters: %+v", ix.Clusters)
	}
	// The exact nearest neighbor of a query should usually be in the probed
	// clusters.
	hits := 0
	for q := range 100 {
		query := slices.Clone(byID[uint32(100+q*7)])
		for j := range query {
			query[j] += float32(r.NormFloat64()) * 0.02
		}
		vec.Normalize(query)
		bestRow, best := -1, float32(-2)
		for row := range ix.Count {
			if s := vec.Dot(query, byID[ix.ID(row)]); s > best {
				bestRow, best = row, s
			}
		}
		for _, rg := range ix.ProbeRanges(query, max(4, len(ix.Clusters.Centers)/8)) {
			if bestRow >= rg[0] && bestRow < rg[1] {
				hits++
				break
			}
		}
	}
	if hits < 95 {
		t.Fatalf("probe recall %d/100", hits)
	}
}
