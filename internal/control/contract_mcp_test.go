package control

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"
)

// testMCP is the part of the Store contract for apps that connect to the
// service over MCP: the registered apps, each account's approvals, and the
// approvals waiting to be traded for tokens.
func testMCP(t *testing.T, newStore func(t *testing.T) Store) {
	ctx := context.Background()

	t.Run("mcp clients", func(t *testing.T) {
		t.Parallel()
		s := newStore(t)
		if c, err := s.GetMCPClient(ctx, "nope"); err != nil || c != nil {
			t.Fatalf("a client out of nowhere: %+v %v", c, err)
		}
		in := MCPClient{ID: "c1", Name: "Claude", RedirectURIs: []string{"https://claude.ai/cb", "http://127.0.0.1:1/cb"}, Created: t0}
		if ok, err := s.PutMCPClient(ctx, in, 10); err != nil || !ok {
			t.Fatalf("put: %v %v", ok, err)
		}
		got, err := s.GetMCPClient(ctx, "c1")
		if err != nil || got == nil {
			t.Fatalf("get: %+v %v", got, err)
		}
		if got.Name != "Claude" || !reflect.DeepEqual(got.RedirectURIs, in.RedirectURIs) {
			t.Fatalf("client: %+v", got)
		}
		wantTime(t, "created", got.Created, t0)
	})

	t.Run("mcp clients are capped", func(t *testing.T) {
		t.Parallel()
		s := newStore(t)
		put := func(id string, age time.Duration) bool {
			ok, err := s.PutMCPClient(ctx, MCPClient{ID: id, Name: id, RedirectURIs: []string{"https://x/cb"}, Created: t0.Add(age)}, 2)
			if err != nil {
				t.Fatal(err)
			}
			return ok
		}
		put("old", 0)
		put("new", time.Minute)
		// Someone signed in with "old", so it has to stay.
		if err := s.PutMCPGrant(ctx, MCPGrant{ID: "g", DID: "did:plc:a", SessionID: "s", ClientID: "old", Created: t0, LastUsed: t0, RefreshHash: "h"}, 10); err != nil {
			t.Fatal(err)
		}
		// At the cap, the oldest client nobody signed in with makes room.
		if !put("newer", 2*time.Minute) {
			t.Fatal("a client that nobody used wasn't evicted")
		}
		if c, _ := s.GetMCPClient(ctx, "new"); c != nil {
			t.Fatal("the evicted client is still there")
		}
		if c, _ := s.GetMCPClient(ctx, "old"); c == nil {
			t.Fatal("a client with an approval was evicted")
		}
		// Every client in use: nothing to evict.
		if err := s.PutMCPGrant(ctx, MCPGrant{ID: "g2", DID: "did:plc:a", SessionID: "s", ClientID: "newer", Created: t0, LastUsed: t0, RefreshHash: "h2"}, 10); err != nil {
			t.Fatal(err)
		}
		if put("another", 3*time.Minute) {
			t.Fatal("a client was added past the cap with nothing to evict")
		}
	})

	t.Run("mcp grants", func(t *testing.T) {
		t.Parallel()
		s := newStore(t)
		if g, err := s.GetMCPGrant(ctx, "nope"); err != nil || g != nil {
			t.Fatalf("a grant out of nowhere: %+v %v", g, err)
		}
		a := MCPGrant{ID: "ga", DID: "did:plc:a", SessionID: "sa", ClientID: "c", Created: t0, LastUsed: t0, RefreshHash: "ha"}
		b := MCPGrant{ID: "gb", DID: "did:plc:a", SessionID: "sa", ClientID: "c", Created: t0.Add(time.Hour), LastUsed: t0.Add(time.Hour), RefreshHash: "hb"}
		other := MCPGrant{ID: "go", DID: "did:plc:b", SessionID: "sb", ClientID: "c", Created: t0, LastUsed: t0, RefreshHash: "ho"}
		for _, g := range []MCPGrant{a, b, other} {
			if err := s.PutMCPGrant(ctx, g, 10); err != nil {
				t.Fatal(err)
			}
		}
		got, err := s.GetMCPGrant(ctx, "ga")
		if err != nil || got == nil || got.DID != "did:plc:a" || got.SessionID != "sa" || got.ClientID != "c" || got.RefreshHash != "ha" || got.PrevRefreshHash != "" {
			t.Fatalf("grant: %+v %v", got, err)
		}
		wantTime(t, "created", got.Created, t0)
		wantTime(t, "last used", got.LastUsed, t0)

		// An account's grants, newest first, and only its own.
		list, err := s.ListMCPGrants(ctx, "did:plc:a")
		if err != nil || len(list) != 2 || list[0].ID != "gb" || list[1].ID != "ga" {
			t.Fatalf("grants: %+v %v", list, err)
		}

		if err := s.TouchMCPGrant(ctx, "ga", t0.Add(2*time.Hour)); err != nil {
			t.Fatal(err)
		}
		got, _ = s.GetMCPGrant(ctx, "ga")
		wantTime(t, "last used after a touch", got.LastUsed, t0.Add(2*time.Hour))
		if err := s.TouchMCPGrant(ctx, "missing", t0); err != nil {
			t.Fatalf("touching a missing grant: %v", err)
		}

		// Only the account that holds a grant can delete it.
		if ok, err := s.DeleteMCPGrant(ctx, "ga", "did:plc:b"); err != nil || ok {
			t.Fatalf("deleted by another account: %v %v", ok, err)
		}
		if g, _ := s.GetMCPGrant(ctx, "ga"); g == nil {
			t.Fatal("another account's delete removed the grant")
		}
		if ok, err := s.DeleteMCPGrant(ctx, "ga", "did:plc:a"); err != nil || !ok {
			t.Fatalf("delete: %v %v", ok, err)
		}
		if ok, _ := s.DeleteMCPGrant(ctx, "ga", "did:plc:a"); ok {
			t.Fatal("deleted a grant twice")
		}
		if g, _ := s.GetMCPGrant(ctx, "go"); g == nil {
			t.Fatal("deleting one grant removed another")
		}
	})

	t.Run("mcp grants per account are capped", func(t *testing.T) {
		t.Parallel()
		s := newStore(t)
		for i, id := range []string{"g1", "g2", "g3"} {
			g := MCPGrant{ID: id, DID: "did:plc:a", SessionID: "s", ClientID: "c", Created: t0.Add(time.Duration(i) * time.Minute),
				LastUsed: t0.Add(time.Duration(i) * time.Minute), RefreshHash: "h" + id}
			if err := s.PutMCPGrant(ctx, g, 2); err != nil {
				t.Fatal(err)
			}
		}
		list, _ := s.ListMCPGrants(ctx, "did:plc:a")
		if len(list) != 2 || list[0].ID != "g3" || list[1].ID != "g2" {
			t.Fatalf("after the cap: %+v", list)
		}
		// Another account's grants don't count.
		if err := s.PutMCPGrant(ctx, MCPGrant{ID: "gb", DID: "did:plc:b", SessionID: "s", ClientID: "c", Created: t0, LastUsed: t0, RefreshHash: "hb"}, 2); err != nil {
			t.Fatal(err)
		}
		if list, _ = s.ListMCPGrants(ctx, "did:plc:a"); len(list) != 2 {
			t.Fatalf("another account's grant evicted one: %+v", list)
		}
	})

	t.Run("mcp refresh tokens rotate", func(t *testing.T) {
		t.Parallel()
		s := newStore(t)
		if err := s.PutMCPGrant(ctx, MCPGrant{ID: "g", DID: "did:plc:a", SessionID: "s", ClientID: "c", Created: t0, LastUsed: t0, RefreshHash: "h1"}, 10); err != nil {
			t.Fatal(err)
		}
		// The wrong client can't use the token, and that doesn't spend it.
		if _, err := s.RotateMCPRefresh(ctx, "h1", "other", "h2", t0); !errors.Is(err, ErrNotFound) {
			t.Fatalf("rotating as another client: %v", err)
		}
		g, err := s.RotateMCPRefresh(ctx, "h1", "c", "h2", t0.Add(time.Hour))
		if err != nil || g == nil || g.ID != "g" || g.RefreshHash != "h2" || g.PrevRefreshHash != "h1" || g.DID != "did:plc:a" || g.SessionID != "s" {
			t.Fatalf("rotate: %+v %v", g, err)
		}
		wantTime(t, "last used", g.LastUsed, t0.Add(time.Hour))
		if g, _ = s.RotateMCPRefresh(ctx, "h2", "c", "h3", t0); g == nil || g.PrevRefreshHash != "h2" {
			t.Fatalf("second rotation: %+v", g)
		}
		// h1 was spent two tokens ago and is unknown now, but h2 is the
		// previous one: using it again means a copy exists, and ends the
		// grant.
		if _, err := s.RotateMCPRefresh(ctx, "h2", "c", "h4", t0); !errors.Is(err, ErrNotFound) {
			t.Fatalf("reusing a spent token: %v", err)
		}
		if g, _ := s.GetMCPGrant(ctx, "g"); g != nil {
			t.Fatalf("the grant survived a reused token: %+v", g)
		}
		if _, err := s.RotateMCPRefresh(ctx, "h3", "c", "h5", t0); !errors.Is(err, ErrNotFound) {
			t.Fatalf("rotating after the grant ended: %v", err)
		}
	})

	t.Run("mcp refresh token is traded once", func(t *testing.T) {
		t.Parallel()
		s := newStore(t)
		if err := s.PutMCPGrant(ctx, MCPGrant{ID: "g", DID: "did:plc:a", SessionID: "s", ClientID: "c", Created: t0, LastUsed: t0, RefreshHash: "h1"}, 10); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		var mu sync.Mutex
		won := 0
		for i := range 20 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				g, err := s.RotateMCPRefresh(ctx, "h1", "c", "next-"+string(rune('a'+i)), t0)
				if err == nil && g != nil {
					mu.Lock()
					won++
					mu.Unlock()
				}
			}()
		}
		wg.Wait()
		// Of several callers with the same token, on any node, one gets
		// the next one. (A loser that arrives after the winner is a reused
		// token and ends the grant, which is the point.)
		if won != 1 {
			t.Fatalf("%d callers traded the same refresh token", won)
		}
	})

	t.Run("mcp codes", func(t *testing.T) {
		t.Parallel()
		s := newStore(t)
		c := MCPCode{Hash: "code1", ClientID: "c", RedirectURI: "https://claude.ai/cb", Challenge: "chal", DID: "did:plc:a", SessionID: "s", Expires: t0.Add(5 * time.Minute)}
		if ok, err := s.PutMCPCode(ctx, c, t0, 10); err != nil || !ok {
			t.Fatalf("put: %v %v", ok, err)
		}
		got, err := s.TakeMCPCode(ctx, "code1", t0.Add(time.Minute))
		if err != nil || got == nil || got.ClientID != "c" || got.RedirectURI != c.RedirectURI || got.Challenge != "chal" || got.DID != "did:plc:a" || got.SessionID != "s" {
			t.Fatalf("take: %+v %v", got, err)
		}
		wantTime(t, "expires", got.Expires, c.Expires)
		if got, err := s.TakeMCPCode(ctx, "code1", t0.Add(time.Minute)); err != nil || got != nil {
			t.Fatalf("a code was taken twice: %+v %v", got, err)
		}
		if got, err := s.TakeMCPCode(ctx, "never", t0); err != nil || got != nil {
			t.Fatalf("a code out of nowhere: %+v %v", got, err)
		}

		// An expired code is gone, not returned.
		if _, err := s.PutMCPCode(ctx, MCPCode{Hash: "old", Expires: t0.Add(time.Minute)}, t0, 10); err != nil {
			t.Fatal(err)
		}
		if got, err := s.TakeMCPCode(ctx, "old", t0.Add(2*time.Minute)); err != nil || got != nil {
			t.Fatalf("an expired code: %+v %v", got, err)
		}
	})

	t.Run("mcp codes are taken once", func(t *testing.T) {
		t.Parallel()
		s := newStore(t)
		if _, err := s.PutMCPCode(ctx, MCPCode{Hash: "code", ClientID: "c", Expires: t0.Add(time.Hour)}, t0, 10); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		var mu sync.Mutex
		got := 0
		for range 20 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				c, err := s.TakeMCPCode(ctx, "code", t0)
				if err != nil {
					t.Error(err)
					return
				}
				if c != nil {
					mu.Lock()
					got++
					mu.Unlock()
				}
			}()
		}
		wg.Wait()
		if got != 1 {
			t.Fatalf("%d callers took the same code", got)
		}
	})

	t.Run("mcp codes are capped", func(t *testing.T) {
		t.Parallel()
		s := newStore(t)
		put := func(hash string, expires time.Time, now time.Time) bool {
			ok, err := s.PutMCPCode(ctx, MCPCode{Hash: hash, Expires: expires}, now, 2)
			if err != nil {
				t.Fatal(err)
			}
			return ok
		}
		if !put("a", t0.Add(time.Minute), t0) || !put("b", t0.Add(time.Hour), t0) {
			t.Fatal("codes under the cap were refused")
		}
		if put("c", t0.Add(time.Hour), t0) {
			t.Fatal("a code past the cap was accepted")
		}
		// An expired code doesn't count against the cap.
		if !put("c", t0.Add(3*time.Hour), t0.Add(2*time.Minute)) {
			t.Fatal("an expired code still counted")
		}
	})
}
