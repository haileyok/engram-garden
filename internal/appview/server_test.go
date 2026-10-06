package appview

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/haileyok/cocoon/space"

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

type fixture struct {
	net     *spacetest.Net
	srv     *Server
	url     string
	alice   *spacetest.Account
	bob     *spacetest.Account
	mallory *spacetest.Account
}

func setup(t *testing.T) *fixture {
	t.Helper()
	st := storetest.New(t, dims)
	n := spacetest.New(t)
	appview := n.NewAccount("did:plc:appview")
	alice := n.NewAccount("did:plc:alice")
	bob := n.NewAccount("did:plc:bob")
	mallory := n.NewAccount("did:plc:mallory")
	for _, a := range []*spacetest.Account{appview, alice, bob} {
		n.AddMember(a.DID)
	}
	client, err := spaceclient.New(n.Session(appview), n.Dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	emb := embed.Hashing{Dims: dims}
	s := &Server{
		Store:      st,
		Embedder:   emb,
		Indexer:    &indexer.Indexer{Store: st, Embedder: emb, Client: client, Dir: n.Dir},
		Dir:        n.Dir,
		ServiceDID: serviceDID,
		Spaces:     []string{n.Space},
	}
	hs := httptest.NewServer(s.Handler())
	t.Cleanup(hs.Close)
	s.PublicURL = hs.URL
	n.RegisterService(serviceDID, SyncerFragment, hs.URL)
	return &fixture{net: n, srv: s, url: hs.URL, alice: alice, bob: bob, mallory: mallory}
}

func memory(text string, tags ...string) map[string]any {
	m := map[string]any{"$type": indexer.Collection, "text": text, "createdAt": time.Now().UTC().Format(time.RFC3339)}
	if len(tags) > 0 {
		m["tags"] = tags
	}
	return m
}

// get calls the appview, signing with a's credential for the given audience
// when a is set.
func (f *fixture) get(t *testing.T, a *spacetest.Account, audience, nsid string, params url.Values) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, f.url+"/xrpc/"+nsid+"?"+params.Encode(), nil)
	if a != nil {
		c, err := spaceclient.New(f.net.Session(a), f.net.Dir, nil)
		if err != nil {
			t.Fatal(err)
		}
		h, err := c.SignedHeaders(context.Background(), f.net.Space, audience)
		if err != nil {
			t.Fatal(err)
		}
		for k, v := range h {
			req.Header.Set(k, v)
		}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&body)
	return resp.StatusCode, body
}

func memories(body map[string]any) []map[string]any {
	raw, _ := body["memories"].([]any)
	out := make([]map[string]any, len(raw))
	for i, m := range raw {
		out[i], _ = m.(map[string]any)
	}
	return out
}

func TestMemberSearchesTheSpace(t *testing.T) {
	t.Parallel()
	f := setup(t)
	ctx := context.Background()
	f.net.Put(f.alice, indexer.Collection, "a1", memory("pop1 deploys go through the deploy repo workflow", "infra"))
	f.net.Put(f.bob, indexer.Collection, "b1", memory("hailey prefers short answers", "prefs"))
	if err := f.srv.Indexer.SyncSpace(ctx, f.net.Space); err != nil {
		t.Fatal(err)
	}

	// Bob finds what Alice wrote.
	status, body := f.get(t, f.bob, serviceDID, "garden.engram.searchMemories", url.Values{"space": {f.net.Space}, "q": {"deploy workflow"}, "limit": {"1"}})
	if status != 200 {
		t.Fatalf("search: %d %v", status, body)
	}
	ms := memories(body)
	if len(ms) != 1 || ms[0]["author"] != f.alice.DID || ms[0]["similarity"].(float64) <= 0 {
		t.Fatalf("search results: %v", body)
	}

	status, body = f.get(t, f.bob, serviceDID, "garden.engram.searchMemories", url.Values{"space": {f.net.Space}, "q": {"anything"}, "tags": {"prefs"}})
	if status != 200 || len(memories(body)) != 1 || memories(body)[0]["author"] != f.bob.DID {
		t.Fatalf("tag-filtered search: %d %v", status, body)
	}

	uri := memories(body)[0]["uri"].(string)
	status, body = f.get(t, f.alice, serviceDID, "garden.engram.getMemory", url.Values{"space": {f.net.Space}, "uri": {uri}})
	if status != 200 || body["memory"].(map[string]any)["text"] != "hailey prefers short answers" {
		t.Fatalf("getMemory: %d %v", status, body)
	}
	status, _ = f.get(t, f.alice, serviceDID, "garden.engram.getMemory", url.Values{"space": {f.net.Space}, "uri": {uri + "x"}})
	if status != 404 {
		t.Fatalf("missing memory: %d", status)
	}

	status, body = f.get(t, f.alice, serviceDID, "garden.engram.listMemories", url.Values{"space": {f.net.Space}, "limit": {"1"}})
	if status != 200 || len(memories(body)) != 1 || body["cursor"] == nil {
		t.Fatalf("listMemories page 1: %d %v", status, body)
	}
	status, body = f.get(t, f.alice, serviceDID, "garden.engram.listMemories", url.Values{"space": {f.net.Space}, "cursor": {body["cursor"].(string)}})
	if status != 200 || len(memories(body)) != 1 || body["cursor"] != nil {
		t.Fatalf("listMemories page 2: %d %v", status, body)
	}
}

