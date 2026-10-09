package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/bluesky-social/indigo/atproto/auth/oauth"
	"github.com/bluesky-social/indigo/atproto/syntax"

	"github.com/haileyok/engram-garden/internal/control"
	"github.com/haileyok/engram-garden/internal/oauthfile"
)

// AuthStore keeps the web app's OAuth sessions and sign-ins in progress in
// the control-plane database, so every web node sees them and a restart
// doesn't sign anyone out. Sessions are overwritten on each token refresh.
// They are the web app's own, apart from the appview's.
type AuthStore struct{ DB control.Store }

var _ oauth.ClientAuthStore = AuthStore{}

// GetSession implements oauth.ClientAuthStore.
func (a AuthStore) GetSession(ctx context.Context, did syntax.DID, sessionID string) (*oauth.ClientSessionData, error) {
	row, err := a.DB.GetWebSession(ctx, did.String(), sessionID)
	if err != nil {
		return nil, fmt.Errorf("loading OAuth session: %w", err)
	}
	if time.Since(row.Updated) > oauthfile.SessionTTL {
		_ = a.DB.DeleteWebSession(ctx, did.String(), sessionID)
		return nil, errors.New("OAuth session expired")
	}
	var sess oauth.ClientSessionData
	if err := json.Unmarshal(row.Data, &sess); err != nil {
		return nil, err
	}
	if sess.AccountDID != did || sess.SessionID != sessionID {
		return nil, errors.New("stored OAuth session doesn't match")
	}
	return &sess, nil
}

// SaveSession implements oauth.ClientAuthStore.
func (a AuthStore) SaveSession(ctx context.Context, sess oauth.ClientSessionData) error {
	raw, err := json.Marshal(sess)
	if err != nil {
		return err
	}
	return a.DB.PutWebSession(ctx, control.Session{
		DID: sess.AccountDID.String(), ID: sess.SessionID, Data: raw, Updated: time.Now().UTC(),
	})
}

// DeleteSession implements oauth.ClientAuthStore.
func (a AuthStore) DeleteSession(ctx context.Context, did syntax.DID, sessionID string) error {
	return a.DB.DeleteWebSession(ctx, did.String(), sessionID)
}

// GetAuthRequestInfo implements oauth.ClientAuthStore. Sign-ins older than
// oauthfile.AuthRequestTTL are gone.
func (a AuthStore) GetAuthRequestInfo(ctx context.Context, state string) (*oauth.AuthRequestData, error) {
	row, err := a.DB.GetWebRequest(ctx, state)
	if err != nil {
		return nil, fmt.Errorf("loading sign-in: %w", err)
	}
	var info oauth.AuthRequestData
	if err := json.Unmarshal(row.Data, &info); err != nil {
		return nil, err
	}
	if time.Since(row.Created) > oauthfile.AuthRequestTTL || info.State != state {
		_ = a.DB.DeleteWebRequest(ctx, state)
		return nil, errors.New("sign-in expired")
	}
	return &info, nil
}

// SaveAuthRequestInfo implements oauth.ClientAuthStore.
func (a AuthStore) SaveAuthRequestInfo(ctx context.Context, info oauth.AuthRequestData) error {
	raw, err := json.Marshal(info)
	if err != nil {
		return err
	}
	return a.DB.PutWebRequest(ctx, control.Request{State: info.State, Data: raw, Created: time.Now().UTC()})
}

// DeleteAuthRequestInfo implements oauth.ClientAuthStore.
func (a AuthStore) DeleteAuthRequestInfo(ctx context.Context, state string) error {
	return a.DB.DeleteWebRequest(ctx, state)
}

// Sweep removes expired sign-ins and sessions.
func (a AuthStore) Sweep(ctx context.Context) (int, error) {
	now := time.Now().UTC()
	return a.DB.DeleteStaleWeb(ctx, now.Add(-oauthfile.AuthRequestTTL), now.Add(-oauthfile.SessionTTL))
}

// ImportResult says what ImportFileSessions did.
type ImportResult struct {
	// Imported sessions were copied into the database.
	Imported int
	// Present sessions were already there, so the file's copy was left
	// alone: it would be older, and a refresh token works once.
	Present int
	// Unreadable files were skipped.
	Unreadable int
	// Moved is where the directory went afterwards. It is "" when there
	// was no directory, or when it was left in place because a file
	// couldn't be read.
	Moved string
}

// ImportFileSessions copies the sessions an earlier web app kept as files
// in dir into the database, so nobody is signed out by moving. It never
// replaces a session the database has. Afterwards the directory is renamed
// out of the way (kept, as a backup), so a session that's later signed out
// isn't copied back in by the next start. Sign-ins in progress aren't
// copied: they last half an hour at most, and whoever was mid-way signs in
// again.
func ImportFileSessions(ctx context.Context, db control.Store, dir string, log *slog.Logger) (ImportResult, error) {
	var res ImportResult
	if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
		return res, nil
	}
	sessions, bad, err := (&oauthfile.FileStore{Dir: dir}).Sessions()
	if err != nil {
		return res, err
	}
	res.Unreadable = bad
	if bad > 0 {
		// Leave the directory where it is: something is wrong with the
		// files, and renaming it would hide them.
		log.Warn("some saved web sessions couldn't be read, so the directory is left in place", "dir", dir, "unreadable", bad)
	}
	for _, s := range sessions {
		raw, err := json.Marshal(s.Data)
		if err != nil {
			return res, err
		}
		ok, err := db.ImportWebSession(ctx, control.Session{DID: s.Data.AccountDID.String(), ID: s.Data.SessionID, Data: raw, Updated: s.SavedAt.UTC()})
		if err != nil {
			return res, fmt.Errorf("importing a session: %w", err)
		}
		if ok {
			res.Imported++
		} else {
			res.Present++
		}
	}
	if bad > 0 {
		return res, nil
	}
	moved := filepath.Clean(dir) + ".imported-" + time.Now().UTC().Format("20060102T150405")
	if err := os.Rename(dir, moved); err != nil {
		return res, fmt.Errorf("sessions are copied, but moving %s out of the way failed (they'd be copied again, harmlessly, at the next start): %w", dir, err)
	}
	res.Moved = moved
	return res, nil
}
