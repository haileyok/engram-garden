package sessionindex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ipfs/go-cid"
	"github.com/multiformats/go-multihash"

	"github.com/haileyok/engram-garden/internal/blob"
	"github.com/haileyok/engram-garden/internal/embed"
	"github.com/haileyok/engram-garden/internal/lex"
	"github.com/haileyok/engram-garden/internal/spacestore"
)

const (
	spaceURI = "at://did:plc:p/space/garden.engram.archive/sessions"
	did      = "did:plc:alice"
)

var model = lex.ModelInfo{Model: "hashing-64", ModelDigest: embed.HashingDigest, Dims: 64}

// blobs serves chunks by CID and counts reads.
type blobs struct {
	mu    sync.Mutex
	data  map[string][]byte
	reads map[string]int
}

func (b *blobs) put(data string) lex.Blob {
	sum, _ := multihash.Sum([]byte(data), multihash.SHA2_256, -1)
	c := cid.NewCidV1(cid.Raw, sum).String()
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.data == nil {
		b.data, b.reads = map[string][]byte{}, map[string]int{}
	}
	b.data[c] = []byte(data)
	return lex.Blob{Link: c, Size: int64(len(data))}
}

func (b *blobs) FetchBlob(_ context.Context, _, _, link string) ([]byte, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.reads[link]++
	d, ok := b.data[link]
	if !ok {
		return nil, errors.New("no blob")
	}
	return d, nil
}

func (b *blobs) total() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := 0
	for _, r := range b.reads {
		n += r
	}
	return n
}

// turns is n turns of transcript lines from turn `from`, each with a step
// and a reply.
func turns(from, n int) string {
	var b strings.Builder
	for i := from; i < from+n; i++ {
		ts := time.Date(2026, 11, 21, 0, 0, 0, 0, time.UTC).Add(time.Duration(i) * time.Second).Format(time.RFC3339Nano)
		fmt.Fprintf(&b, `{"type":"message","id":"u%d","timestamp":"%s","message":{"role":"user","content":[{"type":"text","text":"task number %d about widget %d"}]}}`+"\n", i, ts, i, i)
		fmt.Fprintf(&b, `{"type":"message","id":"a%d","timestamp":"%s","message":{"role":"assistant","content":[{"type":"text","text":"working on widget %d"},{"type":"toolCall","id":"c%d","name":"bash","arguments":{"command":"make widget%d"}}]}}`+"\n", i, ts, i, i, i)
		fmt.Fprintf(&b, `{"type":"message","id":"r%d","timestamp":"%s","message":{"role":"toolResult","toolCallId":"c%d","toolName":"bash","isError":false,"content":[{"type":"text","text":"ok"}]}}`+"\n", i, ts, i)
		fmt.Fprintf(&b, `{"type":"message","id":"f%d","timestamp":"%s","message":{"role":"assistant","content":[{"type":"text","text":"widget %d is done"}]}}`+"\n", i, ts, i)
	}
	return b.String()
}

// record is a session record naming data as chunks cut every `every` turns'
// worth of lines: the last chunk is the one that grows.
func record(b *blobs, chunks []string) map[string]any {
	s := lex.Session{SessionID: "s1", Title: "widgets", UpdatedAt: "2026-11-21T00:00:00Z"}
	for _, c := range chunks {
		s.Chunks = append(s.Chunks, lex.SessionChunkRef{Blob: b.put(c), Bytes: int64(len(c))})
	}
	raw, _ := json.Marshal(s.Record())
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	return m
}

type harness struct {
	t     *testing.T
	x     *Indexer
	b     *blobs
	st    *spacestore.Node
	state json.RawMessage
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	st, err := spacestore.New(spacestore.Options{Blob: blob.Dir{Root: t.TempDir()}, CacheDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close(context.Background()) })
	if _, err := st.SetConfig(context.Background(), spaceURI, &lex.Config{ModelInfo: model}); err != nil {
		t.Fatal(err)
	}
	b := &blobs{}
	return &harness{t: t, b: b, st: st, x: &Indexer{Store: st, Blobs: b, Embed: embed.HashingProvider{}, BatchBytes: 1}}
}

