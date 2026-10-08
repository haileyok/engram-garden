package appview

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/bluesky-social/indigo/atproto/auth/oauth"

	"github.com/haileyok/engram-garden/internal/blob"
	"github.com/haileyok/engram-garden/internal/control"
)

// Where an appview that kept its control-plane state in the bucket put it.
// Only MigrateControl reads these now.
const (
	legacyGrantPrefix        = "grants/"
	legacySessionPrefix      = "oauth/sessions/"
	legacyRegistrationPrefix = "registered-spaces/"
)

// Moved counts what MigrateControl did with one kind of record.
type Moved struct {
	Copied  int // written to the database
	Present int // already in the database, left as it was
}

// MigrateResult says what MigrateControl did.
type MigrateResult struct {
	Grants        Moved
	Sessions      Moved
	Registrations Moved
}

// MigrateControl copies the grants, OAuth sessions and registered spaces
// that an older appview kept in the bucket into the control-plane database.
//
// A record the database already has is left alone, never replaced: once the
// new appview runs it refreshes sessions, and a refresh token is single-use,
// so putting an older copy back would end the grant. Run it with the old
// appview stopped, so the bucket's copies aren't refreshed under it, and
// it's safe to run again.
//
// Sign-ins in progress aren't copied (they last minutes), and neither is the
// registry of indexed spaces, which nothing ever read. Anything that isn't
// readable stops the run, naming the object, rather than being skipped: a
// skipped grant would leave its space unindexed with nothing saying why.
func MigrateControl(ctx context.Context, bs blob.Store, db control.Store) (MigrateResult, error) {
	var res MigrateResult
	var err error
	if res.Grants, err = migrateGrants(ctx, bs, db); err != nil {
		return res, err
	}
	if res.Sessions, err = migrateSessions(ctx, bs, db); err != nil {
		return res, err
	}
	res.Registrations, err = migrateRegistrations(ctx, bs, db)
	return res, err
}

// eachLegacyObject calls each with the JSON objects under a prefix and their contents.
func eachLegacyObject(ctx context.Context, bs blob.Store, prefix string, each func(o blob.Object, raw []byte) error) error {
	objs, err := bs.List(ctx, prefix)
	if err != nil {
		return fmt.Errorf("listing %s: %w", prefix, err)
	}
	for _, o := range objs {
		if !strings.HasSuffix(o.Key, ".json") {
			continue
		}
		raw, err := blob.GetBytes(ctx, bs, o.Key)
		if err != nil {
			return fmt.Errorf("reading %s: %w", o.Key, err)
		}
		if err := each(o, raw); err != nil {
			return fmt.Errorf("%s: %w", o.Key, err)
		}
	}
	return nil
}

func migrateGrants(ctx context.Context, bs blob.Store, db control.Store) (Moved, error) {
	var m Moved
	err := eachLegacyObject(ctx, bs, legacyGrantPrefix, func(o blob.Object, raw []byte) error {
		var g control.Grant
		if err := json.Unmarshal(raw, &g); err != nil {
			return err
		}
		if g.Space == "" || g.DID == "" || g.SessionID == "" {
			return errors.New("not a grant: it has no space, DID or session")
		}
		have, err := db.GetGrant(ctx, g.Space)
		if err != nil {
			return err
		}
		if have != nil {
			m.Present++
			return nil
		}
		if err := db.PutGrant(ctx, g); err != nil {
			return err
		}
		m.Copied++
		return nil
	})
	return m, err
}

func migrateSessions(ctx context.Context, bs blob.Store, db control.Store) (Moved, error) {
	var m Moved
	err := eachLegacyObject(ctx, bs, legacySessionPrefix, func(o blob.Object, raw []byte) error {
		var sess oauth.ClientSessionData
		if err := json.Unmarshal(raw, &sess); err != nil {
			return err
		}
		if sess.AccountDID == "" || sess.SessionID == "" {
			return errors.New("not an OAuth session: it has no account or session ID")
		}
		did, id := sess.AccountDID.String(), sess.SessionID
		switch _, err := db.GetSession(ctx, did, id); {
		case err == nil:
			m.Present++
			return nil
		case !errors.Is(err, control.ErrNotFound):
			return err
		}
		updated := o.Modified
		if updated.IsZero() {
			updated = time.Now()
		}
		if err := db.PutSession(ctx, control.Session{DID: did, ID: id, Data: raw, Updated: updated.UTC()}); err != nil {
			return err
		}
		m.Copied++
		return nil
	})
	return m, err
}

func migrateRegistrations(ctx context.Context, bs blob.Store, db control.Store) (Moved, error) {
	var m Moved
	err := eachLegacyObject(ctx, bs, legacyRegistrationPrefix, func(o blob.Object, raw []byte) error {
		var r struct {
			Space        string    `json:"space"`
			RegisteredAt time.Time `json:"registeredAt"`
		}
		if err := json.Unmarshal(raw, &r); err != nil {
			return err
		}
		if r.Space == "" {
			return errors.New("not a registration: it has no space")
		}
		at := r.RegisteredAt
		if at.IsZero() {
			at = o.Modified
		}
		created, err := db.Register(ctx, control.Registration{Space: r.Space, At: at.UTC()})
		if err != nil {
			return err
		}
		if created {
			m.Copied++
		} else {
			m.Present++
		}
		return nil
	})
	return m, err
}
