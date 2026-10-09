package control

import (
	"context"
	"errors"
	"testing"
	"time"
)

// testWeb is the part of the Store contract for the web app's own OAuth
// sessions and sign-ins. They're kept apart from the appview's: the
// appview's sweep deletes the sessions it lists that none of its grants
// use, so the web app's must never be among them.
func testWeb(t *testing.T, newStore func(t *testing.T) Store) {
	ctx := context.Background()

	t.Run("web sessions", func(t *testing.T) {
		t.Parallel()
		s := newStore(t)
		if _, err := s.GetWebSession(ctx, "did:plc:x", "s1"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("a session out of nowhere: %v", err)
		}
		put := func(did, id, data string, at time.Time) {
			t.Helper()
			if err := s.PutWebSession(ctx, Session{DID: did, ID: id, Data: []byte(data), Updated: at}); err != nil {
				t.Fatal(err)
			}
		}
		put("did:plc:x", "s1", `{"refresh":"r1"}`, t0)
		put("did:plc:x", "s2", `{"refresh":"other"}`, t0)
		got, err := s.GetWebSession(ctx, "did:plc:x", "s1")
		if err != nil || got.DID != "did:plc:x" || got.ID != "s1" {
			t.Fatalf("session: %+v %v", got, err)
		}
		sameJSON(t, string(got.Data), `{"refresh":"r1"}`)
		wantTime(t, "updated", got.Updated, t0)

		// A token refresh overwrites it.
		put("did:plc:x", "s1", `{"refresh":"r2"}`, t0.Add(8*time.Minute))
		got, _ = s.GetWebSession(ctx, "did:plc:x", "s1")
		sameJSON(t, string(got.Data), `{"refresh":"r2"}`)
		wantTime(t, "updated after refresh", got.Updated, t0.Add(8*time.Minute))

		if _, err := s.GetWebSession(ctx, "did:plc:y", "s1"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("found a session under another DID: %v", err)
		}
		if err := s.DeleteWebSession(ctx, "did:plc:x", "s1"); err != nil {
			t.Fatal(err)
		}
		if err := s.DeleteWebSession(ctx, "did:plc:x", "s1"); err != nil {
			t.Fatalf("deleting a missing session: %v", err)
		}
		if _, err := s.GetWebSession(ctx, "did:plc:x", "s1"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("deleted session still there: %v", err)
		}
		if _, err := s.GetWebSession(ctx, "did:plc:x", "s2"); err != nil {
			t.Fatalf("deleting one session removed another: %v", err)
		}
	})

	t.Run("web sessions are imported once", func(t *testing.T) {
		t.Parallel()
		s := newStore(t)
		ok, err := s.ImportWebSession(ctx, Session{DID: "did:plc:x", ID: "s1", Data: []byte(`{"refresh":"from a file"}`), Updated: t0})
		if err != nil || !ok {
			t.Fatalf("import: %v %v", ok, err)
		}
		// A refresh token is used once, so what the database has is newer
		// than any copy: an import never replaces it.
		if err := s.PutWebSession(ctx, Session{DID: "did:plc:x", ID: "s1", Data: []byte(`{"refresh":"newer"}`), Updated: t0.Add(time.Hour)}); err != nil {
			t.Fatal(err)
		}
		ok, err = s.ImportWebSession(ctx, Session{DID: "did:plc:x", ID: "s1", Data: []byte(`{"refresh":"from a file"}`), Updated: t0})
		if err != nil || ok {
			t.Fatalf("second import: %v %v", ok, err)
		}
		got, _ := s.GetWebSession(ctx, "did:plc:x", "s1")
		sameJSON(t, string(got.Data), `{"refresh":"newer"}`)
	})

	t.Run("web sessions are apart from the appview's", func(t *testing.T) {
		t.Parallel()
		s := newStore(t)
		if err := s.PutWebSession(ctx, Session{DID: "did:plc:x", ID: "s1", Data: []byte(`{"web":true}`), Updated: t0}); err != nil {
			t.Fatal(err)
		}
		if err := s.PutSession(ctx, Session{DID: "did:plc:x", ID: "s1", Data: []byte(`{"appview":true}`), Updated: t0}); err != nil {
			t.Fatal(err)
		}
		web, _ := s.GetWebSession(ctx, "did:plc:x", "s1")
		app, _ := s.GetSession(ctx, "did:plc:x", "s1")
		sameJSON(t, string(web.Data), `{"web":true}`)
		sameJSON(t, string(app.Data), `{"appview":true}`)
		// The appview's sweep lists its sessions: the web app's aren't there.
		all, err := s.ListSessions(ctx)
		if err != nil || len(all) != 1 {
			t.Fatalf("the appview's sessions: %+v %v", all, err)
		}
		if err := s.DeleteSession(ctx, "did:plc:x", "s1"); err != nil {
			t.Fatal(err)
		}
		if _, err := s.GetWebSession(ctx, "did:plc:x", "s1"); err != nil {
			t.Fatalf("deleting the appview's session removed the web app's: %v", err)
		}
	})

	t.Run("web sign-ins", func(t *testing.T) {
		t.Parallel()
		s := newStore(t)
		if _, err := s.GetWebRequest(ctx, "st"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("a sign-in out of nowhere: %v", err)
		}
		if err := s.PutWebRequest(ctx, Request{State: "st", Data: []byte(`{"state":"st","verifier":"v1"}`), Created: t0}); err != nil {
			t.Fatal(err)
		}
		got, err := s.GetWebRequest(ctx, "st")
		if err != nil || got.State != "st" {
			t.Fatalf("sign-in: %+v %v", got, err)
		}
		sameJSON(t, string(got.Data), `{"state":"st","verifier":"v1"}`)
		wantTime(t, "created", got.Created, t0)
		// The appview's sign-ins are a different set.
		if _, err := s.GetRequest(ctx, "st"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("the appview sees the web app's sign-in: %v", err)
		}
		if err := s.DeleteWebRequest(ctx, "st"); err != nil {
			t.Fatal(err)
		}
		if err := s.DeleteWebRequest(ctx, "st"); err != nil {
			t.Fatalf("deleting a missing sign-in: %v", err)
		}
		if _, err := s.GetWebRequest(ctx, "st"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("deleted sign-in still there: %v", err)
		}
	})

	t.Run("stale web state", func(t *testing.T) {
		t.Parallel()
		s := newStore(t)
		must := func(err error) {
			t.Helper()
			if err != nil {
				t.Fatal(err)
			}
		}
		must(s.PutWebRequest(ctx, Request{State: "old", Data: []byte(`{}`), Created: t0}))
		must(s.PutWebRequest(ctx, Request{State: "new", Data: []byte(`{}`), Created: t0.Add(time.Hour)}))
		must(s.PutWebSession(ctx, Session{DID: "did:plc:x", ID: "old", Data: []byte(`{}`), Updated: t0}))
		must(s.PutWebSession(ctx, Session{DID: "did:plc:x", ID: "new", Data: []byte(`{}`), Updated: t0.Add(2 * time.Hour)}))
		// An appview session this old isn't the web app's to delete.
		must(s.PutSession(ctx, Session{DID: "did:plc:x", ID: "app", Data: []byte(`{}`), Updated: t0}))

		n, err := s.DeleteStaleWeb(ctx, t0.Add(30*time.Minute), t0.Add(time.Hour))
		if err != nil || n != 2 {
			t.Fatalf("deleted %d, %v", n, err)
		}
		if _, err := s.GetWebRequest(ctx, "old"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("a stale sign-in survived: %v", err)
		}
		if _, err := s.GetWebSession(ctx, "did:plc:x", "old"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("a stale session survived: %v", err)
		}
		if _, err := s.GetWebRequest(ctx, "new"); err != nil {
			t.Fatalf("a fresh sign-in was deleted: %v", err)
		}
		if _, err := s.GetWebSession(ctx, "did:plc:x", "new"); err != nil {
			t.Fatalf("a fresh session was deleted: %v", err)
		}
		if _, err := s.GetSession(ctx, "did:plc:x", "app"); err != nil {
			t.Fatalf("an appview session was deleted: %v", err)
		}
	})
}