// derive runs Derive, applying batches to the store; failAfter > 0 stops
// it after that many batches, as a sync cut short.
func (h *harness) derive(rec map[string]any, full bool, failAfter int) error {
	batches := 0
	return h.x.Derive(context.Background(), spaceURI, did, h.state, []Change{{Rkey: "s1", Record: rec}}, full, func(bt Batch) error {
		if failAfter > 0 && batches == failAfter {
			return errors.New("cut short")
		}
		batches++
		if err := h.st.ApplyRepoChanges(context.Background(), spaceURI, did, spacestore.RepoPosition{State: bt.State}, bt.Upserts, bt.Deletes, false); err != nil {
			return err
		}
		h.state = bt.State
		return nil
	})
}

func (h *harness) passages() map[string]string {
	hits, _, err := h.st.List(context.Background(), spaceURI, 10000, "", spacestore.Filter{Author: did})
	if err != nil {
		h.t.Fatal(err)
	}
	out := map[string]string{}
	for _, hit := range hits {
		out[hit.Rkey] = hit.Text
	}
	return out
}

func TestDeriveReadsOnlyWhatGrew(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	c1, c2, c3 := turns(0, 5), turns(5, 5), turns(10, 2)
	if err := h.derive(record(h.b, []string{c1, c2, c3}), false, 0); err != nil {
		t.Fatal(err)
	}
	if h.b.total() != 3 {
		t.Fatalf("first read %d chunks, want 3", h.b.total())
	}
	// The last chunk grows and a new one follows: only those two are read.
	c3b := c3 + turns(12, 3)
	c4 := turns(15, 2)
	if err := h.derive(record(h.b, []string{c1, c2, c3b, c4}), false, 0); err != nil {
		t.Fatal(err)
	}
	if h.b.total() != 5 {
		t.Fatalf("the append read %d chunks, want 2", h.b.total()-3)
	}
	// The same as reading it all at once.
	whole := newHarness(t)
	if err := whole.derive(record(whole.b, []string{c1, c2, c3b, c4}), true, 0); err != nil {
		t.Fatal(err)
	}
	got, want := h.passages(), whole.passages()
	if len(want) < 17*2 {
		t.Fatalf("only %d passages", len(want))
	}
	for k, w := range want {
		if got[k] != w {
			t.Fatalf("%s: %q, want %q", k, got[k], w)
		}
	}
	if len(got) != len(want) {
		t.Fatalf("%d passages, want %d", len(got), len(want))
	}
}

func TestDeriveResumesAfterBeingCutShort(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	chunks := []string{turns(0, 4), turns(4, 4), turns(8, 4), turns(12, 4)}
	rec := record(h.b, chunks)
	if err := h.derive(rec, true, 2); err == nil {
		t.Fatal("not cut short")
	}
	readBefore := h.b.total()
	// Carries on, even as a full read, without starting over.
	if err := h.derive(rec, true, 0); err != nil {
		t.Fatal(err)
	}
	if again := h.b.total() - readBefore; again > len(chunks)-1 {
		t.Fatalf("resuming read %d chunks of %d", again, len(chunks))
	}
	whole := newHarness(t)
	if err := whole.derive(record(whole.b, chunks), true, 0); err != nil {
		t.Fatal(err)
	}
	got, want := h.passages(), whole.passages()
	if len(got) != len(want) {
		t.Fatalf("%d passages, want %d", len(got), len(want))
	}
}