func TestReaderAuth(t *testing.T) {
	t.Parallel()
	f := setup(t)
	q := url.Values{"space": {f.net.Space}, "q": {"x"}}

	if status, body := f.get(t, nil, "", "garden.engram.searchMemories", q); status != 401 {
		t.Fatalf("no credential: %d %v", status, body)
	}
	// A credential signed for another audience can't be replayed here.
	if status, body := f.get(t, f.alice, f.alice.DID, "garden.engram.searchMemories", q); status != 401 || body["error"] != "BadSpaceSignature" {
		t.Fatalf("wrong audience: %d %v", status, body)
	}
	if status, body := f.get(t, f.alice, serviceDID, "garden.engram.searchMemories", url.Values{"space": {"at://did:plc:x/space/garden.engram.space/other"}, "q": {"x"}}); status != 400 || body["error"] != "UnknownSpace" {
		t.Fatalf("unindexed space: %d %v", status, body)
	}
	if status, body := f.get(t, f.alice, serviceDID, "garden.engram.searchMemories", url.Values{"space": {f.net.Space}}); status != 400 {
		t.Fatalf("missing q: %d %v", status, body)
	}

	// A tampered signature fails.
	c, _ := spaceclient.New(f.net.Session(f.alice), f.net.Dir, nil)
	h, err := c.SignedHeaders(context.Background(), f.net.Space, serviceDID)
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodGet, f.url+"/xrpc/garden.engram.searchMemories?"+q.Encode(), nil)
	for k, v := range h {
		req.Header.Set(k, v)
	}
	req.Header.Set(space.HeaderSpaceAudience, serviceDID+"x")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("tampered audience: %d", resp.StatusCode)
	}
}

func TestNotificationsKeepTheIndexCurrent(t *testing.T) {
	t.Parallel()
	f := setup(t)
	ctx := context.Background()

	if _, err := f.srv.Indexer.Register(ctx, f.net.Space, f.srv.ServiceID()); err != nil {
		t.Fatal(err)
	}
	if regs := f.net.Registrations(); len(regs) != 1 || regs[0] != serviceDID+"#"+SyncerFragment {
		t.Fatalf("registrations: %v", regs)
	}

	f.net.Put(f.alice, indexer.Collection, "a1", memory("remember the milk"))
	if got := f.net.DeliverWrite(f.alice, ""); len(got) != 1 || got[0] != 200 {
		t.Fatalf("delivery statuses: %v", got)
	}
	f.srv.Jobs.Wait()
	status, body := f.get(t, f.bob, serviceDID, "garden.engram.searchMemories", url.Values{"space": {f.net.Space}, "q": {"milk"}})
	if status != 200 || len(memories(body)) != 1 {
		t.Fatalf("notified write not indexed: %d %v", status, body)
	}

	// Only the authority may notify, and only addressed to this service.
	svc := f.srv.ServiceID()
	body2 := map[string]string{"space": f.net.Space, "repo": f.alice.DID, "repoRev": "x", "hash": "x"}
	if s := f.net.Deliver(svc, "com.atproto.space.notifyWrite", body2, f.net.SignServiceAuth(f.mallory, svc, "com.atproto.space.notifyWrite")); s != 403 {
		t.Fatalf("non-authority notification: %d", s)
	}
	if s := f.net.Deliver(svc, "com.atproto.space.notifyWrite", body2, f.net.ServiceAuth("did:web:elsewhere#"+SyncerFragment, "com.atproto.space.notifyWrite")); s != 401 {
		t.Fatalf("misaddressed notification: %d", s)
	}
	if s := f.net.Deliver(svc, "com.atproto.space.notifyWrite", body2, f.net.ServiceAuth(svc, "com.atproto.space.notifySpaceDeleted")); s != 401 {
		t.Fatalf("wrong lxm: %d", s)
	}
	if s := f.net.Deliver(svc, "com.atproto.space.notifyWrite", body2, ""); s != 401 {
		t.Fatalf("unauthenticated notification: %d", s)
	}

	// Space deletion empties the index and closes reads.
	del := map[string]string{"space": f.net.Space}
	if s := f.net.Deliver(svc, "com.atproto.space.notifySpaceDeleted", del, f.net.ServiceAuth(svc, "com.atproto.space.notifySpaceDeleted")); s != 200 {
		t.Fatalf("notifySpaceDeleted: %d", s)
	}
	if status, body := f.get(t, f.bob, serviceDID, "garden.engram.searchMemories", url.Values{"space": {f.net.Space}, "q": {"milk"}}); status != 400 || body["error"] != "UnknownSpace" {
		t.Fatalf("read after deletion: %d %v", status, body)
	}
}

func TestRunSyncsAndRegisters(t *testing.T) {
	t.Parallel()
	f := setup(t)
	f.net.Put(f.alice, indexer.Collection, "a1", memory("polled in"))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { f.srv.Run(ctx, time.Hour, true); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		status, body := f.get(t, f.bob, serviceDID, "garden.engram.searchMemories", url.Values{"space": {f.net.Space}, "q": {"polled"}})
		if status == 200 && len(memories(body)) == 1 && len(f.net.Registrations()) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("Run did not sync and register: %d %v regs=%v", status, body, f.net.Registrations())
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done
}

func TestDIDDocument(t *testing.T) {
	t.Parallel()
	f := setup(t)
	resp, err := http.Get(f.url + "/.well-known/did.json")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var doc struct {
		ID      string `json:"id"`
		Service []struct {
			ID              string `json:"id"`
			ServiceEndpoint string `json:"serviceEndpoint"`
		} `json:"service"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&doc)
	if doc.ID != serviceDID || len(doc.Service) != 1 || doc.Service[0].ID != "#"+SyncerFragment || doc.Service[0].ServiceEndpoint != f.url {
		t.Fatalf("did doc: %+v", doc)
	}
}
