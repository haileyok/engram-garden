package web

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bluesky-social/indigo/atproto/auth/oauth"
	"github.com/bluesky-social/indigo/atproto/syntax"
)

func TestFileStore(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	now := time.Now()
	s := &FileStore{Dir: t.TempDir(), Now: func() time.Time { return now }}
	did := syntax.DID("did:plc:alice")

	if _, err := s.GetSession(ctx, did, "s1"); err == nil {
		t.Fatal("found a missing session")
	}
	sess := oauth.ClientSessionData{AccountDID: did, SessionID: "s1", AccessToken: "a", RefreshToken: "r"}
	if err := s.SaveSession(ctx, sess); err != nil {
		t.Fatal(err)
	}
	sess.AccessToken = "a2"
	if err := s.SaveSession(ctx, sess); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetSession(ctx, did, "s1")
	if err != nil || got.AccessToken != "a2" || got.RefreshToken != "r" {
		t.Fatalf("session: %+v %v", got, err)
	}
	st, err := os.Stat(s.sessionPath(did, "s1"))
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("session file mode: %v %v", st.Mode(), err)
	}
	if err := s.DeleteSession(ctx, did, "s1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetSession(ctx, did, "s1"); err == nil {
		t.Fatal("deleted session still found")
	}

	req := oauth.AuthRequestData{State: "st", PKCEVerifier: "v"}
	if err := s.SaveAuthRequestInfo(ctx, req); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveAuthRequestInfo(ctx, req); err == nil {
		t.Fatal("overwrote a sign-in request")
	}
	if got, err := s.GetAuthRequestInfo(ctx, "st"); err != nil || got.PKCEVerifier != "v" {
		t.Fatalf("request: %+v %v", got, err)
	}

	// Old sign-ins expire.
	now = now.Add(authRequestTTL + time.Minute)
	if _, err := s.GetAuthRequestInfo(ctx, "st"); err == nil {
		t.Fatal("expired request still found")
	}
	if err := s.SaveAuthRequestInfo(ctx, oauth.AuthRequestData{State: "old"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveSession(ctx, oauth.ClientSessionData{AccountDID: did, SessionID: "s2"}); err != nil {
		t.Fatal(err)
	}
	now = now.Add(sessionTTL + time.Hour)
	s.Sweep()
	for _, sub := range []string{"requests", "sessions"} {
		if entries, _ := os.ReadDir(filepath.Join(s.Dir, sub)); len(entries) != 0 {
			t.Fatalf("%s left after sweep: %d", sub, len(entries))
		}
	}
}
