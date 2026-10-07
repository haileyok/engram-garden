package appview

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/haileyok/engram-garden/internal/blob"
	"github.com/haileyok/engram-garden/internal/indexer"
	"github.com/haileyok/engram-garden/internal/spacetest"
)

// registrationFixture is an appview that indexes no spaces until one is
// registered.
func registrationFixture(t *testing.T, open bool) *fixture {
	t.Helper()
	f := setup(t)
	f.srv.Spaces = nil
	f.srv.Blob = blob.Dir{Root: t.TempDir()}
	f.srv.OpenRegistration = open
	return f
}

func (f *fixture) register(t *testing.T, a *spacetest.Account) (int, map[string]any) {
	t.Helper()
	status, raw := do(t, f.net, f.url, http.MethodPost, a, serviceDID, "garden.engram.registerSpace", nil, map[string]string{"space": f.net.Space})
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	return status, body
}

func TestRegisterSpace(t *testing.T) {
	t.Parallel()
	f := registrationFixture(t, true)
	f.net.Put(f.alice, indexer.Collection, "a1", memory("registered spaces get indexed"))
	list := url.Values{"space": {f.net.Space}}

	if status, body := f.get(t, f.bob, serviceDID, "garden.engram.listMemories", list); status != 400 || body["error"] != "UnknownSpace" {
		t.Fatalf("before registering: %d %v", status, body)
	}
	if status, body := f.register(t, nil); status != 401 {
		t.Fatalf("without a credential: %d %v", status, body)
	}
	if status, body := f.register(t, f.alice); status != 200 || body["space"] != f.net.Space {
		t.Fatalf("register: %d %v", status, body)
	}
	f.srv.Jobs.Wait()
	status, body := f.get(t, f.bob, serviceDID, "garden.engram.listMemories", list)
	if status != 200 || len(memories(body)) != 1 {
		t.Fatalf("after registering: %d %v", status, body)
	}
	// Registering again is harmless.
	if status, body := f.register(t, f.bob); status != 200 {
		t.Fatalf("second register: %d %v", status, body)
	}

	// A restarted appview picks the registration up from storage.
	again := &Server{Store: f.srv.Store, Indexer: f.srv.Indexer, Dir: f.srv.Dir, ServiceDID: serviceDID, Blob: f.srv.Blob}
	if again.indexes(f.net.Space) {
		t.Fatal("indexes before loading registrations")
	}
	if err := again.LoadRegistrations(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !again.indexes(f.net.Space) {
		t.Fatal("registration didn't survive a restart")
	}
}

func TestDescribeService(t *testing.T) {
	t.Parallel()
	f := registrationFixture(t, true)
	status, body := f.get(t, nil, "", "garden.engram.describeService", nil)
	if status != 200 || body["did"] != serviceDID || body["account"] != f.client.DID().String() || body["registration"] != "open" {
		t.Fatalf("describe: %d %v", status, body)
	}
}

// TestRegistrationSyncWaitsForASlot: a registration's first sync takes one
// of the node's sync slots, like notified syncs.
func TestRegistrationSyncWaitsForASlot(t *testing.T) {
	t.Parallel()
	f := registrationFixture(t, true)
	f.srv.MaxSyncs = 1
	hold, err := f.srv.acquireNodeSlot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if status, body := f.register(t, f.alice); status != 200 {
		t.Fatalf("register: %d %v", status, body)
	}
	time.Sleep(50 * time.Millisecond)
	if n := f.net.Calls("com.atproto.space.listRepos"); n != 0 {
		t.Fatalf("synced while every slot was taken (%d listRepos calls)", n)
	}
	hold()
	f.srv.Jobs.Wait()
	if f.net.Calls("com.atproto.space.listRepos") == 0 {
		t.Fatal("never synced")
	}
}

func TestRegisterSpaceClosed(t *testing.T) {
	t.Parallel()
	f := registrationFixture(t, false)
	if status, body := f.register(t, f.alice); status != 403 || body["error"] != "RegistrationClosed" {
		t.Fatalf("closed registration: %d %v", status, body)
	}
	if f.srv.indexes(f.net.Space) {
		t.Fatal("registered while closed")
	}
}

// TestRegisterSpaceNeedsAppviewMembership: the authority consents to
// indexing by adding the appview's account to the space.
func TestRegisterSpaceNeedsAppviewMembership(t *testing.T) {
	t.Parallel()
	f := registrationFixture(t, true)
	f.net.RemoveMember(f.client.DID().String())
	status, body := f.register(t, f.alice)
	if status != 403 || body["error"] != "NotAMember" {
		t.Fatalf("appview not a member: %d %v", status, body)
	}
	if f.srv.indexes(f.net.Space) {
		t.Fatal("registered without membership")
	}
}

// TestOtherNodeLearnsRegistration: a node that hasn't seen a registration
// rereads storage when asked about a space it doesn't know.
func TestOtherNodeLearnsRegistration(t *testing.T) {
	t.Parallel()
	f := registrationFixture(t, true)
	if status, body := f.register(t, f.alice); status != 200 {
		t.Fatalf("register: %d %v", status, body)
	}
	f.srv.Jobs.Wait()
	other := &Server{Store: f.srv.Store, Indexer: f.srv.Indexer, Dir: f.srv.Dir, ServiceDID: serviceDID, Blob: f.srv.Blob}
	if !other.knows(context.Background(), f.net.Space) {
		t.Fatal("other node didn't find the registration")
	}
}