func TestDeriveStartsOverForAnotherModel(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	rec := record(h.b, []string{turns(0, 3)})
	if err := h.derive(rec, false, 0); err != nil {
		t.Fatal(err)
	}
	if !Current(h.state, &lex.Config{ModelInfo: model}) {
		t.Fatal("state isn't current")
	}
	other := lex.Config{ModelInfo: lex.ModelInfo{Model: "hashing-32", ModelDigest: embed.HashingDigest, Dims: 32}}
	if Current(h.state, &other) {
		t.Fatal("state counts as current for another model")
	}
	if _, err := h.st.SetConfig(context.Background(), spaceURI, &other); err != nil {
		t.Fatal(err)
	}
	if err := h.derive(rec, false, 0); !errors.Is(err, ErrNeedFull) {
		t.Fatalf("incremental after a model change: %v", err)
	}
	before := h.b.total()
	if err := h.derive(rec, true, 0); err != nil {
		t.Fatal(err)
	}
	if h.b.total() == before {
		t.Fatal("a full read after a model change read nothing")
	}
	keys := slices.Collect(func(yield func(string) bool) {
		for k := range h.passages() {
			if !yield(k) {
				return
			}
		}
	})
	if len(keys) == 0 {
		t.Fatal("nothing indexed with the new model")
	}
}

func TestNormalizeRepo(t *testing.T) {
	t.Parallel()
	for _, u := range []string{"git@github.com:haileyok/engram-garden.git", "https://github.com/haileyok/engram-garden", "ssh://git@github.com/haileyok/engram-garden.git", "github.com/HaileyOK/engram-garden/"} {
		if got := NormalizeRepo(u); got != "github.com/haileyok/engram-garden" {
			t.Errorf("%s: %s", u, got)
		}
	}
}

// recordWith is record with the session's other fields set.
func recordWith(b *blobs, chunks []string, edit func(*lex.Session)) map[string]any {
	m := record(b, chunks)
	raw, _ := json.Marshal(m)
	var s lex.Session
	_ = json.Unmarshal(raw, &s)
	s.SessionID = "s1"
	edit(&s)
	raw, _ = json.Marshal(s.Record())
	out := map[string]any{}
	_ = json.Unmarshal(raw, &out)
	return out
}

func (h *harness) hits() []spacestore.Hit {
	hits, _, err := h.st.List(context.Background(), spaceURI, 10000, "", spacestore.Filter{Author: did})
	if err != nil {
		h.t.Fatal(err)
	}
	return hits
}

func TestDeriveNoticesARewrittenLastChunk(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	if err := h.derive(record(h.b, []string{turns(0, 3)}), false, 0); err != nil {
		t.Fatal(err)
	}
	// The same size, other words: one chunk, so the prefix of full chunks
	// can't tell.
	other := strings.ReplaceAll(turns(0, 3), "widget", "gadget")
	if err := h.derive(record(h.b, []string{other}), false, 0); err != nil {
		t.Fatal(err)
	}
	for k, text := range h.passages() {
		if strings.Contains(text, "widget") && !strings.HasSuffix(k, ":session:record") {
			t.Fatalf("%s is from the old transcript: %q", k, text)
		}
	}
	if len(h.passages()) == 0 {
		t.Fatal("the new transcript wasn't cut")
	}
}

func TestDeriveRetagsWhenFacetsChange(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	chunks := []string{turns(0, 3)}
	if err := h.derive(recordWith(h.b, chunks, func(s *lex.Session) { s.Branch = "old" }), false, 0); err != nil {
		t.Fatal(err)
	}
	if err := h.derive(recordWith(h.b, chunks, func(s *lex.Session) { s.Branch = "new"; s.Title = ""; s.Summary = "" }), false, 0); err != nil {
		t.Fatal(err)
	}
	for _, hit := range h.hits() {
		if TagValue(hit.Tags, TagBranch) != "new" {
			t.Fatalf("%s still says branch %q", hit.Rkey, TagValue(hit.Tags, TagBranch))
		}
		if TagValue(hit.Tags, TagKind) == KindSession {
			t.Fatalf("the session passage stayed with no title or summary: %+v", hit)
		}
	}
}

func TestDeriveStateCurrency(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	rec := record(h.b, []string{turns(0, 4), turns(4, 4)})
	if err := h.derive(rec, true, 1); err == nil {
		t.Fatal("not cut short")
	}
	cfg := &lex.Config{ModelInfo: model}
	if Current(h.state, cfg) {
		t.Fatal("a sync cut short counts as current, so it would never be finished")
	}
	if err := h.derive(rec, true, 0); err != nil {
		t.Fatal(err)
	}
	if !Current(h.state, cfg) {
		t.Fatal("a finished sync isn't current")
	}
	if Current(h.state, &lex.Config{ModelInfo: model, DocumentPrefix: "search_document: "}) {
		t.Fatal("a new document prefix doesn't call for embedding again")
	}
}

