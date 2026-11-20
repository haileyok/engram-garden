package appview

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/haileyok/engram-garden/internal/blob"
	"github.com/haileyok/engram-garden/internal/control/controltest"
	"github.com/haileyok/engram-garden/internal/indexer"
	"github.com/haileyok/engram-garden/internal/lex"
	"github.com/haileyok/engram-garden/internal/lounge"
	"github.com/haileyok/engram-garden/internal/spaceclient"
	"github.com/haileyok/engram-garden/internal/spacetest"
)

type loungeFixture struct {
	net   *spacetest.Net
	srv   *Server
	url   string
	alice *spacetest.Account
	bob   *spacetest.Account
	now   time.Time
}

// setupLounge is a lounge with two member agents and an appview that reads
// it through the authority's grant.
func setupLounge(t *testing.T) *loungeFixture {
	t.Helper()
	n := spacetest.NewTyped(t, "authority", lex.LoungeType, "agents")
	alice := n.NewAccount("did:plc:alice")
	bob := n.NewAccount("did:plc:bob")
	n.AddMember(alice.DID)
	n.AddMember(bob.DID)
	auth := newFakeAuth(n)
	auth.sessions["seed"] = n.Authority.DID
	grants := &Grants{DB: controltest.New(t), Auth: auth}
	if err := grants.Put(context.Background(), Grant{Space: n.Space, DID: n.Authority.DID, SessionID: "seed", GrantedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	client, err := spaceclient.NewDelegated(grants, n.Dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	st := newNode(t, blob.Dir{Root: t.TempDir()}, nil)
	f := &loungeFixture{net: n, alice: alice, bob: bob, now: time.Now().UTC()}
	s := &Server{
		Store:      st,
		Indexer:    &indexer.Indexer{Store: st, Client: client, Dir: n.Dir},
		Lounge:     &lounge.Service{Reader: &indexer.Reader{Client: client, Dir: n.Dir}, Dir: n.Dir, Now: func() time.Time { return f.now }},
		Dir:        n.Dir,
		ServiceDID: serviceDID,
		Spaces:     []string{n.Space},
		Grants:     grants,
		CookieKey:  []byte("0123456789abcdef0123456789abcdef"),
	}
	hs := newHTTPServer(t, s.Handler())
	s.PublicURL = hs
	f.srv, f.url = s, hs
	return f
}

func (f *loungeFixture) get(t *testing.T, a *spacetest.Account, nsid string, params url.Values) (int, map[string]any) {
	t.Helper()
	status, raw := do(t, f.net, f.url, http.MethodGet, a, serviceDID, nsid, params, nil)
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	return status, body
}

func presence(state string, lease time.Time, title string) map[string]any {
	now := time.Now().UTC()
	p := lex.Presence{Kind: "session", State: state, Title: title, Harness: "pi", UpdatedAt: now.Format(time.RFC3339Nano), LeaseUntil: lease.UTC().Format(time.RFC3339Nano)}
	return p.Record()
}

func message(text string, to ...string) map[string]any {
	return lex.Message{To: to, Kind: lex.KindRequest, Text: text, CreatedAt: time.Now().UTC().Format(time.RFC3339Nano)}.Record()
}

func list(body map[string]any, key string) []map[string]any {
	raw, _ := body[key].([]any)
	out := make([]map[string]any, len(raw))
	for i, m := range raw {
		out[i], _ = m.(map[string]any)
	}
	return out
}

func TestLoungePresenceAndMessages(t *testing.T) {
	t.Parallel()
	f := setupLounge(t)
	ctx := context.Background()
	f.net.Put(f.alice, lex.PresenceCollection, lex.PresenceRkey, presence(lex.StateWorking, f.now.Add(5*time.Minute), "fixing the deploy"))
	f.net.Put(f.bob, lex.PresenceCollection, lex.PresenceRkey, presence(lex.StateIdle, f.now.Add(-time.Minute), "stale"))
	f.net.Put(f.bob, lex.MessageCollection, "3m1", message("can you look at pop1?", f.alice.DID))
	f.net.Put(f.bob, lex.MessageCollection, "3m2", message("hello everyone"))
	if err := f.srv.syncLounge(ctx); err != nil {
		t.Fatal(err)
	}

	status, body := f.get(t, f.bob, "garden.engram.listPresence", url.Values{"space": {f.net.Space}})
	agents := list(body, "agents")
	if status != 200 || len(agents) != 1 || agents[0]["did"] != f.alice.DID || agents[0]["online"] != true || agents[0]["title"] != "fixing the deploy" {
		t.Fatalf("online agents: %d %v", status, body)
	}
	_, body = f.get(t, f.bob, "garden.engram.listPresence", url.Values{"space": {f.net.Space}, "includeOffline": {"true"}})
	if agents := list(body, "agents"); len(agents) != 2 || agents[1]["online"] != false {
		t.Fatalf("all agents: %v", body)
	}

	_, body = f.get(t, f.alice, "garden.engram.listMessages", url.Values{"space": {f.net.Space}, "to": {f.alice.DID}})
	if msgs := list(body, "messages"); len(msgs) != 2 || msgs[0]["text"] != "can you look at pop1?" || msgs[0]["author"] != f.bob.DID {
		t.Fatalf("alice's messages: %v", body)
	}
	_, body = f.get(t, f.alice, "garden.engram.listMessages", url.Values{"space": {f.net.Space}, "to": {"did:plc:carol"}})
	if msgs := list(body, "messages"); len(msgs) != 1 || msgs[0]["text"] != "hello everyone" {
		t.Fatalf("carol sees only the broadcast: %v", body)
	}
	_, body = f.get(t, f.alice, "garden.engram.listMessages", url.Values{"space": {f.net.Space}, "limit": {"1"}})
	if msgs := list(body, "messages"); len(msgs) != 1 || body["cursor"] == nil {
		t.Fatalf("first page: %v", body)
	}
	_, body = f.get(t, f.alice, "garden.engram.listMessages", url.Values{"space": {f.net.Space}, "limit": {"1"}, "cursor": {body["cursor"].(string)}})
	if msgs := list(body, "messages"); len(msgs) != 1 || msgs[0]["text"] != "hello everyone" || body["cursor"] != nil {
		t.Fatalf("second page: %v", body)
	}

	// Without a credential, or with the memory endpoints, nothing.
	if status, _ := f.get(t, nil, "garden.engram.listPresence", url.Values{"space": {f.net.Space}}); status != 401 {
		t.Fatalf("no credential: %d", status)
	}
	if status, _ := f.get(t, f.alice, "garden.engram.listMemories", url.Values{"space": {f.net.Space}}); status != 400 {
		t.Fatalf("memory read of a lounge: %d", status)
	}

	// A deleted message and an ended agent go away.
	f.net.Delete(f.bob, lex.MessageCollection, "3m1")
	f.net.Put(f.alice, lex.PresenceCollection, lex.PresenceRkey, presence(lex.StateEnded, f.now.Add(5*time.Minute), "done"))
	if err := f.srv.syncLounge(ctx); err != nil {
		t.Fatal(err)
	}
	_, body = f.get(t, f.bob, "garden.engram.listPresence", url.Values{"space": {f.net.Space}})
	if agents := list(body, "agents"); len(agents) != 0 {
		t.Fatalf("an ended agent is still online: %v", body)
	}
	_, body = f.get(t, f.alice, "garden.engram.listMessages", url.Values{"space": {f.net.Space}, "to": {f.alice.DID}})
	if msgs := list(body, "messages"); len(msgs) != 1 {
		t.Fatalf("deleted message still listed: %v", body)
	}
}

// syncLounge runs the lounge's space sync, as the background loop does.
func (s *Server) syncLounge(ctx context.Context) error {
	return s.Lounge.SyncSpace(ctx, s.Spaces[0])
}

type sseEvent struct {
	id, typ string
	data    map[string]any
}

// subscribe opens a lounge stream as a and returns its events.
func (f *loungeFixture) subscribe(t *testing.T, a *spacetest.Account, params url.Values) <-chan sseEvent {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, f.url+"/xrpc/garden.engram.subscribeLounge?"+params.Encode(), nil)
	c, err := spaceclient.New(f.net.Session(a), f.net.Dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	h, err := c.SignedHeaders(context.Background(), f.net.Space, serviceDID)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range h {
		req.Header.Set(k, v)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	resp, err := http.DefaultClient.Do(req.WithContext(ctx))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("subscribe: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	out := make(chan sseEvent, 64)
	go func() {
		defer resp.Body.Close()
		defer close(out)
		sc := bufio.NewScanner(resp.Body)
		var e sseEvent
		for sc.Scan() {
			line := sc.Text()
			switch {
			case line == "":
				if e.typ != "" {
					out <- e
				}
				e = sseEvent{}
			case strings.HasPrefix(line, "id: "):
				e.id = strings.TrimPrefix(line, "id: ")
			case strings.HasPrefix(line, "event: "):
				e.typ = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				_ = json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &e.data)
			}
		}
	}()
	return out
}

func next(t *testing.T, ch <-chan sseEvent) sseEvent {
	t.Helper()
	select {
	case e, ok := <-ch:
		if !ok {
			t.Fatal("stream ended")
		}
		return e
	case <-time.After(5 * time.Second):
		t.Fatal("no event")
	}
	return sseEvent{}
}

func TestLoungeStreamsWritesAsTheyArrive(t *testing.T) {
	t.Parallel()
	f := setupLounge(t)
	ctx := context.Background()
	if _, err := f.srv.Indexer.Register(ctx, f.net.Space, f.srv.Notify(f.net.Space)); err != nil {
		t.Fatal(err)
	}
	f.net.Put(f.bob, lex.PresenceCollection, lex.PresenceRkey, presence(lex.StateIdle, f.now.Add(5*time.Minute), "bob"))
	if err := f.srv.syncLounge(ctx); err != nil {
		t.Fatal(err)
	}

	events := f.subscribe(t, f.alice, url.Values{"space": {f.net.Space}, "to": {f.alice.DID}})
	if e := next(t, events); e.typ != "presence" || e.data["presence"].(map[string]any)["did"] != f.bob.DID {
		t.Fatalf("snapshot: %+v", e)
	}
	ready := next(t, events)
	if ready.typ != "ready" || ready.id == "" {
		t.Fatalf("ready: %+v", ready)
	}

	// Bob writes a message for alice and one for carol: the notification
	// brings alice hers, and only hers.
	f.net.Put(f.bob, lex.MessageCollection, "3m1", message("for carol", "did:plc:carol"))
	f.net.Put(f.bob, lex.MessageCollection, "3m2", message("for alice", f.alice.DID))
	if got := f.net.DeliverWrite(f.bob, ""); len(got) != 1 || got[0] != 200 {
		t.Fatalf("delivery statuses: %v", got)
	}
	e := next(t, events)
	if e.typ != "message" || e.data["message"].(map[string]any)["text"] != "for alice" {
		t.Fatalf("live message: %+v", e)
	}

	// The lease runs out: alice hears bob went offline.
	f.srv.Jobs.Wait()
	f.now = f.now.Add(10 * time.Minute)
	f.srv.Lounge.Tick()
	if e := next(t, events); e.typ != "presence" || e.data["presence"].(map[string]any)["online"] != false {
		t.Fatalf("lease expiry: %+v", e)
	}

	// Resuming from the ready cursor replays what came after it.
	resumed := f.subscribe(t, f.alice, url.Values{"space": {f.net.Space}, "to": {f.alice.DID}, "cursor": {ready.id}})
	if e := next(t, resumed); e.typ != "message" || e.data["message"].(map[string]any)["text"] != "for alice" {
		t.Fatalf("resumed backlog: %+v", e)
	}
	if e := next(t, resumed); e.typ != "presence" {
		t.Fatalf("resumed backlog: %+v", e)
	}
	if e := next(t, resumed); e.typ != "ready" {
		t.Fatalf("resumed ready: %+v", e)
	}

	// A cursor from another process gets a reset and a snapshot.
	reset := f.subscribe(t, f.alice, url.Values{"space": {f.net.Space}, "cursor": {"otherepoch-4"}})
	if e := next(t, reset); e.typ != "reset" {
		t.Fatalf("stale cursor: %+v", e)
	}
}

func newHTTPServer(t *testing.T, h http.Handler) string {
	t.Helper()
	hs := httptest.NewServer(h)
	t.Cleanup(hs.Close)
	return hs.URL
}
