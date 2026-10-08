package spacestore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/haileyok/engram-garden/internal/blob"
)

// realS3 opens the bucket named by ENGRAM_TEST_S3_* (a real provider, such
// as Wasabi) under a prefix unique to this test, and deletes everything
// under it when the test ends. It skips when ENGRAM_TEST_S3_ENDPOINT is
// unset. Providers like Wasabi bill deleted objects for a minimum storage
// period, so use a dedicated test bucket.
func realS3(t *testing.T) *blob.S3 {
	t.Helper()
	endpoint := os.Getenv("ENGRAM_TEST_S3_ENDPOINT")
	if endpoint == "" {
		t.Skip("ENGRAM_TEST_S3_ENDPOINT unset")
	}
	cfg := blob.S3Config{
		Endpoint: endpoint, Region: os.Getenv("ENGRAM_TEST_S3_REGION"), Bucket: os.Getenv("ENGRAM_TEST_S3_BUCKET"),
		Prefix:    fmt.Sprintf("conformance/spacestore-%s-%d/", time.Now().UTC().Format("20060102T150405"), time.Now().UnixNano()%100000),
		AccessKey: os.Getenv("ENGRAM_TEST_S3_ACCESS_KEY"), SecretKey: os.Getenv("ENGRAM_TEST_S3_SECRET_KEY"),
	}
	if cfg.Region == "" || cfg.Bucket == "" || cfg.AccessKey == "" || cfg.SecretKey == "" {
		t.Fatal("ENGRAM_TEST_S3_ENDPOINT is set, so ENGRAM_TEST_S3_REGION, _BUCKET, _ACCESS_KEY and _SECRET_KEY must be too")
	}
	s, err := blob.NewS3(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		objs, err := s.List(ctx, "")
		if err != nil {
			t.Errorf("cleanup: listing %s: %v", cfg.Prefix, err)
			return
		}
		for _, o := range objs {
			if err := s.Delete(ctx, o.Key); err != nil {
				t.Errorf("cleanup: deleting %s: %v", o.Key, err)
			}
		}
	})
	return s
}

