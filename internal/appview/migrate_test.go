package appview

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/bluesky-social/indigo/atproto/auth/oauth"

	"github.com/haileyok/engram-garden/internal/blob"
	"github.com/haileyok/engram-garden/internal/control"
	"github.com/haileyok/engram-garden/internal/control/controltest"
)

// legacyObjects writes what an appview that kept its control-plane state in
// the bucket left there.
func legacyObjects(t *testing.T, bs blob.Store) {
	t.Helper()
	put := func(key string, v any) {
		t.Helper()
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		if err := blob.PutBytes(context.Background(), bs, key, raw, false); err != nil {
			t.Fatal(err)
		}
	}
	granted := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	put("grants/aaa.json", map[string]any{"space": "at://did:plc:x/garden.engram.space/a", "did": "did:plc:x", "sessionId": "s1", "grantedAt": granted})
	put("grants/bbb.json", map[string]any{"space": "at://did:plc:y/garden.engram.space/b", "did": "did:plc:y", "sessionId": "s2", "grantedAt": granted})
	put("oauth/sessions/h1/h2.json", oauth.ClientSessionData{AccountDID: "did:plc:x", SessionID: "s1", RefreshToken: "bucket-1", Scopes: []string{"atproto"}})
	put("oauth/sessions/h3/h4.json", oauth.ClientSessionData{AccountDID: "did:plc:y", SessionID: "s2", RefreshToken: "bucket-2"})
	put("registered-spaces/aaa.json", map[string]any{"space": "at://did:plc:x/garden.engram.space/a", "registeredAt": granted})
	put("registered-spaces/bbb.json", map[string]any{"space": "at://did:plc:y/garden.engram.space/b", "registeredAt": granted.Add(time.Hour)})

	// None of these are copied: sign-ins in progress are short-lived, the
	// registry was only ever written, and the index stays in the bucket.
	put("oauth/pending/ppp.json", map[string]any{"state": "p", "space": "at://x", "mode": "grant", "created": granted})
	put("oauth/requests/rrr.json", map[string]any{"info": map[string]any{"state": "r"}, "created": granted})
	put("registry-00000000000000000001-00000000000000000001.json", map[string]any{"format": 1})
	put("spaces/abc/manifest-1.json", map[string]any{"format": 1})
}

func TestMigrateControl(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	bs := blob.Dir{Root: t.TempDir()}
	db := controltest.New(t)
	legacyObjects(t, bs)

	// The database already has a newer copy of one session, because the new
	// appview has been running and refreshing it. The bucket's copy is
	// stale and must not replace it: refresh tokens rotate, so going back
	// to an old one would end the grant.
	newer, _ := json.Marshal(oauth.ClientSessionData{AccountDID: "did:plc:y", SessionID: "s2", RefreshToken: "database-2"})
	if err := db.PutSession(ctx, control.Session{DID: "did:plc:y", ID: "s2", Data: newer, Updated: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}

	res, err := MigrateControl(ctx, bs, db)
	if err != nil {
		t.Fatal(err)
	}
	if want := (MigrateResult{
		Grants:        Moved{Copied: 2},
		Sessions:      Moved{Copied: 1, Present: 1},
		Registrations: Moved{Copied: 2},
	}); res != want {
		t.Fatalf("result %+v, want %+v", res, want)
	}

	g, err := db.GetGrant(ctx, "at://did:plc:x/garden.engram.space/a")
	if err != nil || g == nil || g.DID != "did:plc:x" || g.SessionID != "s1" || !g.GrantedAt.Equal(time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)) {
		t.Fatalf("grant: %+v %v", g, err)
	}
	st := AuthStore{db}
	s1, err := st.GetSession(ctx, "did:plc:x", "s1")
	if err != nil || s1.RefreshToken != "bucket-1" || len(s1.Scopes) != 1 {
		t.Fatalf("copied session: %+v %v", s1, err)
	}
	s2, err := st.GetSession(ctx, "did:plc:y", "s2")
	if err != nil || s2.RefreshToken != "database-2" {
		t.Fatalf("the database's newer session was replaced: %+v %v", s2, err)
	}
	row, _ := db.GetSession(ctx, "did:plc:x", "s1")
	if row.Updated.IsZero() {
		t.Fatal("a copied session has no update time")
	}
	regs, err := db.Registrations(ctx)
	if err != nil || len(regs) != 2 {
		t.Fatalf("registrations: %+v %v", regs, err)
	}
	for _, r := range regs {
		if strings.HasSuffix(r.Space, "/b") && !r.At.Equal(time.Date(2026, 10, 1, 13, 0, 0, 0, time.UTC)) {
			t.Fatalf("registration time %v", r.At)
		}
	}

	// Sign-ins in progress aren't copied.
	if n, err := db.DeleteStale(ctx, time.Now().Add(24*time.Hour)); err != nil || n != 0 {
		t.Fatalf("%d sign-in records were copied, %v", n, err)
	}

	// Running it again changes nothing.
	again, err := MigrateControl(ctx, bs, db)
	if err != nil {
		t.Fatal(err)
	}
	if want := (MigrateResult{
		Grants:        Moved{Present: 2},
		Sessions:      Moved{Present: 2},
		Registrations: Moved{Present: 2},
	}); again != want {
		t.Fatalf("second run %+v, want %+v", again, want)
	}
}

// TestMigrateControlRefusesUnreadableObjects: skipping a grant or session
// quietly would leave a space un-indexed, so a bad one stops the run and
// says which.
func TestMigrateControlRefusesUnreadableObjects(t *testing.T) {
	t.Parallel()
	for _, key := range []string{"grants/bad.json", "oauth/sessions/h/bad.json", "registered-spaces/bad.json"} {
		t.Run(key, func(t *testing.T) {
			t.Parallel()
			bs := blob.Dir{Root: t.TempDir()}
			if err := blob.PutBytes(context.Background(), bs, key, []byte(`{"unrelated": true}`), false); err != nil {
				t.Fatal(err)
			}
			if _, err := MigrateControl(context.Background(), bs, control.NewMemory()); err == nil || !strings.Contains(err.Error(), key) {
				t.Fatalf("error %v, want one naming %s", err, key)
			}
		})
	}
}

// TestMigrateControlEmptyBucket: a fresh deployment has nothing to copy.
func TestMigrateControlEmptyBucket(t *testing.T) {
	t.Parallel()
	res, err := MigrateControl(context.Background(), blob.Dir{Root: t.TempDir()}, control.NewMemory())
	if err != nil || res != (MigrateResult{}) {
		t.Fatalf("%+v %v", res, err)
	}
}
