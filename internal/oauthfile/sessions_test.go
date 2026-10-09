package oauthfile

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bluesky-social/indigo/atproto/auth/oauth"
)

func TestFileStoreListsSessions(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	clock := now
	s := &FileStore{Dir: t.TempDir(), Now: func() time.Time { return clock }}

	if got, bad, err := s.Sessions(); err != nil || len(got) != 0 || bad != 0 {
		t.Fatalf("an empty store: %v %d %v", got, bad, err)
	}

	// One that's about to expire, then time passes, then two fresh ones.
	clock = now.Add(-SessionTTL + time.Hour)
	if err := s.SaveSession(ctx, oauth.ClientSessionData{AccountDID: "did:plc:old", SessionID: "s0", RefreshToken: "r0"}); err != nil {
		t.Fatal(err)
	}
	clock = now
	for _, id := range []string{"s1", "s2"} {
		if err := s.SaveSession(ctx, oauth.ClientSessionData{AccountDID: "did:plc:alice", SessionID: id, RefreshToken: "r-" + id}); err != nil {
			t.Fatal(err)
		}
	}
	// A file that isn't a session is counted, not fatal.
	if err := os.WriteFile(filepath.Join(s.Dir, "sessions", "junk.json"), []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, bad, err := s.Sessions()
	if err != nil || bad != 1 || len(got) == 0 {
		t.Fatalf("sessions: %v bad=%d %v", got, bad, err)
	}
	byID := map[string]SavedSession{}
	for _, g := range got {
		byID[g.Data.SessionID] = g
	}
	if len(byID) != 3 || byID["s1"].Data.RefreshToken != "r-s1" || byID["s2"].Data.AccountDID != "did:plc:alice" || !byID["s1"].SavedAt.Equal(now) {
		t.Fatalf("sessions: %+v", byID)
	}

	// An hour later the first has expired: it isn't listed.
	clock = now.Add(2 * time.Hour)
	got, _, err = s.Sessions()
	if err != nil || len(got) != 2 {
		t.Fatalf("after the first expired: %v %v", got, err)
	}
}
