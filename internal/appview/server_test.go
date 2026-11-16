package appview

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/haileyok/cocoon/space"

	"github.com/haileyok/engram-garden/internal/blob"
	"github.com/haileyok/engram-garden/internal/control/controltest"
	"github.com/haileyok/engram-garden/internal/embed"
	"github.com/haileyok/engram-garden/internal/indexer"
	"github.com/haileyok/engram-garden/internal/lex"
	"github.com/haileyok/engram-garden/internal/routing"
	"github.com/haileyok/engram-garden/internal/spaceclient"
	"github.com/haileyok/engram-garden/internal/spacestore"
	"github.com/haileyok/engram-garden/internal/spacetest"
)

const serviceDID = "did:web:engram.test"

var model = lex.ModelInfo{Model: "hashing-256", ModelDigest: embed.HashingDigest, Dims: 256}

type fixture struct {
	net     *spacetest.Net
	srv     *Server
	url     string
	alice   *spacetest.Account
	bob     *spacetest.Account
	mallory *spacetest.Account
	client  *spaceclient.Client
	grants  *Grants
	auth    *fakeAuth
}

func newNode(t *testing.T, bs blob.Store, lease func(string) (uint64, bool)) *spacestore.Node {
	t.Helper()
	n, err := spacestore.New(spacestore.Options{Blob: bs, CacheDir: t.TempDir(), Lease: lease})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = n.Close(context.Background()) })
	return n
}

// newNet is a space with three accounts, two of them members, and an
// appview that reads it through the authority's grant.
func newNet(t *testing.T) (*spacetest.Net, *Grants, *fakeAuth, *spaceclient.Client, [3]*spacetest.Account) {
	t.Helper()
	n := spacetest.New(t)
	alice := n.NewAccount("did:plc:alice")
	bob := n.NewAccount("did:plc:bob")
	mallory := n.NewAccount("did:plc:mallory")
	for _, a := range []*spacetest.Account{alice, bob} {
		n.AddMember(a.DID)
	}
	n.Put(n.Authority, lex.ConfigCollection, lex.ConfigRkey, lex.Config{ModelInfo: model}.Record(time.Now()))
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
	return n, grants, auth, client, [3]*spacetest.Account{alice, bob, mallory}
}

func setup(t *testing.T) *fixture {
	t.Helper()
	n, grants, auth, client, accts := newNet(t)
	st := newNode(t, blob.Dir{Root: t.TempDir()}, nil)
	s := &Server{
		Store:         st,
		Indexer:       &indexer.Indexer{Store: st, Client: client, Dir: n.Dir},
		Dir:           n.Dir,
		ServiceDID:    serviceDID,
		Spaces:        []string{n.Space},
		Grants:        grants,
		ReturnOrigins: []string{webOrigin},
		CookieKey:     []byte("0123456789abcdef0123456789abcdef"),
	}
	hs := httptest.NewServer(s.Handler())
	t.Cleanup(hs.Close)
	s.PublicURL = hs.URL
	return &fixture{net: n, srv: s, url: hs.URL, alice: accts[0], bob: accts[1], mallory: accts[2], client: client, grants: grants, auth: auth}
}

func vector(text string) []float32 {
	v, _ := embed.Hashing{Dims: model.Dims}.Embed(context.Background(), []string{text})
	return v[0]
}

func memory(text string, tags ...string) map[string]any {
	m := map[string]any{"$type": indexer.Collection, "text": text, "createdAt": time.Now().UTC().Format(time.RFC3339)}
	if len(tags) > 0 {
		m["tags"] = tags
	}
	m["embedding"] = lex.EmbeddingRecord(model, vector(lex.EmbedText("", text, tags)))
	return m
}

func searchParams(spaceURI, q string) url.Values {
	return url.Values{
		"space": {spaceURI}, "q": {q}, "vector": {lex.EncodeQueryVector(vector(q))},
		"model": {model.Model}, "modelDigest": {model.ModelDigest},
	}
}

// do calls a base URL, signing with a's credential for the given audience
// when a is set.
func do(t *testing.T, n *spacetest.Net, base, method string, a *spacetest.Account, audience, nsid string, params url.Values, body any) (int, []byte) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		rd = bytes.NewReader(raw)
	}
	req, _ := http.NewRequest(method, base+"/xrpc/"+nsid+"?"+params.Encode(), rd)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if a != nil {
		c, err := spaceclient.New(n.Session(a), n.Dir, nil)
		if err != nil {
			t.Fatal(err)
		}
		h, err := c.SignedHeaders(context.Background(), n.Space, audience)
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
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw
}

