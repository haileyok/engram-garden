package appview

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/haileyok/engram-garden/internal/agent"
	"github.com/haileyok/engram-garden/internal/blob"
	"github.com/haileyok/engram-garden/internal/control/controltest"
	"github.com/haileyok/engram-garden/internal/embed"
	"github.com/haileyok/engram-garden/internal/indexer"
	"github.com/haileyok/engram-garden/internal/lex"
	"github.com/haileyok/engram-garden/internal/sessionindex"
	"github.com/haileyok/engram-garden/internal/spaceclient"
	"github.com/haileyok/engram-garden/internal/spacestore"
	"github.com/haileyok/engram-garden/internal/spacetest"
)

// piLines writes transcript entries in pi's session format.
type piLines struct {
	b strings.Builder
	n int
}

func (p *piLines) entry(typ string, fields map[string]any) string {
	p.n++
	id := fmt.Sprintf("x%04d", p.n)
	m := map[string]any{"type": typ, "id": id, "timestamp": time.Date(2026, 11, 21, 10, 0, p.n, 0, time.UTC).Format(time.RFC3339Nano)}
	for k, v := range fields {
		m[k] = v
	}
	raw, _ := json.Marshal(m)
	p.b.Write(raw)
	p.b.WriteByte('\n')
	return id
}

