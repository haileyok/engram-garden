// Package oauthfile keeps OAuth sessions and sign-ins in progress as files,
// for the web app and the engram CLI.
package oauthfile

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/bluesky-social/indigo/atproto/auth/oauth"
	"github.com/bluesky-social/indigo/atproto/syntax"
)

const (
	// AuthRequestTTL is how long a sign-in may take.
	AuthRequestTTL = 30 * time.Minute
	// sessionTTL drops sessions unused this long. Refreshing tokens saves
	// the session, so active sessions stay.
	sessionTTL = 180 * 24 * time.Hour
)

// FileStore keeps OAuth sessions and pending sign-ins as files in a
// directory, one per session. It doesn't coordinate processes: the web app
// is one process, and the agent tools lock a session around each use of it
// (internal/agent).
type FileStore struct {
	Dir string
	Now func() time.Time
}

var _ oauth.ClientAuthStore = (*FileStore)(nil)

var errNotFound = errors.New("not found")

type stored[T any] struct {
	SavedAt time.Time `json:"savedAt"`
	Data    T         `json:"data"`
}

func (s *FileStore) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func name(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil)) + ".json"
}

func (s *FileStore) sessionPath(did syntax.DID, sessionID string) string {
	return filepath.Join(s.Dir, "sessions", name(did.String(), sessionID))
}

func (s *FileStore) requestPath(state string) string {
	return filepath.Join(s.Dir, "requests", name(state))
}

func write(path string, v any, exclusive bool) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if exclusive {
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return err
		}
		if _, err := f.Write(raw); err != nil {
			f.Close()
			return err
		}
		return f.Close()
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func read[T any](path string, ttl time.Duration, now time.Time) (*T, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, errNotFound
	}
	if err != nil {
		return nil, err
	}
	var st stored[T]
	if err := json.Unmarshal(raw, &st); err != nil {
		return nil, err
	}
	if now.Sub(st.SavedAt) > ttl {
		_ = os.Remove(path)
		return nil, errNotFound
	}
	return &st.Data, nil
}

func (s *FileStore) GetSession(_ context.Context, did syntax.DID, sessionID string) (*oauth.ClientSessionData, error) {
	sess, err := read[oauth.ClientSessionData](s.sessionPath(did, sessionID), sessionTTL, s.now())
	if err != nil {
		return nil, fmt.Errorf("session: %w", err)
	}
	return sess, nil
}

func (s *FileStore) SaveSession(_ context.Context, sess oauth.ClientSessionData) error {
	return write(s.sessionPath(sess.AccountDID, sess.SessionID), stored[oauth.ClientSessionData]{SavedAt: s.now(), Data: sess}, false)
}

func (s *FileStore) DeleteSession(_ context.Context, did syntax.DID, sessionID string) error {
	err := os.Remove(s.sessionPath(did, sessionID))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func (s *FileStore) GetAuthRequestInfo(_ context.Context, state string) (*oauth.AuthRequestData, error) {
	info, err := read[oauth.AuthRequestData](s.requestPath(state), AuthRequestTTL, s.now())
	if err != nil {
		return nil, fmt.Errorf("sign-in request: %w", err)
	}
	return info, nil
}

// SaveAuthRequestInfo stores a new sign-in. It never overwrites one.
func (s *FileStore) SaveAuthRequestInfo(_ context.Context, info oauth.AuthRequestData) error {
	return write(s.requestPath(info.State), stored[oauth.AuthRequestData]{SavedAt: s.now(), Data: info}, true)
}

func (s *FileStore) DeleteAuthRequestInfo(_ context.Context, state string) error {
	err := os.Remove(s.requestPath(state))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// Sweep removes expired sign-ins and sessions.
func (s *FileStore) Sweep() {
	now := s.now()
	for sub, ttl := range map[string]time.Duration{"requests": AuthRequestTTL, "sessions": sessionTTL} {
		entries, _ := os.ReadDir(filepath.Join(s.Dir, sub))
		for _, e := range entries {
			p := filepath.Join(s.Dir, sub, e.Name())
			raw, err := os.ReadFile(p)
			if err != nil {
				continue
			}
			var st struct {
				SavedAt time.Time `json:"savedAt"`
			}
			if json.Unmarshal(raw, &st) != nil || now.Sub(st.SavedAt) > ttl {
				_ = os.Remove(p)
			}
		}
	}
}