// TestRealBucket runs a space's life through a real object store, such as
// Wasabi, when ENGRAM_TEST_S3_* is set: flushes, a cold load by a node with
// an empty disk cache, a merge, garbage collection, and fencing. It mirrors
// TestMergeAndGC and TestFencing, which use a local directory.
func TestRealBucket(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := realS3(t)
	cond, err := blob.Probe(ctx, s)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	t.Logf("conditional writes honored: %v", cond)

	// The bucket stamps objects with its own clock, so the test clock is the
	// real one plus an offset that jumps forward to trigger merges and make
	// objects old enough to collect.
	var offset atomic.Int64
	now := func() time.Time { return time.Now().Add(time.Duration(offset.Load())) }
	f := newFixture(t)
	f.blob = s
	f.opt = Options{MaxSegments: 3, Now: now, MergeInterval: time.Hour, MinRetention: 90 * 24 * time.Hour, ConditionalWrites: cond}
	// cold is a node with nothing on local disk: every read goes to the
	// bucket.
	cold := func(mod ...func(*Options)) *Node {
		return f.node(append([]func(*Options){func(o *Options) { o.CacheDir = t.TempDir() }}, mod...)...)
	}

	n := f.node()
	configure(t, n, SpaceConfig{ModelInfo: modelA})
	for i := range 5 {
		ms := []Memory{mem("did:plc:alice", fmt.Sprintf("k%d", i), fmt.Sprintf("memory about topic%d", i))}
		var dels []string
		if i == 4 {
			dels = []string{"k0", "k1"}
		}
		if err := n.ApplyRepoChanges(ctx, testSpace, "did:plc:alice", pos(fmt.Sprint(i)), ms, dels, false); err != nil {
			t.Fatal(err)
		}
		if err := n.Flush(ctx, testSpace); err != nil {
			t.Fatal(err)
		}
	}
	if st, _ := n.Status(ctx, testSpace); st.Segments <= 3 || st.Memories != 3 {
		t.Fatalf("before the merge: %+v", st)
	}

	// A cold node loads the space from the bucket alone and answers from
	// range reads.
	c := cold()
	res := search(t, c, modelA, "topic3", Filter{})
	if len(res.Hits) != 3 || res.Hits[0].Rkey != "k3" || res.Approximate {
		t.Fatalf("cold search: %+v", res)
	}
	if reads, bytes := c.RemoteReads(); reads == 0 {
		t.Fatalf("cold search made no range reads from the bucket (%d bytes)", bytes)
	} else {
		t.Logf("cold search: %d range reads, %d bytes", reads, bytes)
	}
	if h, err := c.Get(ctx, testSpace, "did:plc:alice", "k3"); err != nil || h.Text != "memory about topic3" {
		t.Fatalf("cold get: %+v %v", h, err)
	}

	// A day later, the next flush merges the pile of small segments.
	offset.Add(int64(2 * time.Hour))
	_ = n.ApplyRepoChanges(ctx, testSpace, "did:plc:alice", pos("5"), []Memory{mem("did:plc:alice", "k5", "memory about topic5")}, nil, false)
	if err := n.Flush(ctx, testSpace); err != nil {
		t.Fatal(err)
	}
	if st, _ := n.Status(ctx, testSpace); st.Segments != 1 || st.Memories != 4 {
		t.Fatalf("after merge: %+v", st)
	}
	for _, node := range []*Node{n, cold()} {
		res := search(t, node, modelA, "topic3", Filter{})
		if len(res.Hits) != 4 || res.Hits[0].Rkey != "k3" {
			t.Fatalf("search after merge: %+v", res.Hits)
		}
		if _, err := node.Get(ctx, testSpace, "did:plc:alice", "k1"); !errors.Is(err, ErrNotFound) {
			t.Fatal("merged-away deletion came back")
		}
	}

	count := func(sub string) (n int) {
		objs, err := s.List(ctx, spacePrefix(testSpace))
		if err != nil {
			t.Fatal(err)
		}
		for _, o := range objs {
			if strings.Contains(o.Key, sub) {
				n++
			}
		}
		return n
	}
	if got := count("/seg-"); got != 7 {
		t.Fatalf("after merge: %d segment objects, want 6 inputs + 1 merged", got)
	}

	// Garbage collection goes by the age the bucket reports. Nothing is old
	// enough yet; after 100 days, only the merged segment and the current
	// manifests are left, and the orphan from a "crashed flush" goes too.
	_ = blob.PutBytes(ctx, s, SegmentKey(testSpace, "orphan"), []byte("x"), false)
	n.mu.Lock()
	sp := n.spaces[testSpace]
	n.mu.Unlock()
	gc := func() {
		t.Helper()
		sp.flushMu.Lock()
		err := sp.gcLocked(ctx)
		sp.flushMu.Unlock()
		if err != nil {
			t.Fatal(err)
		}
	}
	gc()
	if got := count("/seg-"); got != 8 {
		t.Fatalf("collected too early: %d segment objects", got)
	}
	offset.Add(int64(100 * 24 * time.Hour))
	gc()
	if got := count("/seg-"); got != 1 {
		t.Fatalf("after GC: %d segment objects, want only the merged one", got)
	}
	if got := count("/manifest-"); got != 2 {
		t.Fatalf("after GC: %d manifests, want the current one and GC's own", got)
	}
	if res := search(t, cold(), modelA, "topic3", Filter{}); len(res.Hits) != 4 {
		t.Fatalf("search after GC: %+v", res.Hits)
	}

	// Two writers holding the same token: only a store that honors
	// conditional writes stops the second.
	if !cond {
		t.Log("skipping the racing-writers check: the bucket doesn't honor conditional writes")
		return
	}
	b, c2 := cold(), cold()
	for _, w := range []struct {
		node *Node
		text string
	}{{b, "from b"}, {c2, "from c"}} {
		if err := w.node.ApplyRepoChanges(ctx, testSpace, "did:plc:bob", pos("r1"), []Memory{mem("did:plc:bob", "b1", w.text)}, nil, false); err != nil {
			t.Fatal(err)
		}
	}
	if err := b.Flush(ctx, testSpace); err != nil {
		t.Fatal(err)
	}
	if err := c2.Flush(ctx, testSpace); !errors.Is(err, ErrStaleOwner) {
		t.Fatalf("racing writer: %v", err)
	}
}
