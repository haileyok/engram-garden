package control

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sort"
	"sync"
	"testing"
	"time"
)

// t0 is a fixed time with microsecond precision, the most a database keeps.
var t0 = time.Date(2026, 10, 8, 5, 0, 0, 123456000, time.UTC)

func sameJSON(t *testing.T, got, want string) {
	t.Helper()
	var g, w any
	if err := json.Unmarshal([]byte(got), &g); err != nil {
		t.Fatalf("stored data isn't JSON: %q: %v", got, err)
	}
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(g, w) {
		t.Fatalf("data %s, want %s", got, want)
	}
}

func wantTime(t *testing.T, what string, got, want time.Time) {
	t.Helper()
	if !got.Equal(want) {
		t.Fatalf("%s: %v, want %v", what, got, want)
	}
}

// testStore runs the contract every Store must meet. newStore returns an
// empty store; each case gets its own.
func testStore(t *testing.T, newStore func(t *testing.T) Store) {
	ctx := context.Background()

	t.Run("grants", func(t *testing.T) {
		t.Parallel()
		s := newStore(t)
		if g, err := s.GetGrant(ctx, "at://a"); err != nil || g != nil {
			t.Fatalf("a grant out of nowhere: %+v %v", g, err)
		}
		if err := s.PutGrant(ctx, Grant{Space: "at://a", DID: "did:plc:x", SessionID: "s1", GrantedAt: t0}); err != nil {
			t.Fatal(err)
		}
		if err := s.PutGrant(ctx, Grant{Space: "at://b", DID: "did:plc:y", SessionID: "s2", GrantedAt: t0}); err != nil {
			t.Fatal(err)
		}
		g, err := s.GetGrant(ctx, "at://a")
		if err != nil || g == nil || g.Space != "at://a" || g.DID != "did:plc:x" || g.SessionID != "s1" {
			t.Fatalf("grant: %+v %v", g, err)
		}
		wantTime(t, "granted at", g.GrantedAt, t0)

		// A new grant replaces the old one.
		if err := s.PutGrant(ctx, Grant{Space: "at://a", DID: "did:plc:x", SessionID: "s3", GrantedAt: t0.Add(time.Hour)}); err != nil {
			t.Fatal(err)
		}
		if g, _ = s.GetGrant(ctx, "at://a"); g == nil || g.SessionID != "s3" {
			t.Fatalf("grant wasn't replaced: %+v", g)
		}
		wantTime(t, "granted at after replacing", g.GrantedAt, t0.Add(time.Hour))

		all, err := s.ListGrants(ctx)
		if err != nil || len(all) != 2 {
			t.Fatalf("grants: %+v %v", all, err)
		}
		sort.Slice(all, func(i, j int) bool { return all[i].Space < all[j].Space })
		if all[0].Space != "at://a" || all[0].SessionID != "s3" || all[1].Space != "at://b" || all[1].DID != "did:plc:y" {
			t.Fatalf("grants: %+v", all)
		}

		if err := s.DeleteGrant(ctx, "at://a"); err != nil {
			t.Fatal(err)
		}
		if err := s.DeleteGrant(ctx, "at://a"); err != nil {
			t.Fatalf("deleting a missing grant: %v", err)
		}
		if g, err := s.GetGrant(ctx, "at://a"); err != nil || g != nil {
			t.Fatalf("deleted grant still there: %+v %v", g, err)
		}
		if g, _ := s.GetGrant(ctx, "at://b"); g == nil {
			t.Fatal("deleting one grant removed another")
		}
	})

	t.Run("sessions", func(t *testing.T) {
		t.Parallel()
		s := newStore(t)
		if _, err := s.GetSession(ctx, "did:plc:x", "s1"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("a session out of nowhere: %v", err)
		}
		put := func(did, id, data string, at time.Time) {
			t.Helper()
			if err := s.PutSession(ctx, Session{DID: did, ID: id, Data: []byte(data), Updated: at}); err != nil {
				t.Fatal(err)
			}
		}
		put("did:plc:x", "s1", `{"refresh":"r1"}`, t0)
		put("did:plc:x", "s2", `{"refresh":"other"}`, t0)
		put("did:plc:y", "s1", `{"refresh":"another account"}`, t0)

		got, err := s.GetSession(ctx, "did:plc:x", "s1")
		if err != nil || got.DID != "did:plc:x" || got.ID != "s1" {
			t.Fatalf("session: %+v %v", got, err)
		}
		sameJSON(t, string(got.Data), `{"refresh":"r1"}`)
		wantTime(t, "updated", got.Updated, t0)

		// A token refresh overwrites it.
		put("did:plc:x", "s1", `{"refresh":"r2"}`, t0.Add(8*time.Minute))
		got, err = s.GetSession(ctx, "did:plc:x", "s1")
		if err != nil {
			t.Fatal(err)
		}
		sameJSON(t, string(got.Data), `{"refresh":"r2"}`)
		wantTime(t, "updated after refresh", got.Updated, t0.Add(8*time.Minute))

		if _, err := s.GetSession(ctx, "did:plc:z", "s1"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("found a session under another DID: %v", err)
		}
		if _, err := s.GetSession(ctx, "did:plc:x", "s9"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("found a session under another ID: %v", err)
		}

		all, err := s.ListSessions(ctx)
		if err != nil || len(all) != 3 {
			t.Fatalf("sessions: %+v %v", all, err)
		}

		if err := s.DeleteSession(ctx, "did:plc:x", "s1"); err != nil {
			t.Fatal(err)
		}
		if err := s.DeleteSession(ctx, "did:plc:x", "s1"); err != nil {
			t.Fatalf("deleting a missing session: %v", err)
		}
		if _, err := s.GetSession(ctx, "did:plc:x", "s1"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("deleted session still there: %v", err)
		}
		if _, err := s.GetSession(ctx, "did:plc:y", "s1"); err != nil {
			t.Fatalf("deleting one session removed another: %v", err)
		}
	})

	t.Run("requests", func(t *testing.T) {
		t.Parallel()
		s := newStore(t)
		if _, err := s.GetRequest(ctx, "st"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("a sign-in out of nowhere: %v", err)
		}
		if err := s.PutRequest(ctx, Request{State: "st", Data: []byte(`{"state":"st","verifier":"v1"}`), Created: t0}); err != nil {
			t.Fatal(err)
		}
		got, err := s.GetRequest(ctx, "st")
		if err != nil || got.State != "st" {
			t.Fatalf("sign-in: %+v %v", got, err)
		}
		sameJSON(t, string(got.Data), `{"state":"st","verifier":"v1"}`)
		wantTime(t, "created", got.Created, t0)

		if err := s.PutRequest(ctx, Request{State: "st", Data: []byte(`{"state":"st","verifier":"v2"}`), Created: t0.Add(time.Minute)}); err != nil {
			t.Fatal(err)
		}
		got, _ = s.GetRequest(ctx, "st")
		sameJSON(t, string(got.Data), `{"state":"st","verifier":"v2"}`)

		if err := s.DeleteRequest(ctx, "st"); err != nil {
			t.Fatal(err)
		}
		if err := s.DeleteRequest(ctx, "st"); err != nil {
			t.Fatalf("deleting a missing sign-in: %v", err)
		}
		if _, err := s.GetRequest(ctx, "st"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("deleted sign-in still there: %v", err)
		}
	})

	t.Run("pending", func(t *testing.T) {
		t.Parallel()
		s := newStore(t)
		if p, err := s.TakePending(ctx, "st"); err != nil || p != nil {
			t.Fatalf("a pending sign-in out of nowhere: %+v %v", p, err)
		}
		want := Pending{State: "st", Space: "at://a", Mode: "grant", Return: "https://engram.test/space?uri=x", Created: t0}
		if err := s.PutPending(ctx, want); err != nil {
			t.Fatal(err)
		}
		// A sign-in's request and its pending record are separate things
		// under the same state.
		if err := s.PutRequest(ctx, Request{State: "st", Data: []byte(`{}`), Created: t0}); err != nil {
			t.Fatal(err)
		}
		got, err := s.TakePending(ctx, "st")
		if err != nil || got == nil {
			t.Fatalf("take: %+v %v", got, err)
		}
		if got.State != "st" || got.Space != want.Space || got.Mode != want.Mode || got.Return != want.Return {
			t.Fatalf("pending: %+v", got)
		}
		wantTime(t, "created", got.Created, t0)
		if p, err := s.TakePending(ctx, "st"); err != nil || p != nil {
			t.Fatalf("taken twice: %+v %v", p, err)
		}
		if _, err := s.GetRequest(ctx, "st"); err != nil {
			t.Fatalf("taking the pending record removed the request: %v", err)
		}

		// With no return URL.
		if err := s.PutPending(ctx, Pending{State: "st2", Space: "at://a", Mode: "stop", Created: t0}); err != nil {
			t.Fatal(err)
		}
		if got, _ = s.TakePending(ctx, "st2"); got == nil || got.Return != "" || got.Mode != "stop" {
			t.Fatalf("pending without a return URL: %+v", got)
		}
	})

	t.Run("pending is taken once", func(t *testing.T) {
		t.Parallel()
		s := newStore(t)
		if err := s.PutPending(ctx, Pending{State: "st", Space: "at://a", Mode: "grant", Created: t0}); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		var mu sync.Mutex
		won := 0
		for range 20 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				p, err := s.TakePending(ctx, "st")
				if err != nil {
					t.Error(err)
					return
				}
				if p != nil {
					mu.Lock()
					won++
					mu.Unlock()
				}
			}()
		}
		wg.Wait()
		if won != 1 {
			t.Fatalf("%d callers took the same pending sign-in", won)
		}
	})

	t.Run("stale sign-ins", func(t *testing.T) {
		t.Parallel()
		s := newStore(t)
		cutoff := t0.Add(10 * time.Minute)
		must := func(err error) {
			t.Helper()
			if err != nil {
				t.Fatal(err)
			}
		}
		must(s.PutRequest(ctx, Request{State: "old-r", Data: []byte(`{}`), Created: t0}))
		must(s.PutPending(ctx, Pending{State: "old-p", Space: "at://a", Mode: "grant", Created: t0}))
		must(s.PutRequest(ctx, Request{State: "edge-r", Data: []byte(`{}`), Created: cutoff}))
		must(s.PutRequest(ctx, Request{State: "new-r", Data: []byte(`{}`), Created: t0.Add(20 * time.Minute)}))
		must(s.PutPending(ctx, Pending{State: "new-p", Space: "at://a", Mode: "grant", Created: t0.Add(20 * time.Minute)}))
		// Sessions, grants and registrations are never stale.
		must(s.PutSession(ctx, Session{DID: "did:plc:x", ID: "s1", Data: []byte(`{}`), Updated: t0}))
		must(s.PutGrant(ctx, Grant{Space: "at://a", DID: "did:plc:x", SessionID: "s1", GrantedAt: t0}))
		_, err := s.Register(ctx, Registration{Space: "at://a", At: t0})
		must(err)

		n, err := s.DeleteStale(ctx, cutoff)
		if err != nil || n != 2 {
			t.Fatalf("removed %d, %v; want the 2 old ones", n, err)
		}
		if _, err := s.GetRequest(ctx, "old-r"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("old request left: %v", err)
		}
		if p, _ := s.TakePending(ctx, "old-p"); p != nil {
			t.Fatal("old pending left")
		}
		for _, st := range []string{"edge-r", "new-r"} {
			if _, err := s.GetRequest(ctx, st); err != nil {
				t.Fatalf("removed %s, which isn't older than the cutoff: %v", st, err)
			}
		}
		if p, _ := s.TakePending(ctx, "new-p"); p == nil {
			t.Fatal("removed a recent pending sign-in")
		}
		if n, err := s.DeleteStale(ctx, cutoff); err != nil || n != 0 {
			t.Fatalf("second pass removed %d, %v", n, err)
		}
		if _, err := s.GetSession(ctx, "did:plc:x", "s1"); err != nil {
			t.Fatalf("removed a session: %v", err)
		}
		if g, _ := s.GetGrant(ctx, "at://a"); g == nil {
			t.Fatal("removed a grant")
		}
		if regs, _ := s.Registrations(ctx); len(regs) != 1 {
			t.Fatalf("removed a registration: %+v", regs)
		}
	})

	t.Run("registrations", func(t *testing.T) {
		t.Parallel()
		s := newStore(t)
		if regs, err := s.Registrations(ctx); err != nil || len(regs) != 0 {
			t.Fatalf("registrations out of nowhere: %+v %v", regs, err)
		}
		if created, err := s.Register(ctx, Registration{Space: "at://a", At: t0}); err != nil || !created {
			t.Fatalf("first registration: %v %v", created, err)
		}
		// Registering again keeps the first time.
		if created, err := s.Register(ctx, Registration{Space: "at://a", At: t0.Add(time.Hour)}); err != nil || created {
			t.Fatalf("second registration: %v %v", created, err)
		}
		if created, err := s.Register(ctx, Registration{Space: "at://b", At: t0.Add(time.Minute)}); err != nil || !created {
			t.Fatalf("another space: %v %v", created, err)
		}
		regs, err := s.Registrations(ctx)
		if err != nil || len(regs) != 2 {
			t.Fatalf("registrations: %+v %v", regs, err)
		}
		sort.Slice(regs, func(i, j int) bool { return regs[i].Space < regs[j].Space })
		if regs[0].Space != "at://a" || regs[1].Space != "at://b" {
			t.Fatalf("registrations: %+v", regs)
		}
		wantTime(t, "first registration time", regs[0].At, t0)
		wantTime(t, "other space's time", regs[1].At, t0.Add(time.Minute))
	})

	t.Run("registered once", func(t *testing.T) {
		t.Parallel()
		s := newStore(t)
		var wg sync.WaitGroup
		var mu sync.Mutex
		created := 0
		for range 20 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				ok, err := s.Register(ctx, Registration{Space: "at://a", At: t0})
				if err != nil {
					t.Error(err)
					return
				}
				if ok {
					mu.Lock()
					created++
					mu.Unlock()
				}
			}()
		}
		wg.Wait()
		if created != 1 {
			t.Fatalf("%d callers registered the same space", created)
		}
	})

	testMCP(t, newStore)
}

func TestMemory(t *testing.T) {
	t.Parallel()
	testStore(t, func(t *testing.T) Store { return NewMemory() })
}
