package web

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bluesky-social/indigo/atproto/auth/oauth"
	"github.com/bluesky-social/indigo/atproto/syntax"

	"github.com/haileyok/engram-garden/internal/control"
	"github.com/haileyok/engram-garden/internal/control/controltest"
	"github.com/haileyok/engram-garden/internal/oauthfile"
)

func TestAuthStoreKeepsSessionsAndSignIns(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	a := AuthStore{DB: controltest.New(t)}
	did := syntax.DID("did:plc:alice")

	if _, err := a.GetSession(ctx, did, "s1"); err == nil {
		t.Fatal("found a session out of nowhere")
	}
	sess := oauth.ClientSessionData{AccountDID: did, SessionID: "s1", AccessToken: "a", RefreshToken: "r1"}
	if err := a.SaveSession(ctx, sess); err != nil {
		t.Fatal(err)
	}
	sess.RefreshToken = "r2"
	if err := a.SaveSession(ctx, sess); err != nil {
		t.Fatal(err)
	}
	got, err := a.GetSession(ctx, did, "s1")
	if err != nil || got.RefreshToken != "r2" || got.AccessToken != "a" {
		t.Fatalf("session: %+v %v", got, err)
	}
	if err := a.DeleteSession(ctx, did, "s1"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.GetSession(ctx, did, "s1"); err == nil {
		t.Fatal("deleted session still there")
	}

	if err := a.SaveAuthRequestInfo(ctx, oauth.AuthRequestData{State: "st", AuthServerURL: "https://pds.test"}); err != nil {
		t.Fatal(err)
	}
	info, err := a.GetAuthRequestInfo(ctx, "st")
	if err != nil || info.AuthServerURL != "https://pds.test" {
		t.Fatalf("sign-in: %+v %v", info, err)
	}
	if err := a.DeleteAuthRequestInfo(ctx, "st"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.GetAuthRequestInfo(ctx, "st"); err == nil {
		t.Fatal("deleted sign-in still there")
	}

	// A sign-in nobody finished is stale after AuthRequestTTL.
	db := controltest.New(t)
	if err := db.PutWebRequest(ctx, control.Request{State: "old", Data: []byte(`{"state":"old"}`), Created: time.Now().Add(-oauthfile.AuthRequestTTL - time.Minute)}); err != nil {
		t.Fatal(err)
	}
	if _, err := (AuthStore{DB: db}).GetAuthRequestInfo(ctx, "old"); err == nil {
		t.Fatal("an expired sign-in was returned")
	}
	if err := db.PutWebRequest(ctx, control.Request{State: "old2", Data: []byte(`{"state":"old2"}`), Created: time.Now().Add(-oauthfile.AuthRequestTTL - time.Minute)}); err != nil {
		t.Fatal(err)
	}
	if n, err := (AuthStore{DB: db}).Sweep(ctx); err != nil || n != 1 {
		t.Fatalf("sweep: %d %v", n, err)
	}
}

func saveFileSessions(t *testing.T, dir string, ids ...string) {
	t.Helper()
	fs := &oauthfile.FileStore{Dir: dir}
	for _, id := range ids {
		sess := oauth.ClientSessionData{AccountDID: "did:plc:alice", SessionID: id, AccessToken: "access-" + id, RefreshToken: "file-" + id,
			DPoPPrivateKeyMultibase: "key-" + id, HostURL: "https://pds.test"}
		if err := fs.SaveSession(context.Background(), sess); err != nil {
			t.Fatal(err)
		}
	}
}

func TestImportFileSessionsKeepsEveryoneSignedIn(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := controltest.New(t)
	dir := filepath.Join(t.TempDir(), "oauth")
	saveFileSessions(t, dir, "s1", "s2", "s3")

	// The database already has s3, refreshed since the file was written.
	if err := (AuthStore{DB: db}).SaveSession(ctx, oauth.ClientSessionData{AccountDID: "did:plc:alice", SessionID: "s3", RefreshToken: "newer"}); err != nil {
		t.Fatal(err)
	}

	res, err := ImportFileSessions(ctx, db, dir, slog.Default())
	if err != nil || res.Imported != 2 || res.Present != 1 || res.Unreadable != 0 || res.Moved == "" {
		t.Fatalf("import: %+v %v", res, err)
	}

	a := AuthStore{DB: db}
	for _, id := range []string{"s1", "s2"} {
		got, err := a.GetSession(ctx, "did:plc:alice", id)
		if err != nil || got.RefreshToken != "file-"+id || got.AccessToken != "access-"+id || got.DPoPPrivateKeyMultibase != "key-"+id || got.HostURL != "https://pds.test" {
			t.Fatalf("session %s after the import: %+v %v", id, got, err)
		}
	}
	if got, _ := a.GetSession(ctx, "did:plc:alice", "s3"); got == nil || got.RefreshToken != "newer" {
		t.Fatalf("the import replaced a newer session: %+v", got)
	}

	// The files are kept, out of the way.
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("the directory is still in place: %v", err)
	}
	if !strings.HasPrefix(res.Moved, dir+".imported-") {
		t.Fatalf("moved to %q", res.Moved)
	}
	if left, _, _ := (&oauthfile.FileStore{Dir: res.Moved}).Sessions(); len(left) != 3 {
		t.Fatalf("the saved copies: %d", len(left))
	}

	// A start with nothing left to import does nothing, so a session that
	// was signed out meanwhile isn't brought back.
	if err := a.DeleteSession(ctx, "did:plc:alice", "s1"); err != nil {
		t.Fatal(err)
	}
	res, err = ImportFileSessions(ctx, db, dir, slog.Default())
	if err != nil || res != (ImportResult{}) {
		t.Fatalf("second import: %+v %v", res, err)
	}
	if _, err := a.GetSession(ctx, "did:plc:alice", "s1"); err == nil {
		t.Fatal("a signed-out session came back")
	}
}

func TestImportFileSessionsLeavesUnreadableFilesInPlace(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := controltest.New(t)
	dir := filepath.Join(t.TempDir(), "oauth")
	saveFileSessions(t, dir, "s1")
	if err := os.WriteFile(filepath.Join(dir, "sessions", "junk.json"), []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := ImportFileSessions(ctx, db, dir, slog.Default())
	if err != nil || res.Imported != 1 || res.Unreadable != 1 || res.Moved != "" {
		t.Fatalf("import: %+v %v", res, err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("the directory moved even though a file couldn't be read: %v", err)
	}
	if _, err := (AuthStore{DB: db}).GetSession(ctx, "did:plc:alice", "s1"); err != nil {
		t.Fatalf("the readable session wasn't imported: %v", err)
	}
}