func (f *fixture) get(t *testing.T, a *spacetest.Account, audience, nsid string, params url.Values) (int, map[string]any) {
	t.Helper()
	status, raw := do(t, f.net, f.url, http.MethodGet, a, audience, nsid, params, nil)
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	return status, body
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
	f.net.Put(f.bob, indexer.Collection, "b2", map[string]any{"$type": indexer.Collection, "text": "no vector", "createdAt": time.Now().UTC().Format(time.RFC3339)})
	if err := f.srv.Indexer.SyncSpace(ctx, f.net.Space); err != nil {
		t.Fatal(err)
	}

	// Bob finds what Alice wrote.
	p := searchParams(f.net.Space, "deploy workflow")
	p.Set("limit", "1")
	status, body := f.get(t, f.bob, serviceDID, "garden.engram.searchMemories", p)
	if status != 200 {
		t.Fatalf("search: %d %v", status, body)
	}
	ms := memories(body)
	if len(ms) != 1 || ms[0]["author"] != f.alice.DID || ms[0]["similarity"].(float64) <= 0 || body["approximate"] != nil {
		t.Fatalf("search results: %v", body)
	}

	p = searchParams(f.net.Space, "anything")
	p["tags"] = []string{"prefs"}
	status, body = f.get(t, f.bob, serviceDID, "garden.engram.searchMemories", p)
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

	// Status reports the model and the memory without a vector.
	status, body = f.get(t, f.alice, serviceDID, "garden.engram.getSpaceStatus", url.Values{"space": {f.net.Space}})
	sk, _ := body["skipped"].([]any)
	if status != 200 || body["memories"].(float64) != 2 || len(sk) != 1 || body["active"].(map[string]any)["model"] != model.Model {
		t.Fatalf("status: %d %v", status, body)
	}

	// A query from another model is refused with a clear error.
	p = searchParams(f.net.Space, "x")
	p.Set("modelDigest", "sha256:other")
	if status, body := f.get(t, f.bob, serviceDID, "garden.engram.searchMemories", p); status != 400 || body["error"] != "ModelMismatch" {
		t.Fatalf("wrong model: %d %v", status, body)
	}

	// Warming is accepted.
	if status, raw := do(t, f.net, f.url, http.MethodPost, f.bob, serviceDID, "garden.engram.warmSpace", nil, map[string]string{"space": f.net.Space}); status != 200 {
		t.Fatalf("warm: %d %s", status, raw)
	}
}