func TestDeriveSessionIDsWithColons(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	rec := recordWith(h.b, []string{turns(0, 2)}, func(s *lex.Session) { s.SessionID = "proj:123" })
	ch := []Change{{Rkey: "proj:123", Record: rec}}
	run := func(full bool) {
		t.Helper()
		err := h.x.Derive(context.Background(), spaceURI, did, h.state, ch, full, func(bt Batch) error {
			h.state = bt.State
			return h.st.ApplyRepoChanges(context.Background(), spaceURI, did, spacestore.RepoPosition{State: bt.State}, bt.Upserts, bt.Deletes, false)
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	run(false)
	n := len(h.passages())
	run(true) // a full re-export keeps what's still there
	if n == 0 || len(h.passages()) != n {
		t.Fatalf("%d passages, then %d after a full read", n, len(h.passages()))
	}
}

func TestFacetsAreRedacted(t *testing.T) {
	t.Parallel()
	if got := NormalizeRepo("https://hailey:hunter2hunter2@github.com/haileyok/engram-garden.git"); got != "github.com/haileyok/engram-garden" {
		t.Fatalf("repo %q", got)
	}
	h := newHarness(t)
	var b strings.Builder
	b.WriteString(`{"type":"message","id":"u1","timestamp":"2026-11-21T00:00:00Z","message":{"role":"user","content":"go"}}` + "\n")
	b.WriteString(`{"type":"message","id":"a1","timestamp":"2026-11-21T00:00:01Z","message":{"role":"assistant","content":[{"type":"toolCall","id":"c1","name":"read","arguments":{"path":"/tmp/ghp_aB3dE5fG7hI9jK1lM3nO5pQ7rS9tU1vW3xY5.txt"}}]}}` + "\n")
	if err := h.derive(recordWith(h.b, []string{b.String()}, func(s *lex.Session) { s.Branch = "fix/token=abcdef123456" }), false, 0); err != nil {
		t.Fatal(err)
	}
	for _, hit := range h.hits() {
		for _, tag := range hit.Tags {
			if strings.Contains(tag, "ghp_aB3dE5") || strings.Contains(tag, "abcdef123456") {
				t.Fatalf("tag %q", tag)
			}
		}
	}
}

// A full last chunk that another chunk follows is read once, not again
// with the next one, and the session isn't cut over.
func TestDeriveMovesPastAFullTail(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	a, b := turns(0, 3), turns(3, 2)
	if err := h.derive(record(h.b, []string{a}), false, 0); err != nil {
		t.Fatal(err)
	}
	if err := h.derive(record(h.b, []string{a, b}), false, 0); err != nil {
		t.Fatal(err)
	}
	b2 := b + turns(5, 1)
	if err := h.derive(record(h.b, []string{a, b2}), false, 0); err != nil {
		t.Fatal(err)
	}
	aLink := h.b.put(a).Link
	if n := h.b.reads[aLink]; n != 1 {
		t.Fatalf("the first chunk was read %d times", n)
	}
}

// Without startedAt, the session passage's time follows updatedAt.
func TestSessionPassageTimeFollowsUpdatedAt(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	chunks := []string{turns(0, 1)}
	for _, at := range []string{"2026-11-21T01:00:00Z", "2026-11-22T01:00:00Z"} {
		if err := h.derive(recordWith(h.b, chunks, func(s *lex.Session) { s.UpdatedAt = at }), false, 0); err != nil {
			t.Fatal(err)
		}
	}
	for _, hit := range h.hits() {
		if TagValue(hit.Tags, TagKind) == KindSession && hit.CreatedAt.Format(time.RFC3339) != "2026-11-22T01:00:00Z" {
			t.Fatalf("session passage at %s", hit.CreatedAt)
		}
	}
}
