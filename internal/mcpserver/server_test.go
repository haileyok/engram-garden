package mcpserver

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/haileyok/engram-garden/internal/agent"
	"github.com/haileyok/engram-garden/internal/appview"
	"github.com/haileyok/engram-garden/internal/blob"
	"github.com/haileyok/engram-garden/internal/embed"
	"github.com/haileyok/engram-garden/internal/indexer"
	"github.com/haileyok/engram-garden/internal/lex"
	"github.com/haileyok/engram-garden/internal/spaceclient"
	"github.com/haileyok/engram-garden/internal/spacestore"
	"github.com/haileyok/engram-garden/internal/spacetest"
)

const serviceDID = "did:web:engram.test"

var (
	model     = lex.ModelInfo{Model: "hashing-256", ModelDigest: embed.HashingDigest, Dims: 256}
	nextModel = lex.ModelInfo{Model: "hashing-128", ModelDigest: embed.HashingDigest, Dims: 128}
)

type world struct {
	net *spacetest.Net
	av  *appview.Server
	url string
}

func newWorld(t *testing.T, declare bool) *world {
	t.Helper()
	return newSpaceWorld(t, declare, "authority", "memory")
}

// newSpaceWorld is newWorld for a space with the given authority and key,
// each world its own network and appview.
func newSpaceWorld(t *testing.T, declare bool, authority, skey string) *world {
	t.Helper()
	n := spacetest.NewSpace(t, authority, skey)
	avAcct := n.NewAccount("did:plc:appview")
	alice := n.NewAccount("did:plc:alice")
	bob := n.NewAccount("did:plc:bob")
	for _, a := range []*spacetest.Account{n.Authority, avAcct, alice, bob} {
		n.AddMember(a.DID)
	}
	avClient, err := spaceclient.New(n.Session(avAcct), n.Dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	st, err := spacestore.New(spacestore.Options{Blob: blob.Dir{Root: t.TempDir()}, CacheDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close(context.Background()) })
	av := &appview.Server{
		Store: st, Dir: n.Dir, ServiceDID: serviceDID, Spaces: []string{n.Space},
		Indexer: &indexer.Indexer{Store: st, Client: avClient, Dir: n.Dir},
	}
	hs := httptest.NewServer(av.Handler())
	t.Cleanup(hs.Close)
	n.RegisterService(serviceDID, appview.SyncerFragment, hs.URL)
	if _, err := av.Indexer.Register(context.Background(), n.Space, av.ServiceID()); err != nil {
		t.Fatal(err)
	}
	w := &world{net: n, av: av, url: hs.URL}
	if declare {
		w.declare(t, lex.Config{ModelInfo: model, DocumentPrefix: "search_document: ", QueryPrefix: "search_query: "})
	}
	return w
}

// declare writes the authority's config and delivers its notification.
func (w *world) declare(t *testing.T, c lex.Config) {
	t.Helper()
	w.net.Put(w.net.Authority, lex.ConfigCollection, lex.ConfigRkey, c.Record(time.Now()))
	w.deliver(t, w.net.Authority.DID)
}

func (w *world) tools(t *testing.T, did string, p embed.Provider) *agent.Agent {
	t.Helper()
	c, err := spaceclient.New(w.net.Session(w.net.AccountByDID(did)), w.net.Dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	return &agent.Agent{Client: c, Space: w.net.Space, AppviewURL: w.url, AppviewDID: serviceDID, Provider: p, ConfigTTL: time.Millisecond}
}

// connect starts the MCP server over agents for one or more spaces (the
// first is the default) and connects a client.
func connect(t *testing.T, agents ...*agent.Agent) *mcp.ClientSession {
	t.Helper()
	ct, st := mcp.NewInMemoryTransports()
	ctx := context.Background()
	if _, err := NewServer(agent.SpacesOf(agents...)).Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

// call invokes a tool and decodes its structured result into out, returning
// the error text when the tool reports one.
func call(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any, out any) string {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if res.IsError {
		var msgs []string
		for _, c := range res.Content {
			if tc, ok := c.(*mcp.TextContent); ok {
				msgs = append(msgs, tc.Text)
			}
		}
		return strings.Join(msgs, " ")
	}
	if out != nil {
		raw, _ := json.Marshal(res.StructuredContent)
		if err := json.Unmarshal(raw, out); err != nil {
			t.Fatal(err)
		}
	}
	return ""
}

// deliver forwards the latest write as the authority would, then waits for
// the appview to index it.
func (w *world) deliver(t *testing.T, did string) {
	t.Helper()
	w.net.DeliverWrite(w.net.AccountByDID(did), "")
	w.av.Jobs.Wait()
}

// TestSeveralSpaces: one agent in two spaces stores where it's told, recalls
// from both (each result naming its space) or one, and learns each space's
// model from list_spaces.
func TestSeveralSpaces(t *testing.T) {
	t.Parallel()
	team := newSpaceWorld(t, true, "teamlead", "team")
	mine := newSpaceWorld(t, true, "noteskeeper", "notes")
	alice := connect(t, team.tools(t, "did:plc:alice", embed.HashingProvider{}), mine.tools(t, "did:plc:alice", embed.HashingProvider{}))

	var spaces ListSpacesOut
	if msg := call(t, alice, "list_spaces", nil, &spaces); msg != "" {
		t.Fatal(msg)
	}
	byName := map[string]agent.SpaceInfo{}
	for _, s := range spaces.Spaces {
		byName[s.Name] = s
	}
	if len(byName) != 2 || !byName["team"].Default || byName["notes"].Default || !byName["notes"].SetUp ||
		byName["team"].Model == nil || *byName["team"].Model != model || byName["notes"].LocalModel != "ready" ||
		byName["team"].QueryPrefix != "search_query: " {
		t.Fatalf("list_spaces: %+v", spaces)
	}

	var stored RememberOut
	if msg := call(t, alice, "remember", map[string]any{"text": "the deploy key rotates every friday", "space": "team"}, &stored); msg != "" {
		t.Fatal(msg)
	}
	team.deliver(t, "did:plc:alice")
	var note RememberOut
	if msg := call(t, alice, "remember", map[string]any{"text": "my friday deploy checklist lives in notes.md", "space": "notes"}, &note); msg != "" {
		t.Fatal(msg)
	}
	mine.deliver(t, "did:plc:alice")
	if !strings.HasPrefix(note.URI, mine.net.Space+"/") || !strings.HasPrefix(stored.URI, team.net.Space+"/") {
		t.Fatalf("stored in the wrong spaces: %s %s", stored.URI, note.URI)
	}

	var found MemoriesOut
	if msg := call(t, alice, "recall", map[string]any{"query": "friday deploy"}, &found); msg != "" {
		t.Fatal(msg)
	}
	got := map[string]string{}
	for _, m := range found.Memories {
		got[m.Space] = m.Text
	}
	if len(found.Memories) != 2 || got["team"] == "" || got["notes"] == "" {
		t.Fatalf("recall across spaces: %+v", found)
	}
	if msg := call(t, alice, "recall", map[string]any{"query": "friday deploy", "space": "notes"}, &found); msg != "" {
		t.Fatal(msg)
	}
	if len(found.Memories) != 1 || found.Memories[0].Space != "notes" {
		t.Fatalf("recall in one space: %+v", found)
	}

	// list_memories is the default space unless told otherwise.
	if msg := call(t, alice, "list_memories", nil, &found); msg != "" {
		t.Fatal(msg)
	}
	if len(found.Memories) != 1 || found.Memories[0].Space != "team" {
		t.Fatalf("list default space: %+v", found)
	}
	// get_memory and forget find the space from the URI.
	var one GetOut
	if msg := call(t, alice, "get_memory", map[string]any{"uri": note.URI}, &one); msg != "" || one.Memory.Space != "notes" {
		t.Fatalf("get_memory: %q %+v", msg, one)
	}
	if msg := call(t, alice, "forget", map[string]any{"uri": note.URI}, nil); msg != "" {
		t.Fatal(msg)
	}
	if msg := call(t, alice, "remember", map[string]any{"text": "x", "space": "nowhere"}, nil); !strings.Contains(msg, "nowhere") {
		t.Fatalf("unknown space: %q", msg)
	}
}

// TestRunningASpace: the space's authority creates a space, manages members
// and declares a model over MCP; other accounts are told they can't.
func TestRunningASpace(t *testing.T) {
	t.Parallel()
	w := newWorld(t, false)
	sp := agent.SpacesOf(w.tools(t, w.net.Authority.DID, embed.HashingProvider{}))
	sp.Settings.Embed.Provider = "hashing"
	var saved []string
	sp.Save = func(s agent.Settings) error {
		saved = saved[:0]
		for _, e := range s.Spaces {
			saved = append(saved, e.Name)
		}
		return nil
	}
	ct, st := mcp.NewInMemoryTransports()
	ctx := context.Background()
	if _, err := NewServer(sp).Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	owner, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = owner.Close() })

	var created agent.CreateSpaceOut
	if msg := call(t, owner, "create_space", map[string]any{"name": "runbooks"}, &created); msg != "" {
		t.Fatal(msg)
	}
	if created.Space.Name != "runbooks" || !strings.HasSuffix(created.Space.URI, "/garden.engram.space/runbooks") || len(saved) != 2 {
		t.Fatalf("create_space: %+v, saved %v", created, saved)
	}
	var added agent.MemberOut
	if msg := call(t, owner, "add_member", map[string]any{"space": "runbooks", "member": "did:plc:alice", "readOnly": true}, &added); msg != "" {
		t.Fatal(msg)
	}
	var members agent.MembersOut
	if msg := call(t, owner, "list_members", map[string]any{"space": "runbooks"}, &members); msg != "" {
		t.Fatal(msg)
	}
	if len(members.Members) != 1 || members.Members[0].DID != "did:plc:alice" {
		t.Fatalf("list_members: %+v", members)
	}
	if msg := call(t, owner, "remove_member", map[string]any{"space": "runbooks", "member": "did:plc:alice"}, nil); msg != "" {
		t.Fatal(msg)
	}

	// The model of the original space (the test network keeps records only
	// there).
	var m agent.ModelOut
	if msg := call(t, owner, "set_model", map[string]any{"space": "memory", "action": "declare", "model": "hashing-64", "dims": 64}, &m); msg != "" {
		t.Fatal(msg)
	}
	if m.Model == nil || m.Model.Dims != 64 || m.Model.Model != "hashing-64" {
		t.Fatalf("set_model: %+v", m)
	}
	var spaces ListSpacesOut
	if msg := call(t, owner, "list_spaces", nil, &spaces); msg != "" {
		t.Fatal(msg)
	}
	for _, s := range spaces.Spaces {
		if s.Name == "memory" && (s.Model == nil || s.Model.Dims != 64) {
			t.Fatalf("list_spaces after set_model: %+v", s)
		}
	}
	if msg := call(t, owner, "set_model", map[string]any{"space": "memory", "action": "bogus"}, nil); !strings.Contains(msg, "action must be") {
		t.Fatalf("bad action: %q", msg)
	}
	// This appview takes no grants, and says so.
	if msg := call(t, owner, "index_space", map[string]any{"space": "memory"}, nil); !strings.Contains(msg, "grants") {
		t.Fatalf("index_space without grants: %q", msg)
	}

	// A member who isn't the authority can't run the space.
	alice := connect(t, w.tools(t, "did:plc:alice", embed.HashingProvider{}))
	if msg := call(t, alice, "add_member", map[string]any{"member": "did:plc:bob"}, nil); !strings.Contains(msg, "authority") {
		t.Fatalf("add_member as a member: %q", msg)
	}
}

func TestAgentsShareMemories(t *testing.T) {
	t.Parallel()
	w := newWorld(t, true)
	alice := connect(t, w.tools(t, "did:plc:alice", embed.HashingProvider{}))
	bob := connect(t, w.tools(t, "did:plc:bob", embed.HashingProvider{}))

	tools, err := alice.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, tl := range tools.Tools {
		names[tl.Name] = true
	}
	for _, want := range []string{"remember", "recall", "get_memory", "list_memories", "forget"} {
		if !names[want] {
			t.Errorf("missing tool %s", want)
		}
	}

	var rem RememberOut
	if msg := call(t, alice, "remember", map[string]any{
		"text":   "Attie pop1 deploys go through the deploy repo's Deploy Attie workflow with one SHA for every service",
		"tags":   []string{"attie", "deploy"},
		"source": "skill:attie-deploy",
	}, &rem); msg != "" {
		t.Fatal(msg)
	}
	if !strings.Contains(rem.URI, "/did:plc:alice/garden.engram.memory/") {
		t.Fatalf("remember uri: %q", rem.URI)
	}
	call(t, alice, "remember", map[string]any{"text": "Hailey prefers compact answers"}, nil)
	w.deliver(t, "did:plc:alice")

	// The records carried their vectors, so both were indexed.
	st, _ := w.av.Store.Status(context.Background(), w.net.Space)
	if st.Memories != 2 || len(st.Skipped) != 0 {
		t.Fatalf("appview status: %+v", st)
	}

	// Bob recalls what Alice remembered.
	var got MemoriesOut
	if msg := call(t, bob, "recall", map[string]any{"query": "how do attie deploys work", "limit": 1}, &got); msg != "" {
		t.Fatal(msg)
	}
	if len(got.Memories) != 1 || got.Memories[0].URI != rem.URI || got.Memories[0].Author != "did:plc:alice" ||
		got.Memories[0].Source != "skill:attie-deploy" || got.Memories[0].Similarity == nil {
		t.Fatalf("recall: %+v", got)
	}

	var one GetOut
	if msg := call(t, bob, "get_memory", map[string]any{"uri": rem.URI}, &one); msg != "" || len(one.Memory.Tags) != 2 {
		t.Fatalf("get_memory: %q %+v", msg, one)
	}
	if msg := call(t, bob, "list_memories", map[string]any{"tags": []string{"deploy"}}, &got); msg != "" || len(got.Memories) != 1 {
		t.Fatalf("list_memories: %q %+v", msg, got)
	}

	// Bob can't forget Alice's memory; Alice can.
	if msg := call(t, bob, "forget", map[string]any{"uri": rem.URI}, nil); !strings.Contains(msg, "only forget your own") {
		t.Fatalf("bob forgot alice's memory: %q", msg)
	}
	if msg := call(t, alice, "forget", map[string]any{"uri": rem.URI}, nil); msg != "" {
		t.Fatal(msg)
	}
	w.deliver(t, "did:plc:alice")
	if msg := call(t, bob, "get_memory", map[string]any{"uri": rem.URI}, nil); !strings.Contains(msg, "MemoryNotFound") {
		t.Fatalf("forgotten memory still there: %q", msg)
	}

	// Input validation.
	if msg := call(t, alice, "remember", map[string]any{"text": "  "}, nil); msg == "" {
		t.Fatal("blank memory accepted")
	}
	if msg := call(t, alice, "recall", map[string]any{"query": ""}, nil); msg == "" {
		t.Fatal("blank query accepted")
	}
	if msg := call(t, alice, "forget", map[string]any{"uri": "at://elsewhere/x"}, nil); !strings.Contains(msg, "not a record URI") {
		t.Fatalf("bad uri: %q", msg)
	}
	// Warming the space is accepted.
	if err := w.tools(t, "did:plc:alice", embed.HashingProvider{}).Warm(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// otherModel is a provider whose local model never matches.
type otherModel struct{}

func (otherModel) For(_ context.Context, m lex.ModelInfo) (embed.Embedder, error) {
	return nil, &embed.ModelMismatchError{Want: m, LocalName: m.Model, Local: "sha256:different"}
}

func TestRefusesAWrongModel(t *testing.T) {
	t.Parallel()
	w := newWorld(t, true)
	alice := connect(t, w.tools(t, "did:plc:alice", otherModel{}))
	msg := call(t, alice, "remember", map[string]any{"text": "anything"}, nil)
	if !strings.Contains(msg, "not stored") || !strings.Contains(msg, "sha256:different") {
		t.Fatalf("remember with the wrong model: %q", msg)
	}
	if n := w.net.Calls("com.atproto.space.createRecord"); n != 0 {
		t.Fatalf("a record was written anyway (%d)", n)
	}
	if msg := call(t, alice, "recall", map[string]any{"query": "anything"}, nil); msg == "" {
		t.Fatal("recall with the wrong model succeeded")
	}
}

func TestNoDeclaredModel(t *testing.T) {
	t.Parallel()
	w := newWorld(t, false)
	alice := connect(t, w.tools(t, "did:plc:alice", embed.HashingProvider{}))
	if msg := call(t, alice, "remember", map[string]any{"text": "anything"}, nil); !strings.Contains(msg, "engram model --set") {
		t.Fatalf("remember without a config: %q", msg)
	}
}

func TestModelChangeReembeds(t *testing.T) {
	t.Parallel()
	w := newWorld(t, true)
	at := w.tools(t, "did:plc:alice", embed.HashingProvider{})
	alice := connect(t, at)
	call(t, alice, "remember", map[string]any{"text": "the kettle is in the left cupboard"}, nil)
	call(t, alice, "remember", map[string]any{"text": "tea bags are next to the kettle"}, nil)
	w.deliver(t, "did:plc:alice")

	// The authority announces a new model; alice's agent re-embeds.
	w.declare(t, lex.Config{ModelInfo: model, Next: &nextModel})
	n, err := at.Reembed(context.Background())
	if err != nil || n != 2 {
		t.Fatalf("reembed: %d %v", n, err)
	}
	if n, _ := at.Reembed(context.Background()); n != 0 {
		t.Fatalf("second reembed rewrote %d", n)
	}
	w.deliver(t, "did:plc:alice")
	st, _ := w.av.Store.Status(context.Background(), w.net.Space)
	if st.Building == nil || st.BuildingMemories != 2 {
		t.Fatalf("building index: %+v", st)
	}
	// New memories carry both vectors right away.
	call(t, alice, "remember", map[string]any{"text": "milk is in the fridge door"}, nil)
	w.deliver(t, "did:plc:alice")
	if st, _ = w.av.Store.Status(context.Background(), w.net.Space); st.BuildingMemories != 3 {
		t.Fatalf("new memory missing its next vector: %+v", st)
	}

	// Promote: recall switches to the new model after re-reading the config.
	w.declare(t, lex.Config{ModelInfo: nextModel})
	var got MemoriesOut
	if msg := call(t, alice, "recall", map[string]any{"query": "where is the kettle", "limit": 1}, &got); msg != "" {
		t.Fatal(msg)
	}
	if len(got.Memories) != 1 || !strings.Contains(got.Memories[0].Text, "kettle") {
		t.Fatalf("recall after promotion: %+v", got)
	}
}
