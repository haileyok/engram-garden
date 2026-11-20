package mcpserver

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/bluesky-social/indigo/atproto/atcrypto"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/haileyok/engram-garden/internal/agent"
	"github.com/haileyok/engram-garden/internal/appview"
	"github.com/haileyok/engram-garden/internal/blob"
	"github.com/haileyok/engram-garden/internal/indexer"
	"github.com/haileyok/engram-garden/internal/lex"
	"github.com/haileyok/engram-garden/internal/lounge"
	"github.com/haileyok/engram-garden/internal/spaceclient"
	"github.com/haileyok/engram-garden/internal/spacestore"
	"github.com/haileyok/engram-garden/internal/spacetest"
)

type loungeWorld struct {
	net *spacetest.Net
	av  *appview.Server
	url string
	pv  *agent.Provisioner
}

// newLoungeWorld is a principal's lounge on a PDS that creates accounts, an
// appview that serves it, and a provisioner that hands out identities.
func newLoungeWorld(t *testing.T) *loungeWorld {
	t.Helper()
	n := spacetest.NewTyped(t, "principal", lex.LoungeType, "agents")
	n.AutoDeliver = true
	n.InviteCode = "agents-invite"
	avAcct := n.NewAccount("did:plc:appview")
	n.AddMember(avAcct.DID)
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
		Lounge:  &lounge.Service{Reader: &indexer.Reader{Client: avClient, Dir: n.Dir}, Dir: n.Dir},
	}
	hs := httptest.NewServer(av.Handler())
	t.Cleanup(hs.Close)
	av.PublicURL = hs.URL
	if _, err := av.Indexer.Register(context.Background(), n.Space, av.Notify(n.Space)); err != nil {
		t.Fatal(err)
	}
	if err := av.Lounge.SyncSpace(context.Background(), n.Space); err != nil {
		t.Fatal(err)
	}
	rk, _ := atcrypto.GeneratePrivateKeyK256()
	rpub, _ := rk.PublicKey()
	pv := &agent.Provisioner{
		Principal: agent.Principal{
			Account: agent.Account{DID: n.Authority.DID, Handle: "principal.test"},
			PDS:     n.Server.URL, HandleDomain: ".pds.test", InviteCode: "agents-invite",
			RecoveryKey: rpub.DIDKey(), Lounge: n.Space, Stock: 1,
		},
		Session: n.Session(n.Authority),
		Dir:     t.TempDir(),
	}
	return &loungeWorld{net: n, av: av, url: hs.URL, pv: pv}
}

// join claims an identity and opens its place in the lounge, running it
// until the test ends.
func (w *loungeWorld) join(t *testing.T, in agent.ClaimIn) (*agent.Lounge, agent.Identity) {
	t.Helper()
	ctx := context.Background()
	id, path, err := w.pv.Claim(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	s := agent.Settings{Identity: path, AppviewURL: w.url, AppviewDID: serviceDID}
	l, got, err := agent.OpenLounge(ctx, s, agent.Options{Dir: w.net.Dir})
	if err != nil {
		t.Fatal(err)
	}
	if got.DID != id.DID || l.DID() != id.DID {
		t.Fatalf("opened %s as %s", id.DID, l.DID())
	}
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { defer close(done); l.Run(runCtx) }()
	t.Cleanup(func() { cancel(); <-done })
	return l, id
}

// waitFor polls a lounge's events until one matches.
func waitFor(t *testing.T, l *agent.Lounge, after int64, match func(agent.LoungeEvent) bool) (agent.LoungeEvent, int64) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		out := l.Events(context.Background(), agent.EventsIn{After: after, Wait: 1})
		for _, e := range out.Events {
			if match(e) {
				return e, e.Seq
			}
		}
		after = out.Next
	}
	t.Fatal("no matching event")
	return agent.LoungeEvent{}, 0
}