func (p *piLines) user(text string) string {
	return p.entry("message", map[string]any{"message": map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": text}}}})
}

func (p *piLines) say(text string, calls ...[3]string) string {
	content := []any{map[string]any{"type": "thinking", "thinking": "private reasoning"}}
	if text != "" {
		content = append(content, map[string]any{"type": "text", "text": text})
	}
	for _, c := range calls {
		content = append(content, map[string]any{"type": "toolCall", "id": c[0], "name": c[1], "arguments": json.RawMessage(c[2])})
	}
	return p.entry("message", map[string]any{"message": map[string]any{"role": "assistant", "content": content}})
}

func (p *piLines) result(id, tool, out string, isErr bool) string {
	return p.entry("message", map[string]any{"message": map[string]any{"role": "toolResult", "toolCallId": id, "toolName": tool, "isError": isErr, "content": []any{map[string]any{"type": "text", "text": out}}}})
}

type archiveFixture struct {
	n      *spacetest.Net
	srv    *Server
	st     *spacestore.Node
	alice  *spacetest.Account
	bob    *spacetest.Account
	al, bo *agent.Archive
}

// newArchiveFixture is an archive with two identities in it (alice, and
// bob, a subagent alice started) and an appview indexing it.
func newArchiveFixture(t *testing.T, limits spacestore.Limits) *archiveFixture {
	t.Helper()
	ctx := context.Background()
	n := spacetest.NewTyped(t, "principal", lex.ArchiveType, "sessions")
	alice := n.NewAccount("did:plc:alice")
	bob := n.NewAccount("did:plc:bob")
	for _, a := range []*spacetest.Account{alice, bob, n.Authority} {
		n.AddMember(a.DID)
	}
	n.Put(n.Authority, lex.ConfigCollection, lex.ConfigRkey, lex.Config{ModelInfo: model}.Record(time.Now()))
	auth := newFakeAuth(n)
	auth.sessions["seed"] = n.Authority.DID
	grants := &Grants{DB: controltest.New(t), Auth: auth}
	if err := grants.Put(ctx, Grant{Space: n.Space, DID: n.Authority.DID, SessionID: "seed", GrantedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	client, err := spaceclient.NewDelegated(grants, n.Dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	st, err := spacestore.New(spacestore.Options{Blob: blob.Dir{Root: t.TempDir()}, CacheDir: t.TempDir(), Limits: limits})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close(context.Background()) })
	srv := &Server{
		Store: st, Dir: n.Dir, ServiceDID: serviceDID, Spaces: []string{n.Space}, Grants: grants,
		Indexer: &indexer.Indexer{Store: st, Client: client, Dir: n.Dir,
			Sessions: &sessionindex.Indexer{Store: st, Blobs: client, Embed: embed.HashingProvider{}, BatchBytes: 400}},
		TextSearch: &QueryEmbedder{Provider: embed.HashingProvider{}},
	}
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(hs.Close)
	srv.PublicURL = hs.URL
	open := func(a *spacetest.Account, id agent.Identity) *agent.Archive {
		c, err := spaceclient.New(n.Session(a), n.Dir, nil)
		if err != nil {
			t.Fatal(err)
		}
		return &agent.Archive{Client: c, Space: n.Space, Identity: id, AppviewURL: hs.URL, AppviewDID: serviceDID}
	}
	return &archiveFixture{n: n, srv: srv, st: st, alice: alice, bob: bob,
		al: open(alice, agent.Identity{DID: alice.DID, Kind: "session"}),
		bo: open(bob, agent.Identity{DID: bob.DID, Kind: "subagent", Parent: alice.DID})}
}

// TestArchiveIndexAndSearch: a session identity keeps its transcript in an
// archive; the appview cuts it into passages, embeds them, and agents
// search them and read around a hit. Appends read only the growing chunk;
// a rewritten or deleted session leaves nothing stale behind.
func TestArchiveIndexAndSearch(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newArchiveFixture(t, spacestore.Limits{})
	n, srv, alice, bob, al, bo := f.n, f.srv, f.alice, f.bob, f.al, f.bo
	sync := func() {
		t.Helper()
		if err := srv.Indexer.SyncSpace(ctx, n.Space); err != nil {
			t.Fatal(err)
		}
	}

	var p piLines
	p.entry("session", map[string]any{"cwd": "/root/eg"})
	p.user("why does the indexer drop memories when the model changes?")
	p.entry("custom_message", map[string]any{"customType": "engram-recall", "content": "<memories>recalled memory text</memories>"})
	p.say("Let me run the indexer tests.", [3]string{"c1", "bash", `{"command":"cd /root/eg && go test ./internal/indexer/ -run TestModelChange"}`})
	fail := p.result("c1", "bash", "--- FAIL: TestModelChange\n    indexer_test.go:88: dropped 3 memories after SetConfig\nFAIL", true)
	p.say("SetConfig marks the resync before the new vectors exist, so the position has to wait for them.",
		[3]string{"c2", "edit", `{"path":"internal/indexer/indexer.go","oldText":"a","newText":"b"}`})
	p.result("c2", "edit", "ok", false)
	p.say("Fixed: positions wait for the new model's vectors. The deploy token was ghp_aB3dE5fG7hI9jK1lM3nO5pQ7rS9tU1vW3xY5.")
	first := p.b.String()
	meta := lex.Session{SessionID: "s1", Title: "indexer drops memories on model change", Summary: "Fixing the resync.", Repo: "git@github.com:haileyok/engram-garden.git", Branch: "hailey/fix", Status: "active"}
	if _, err := al.PushSession(ctx, agent.PushSessionIn{Session: meta, Data: first}); err != nil {
		t.Fatal(err)
	}
	if _, err := bo.PushSession(ctx, agent.PushSessionIn{Session: lex.Session{SessionID: "s2", Title: "look up kubectl rollout"}, Data: (func() string {
		var q piLines
		q.user("check the rollout")
		q.say("Checking.", [3]string{"k1", "bash", `{"command":"kubectl rollout status deploy/appview"}`})
		q.result("k1", "bash", "deployment \"appview\" successfully rolled out", false)
		q.say("It rolled out.")
		return q.b.String()
	})()}); err != nil {
		t.Fatal(err)
	}
	sync()

	// The error and its fix, found by the message, with the session's facets.
	out, err := bo.SearchSessions(ctx, agent.SearchSessionsIn{Query: "dropped 3 memories after SetConfig", Kind: "error"})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Passages) != 1 {
		t.Fatalf("error search: %+v", out)
	}
	hit := out.Passages[0]
	if hit.Author != alice.DID || hit.SessionID != "s1" || hit.Entry != fail || !strings.Contains(hit.Text, "Then: SetConfig marks the resync") ||
		hit.Repo != "github.com/haileyok/engram-garden" || hit.Branch != "hailey/fix" || hit.Agent != "session" || !strings.Contains(out.Note, "quoted") {
		t.Fatalf("hit: %+v", hit)
	}
	if hit.Session != n.Space+"/"+alice.DID+"/garden.engram.session/s1" {
		t.Fatalf("session uri %q", hit.Session)
	}
	// Filters by command, file, agent kind and repo.
	for name, in := range map[string]agent.SearchSessionsIn{
		"command":  {Query: "indexer tests", Command: "go test"},
		"file":     {Query: "positions wait", File: "internal/indexer/indexer.go"},
		"repo":     {Query: "drop memories", Repo: "https://github.com/haileyok/engram-garden"},
		"no query": {Session: "s1", Kind: "turn"},
	} {
		got, err := bo.SearchSessions(ctx, in)
		if err != nil || len(got.Passages) == 0 || got.Passages[0].SessionID != "s1" {
			t.Fatalf("%s: %+v %v", name, got, err)
		}
	}
	sub, err := al.SearchSessions(ctx, agent.SearchSessionsIn{Query: "rollout", Agent: "subagent", Parent: alice.DID})
	if err != nil || len(sub.Passages) == 0 || sub.Passages[0].Author != bob.DID {
		t.Fatalf("subagent: %+v %v", sub, err)
	}
	// What's never indexed, and what's redacted.
	for _, q := range []string{"recalled memory text", "private reasoning", "ghp_aB3dE5fG7hI9jK1lM3nO5pQ7rS9tU1vW3xY5"} {
		got, err := bo.SearchSessions(ctx, agent.SearchSessionsIn{Query: q, Mode: "keyword", Limit: 50})
		if err != nil && !strings.Contains(err.Error(), "keyword") {
			t.Fatal(err)
		}
		for _, h := range got.Passages {
			if strings.Contains(h.Text, q) {
				t.Fatalf("%q is in %+v", q, h)
			}
		}
	}
	all, _ := bo.SearchSessions(ctx, agent.SearchSessionsIn{Session: "s1", Limit: 50})
	for _, h := range all.Passages {
		if strings.Contains(h.Text, "ghp_aB3dE5") || strings.Contains(h.Text, "private reasoning") || strings.Contains(h.Text, "recalled memory") {
			t.Fatalf("leaked into %s: %q", h.Kind, h.Text)
		}
	}

	// Reading around the hit, as another agent.
	rd, err := bo.ReadSession(ctx, agent.ReadSessionIn{Session: hit.Session, Entry: hit.Entry, Offset: hit.Offset, Before: 2, After: 1})
	if err != nil {
		t.Fatal(err)
	}
	var target *agent.SessionEntry
	for i := range rd.Entries {
		if rd.Entries[i].Target {
			target = &rd.Entries[i]
		}
	}
	if target == nil || target.ID != fail || target.Role != "error" || len(rd.Entries) != 4 || !rd.MoreBefore || !rd.MoreAfter {
		t.Fatalf("read: %+v", rd)
	}
	if rd.Entries[0].Role != "user" || rd.Entries[1].Role != "assistant" || !strings.Contains(rd.Entries[1].Text, "→ $ cd /root/eg && go test") {
		t.Fatalf("read before: %+v", rd.Entries[:2])
	}

	// An append reads only the chunk that grew.
	before := n.Calls("com.atproto.space.getBlob")
	var more piLines
	more.n = p.n
	more.user("now ship the fix with the rollback plan")
	more.say("Shipped, with the rollback pin noted.")
	if _, err := al.PushSession(ctx, agent.PushSessionIn{Session: meta, FromBytes: int64(len(first)), Data: more.b.String()}); err != nil {
		t.Fatal(err)
	}
	afterPush := n.Calls("com.atproto.space.getBlob")
	sync()
	if got := n.Calls("com.atproto.space.getBlob") - afterPush; got != 1 {
		t.Fatalf("an append read %d chunks, want 1 (push read %d)", got, afterPush-before)
	}
	got, err := bo.SearchSessions(ctx, agent.SearchSessionsIn{Query: "rollback plan", Kind: "turn"})
	if err != nil || len(got.Passages) == 0 || !strings.Contains(got.Passages[0].Text, "Shipped, with the rollback pin") {
		t.Fatalf("after append: %+v %v", got, err)
	}

	// A resync rewrites the transcript: what's gone from it leaves the index.
	var redo piLines
	redo.user("a different start entirely")
	redo.say("Different.")
	if _, err := al.PushSession(ctx, agent.PushSessionIn{Session: meta, Replace: true, Data: redo.b.String()}); err != nil {
		t.Fatal(err)
	}
	sync()
	all, _ = bo.SearchSessions(ctx, agent.SearchSessionsIn{Session: "s1", Limit: 50})
	for _, h := range all.Passages {
		if strings.Contains(h.Text, "SetConfig") {
			t.Fatalf("a rewritten transcript's passage stayed: %+v", h)
		}
	}
	if len(all.Passages) == 0 {
		t.Fatal("the rewritten transcript wasn't indexed")
	}

	// A deleted session leaves nothing.
	if _, err := al.DeleteSession(ctx, agent.DeleteSessionIn{Session: "s1"}); err != nil {
		t.Fatal(err)
	}
	sync()
	all, _ = bo.SearchSessions(ctx, agent.SearchSessionsIn{Session: "s1", Limit: 50})
	if len(all.Passages) != 0 {
		t.Fatalf("a deleted session's passages stayed: %+v", all.Passages)
	}
	if other, _ := bo.SearchSessions(ctx, agent.SearchSessionsIn{Session: "s2"}); len(other.Passages) == 0 {
		t.Fatal("another session's passages went too")
	}
}

// TestArchiveOverLimitHoldsProgress: passages the store won't take over its
// limits are pulled again later, so the cutter's progress isn't published
// past them.
func TestArchiveOverLimitHoldsProgress(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newArchiveFixture(t, spacestore.Limits{MaxMemories: 2})
	var p piLines
	for i := range 6 {
		p.user(fmt.Sprintf("question %d about widgets", i))
		p.say(fmt.Sprintf("answer %d about widgets", i))
	}
	if _, err := f.al.PushSession(ctx, agent.PushSessionIn{Session: lex.Session{SessionID: "s1", Title: "widgets"}, Data: p.b.String()}); err != nil {
		t.Fatal(err)
	}
	_ = f.srv.Indexer.SyncSpace(ctx, f.n.Space)
	pos, err := f.st.RepoState(ctx, f.n.Space, f.alice.DID)
	if err != nil {
		t.Fatal(err)
	}
	if pos != nil && sessionindex.Current(pos.State, &lex.Config{ModelInfo: model}) && len(pos.State) > 0 {
		t.Fatalf("progress was published past held passages: %s", pos.State)
	}
}
