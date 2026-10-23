package agent

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/haileyok/engram-garden/internal/lex"
	"github.com/haileyok/engram-garden/internal/spaceclient"
	"github.com/haileyok/engram-garden/internal/spacetest"
)

func sessionAgent(t *testing.T, n *spacetest.Net, did string) *Agent {
	t.Helper()
	acct := n.AccountByDID(did)
	c, err := spaceclient.New(n.Session(acct), n.Dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	return &Agent{Client: c, Space: n.Space}
}

func sessionNet(t *testing.T) *spacetest.Net {
	t.Helper()
	n := spacetest.New(t)
	for _, did := range []string{"did:plc:alice", "did:plc:bob"} {
		n.NewAccount(did)
		n.AddMember(did)
	}
	n.AddMember(n.Authority.DID)
	return n
}

// transcript is n complete JSONL lines, shaped like a harness's session file.
func transcript(n int) string {
	var b strings.Builder
	for i := range n {
		fmt.Fprintf(&b, `{"type":"message","id":"e%04d","message":{"role":"user","content":"line %d"}}`+"\n", i, i)
	}
	return b.String()
}

func entryCount(s string) int {
	n := 0
	for _, line := range strings.Split(s, "\n") {
		if line != "" {
			n++
		}
	}
	return n
}

func TestPushAndPullSession(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	n := sessionNet(t)
	alice := sessionAgent(t, n, "did:plc:alice")

	data := transcript(20)
	out, err := alice.PushSession(ctx, PushSessionIn{
		Session: lex.Session{SessionID: "01a16641-36cd-7695-a8b2-e41aada6d16d", Title: "template tenancy audit", Summary: "Audit the handlers.", Status: "active", Cwd: "/root/worktrees/attie", Machine: "dev-box", Harness: "pi", GitHead: "cbb578c"},
		Data:    data,
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Mode != lex.SessionModeBlob {
		t.Fatalf("mode %q, want blob (note %q)", out.Mode, out.Note)
	}
	if out.Bytes != int64(len(data)) || out.Entries != 20 || out.Chunks != 1 || out.Stored != int64(len(data)) {
		t.Fatalf("push: %+v", out)
	}

	got, err := alice.PullSession(ctx, GetSessionIn{Session: "01a16641-36cd-7695-a8b2-e41aada6d16d"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Data != data {
		t.Fatalf("pulled %d bytes, want %d", len(got.Data), len(data))
	}
	if got.Session.Title != "template tenancy audit" || got.Session.GitHead != "cbb578c" || got.Session.Status != "active" {
		t.Fatalf("manifest: %+v", got.Session)
	}
	if got.Session.Head == nil || got.Session.Head.Bytes != int64(len(data)) || got.Session.Head.Entries != 20 || got.Session.Head.EntryID != "e0019" {
		t.Fatalf("head: %+v", got.Session.Head)
	}

	// Another member reads it, transcript and all.
	bob := sessionAgent(t, n, "did:plc:bob")
	list, _, err := bob.Sessions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Author != "did:plc:alice" || list[0].SessionID != "01a16641-36cd-7695-a8b2-e41aada6d16d" {
		t.Fatalf("bob lists %+v", list)
	}
	theirs, err := bob.PullSession(ctx, GetSessionIn{Session: "01a16641-36cd-7695-a8b2-e41aada6d16d"})
	if err != nil {
		t.Fatal(err)
	}
	if theirs.Data != data {
		t.Fatal("bob's copy differs")
	}
}

func TestPushSessionAppends(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	n := sessionNet(t)
	alice := sessionAgent(t, n, "did:plc:alice")
	id := "session-append"

	first := transcript(3)
	if _, err := alice.PushSession(ctx, PushSessionIn{Session: lex.Session{SessionID: id, Title: "t"}, Data: first}); err != nil {
		t.Fatal(err)
	}
	// A second push carries only what grew.
	second := `{"type":"message","id":"e0003","message":{"role":"user","content":"more"}}` + "\n"
	out, err := alice.PushSession(ctx, PushSessionIn{Session: lex.Session{SessionID: id, Title: "t"}, FromBytes: int64(len(first)), Data: second})
	if err != nil {
		t.Fatal(err)
	}
	if out.Resync || out.Bytes != int64(len(first)+len(second)) || out.Entries != 4 {
		t.Fatalf("append: %+v", out)
	}
	got, err := alice.PullSession(ctx, GetSessionIn{Session: id})
	if err != nil {
		t.Fatal(err)
	}
	if got.Data != first+second {
		t.Fatalf("pulled %q, want %q", got.Data, first+second)
	}
}

func TestPushSessionHoldsBackPartialLine(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	n := sessionNet(t)
	alice := sessionAgent(t, n, "did:plc:alice")
	id := "session-partial"

	whole := transcript(2)
	partial := `{"type":"message","id":"e9999","message":{"role":"us`
	out, err := alice.PushSession(ctx, PushSessionIn{Session: lex.Session{SessionID: id, Title: "t"}, Data: whole + partial})
	if err != nil {
		t.Fatal(err)
	}
	if out.Stored != int64(len(whole)) || out.Bytes != int64(len(whole)) || out.Entries != 2 {
		t.Fatalf("partial: %+v", out)
	}
	// The rest of the line arrives later, from where the stored transcript ends.
	rest := "er\",\"content\":\"late\"}}\n"
	out, err = alice.PushSession(ctx, PushSessionIn{Session: lex.Session{SessionID: id, Title: "t"}, FromBytes: int64(len(whole)), Data: partial[0:] + rest})
	if err != nil {
		t.Fatal(err)
	}
	got, err := alice.PullSession(ctx, GetSessionIn{Session: id})
	if err != nil {
		t.Fatal(err)
	}
	if got.Data != whole+partial+rest {
		t.Fatalf("pulled %q", got.Data)
	}
	if out.Entries != 3 {
		t.Fatalf("entries %d, want 3", out.Entries)
	}
}

func TestPushSessionAsksForResync(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	n := sessionNet(t)
	alice := sessionAgent(t, n, "did:plc:alice")
	id := "session-resync"

	first := transcript(2)
	if _, err := alice.PushSession(ctx, PushSessionIn{Session: lex.Session{SessionID: id, Title: "t"}, Data: first}); err != nil {
		t.Fatal(err)
	}
	// A caller that thinks less is stored is told to start over, and nothing
	// is written.
	out, err := alice.PushSession(ctx, PushSessionIn{Session: lex.Session{SessionID: id, Title: "t"}, FromBytes: 0, Data: first + transcript(1)})
	if err != nil {
		t.Fatal(err)
	}
	if !out.Resync || out.Bytes != int64(len(first)) {
		t.Fatalf("resync: %+v", out)
	}
	// Starting over replaces the transcript rather than doubling it.
	whole := transcript(5)
	out, err = alice.PushSession(ctx, PushSessionIn{Session: lex.Session{SessionID: id, Title: "t"}, Replace: true, Data: whole})
	if err != nil {
		t.Fatal(err)
	}
	if out.Resync {
		t.Fatalf("unexpected resync: %+v", out)
	}
	got, err := alice.PullSession(ctx, GetSessionIn{Session: id})
	if err != nil {
		t.Fatal(err)
	}
	if got.Data != whole || got.Session.Head.Bytes != int64(len(whole)) {
		t.Fatalf("after resync: %q", got.Data)
	}
}

func TestPushSessionChunksBigTranscripts(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	n := sessionNet(t)
	alice := sessionAgent(t, n, "did:plc:alice")
	id := "session-big"

	line := `{"type":"message","id":"big","message":{"role":"user","content":"` + strings.Repeat("x", 4000) + `"}}` + "\n"
	data := strings.Repeat(line, 1000) // ~4 MB, so more than one chunk
	out, err := alice.PushSession(ctx, PushSessionIn{Session: lex.Session{SessionID: id, Title: "t"}, Data: data})
	if err != nil {
		t.Fatal(err)
	}
	if out.Chunks < 2 || out.Bytes != int64(len(data)) {
		t.Fatalf("push: %+v", out)
	}
	got, err := alice.PullSession(ctx, GetSessionIn{Session: id})
	if err != nil {
		t.Fatal(err)
	}
	if got.Data != data {
		t.Fatalf("pulled %d bytes, want %d", len(got.Data), len(data))
	}
}

func TestPushSessionFallsBackToChunkRecords(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	n := sessionNet(t)
	n.NoBlobs = true // a sign-in from before blobs were asked for
	alice := sessionAgent(t, n, "did:plc:alice")
	id := "session-record"

	data := transcript(50)
	out, err := alice.PushSession(ctx, PushSessionIn{Session: lex.Session{SessionID: id, Title: "t"}, Data: data})
	if err != nil {
		t.Fatal(err)
	}
	if out.Mode != lex.SessionModeRecord || out.Bytes != int64(len(data)) || out.Stored != int64(len(data)) {
		t.Fatalf("record mode: %+v", out)
	}
	if out.Note == "" {
		t.Fatal("a fallback should say why")
	}
	if out.Chunks == 0 {
		t.Fatal("no chunks")
	}
	got, err := alice.PullSession(ctx, GetSessionIn{Session: id})
	if err != nil {
		t.Fatal(err)
	}
	if got.Data != data {
		t.Fatalf("pulled %d bytes, want %d", len(got.Data), len(data))
	}
	// Chunk records don't show up as sessions of their own.
	list, _, err := alice.Sessions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].SessionID != id {
		t.Fatalf("listing shows %d sessions", len(list))
	}
}

func TestPushSessionFallsBackToMetadata(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	n := sessionNet(t)
	n.NoBlobs = true
	n.NoChunkRecords = true
	alice := sessionAgent(t, n, "did:plc:alice")
	id := "session-metadata"

	out, err := alice.PushSession(ctx, PushSessionIn{Session: lex.Session{SessionID: id, Title: "a handoff", Summary: "Read this one instead."}, Data: transcript(5)})
	if err != nil {
		t.Fatal(err)
	}
	if out.Mode != lex.SessionModeMetadata || out.Stored != 0 || out.Note == "" {
		t.Fatalf("metadata mode: %+v", out)
	}
	// The record is there, with the handoff in it...
	list, _, err := alice.Sessions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Summary != "Read this one instead." || list[0].Mode != lex.SessionModeMetadata {
		t.Fatalf("listed %+v", list)
	}
	// ...and there's no transcript to pull, which says so rather than
	// handing back an empty file.
	_, err = alice.PullSession(ctx, GetSessionIn{Session: id})
	if err == nil || !strings.Contains(err.Error(), "no stored transcript") {
		t.Fatalf("err %v", err)
	}
}

func TestPushSessionResyncsInRecordMode(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	n := sessionNet(t)
	n.NoBlobs = true
	alice := sessionAgent(t, n, "did:plc:alice")
	id := "session-record-resync"

	first := transcript(3)
	if _, err := alice.PushSession(ctx, PushSessionIn{Session: lex.Session{SessionID: id, Title: "t"}, Data: first}); err != nil {
		t.Fatal(err)
	}
	// Starting over must leave the transcript readable: the chunk records it
	// rewrites are the ones the old manifest named.
	whole := transcript(8)
	out, err := alice.PushSession(ctx, PushSessionIn{Session: lex.Session{SessionID: id, Title: "t"}, Replace: true, Data: whole})
	if err != nil {
		t.Fatal(err)
	}
	if out.Mode != lex.SessionModeRecord {
		t.Fatalf("mode %q", out.Mode)
	}
	got, err := alice.PullSession(ctx, GetSessionIn{Session: id})
	if err != nil {
		t.Fatal(err)
	}
	if got.Data != whole {
		t.Fatalf("pulled %d bytes, want %d", len(got.Data), len(whole))
	}
}

func TestDeleteSession(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	n := sessionNet(t)
	alice := sessionAgent(t, n, "did:plc:alice")
	id := "session-delete"

	if _, err := alice.PushSession(ctx, PushSessionIn{Session: lex.Session{SessionID: id, Title: "t"}, Data: transcript(5)}); err != nil {
		t.Fatal(err)
	}
	if _, err := alice.DeleteSession(ctx, DeleteSessionIn{Session: id}); err != nil {
		t.Fatal(err)
	}
	if _, err := alice.PullSession(ctx, GetSessionIn{Session: id}); err == nil {
		t.Fatal("pulled a deleted session")
	}
	// Someone else's session can't be deleted.
	if _, err := alice.PushSession(ctx, PushSessionIn{Session: lex.Session{SessionID: "session-alices", Title: "t"}, Data: transcript(1)}); err != nil {
		t.Fatal(err)
	}
	bob := sessionAgent(t, n, "did:plc:bob")
	if _, err := bob.DeleteSession(ctx, DeleteSessionIn{Session: "session-alices"}); err == nil {
		t.Fatal("deleted another member's session")
	}
}

func TestPushSessionRejectsBigPush(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	n := sessionNet(t)
	alice := sessionAgent(t, n, "did:plc:alice")
	_, err := alice.PushSession(ctx, PushSessionIn{Session: lex.Session{SessionID: "session-huge", Title: "t"}, Data: strings.Repeat("x", MaxPushBytes+1)})
	if err == nil || !strings.Contains(err.Error(), "the most is") {
		t.Fatalf("err %v", err)
	}
}
