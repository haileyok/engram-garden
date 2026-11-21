package agent

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/haileyok/engram-garden/internal/lex"
	"github.com/haileyok/engram-garden/internal/spaceclient"
	"github.com/haileyok/engram-garden/internal/spacetest"
)

const sid = "01a16641-36cd-7695-a8b2-e41aada6d16d"

// archiveNet is an archive with two session identities in it, alice (a
// session) and bob (a subagent alice started).
func archiveNet(t *testing.T) *spacetest.Net {
	t.Helper()
	n := spacetest.NewTyped(t, "principal", lex.ArchiveType, "sessions")
	for _, did := range []string{"did:plc:alice", "did:plc:bob"} {
		n.NewAccount(did)
		n.AddMember(did)
	}
	n.AddMember(n.Authority.DID)
	return n
}

// archiveAs opens the archive as one of its identities, or as the
// principal (who only reads) when id is zero.
func archiveAs(t *testing.T, n *spacetest.Net, did string, id Identity) *Archive {
	t.Helper()
	c, err := spaceclient.New(n.Session(n.AccountByDID(did)), n.Dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	return &Archive{Client: c, Space: n.Space, Identity: id}
}

func alice(t *testing.T, n *spacetest.Net) *Archive {
	return archiveAs(t, n, "did:plc:alice", Identity{DID: "did:plc:alice", Kind: "session"})
}

// transcript is lines from..to-1, shaped like a harness's session file.
func transcript(from, to int) string {
	var b strings.Builder
	for i := from; i < to; i++ {
		fmt.Fprintf(&b, `{"type":"message","id":"e%05d","message":{"role":"user","content":"line %d"}}`+"\n", i, i)
	}
	return b.String()
}

func TestArchivePushAndPull(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	n := archiveNet(t)
	a := alice(t, n)

	data := transcript(0, 20)
	out, err := a.PushSession(ctx, PushSessionIn{
		Session: lex.Session{SessionID: sid, Title: "template tenancy audit", Summary: "Audit the handlers.", Status: "active", Cwd: "/root/worktrees/attie", Machine: "dev-box", Harness: "pi", GitHead: "cbb578c", Kind: "subagent", Parent: "did:plc:mallory"},
		Data:    data,
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Bytes != int64(len(data)) || out.Entries != 20 || out.Chunks != 1 || out.Stored != int64(len(data)) {
		t.Fatalf("push: %+v", out)
	}
	if out.URI != n.Space+"/did:plc:alice/garden.engram.session/"+sid {
		t.Fatalf("uri %q", out.URI)
	}

	// Bob reads it by ID, the principal by URI.
	bob := archiveAs(t, n, "did:plc:bob", Identity{DID: "did:plc:bob", Kind: "subagent", Parent: "did:plc:alice"})
	principal := archiveAs(t, n, n.Authority.DID, Identity{})
	for name, r := range map[string]*Archive{"bob": bob, "principal": principal} {
		for _, key := range []string{sid, out.URI} {
			got, err := r.PullSession(ctx, GetSessionIn{Session: key})
			if err != nil {
				t.Fatalf("%s pulling %s: %v", name, key, err)
			}
			if got.Data != data || got.Author != "did:plc:alice" || got.Title != "template tenancy audit" {
				t.Fatalf("%s pulled %d bytes by %s, %q", name, len(got.Data), got.Author, got.Title)
			}
			// Kind and parent are the identity's, whatever the caller said.
			if got.Kind != "session" || got.Parent != "" {
				t.Fatalf("kind %q parent %q, want the identity's", got.Kind, got.Parent)
			}
			if got.Head.EntryID != "e00019" || got.Head.Entries != 20 || got.StartedAt != "" || got.Status != "active" {
				t.Fatalf("record: %+v", got.Session)
			}
		}
	}

	// Only identities write.
	if _, err := principal.PushSession(ctx, PushSessionIn{Session: lex.Session{SessionID: "p1", Title: "t"}, Data: data}); err == nil {
		t.Fatal("the principal pushed a session")
	}
	list, err := principal.ListSessions(ctx, ListSessionsIn{})
	if err != nil || len(list.Sessions) != 1 || list.Sessions[0].URI != out.URI {
		t.Fatalf("list: %+v %v", list, err)
	}
}

func TestArchiveAppendGrowsTheLastChunk(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	n := archiveNet(t)
	a := alice(t, n)
	meta := lex.Session{SessionID: sid, Title: "t"}

	var stored strings.Builder
	var first string
	for i := range 5 {
		piece := transcript(i*10, i*10+10)
		out, err := a.PushSession(ctx, PushSessionIn{Session: meta, FromBytes: int64(stored.Len()), Data: piece})
		if err != nil {
			t.Fatal(err)
		}
		stored.WriteString(piece)
		if out.Chunks != 1 || out.Bytes != int64(stored.Len()) || out.Entries != (i+1)*10 {
			t.Fatalf("push %d: %+v", i, out)
		}
		got, _ := a.getSession(ctx, "did:plc:alice", sid)
		if i == 0 {
			first = got.Chunks[0].Blob.Link
		}
		if got.Chunks[0].FirstEntry != "e00000" || got.Chunks[0].LastEntry != fmt.Sprintf("e%05d", i*10+9) {
			t.Fatalf("chunk %+v", got.Chunks[0])
		}
	}
	// The replaced blobs aren't the space's any more.
	if _, err := FetchBlob(ctx, a.Client, a.Space, "did:plc:alice", first); err == nil {
		t.Fatal("a replaced chunk is still served")
	}

	// A fresh process (no tail in memory) reads the tail back and goes on.
	again := alice(t, n)
	piece := transcript(50, 60)
	if _, err := again.PushSession(ctx, PushSessionIn{Session: meta, FromBytes: int64(stored.Len()), Data: piece}); err != nil {
		t.Fatal(err)
	}
	stored.WriteString(piece)
	got, err := again.PullSession(ctx, GetSessionIn{Session: sid})
	if err != nil || got.Data != stored.String() || len(got.Chunks) != 1 {
		t.Fatalf("pulled %d bytes in %d chunks (%v), want %d", len(got.Data), len(got.Chunks), err, stored.Len())
	}
}

func TestArchiveChunksRollOver(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	n := archiveNet(t)
	a := alice(t, n)
	meta := lex.Session{SessionID: sid, Title: "t"}

	// ~100 bytes a line: 7.5 MB in pushes of at most MaxPushBytes.
	var stored strings.Builder
	line := 0
	for stored.Len() < 7_500_000 {
		var b strings.Builder
		for b.Len() < 2_000_000 {
			b.WriteString(transcript(line, line+1))
			line++
		}
		out, err := a.PushSession(ctx, PushSessionIn{Session: meta, FromBytes: int64(stored.Len()), Data: b.String()})
		if err != nil {
			t.Fatal(err)
		}
		stored.WriteString(b.String())
		if out.Bytes != int64(stored.Len()) {
			t.Fatalf("stored %d, want %d", out.Bytes, stored.Len())
		}
	}
	got, err := a.PullSession(ctx, GetSessionIn{Session: sid})
	if err != nil || got.Data != stored.String() {
		t.Fatalf("pull: %v (%d bytes, want %d)", err, len(got.Data), stored.Len())
	}
	if len(got.Chunks) != 3 {
		t.Fatalf("%d chunks for %d bytes, want 3", len(got.Chunks), stored.Len())
	}
	for i, c := range got.Chunks[:2] {
		if c.Bytes < MaxChunkBytes-200 || c.Bytes > MaxChunkBytes {
			t.Errorf("chunk %d is %d bytes", i, c.Bytes)
		}
	}
	if got.Head.Entries != line {
		t.Fatalf("entries %d, want %d", got.Head.Entries, line)
	}

	if _, err := a.PushSession(ctx, PushSessionIn{Session: meta, FromBytes: int64(stored.Len()), Data: strings.Repeat("x", MaxPushBytes+1)}); err == nil {
		t.Fatal("a push over MaxPushBytes was taken")
	}
}

func TestArchiveResyncAndPartialLines(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	n := archiveNet(t)
	a := alice(t, n)
	meta := lex.Session{SessionID: sid, Title: "t"}

	// A trailing line without its newline waits.
	data := transcript(0, 3) + `{"type":"message","id":"e00003"`
	out, err := a.PushSession(ctx, PushSessionIn{Session: meta, Data: data})
	if err != nil {
		t.Fatal(err)
	}
	if out.Bytes != int64(len(transcript(0, 3))) || out.Stored != out.Bytes || out.Entries != 3 {
		t.Fatalf("partial: %+v", out)
	}

	// Pushing from the wrong place writes nothing and says how much is stored.
	out, err = a.PushSession(ctx, PushSessionIn{Session: meta, FromBytes: 5, Data: transcript(3, 4)})
	if err != nil || !out.Resync || out.Bytes != int64(len(transcript(0, 3))) {
		t.Fatalf("resync: %+v %v", out, err)
	}

	// Replace starts the transcript over.
	whole := transcript(100, 104)
	out, err = a.PushSession(ctx, PushSessionIn{Session: meta, Replace: true, Data: whole})
	if err != nil || out.Bytes != int64(len(whole)) || out.Chunks != 1 {
		t.Fatalf("replace: %+v %v", out, err)
	}
	got, _ := a.PullSession(ctx, GetSessionIn{Session: sid})
	if got.Data != whole {
		t.Fatalf("after replace: %q", got.Data)
	}

	// A push with nothing new updates the record alone.
	meta.Status = lex.SessionEnded
	out, err = a.PushSession(ctx, PushSessionIn{Session: meta, FromBytes: int64(len(whole))})
	if err != nil || out.Bytes != int64(len(whole)) {
		t.Fatalf("metadata push: %+v %v", out, err)
	}
	got, _ = a.PullSession(ctx, GetSessionIn{Session: sid})
	if got.Status != lex.SessionEnded || got.Data != whole {
		t.Fatalf("after ending: %q %q", got.Status, got.Data)
	}
}

func TestArchiveConcurrentPushesKeepEveryLine(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	n := archiveNet(t)
	a := alice(t, n)
	meta := lex.Session{SessionID: sid, Title: "t"}
	base := transcript(0, 5)
	if _, err := a.PushSession(ctx, PushSessionIn{Session: meta, Data: base}); err != nil {
		t.Fatal(err)
	}
	// Two pushes from the same place: one lands, the other is told to resync.
	var wg sync.WaitGroup
	outs := make([]PushSessionOut, 2)
	for i := range 2 {
		wg.Go(func() {
			out, err := a.PushSession(ctx, PushSessionIn{Session: meta, FromBytes: int64(len(base)), Data: transcript(5+i*10, 15+i*10)})
			if err != nil {
				t.Error(err)
			}
			outs[i] = out
		})
	}
	wg.Wait()
	if outs[0].Resync == outs[1].Resync {
		t.Fatalf("both resync=%v", outs[0].Resync)
	}
	got, _ := a.PullSession(ctx, GetSessionIn{Session: sid})
	if entries := strings.Count(got.Data, "\n"); entries != 15 {
		t.Fatalf("%d lines stored, want 15", entries)
	}
}

func TestArchiveChecksBlobsAgainstTheirCID(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	n := archiveNet(t)
	a := alice(t, n)
	if _, err := a.PushSession(ctx, PushSessionIn{Session: lex.Session{SessionID: sid, Title: "t"}, Data: transcript(0, 3)}); err != nil {
		t.Fatal(err)
	}
	n.CorruptBlobs = true
	bob := archiveAs(t, n, "did:plc:bob", Identity{DID: "did:plc:bob"})
	if _, err := bob.PullSession(ctx, GetSessionIn{Session: sid}); err == nil || !strings.Contains(err.Error(), "doesn't match its CID") {
		t.Fatalf("a damaged chunk was read: %v", err)
	}
	// Growing a damaged tail fails too, rather than storing it again.
	again := alice(t, n)
	if _, err := again.PushSession(ctx, PushSessionIn{Session: lex.Session{SessionID: sid, Title: "t"}, FromBytes: int64(len(transcript(0, 3))), Data: transcript(3, 4)}); err == nil {
		t.Fatal("pushed onto a damaged tail")
	}
}

func TestArchiveDelete(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	n := archiveNet(t)
	a := alice(t, n)
	out, err := a.PushSession(ctx, PushSessionIn{Session: lex.Session{SessionID: sid, Title: "t"}, Data: transcript(0, 3)})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := a.getSession(ctx, "did:plc:alice", sid)
	bob := archiveAs(t, n, "did:plc:bob", Identity{DID: "did:plc:bob"})
	if _, err := bob.DeleteSession(ctx, DeleteSessionIn{Session: sid}); err == nil {
		t.Fatal("bob deleted alice's session")
	}
	del, err := a.DeleteSession(ctx, DeleteSessionIn{Session: sid})
	if err != nil || del.Deleted != out.URI {
		t.Fatalf("delete: %+v %v", del, err)
	}
	if _, err := a.PullSession(ctx, GetSessionIn{Session: sid}); err == nil {
		t.Fatal("a deleted session was pulled")
	}
	if _, err := FetchBlob(ctx, bob.Client, n.Space, "did:plc:alice", got.Chunks[0].Blob.Link); err == nil {
		t.Fatal("a deleted session's chunk is still served")
	}
}

func TestSplitChunks(t *testing.T) {
	t.Parallel()
	data := "aaaa\nbb\ncccccccccc\nd\n"
	got := splitChunks(data, 8)
	var parts []string
	for _, p := range got {
		parts = append(parts, p.data)
	}
	// A line longer than the limit is a chunk of its own.
	want := []string{"aaaa\nbb\n", "cccccccccc\n", "d\n"}
	if strings.Join(parts, "|") != strings.Join(want, "|") {
		t.Fatalf("chunks %q, want %q", parts, want)
	}
	if strings.Join(parts, "") != data {
		t.Fatal("chunks lost bytes")
	}
}