// TestAgentsMeetInTheLounge: two agent sessions get identities of their own
// (declared agent-of the principal in their first PLC operation, endorsed by
// the principal, added to the lounge), see each other come online, and
// message each other, with the appview pushing each write as it lands.
func TestAgentsMeetInTheLounge(t *testing.T) {
	t.Parallel()
	w := newLoungeWorld(t)
	ctx := context.Background()

	// The stock is filled ahead of time; the first claim takes from it.
	if made, err := w.pv.Fill(ctx, 1); err != nil || made != 1 || w.pv.StockCount() != 1 {
		t.Fatalf("fill: %d %v", made, err)
	}
	alice, aliceID := w.join(t, agent.ClaimIn{Kind: "session", Session: "s-alice"})
	if w.pv.StockCount() != 0 {
		t.Fatalf("the claim didn't take the stocked identity")
	}
	bob, bobID := w.join(t, agent.ClaimIn{Kind: "subagent", Session: "s-bob", Parent: aliceID.DID})

	// Each account was created with the agent-of declaration and the
	// recovery key, and endorsed: the relationship is confirmed.
	if len(w.net.Created) != 2 {
		t.Fatalf("accounts created: %+v", w.net.Created)
	}
	for _, c := range w.net.Created {
		if len(c.AlsoKnownAs) != 1 || c.AlsoKnownAs[0] != lex.IdentLinkAgentOf+w.net.Authority.DID || c.RecoveryKey != w.pv.Principal.RecoveryKey {
			t.Fatalf("created: %+v", c)
		}
		got, err := agent.Principals(ctx, w.net.Dir, nil, c.DID)
		if err != nil || len(got) != 1 || !got[w.net.Authority.DID] {
			t.Fatalf("principals of %s: %v %v", c.DID, got, err)
		}
	}
	members := w.net.Members(w.net.Space)
	for _, d := range []string{aliceID.DID, bobID.DID} {
		found := false
		for _, m := range members {
			found = found || m == d
		}
		if !found {
			t.Fatalf("%s isn't in the lounge: %v", d, members)
		}
	}
	// An identity that isn't endorsed is unconfirmed.
	stranger := w.net.NewAccount("did:plc:stranger")
	stranger.AlsoKnownAs = []string{lex.IdentLinkAgentOf + w.net.Authority.DID}
	if got, _ := agent.Principals(ctx, w.net.Dir, nil, stranger.DID); got[w.net.Authority.DID] {
		t.Fatalf("an unendorsed declaration was confirmed")
	}

	// Both come online; alice sees bob, a subagent she started.
	if _, err := alice.SetPresence(ctx, agent.PresenceIn{State: lex.StateWorking, Title: "deploying attie", Kind: "session", Harness: "pi"}); err != nil {
		t.Fatal(err)
	}
	if _, err := bob.SetPresence(ctx, agent.PresenceIn{State: lex.StateWorking, Title: "reading logs", Kind: "subagent", Parent: aliceID.DID}); err != nil {
		t.Fatal(err)
	}
	_, seenA := waitFor(t, alice, 0, func(e agent.LoungeEvent) bool {
		return e.Presence != nil && e.Presence.DID == bobID.DID && e.Presence.Online && e.Presence.Parent == aliceID.DID
	})

	// Through the MCP tools: alice lists the agents and messages bob.
	ct, stt := mcp.NewInMemoryTransports()
	srv := NewServer(agent.SpacesOf(&agent.Agent{Space: "at://did:plc:x/space/garden.engram.space/m"}), WithLounge(alice))
	if _, err := srv.Connect(ctx, stt, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	var agents agent.AgentsOut
	if msg := call(t, cs, "list_agents", nil, &agents); msg != "" {
		t.Fatal(msg)
	}
	if agents.Self != aliceID.DID || len(agents.Agents) != 2 {
		t.Fatalf("list_agents: %+v", agents)
	}
	var sent agent.SendOut
	if msg := call(t, cs, "send_message", map[string]any{"to": []string{bobID.DID}, "kind": "request", "text": "what do the logs say?"}, &sent); msg != "" {
		t.Fatal(msg)
	}

	// Bob gets it pushed, and replies; the reply goes back to alice.
	req, _ := waitFor(t, bob, 0, func(e agent.LoungeEvent) bool { return e.Message != nil && e.Message.URI == sent.URI })
	if req.Message.Author != aliceID.DID || req.Message.Kind != lex.KindRequest || req.Message.SamePrincipal == nil || !*req.Message.SamePrincipal {
		t.Fatalf("bob got: %+v", req.Message)
	}
	reply, err := bob.Send(ctx, agent.SendIn{Text: "all clear", ReplyTo: sent.URI})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := waitFor(t, alice, seenA, func(e agent.LoungeEvent) bool { return e.Message != nil && e.Message.URI == reply.URI })
	if got.Message.Kind != lex.KindReply || got.Message.Reply == nil || got.Message.Reply.Root != sent.URI ||
		len(got.Message.To) != 1 || got.Message.To[0] != aliceID.DID {
		t.Fatalf("alice got: %+v", got.Message)
	}

	// A message for someone else doesn't reach alice; one to everyone does.
	if _, err := bob.Send(ctx, agent.SendIn{To: []string{"did:plc:carol"}, Text: "psst"}); err != nil {
		t.Fatal(err)
	}
	all, err := bob.Send(ctx, agent.SendIn{Text: "heading out"})
	if err != nil {
		t.Fatal(err)
	}
	e, _ := waitFor(t, alice, seenA, func(e agent.LoungeEvent) bool {
		return e.Message != nil && (e.Message.URI == all.URI || e.Message.Text == "psst")
	})
	if e.Message.Text == "psst" {
		t.Fatal("alice got a message for carol")
	}

	// list_messages has the conversation.
	var msgs agent.MessagesOut
	if m := call(t, cs, "list_messages", nil, &msgs); m != "" {
		t.Fatal(m)
	}
	if len(msgs.Messages) != 3 { // her request, bob's reply, the broadcast
		t.Fatalf("list_messages: %+v", msgs)
	}

	// Bob ends: alice sees him go offline.
	if err := bob.End(ctx); err != nil {
		t.Fatal(err)
	}
	waitFor(t, alice, seenA, func(e agent.LoungeEvent) bool {
		return e.Presence != nil && e.Presence.DID == bobID.DID && !e.Presence.Online
	})
}