func TestReaderAuth(t *testing.T) {
	t.Parallel()
	f := setup(t)
	q := searchParams(f.net.Space, "x")

	if status, body := f.get(t, nil, "", "garden.engram.searchMemories", q); status != 401 {
		t.Fatalf("no credential: %d %v", status, body)
	}
	// A credential signed for another audience can't be replayed here.
	if status, body := f.get(t, f.alice, f.alice.DID, "garden.engram.searchMemories", q); status != 401 || body["error"] != "BadSpaceSignature" {
		t.Fatalf("wrong audience: %d %v", status, body)
	}
	if status, body := f.get(t, f.alice, serviceDID, "garden.engram.searchMemories", searchParams("at://did:plc:x/space/garden.engram.space/other", "x")); status != 400 || body["error"] != "UnknownSpace" {
		t.Fatalf("unindexed space: %d %v", status, body)
	}
	if status, body := f.get(t, f.alice, serviceDID, "garden.engram.searchMemories", url.Values{"space": {f.net.Space}, "q": {"x"}}); status != 400 {
		t.Fatalf("missing vector: %d %v", status, body)
	}
	if status, _ := f.get(t, nil, "", "garden.engram.exportSpace", url.Values{"space": {f.net.Space}}); status != 401 {
		t.Fatalf("export without a credential: %d", status)
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

// TestNotificationForSpaceWithoutGrant: a space whose authority never let the
// appview read it (or took that back) can't be synced, however many writes
// are notified. They're counted apart from failed syncs, so a hoster can
// alert on them, and indexing picks up the missed writes once access is
// granted.
func TestNotificationForSpaceWithoutGrant(t *testing.T) {
	t.Parallel()
	f := setup(t)
	ctx := context.Background()
	// The registration outlives the grant at the authority's server.
	if _, err := f.srv.Indexer.Register(ctx, f.net.Space, f.srv.Notify(f.net.Space)); err != nil {
		t.Fatal(err)
	}
	if err := f.srv.Indexer.SyncSpace(ctx, f.net.Space); err != nil {
		t.Fatal(err)
	}
	if err := f.grants.Delete(ctx, f.net.Space); err != nil {
		t.Fatal(err)
	}
	notGranted := func() float64 {
		return metricValue(t, "engram_notifications_total", map[string]string{"kind": "write", "result": "not_granted"})
	}
	before := notGranted()

	f.net.Put(f.alice, indexer.Collection, "a1", memory("remember the milk"))
	if got := f.net.DeliverWrite(f.alice, ""); len(got) != 1 || got[0] != 200 {
		t.Fatalf("delivery statuses: %v", got)
	}
	f.srv.Jobs.Wait()
	// Other tests share the metrics registry: at least.
	if got := notGranted() - before; got < 1 {
		t.Fatalf("a write for a space without a grant was counted %v times as not_granted", got)
	}
	if a := f.access(t); a["state"] != "missing" {
		t.Fatalf("access without a grant: %v", a)
	}
	if status, body := f.get(t, f.bob, serviceDID, "garden.engram.searchMemories", searchParams(f.net.Space, "milk")); status != 200 || len(memories(body)) != 0 {
		t.Fatalf("a write was indexed without a grant: %d %v", status, body)
	}

	if err := f.grants.Put(ctx, Grant{Space: f.net.Space, DID: f.net.Authority.DID, SessionID: "seed", GrantedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	f.net.Put(f.alice, indexer.Collection, "a2", memory("remember the eggs"))
	if got := f.net.DeliverWrite(f.alice, ""); len(got) != 1 || got[0] != 200 {
		t.Fatalf("delivery statuses: %v", got)
	}
	f.srv.Jobs.Wait()
	// The earlier write comes along with this one.
	status, body := f.get(t, f.bob, serviceDID, "garden.engram.searchMemories", searchParams(f.net.Space, "remember"))
	if status != 200 || len(memories(body)) != 2 {
		t.Fatalf("after the grant: %d %v", status, body)
	}
}

func TestNotificationsKeepTheIndexCurrent(t *testing.T) {
	t.Parallel()
	f := setup(t)
	ctx := context.Background()

	if _, err := f.srv.Indexer.Register(ctx, f.net.Space, f.srv.Notify(f.net.Space)); err != nil {
		t.Fatal(err)
	}
	if regs := f.net.Registrations(); len(regs) != 1 || regs[0].Endpoint != f.url || regs[0].Token != f.srv.Notify(f.net.Space).Token {
		t.Fatalf("registrations: %v", regs)
	}

	f.net.Put(f.alice, indexer.Collection, "a1", memory("remember the milk"))
	if got := f.net.DeliverWrite(f.alice, ""); len(got) != 1 || got[0] != 200 {
		t.Fatalf("delivery statuses: %v", got)
	}
	f.srv.Jobs.Wait()
	status, body := f.get(t, f.bob, serviceDID, "garden.engram.searchMemories", searchParams(f.net.Space, "milk"))
	if status != 200 || len(memories(body)) != 1 {
		t.Fatalf("notified write not indexed: %d %v", status, body)
	}

	// Only the host this service registered with, which holds the token for
	// the space, may notify.
	reg := f.net.Registrations()[0]
	update := map[string]any{"$type": "com.atproto.space.defs#repoWrite", "repo": f.alice.DID, "repoRev": "x", "data": space.LexBytes{1}}
	for name, body := range map[string]map[string]any{
		"wrong token":   {"space": f.net.Space, "token": "guess", "update": update},
		"no token":      {"space": f.net.Space, "update": update},
		"another space": {"space": "at://did:plc:x/space/garden.engram.space/other", "token": reg.Token, "update": update},
	} {
		if s := f.net.Deliver(reg.Endpoint, "com.atproto.space.notifySpaceUpdate", body); s != 400 && s != 403 {
			t.Fatalf("%s: notification answered %d", name, s)
		}
	}
	if out, s := f.net.Post(reg.Endpoint, "com.atproto.space.confirmRegistration", map[string]string{"space": f.net.Space, "token": "guess", "challenge": "c"}); s != 403 {
		t.Fatalf("confirmed a registration for the wrong token: %d %s", s, out)
	}

	// A registration is confirmed by echoing the challenge for the right token.
	out, s := f.net.Post(reg.Endpoint, "com.atproto.space.confirmRegistration", map[string]string{"space": f.net.Space, "token": reg.Token, "challenge": "abc"})
	if s != 200 || !strings.Contains(string(out), `"abc"`) {
		t.Fatalf("confirmRegistration: %d %s", s, out)
	}

	// A host that reports the space deleted is asked again before the index
	// drops it: a notice for a space that still authorizes changes nothing.
	del := map[string]any{"$type": "com.atproto.space.notifySpaceUpdate#spaceDeleted"}
	if got := f.net.DeliverUpdate(del); len(got) != 1 || got[0] != 200 {
		t.Fatalf("spaceDeleted: %v", got)
	}
	f.srv.Jobs.Wait()
	if status, body := f.get(t, f.bob, serviceDID, "garden.engram.searchMemories", searchParams(f.net.Space, "milk")); status != 200 || len(memories(body)) != 1 {
		t.Fatalf("dropped a space that still authorizes: %d %v", status, body)
	}
	// Once the host really has deleted it, the notice empties the index and
	// closes reads.
	f.net.DeleteSpace()
	if got := f.net.DeliverUpdate(del); len(got) != 1 || got[0] != 200 {
		t.Fatalf("spaceDeleted: %v", got)
	}
	f.srv.Jobs.Wait()
	if deleted, err := f.srv.Store.SpaceDeleted(ctx, f.net.Space); err != nil || !deleted {
		t.Fatalf("space not marked deleted: %v %v", deleted, err)
	}
}

// TestRemovedWriterNotification: a repoRemoved update drops the repo's
// memories, and a lapsed registration is renewed on the host's notice.
func TestRemovedWriterAndExpiryNotifications(t *testing.T) {
	t.Parallel()
	f := setup(t)
	ctx := context.Background()
	if _, err := f.srv.Indexer.Register(ctx, f.net.Space, f.srv.Notify(f.net.Space)); err != nil {
		t.Fatal(err)
	}
	f.net.Put(f.alice, indexer.Collection, "a1", memory("alice stays"))
	_, bobRev := f.net.Put(f.bob, indexer.Collection, "b1", memory("bob leaves"))
	if err := f.srv.Indexer.SyncSpace(ctx, f.net.Space); err != nil {
		t.Fatal(err)
	}
	removedRev := f.net.DropWriter(f.bob)
	removal := map[string]any{"$type": "com.atproto.space.defs#repoRemoved", "repo": f.bob.DID, "spaceRev": removedRev, "prevSpaceRev": bobRev}
	if got := f.net.DeliverUpdate(removal); len(got) != 1 || got[0] != 200 {
		t.Fatalf("repoRemoved: %v", got)
	}
	f.srv.Jobs.Wait()
	status, body := f.get(t, f.alice, serviceDID, "garden.engram.searchMemories", searchParams(f.net.Space, "bob leaves"))
	if status != 200 {
		t.Fatalf("search: %d %v", status, body)
	}
	for _, m := range memories(body) {
		if m["author"] == f.bob.DID {
			t.Fatalf("removed writer's memory still indexed: %v", m)
		}
	}
	if len(memories(body)) != 1 {
		t.Fatalf("want only alice's memory left: %v", body)
	}

	// An expiry notice has the service authorize again, and read what it
	// missed.
	before := f.net.Calls("com.atproto.space.authorizeSpaceSync")
	f.net.Put(f.alice, indexer.Collection, "a2", memory("written while unregistered"))
	expired := map[string]any{"$type": "com.atproto.space.notifySpaceUpdate#registrationExpired", "spaceRev": "3kbcq3p7ad400"}
	if got := f.net.DeliverUpdate(expired); len(got) != 1 || got[0] != 200 {
		t.Fatalf("registrationExpired: %v", got)
	}
	f.srv.Jobs.Wait()
	if f.net.Calls("com.atproto.space.authorizeSpaceSync") <= before {
		t.Fatal("did not reauthorize after the registration expired")
	}
	status, body = f.get(t, f.alice, serviceDID, "garden.engram.searchMemories", searchParams(f.net.Space, "unregistered"))
	found := false
	for _, m := range memories(body) {
		found = found || m["text"] == "written while unregistered"
	}
	if status != 200 || !found {
		t.Fatalf("missed write not synced after expiry: %d %v", status, body)
	}
}

func TestRunSyncsAndRegisters(t *testing.T) {
	t.Parallel()
	f := setup(t)
	f.srv.DB = controltest.New(t)
	f.net.Put(f.alice, indexer.Collection, "a1", memory("polled in"))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { f.srv.Run(ctx, time.Hour, true); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		status, body := f.get(t, f.bob, serviceDID, "garden.engram.searchMemories", searchParams(f.net.Space, "polled"))
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

func TestExportThenImportElsewhere(t *testing.T) {
	t.Parallel()
	f := setup(t)
	ctx := context.Background()
	f.net.Put(f.alice, indexer.Collection, "a1", memory("exported memory about tea"))
	if err := f.srv.Indexer.SyncSpace(ctx, f.net.Space); err != nil {
		t.Fatal(err)
	}
	status, tarball := do(t, f.net, f.url, http.MethodGet, f.bob, serviceDID, "garden.engram.exportSpace", url.Values{"space": {f.net.Space}}, nil)
	if status != 200 || len(tarball) == 0 {
		t.Fatalf("export: %d", status)
	}
	other := newNode(t, blob.Dir{Root: t.TempDir()}, nil)
	if _, err := other.Import(ctx, bytes.NewReader(tarball)); err != nil {
		t.Fatal(err)
	}
	res, err := other.Search(ctx, f.net.Space, spacestore.SearchQuery{Vector: vector("tea"), Model: model, Limit: 3})
	if err != nil || len(res.Hits) != 1 || res.Hits[0].Text != "exported memory about tea" {
		t.Fatalf("search on the importing appview: %+v %v", res, err)
	}
}

// TestForwardingToOwner runs two nodes: requests to the node that doesn't
// own the space are forwarded to the one that does.
func TestForwardingToOwner(t *testing.T) {
	t.Parallel()
	n, _, _, client, accts := newNet(t)
	alice, bob := accts[0], accts[1]
	bs := blob.Dir{Root: t.TempDir()}
	var handlers [2]http.Handler
	var servers [2]*httptest.Server
	for i := range servers {
		servers[i] = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { handlers[i].ServeHTTP(w, r) }))
		t.Cleanup(servers[i].Close)
	}
	nodes := []routing.Node{{ID: "a", URL: servers[0].URL}, {ID: "b", URL: servers[1].URL}}
	var srvs [2]*Server
	for i, id := range []string{"a", "b"} {
		ring := &routing.Ring{Self: id, Nodes: nodes, Epoch: 1}
		st := newNode(t, bs, ring.Lease)
		srvs[i] = &Server{
			Store: st, Indexer: &indexer.Indexer{Store: st, Client: client, Dir: n.Dir},
			Dir: n.Dir, ServiceDID: serviceDID, Spaces: []string{n.Space}, Ring: ring,
		}
		handlers[i] = srvs[i].Handler()
	}
	ownerIdx := 0
	if srvs[1].Ring.Owns(n.Space) {
		ownerIdx = 1
	}
	owner, other := srvs[ownerIdx], servers[1-ownerIdx]

	n.Put(alice, indexer.Collection, "a1", memory("forwarded memory about kites"))
	// Notifications arrive at the non-owner.
	notify := owner.Notify(n.Space)
	notify.Endpoint = other.URL
	if _, err := owner.Indexer.Register(context.Background(), n.Space, notify); err != nil {
		t.Fatal(err)
	}
	if got := n.DeliverWrite(n.Authority, ""); len(got) != 1 || got[0] != 200 {
		t.Fatalf("config notification via the non-owner: %v", got)
	}
	if got := n.DeliverWrite(alice, ""); len(got) != 1 || got[0] != 200 {
		t.Fatalf("notification via the non-owner: %v", got)
	}
	owner.Jobs.Wait()
	srvs[1-ownerIdx].Jobs.Wait()
	// The non-owner never loaded the space.
	if _, err := srvs[1-ownerIdx].Store.Status(context.Background(), n.Space); err == nil {
		t.Fatal("the non-owner loaded the space")
	}
	var body map[string]any
	deadline := time.Now().Add(5 * time.Second)
	for {
		status, raw := do(t, n, other.URL, http.MethodGet, bob, serviceDID, "garden.engram.searchMemories", searchParams(n.Space, "kites"), nil)
		_ = json.Unmarshal(raw, &body)
		if status == 200 && len(memories(body)) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("forwarded search: %d %s", status, raw)
		}
		time.Sleep(20 * time.Millisecond)
	}
	// A request already forwarded once isn't forwarded again.
	req, _ := http.NewRequest(http.MethodGet, other.URL+"/xrpc/garden.engram.searchMemories?"+searchParams(n.Space, "x").Encode(), nil)
	req.Header.Set(ForwardedHeader, "z")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("forwarding loop guard: %d", resp.StatusCode)
	}
}

// TestNotificationFloodIsBounded holds a space at its sync cap: further
// notifications start no work, but leave a follow-up so their writes still
// get indexed.
func TestNotificationFloodIsBounded(t *testing.T) {
	t.Parallel()
	f := setup(t)
	f.srv.MaxSpaceSyncs = 1
	if _, err := f.srv.Indexer.Register(context.Background(), f.net.Space, f.srv.Notify(f.net.Space)); err != nil {
		t.Fatal(err)
	}
	release, ok := f.srv.trySpaceSlot(f.net.Space)
	if !ok {
		t.Fatal("no slot")
	}
	f.net.Put(f.alice, indexer.Collection, "a1", memory("flooded memory about otters"))
	for range 20 {
		if got := f.net.DeliverWrite(f.alice, ""); got[0] != 200 {
			t.Fatalf("delivery: %v", got)
		}
	}
	f.srv.Jobs.Wait() // nothing started while the slot was held
	if _, ok := f.srv.followUp.Load(f.net.Space); !ok {
		t.Fatal("no follow-up recorded")
	}
	release()
	// The next notification runs, and its follow-up syncs the space.
	f.net.Put(f.bob, indexer.Collection, "b1", memory("later memory"))
	f.net.DeliverWrite(f.bob, "")
	f.srv.Jobs.Wait()
	status, body := f.get(t, f.bob, serviceDID, "garden.engram.searchMemories", searchParams(f.net.Space, "otters"))
	if status != 200 || len(memories(body)) == 0 || memories(body)[0]["text"] != "flooded memory about otters" {
		t.Fatalf("flooded write not indexed: %d %v", status, body)
	}
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
		ID string `json:"id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&doc)
	if doc.ID != serviceDID {
		t.Fatalf("did doc: %+v", doc)
	}
}

// since and until, which searches and listings both take, pick a window of
// creation time; a value that isn't a datetime is a bad request.
func TestSearchAndListTakeATimeWindow(t *testing.T) {
	t.Parallel()
	f := setup(t)
	ctx := context.Background()
	day := func(d int) string { return time.Date(2026, 10, d, 12, 0, 0, 0, time.UTC).Format(time.RFC3339) }
	for i, rkey := range []string{"a1", "a2", "a3"} {
		rec := memory("deploy notes "+rkey, "infra")
		rec["createdAt"] = day(i + 1)
		f.net.Put(f.alice, indexer.Collection, rkey, rec)
	}
	if err := f.srv.Indexer.SyncSpace(ctx, f.net.Space); err != nil {
		t.Fatal(err)
	}
	rkeysOf := func(body map[string]any) (out []string) {
		for _, m := range memories(body) {
			out = append(out, m["uri"].(string)[strings.LastIndex(m["uri"].(string), "/")+1:])
		}
		slices.Sort(out)
		return out
	}
	for _, nsid := range []string{"garden.engram.searchMemories", "garden.engram.listMemories"} {
		params := func(since, until string) url.Values {
			p := url.Values{"space": {f.net.Space}}
			if nsid == "garden.engram.searchMemories" {
				p = searchParams(f.net.Space, "deploy notes")
			}
			if since != "" {
				p.Set("since", since)
			}
			if until != "" {
				p.Set("until", until)
			}
			return p
		}
		status, body := f.get(t, f.bob, serviceDID, nsid, params(day(2), day(3)))
		if got := rkeysOf(body); status != 200 || !slices.Equal(got, []string{"a2"}) {
			t.Errorf("%s since day 2, until day 3 = %d %v, want just a2", nsid, status, got)
		}
		status, body = f.get(t, f.bob, serviceDID, nsid, params("", day(3)))
		if got := rkeysOf(body); status != 200 || !slices.Equal(got, []string{"a1", "a2"}) {
			t.Errorf("%s until day 3 = %d %v, want a1 and a2", nsid, status, got)
		}
		if status, body := f.get(t, f.bob, serviceDID, nsid, params("", "yesterday")); status != 400 || body["error"] != "InvalidRequest" {
			t.Errorf("%s until=yesterday = %d %v, want 400 InvalidRequest", nsid, status, body)
		}
	}
}
