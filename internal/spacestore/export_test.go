package spacestore

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"strings"
	"testing"

	"github.com/haileyok/engram-garden/internal/blob"
)

func TestExportImport(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	src := newFixture(t)
	n := src.node()
	seed(t, n, 50, modelA)
	// Unflushed changes are included: export flushes first.
	if err := n.ApplyRepoChanges(ctx, testSpace, "did:plc:bob", pos("rb"), []Memory{mem("did:plc:bob", "b1", "bob's buffered memory")}, nil, false); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := n.Export(ctx, testSpace, &buf); err != nil {
		t.Fatal(err)
	}

	dst := newFixture(t)
	m := dst.node(func(o *Options) { o.Lease = func(string) (uint64, bool) { return 7, true } })
	got, err := m.Import(ctx, bytes.NewReader(buf.Bytes()))
	if err != nil || got != testSpace {
		t.Fatalf("import: %q %v", got, err)
	}
	st, err := m.Status(ctx, testSpace)
	if err != nil || st.Memories != 51 || st.Token != 7 {
		t.Fatalf("imported status %+v %v", st, err)
	}
	if res := search(t, m, modelA, "buffered memory", Filter{}); res.Hits[0].Rkey != "b1" {
		t.Fatalf("search after import: %+v", res.Hits)
	}
	if p, _ := m.RepoState(ctx, testSpace, "did:plc:bob"); p == nil || p.Rev != "rb" {
		t.Fatalf("positions not imported: %+v", p)
	}
}

func TestImportRejectsDamage(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	src := newFixture(t)
	n := src.node()
	seed(t, n, 20, modelA)
	var buf bytes.Buffer
	if err := n.Export(ctx, testSpace, &buf); err != nil {
		t.Fatal(err)
	}
	// Rewrite the tar with one byte flipped in the segment.
	var bad bytes.Buffer
	tr, tw := tar.NewReader(bytes.NewReader(buf.Bytes())), tar.NewWriter(&bad)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		body, _ := io.ReadAll(tr)
		if strings.HasPrefix(h.Name, "seg-") {
			body[len(body)/2] ^= 0xff
		}
		_ = tw.WriteHeader(h)
		_, _ = tw.Write(body)
	}
	_ = tw.Close()
	dst := newFixture(t)
	if _, err := dst.node().Import(ctx, &bad); err == nil {
		t.Fatal("damaged segment imported")
	}
	if objs, _ := dst.blob.List(ctx, "spaces/"); len(objs) != 0 {
		t.Fatalf("a failed import left %d objects", len(objs))
	}
	// A manifest without its segments is rejected too.
	var partial bytes.Buffer
	tr, tw = tar.NewReader(bytes.NewReader(buf.Bytes())), tar.NewWriter(&partial)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		body, _ := io.ReadAll(tr)
		if h.Name == ExportManifestName {
			_ = tw.WriteHeader(h)
			_, _ = tw.Write(body)
		}
	}
	_ = tw.Close()
	if _, err := dst.node().Import(ctx, &partial); err == nil || !strings.Contains(err.Error(), "missing segment") {
		t.Fatalf("partial export: %v", err)
	}
	_ = blob.ErrNotFound
}
