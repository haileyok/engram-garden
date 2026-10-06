package mcpserver

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/haileyok/engram-garden/internal/appview"
	"github.com/haileyok/engram-garden/internal/embed"
	"github.com/haileyok/engram-garden/internal/indexer"
	"github.com/haileyok/engram-garden/internal/spaceclient"
	"github.com/haileyok/engram-garden/internal/spacetest"
	"github.com/haileyok/engram-garden/internal/store/storetest"
)

const (
	dims       = 256
	serviceDID = "did:web:engram.test"
)

type world struct {
	net *spacetest.Net
	av  *appview.Server
}

func newWorld(t *testing.T) (*world, *mcp.ClientSession, *mcp.ClientSession) {
	t.Helper()
	st := storetest.New(t, dims)
	n := spacetest.New(t)
	avAcct := n.NewAccount("did:plc:appview")
	alice := n.NewAccount("did:plc:alice")
	bob := n.NewAccount("did:plc:bob")
	for _, a := range []*spacetest.Account{avAcct, alice, bob} {
		n.AddMember(a.DID)
	}
	avClient, err := spaceclient.New(n.Session(avAcct), n.Dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	emb := embed.Hashing{Dims: dims}
	av := &appview.Server{
		Store: st, Embedder: emb, Dir: n.Dir, ServiceDID: serviceDID, Spaces: []string{n.Space},
		Indexer: &indexer.Indexer{Store: st, Embedder: emb, Client: avClient, Dir: n.Dir},
	}
	hs := httptest.NewServer(av.Handler())
	t.Cleanup(hs.Close)
	n.RegisterService(serviceDID, appview.SyncerFragment, hs.URL)
	if _, err := av.Indexer.Register(context.Background(), n.Space, av.ServiceID()); err != nil {
		t.Fatal(err)
	}
	w := &world{net: n, av: av}
	return w, w.connect(t, alice, hs.URL), w.connect(t, bob, hs.URL)
}

func (w *world) connect(t *testing.T, a *spacetest.Account, appviewURL string) *mcp.ClientSession {
	t.Helper()
	c, err := spaceclient.New(w.net.Session(a), w.net.Dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	tools := &Tools{Client: c, Space: w.net.Space, AppviewURL: appviewURL, AppviewDID: serviceDID}
	ct, st := mcp.NewInMemoryTransports()
	ctx := context.Background()
	if _, err := tools.NewServer().Connect(ctx, st, nil); err != nil {
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

func TestAgentsShareMemories(t *testing.T) {
	t.Parallel()
	w, alice, bob := newWorld(t)

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
}
