package appview

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/bluesky-social/indigo/atproto/syntax"
	"github.com/haileyok/cocoon/oauth/scopes"
	"github.com/haileyok/cocoon/space"

	"github.com/haileyok/engram-garden/internal/blob"
	"github.com/haileyok/engram-garden/internal/spaceclient"
)

// grantCookie ties a grant's callback to the browser that started it, so
// nobody can send someone a callback link that acts with a sign-in they
// didn't start.
const grantCookie = "engram_grant"

// The browser side of granting and stopping:
//
//	GET /oauth/grant?space=<uri>&mode=grant|stop&return=<url>
//	  -> the space authority's authorization server
//	GET /oauth/callback?state=…
//	  -> back to return, with indexing=granted|stopped or indexing_error=…,
//	     or a short page when there's no return URL.

func (s *Server) grantsReady() bool { return s.Grants != nil && s.Grants.Auth != nil }

// GrantURL is the page that starts a grant, or empty when this service
// can't take grants.
func (s *Server) GrantURL() string {
	if !s.grantsReady() || s.PublicURL == "" {
		return ""
	}
	return strings.TrimSuffix(s.PublicURL, "/") + "/oauth/grant"
}

func (s *Server) handleGrant(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	ret := q.Get("return")
	if ret != "" && !s.allowedReturn(ret) {
		s.page(w, http.StatusBadRequest, "Can't go back there", "This appview won't return to that address after signing in.")
		return
	}
	fail := func(status int, msg string) { s.finish(w, r, ret, status, "", msg) }
	if !s.grantsReady() {
		fail(http.StatusNotFound, "This appview isn't set up to take grants.")
		return
	}
	mode := q.Get("mode")
	if mode == "" {
		mode = modeGrant
	}
	if mode != modeGrant && mode != modeStop {
		fail(http.StatusBadRequest, "mode must be grant or stop.")
		return
	}
	raw := q.Get("space")
	ref, err := space.ParseRef(raw)
	if err != nil || ref.String() != raw {
		fail(http.StatusBadRequest, "That isn't a space URI.")
		return
	}
	if mode == modeGrant && !s.OpenRegistration && !s.indexes(raw) {
		fail(http.StatusForbidden, "This appview indexes only the spaces its operator configures.")
		return
	}
	redirect, state, err := s.Grants.Auth.Start(r.Context(), syntax.DID(ref.Authority), mode)
	if err != nil {
		s.log().Info("couldn't start a grant", "space", raw, "mode", mode, "err", err)
		fail(http.StatusBadGateway, "Couldn't start signing in to the space authority's account ("+ref.Authority+").")
		return
	}
	if err := s.Grants.savePending(r.Context(), pending{State: state, Space: raw, Mode: mode, Return: ret, Created: time.Now().UTC()}); err != nil {
		s.log().Error("saving a grant in progress failed", "err", err)
		fail(http.StatusInternalServerError, "Couldn't start signing in. Try again.")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: grantCookie, Value: s.mac("grant." + state), Path: "/oauth/callback",
		HttpOnly: true, Secure: strings.HasPrefix(s.PublicURL, "https://"), SameSite: http.SameSiteLaxMode,
		MaxAge: int(pendingTTL.Seconds()),
	})
	http.Redirect(w, r, redirect, http.StatusSeeOther)
}

func (s *Server) handleCallback(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: grantCookie, Value: "", Path: "/oauth/callback", MaxAge: -1, HttpOnly: true,
		Secure: strings.HasPrefix(s.PublicURL, "https://"), SameSite: http.SameSiteLaxMode})
	if !s.grantsReady() {
		s.page(w, http.StatusNotFound, "Not set up", "This appview isn't set up to take grants.")
		return
	}
	q := r.URL.Query()
	state := q.Get("state")
	if !s.startedHere(r, state) {
		s.log().Info("refused a grant callback this browser didn't start")
		s.page(w, http.StatusBadRequest, "Start again", "This sign-in wasn't started from this browser. Start again from the web app.")
		return
	}
	ctx := r.Context()
	p, err := s.Grants.takePending(ctx, state)
	if err != nil {
		s.writePageErr(w, err)
		return
	}
	if p == nil {
		s.page(w, http.StatusBadRequest, "Start again", "This sign-in expired or was already used. Start again from the web app.")
		return
	}
	res, err := s.Grants.Auth.Finish(ctx, p.Mode, q)
	if errors.Is(err, ErrDeclined) {
		s.finish(w, r, p.Return, http.StatusBadRequest, "", "You declined, so nothing changed.")
		return
	}
	if err != nil {
		s.log().Info("grant callback failed", "space", p.Space, "err", err)
		s.finish(w, r, p.Return, http.StatusBadGateway, "", "Signing in didn't work. Try again.")
		return
	}
	ref, _ := space.ParseRef(p.Space)
	if res.DID.String() != ref.Authority {
		s.revoke(ctx, p.Mode, res.DID, res.SessionID)
		s.finish(w, r, p.Return, http.StatusForbidden, "",
			fmt.Sprintf("Only the space's authority (%s) can do this, and you signed in as %s.", ref.Authority, res.DID))
		return
	}
	if p.Mode == modeStop {
		s.completeStop(w, r, p, res)
		return
	}
	s.completeGrant(w, r, p, res)
}

func (s *Server) completeGrant(w http.ResponseWriter, r *http.Request, p *pending, res *AuthResult) {
	ctx := r.Context()
	discard := func() { s.revoke(ctx, modeGrant, res.DID, res.SessionID) }
	if !readsSpace(res.Scopes, res.DID.String(), p.Space) {
		s.log().Info("a grant came back without read access", "space", p.Space, "scopes", res.Scopes)
		discard()
		s.finish(w, r, p.Return, http.StatusForbidden, "", "Your account's server didn't give read access to your memory spaces, so the appview can't index this one.")
		return
	}
	// Check the new grant really reads the space before it replaces
	// anything.
	if err := s.checkGrant(ctx, p.Space, res); err != nil {
		s.log().Info("a grant couldn't read its space", "space", p.Space, "err", err)
		discard()
		s.finish(w, r, p.Return, http.StatusForbidden, "", "The appview couldn't read the space with your grant. Try again.")
		return
	}
	old, err := s.Grants.Get(ctx, p.Space)
	if err != nil {
		discard()
		s.writePageErr(w, err)
		return
	}
	g := Grant{Space: p.Space, DID: res.DID.String(), SessionID: res.SessionID, GrantedAt: time.Now().UTC()}
	if err := s.Grants.Put(ctx, g); err != nil {
		discard()
		s.writePageErr(w, err)
		return
	}
	s.Indexer.Client.Invalidate(p.Space)
	if old != nil && old.SessionID != g.SessionID {
		s.revoke(ctx, modeGrant, syntax.DID(old.DID), old.SessionID)
	}
	if err := s.register(ctx, p.Space); err != nil {
		s.writePageErr(w, err)
		return
	}
	s.log().Info("space granted", "space", p.Space, "by", g.DID)
	s.finish(w, r, p.Return, http.StatusOK, "granted", "")
}

func (s *Server) completeStop(w http.ResponseWriter, r *http.Request, p *pending, res *AuthResult) {
	ctx := r.Context()
	// The stop sign-in only proved who's asking.
	defer s.revoke(ctx, modeStop, res.DID, res.SessionID)
	g, err := s.Grants.Get(ctx, p.Space)
	if err != nil {
		s.writePageErr(w, err)
		return
	}
	if g != nil {
		if err := s.Grants.Delete(ctx, p.Space); err != nil {
			s.writePageErr(w, err)
			return
		}
		s.revoke(ctx, modeGrant, syntax.DID(g.DID), g.SessionID)
	}
	s.Indexer.Client.Invalidate(p.Space)
	s.log().Info("space indexing stopped", "space", p.Space, "by", res.DID)
	s.finish(w, r, p.Return, http.StatusOK, "stopped", "")
}

// register records the space as indexed and starts syncing it when this
// node owns it. Other nodes pick it up at their next poll.
func (s *Server) register(ctx context.Context, spaceURI string) error {
	if s.Blob != nil {
		raw, _ := json.Marshal(registration{Space: spaceURI, RegisteredAt: time.Now().UTC()})
		if err := blob.PutBytes(ctx, s.Blob, registrationKey(spaceURI), raw, true); err != nil && !errors.Is(err, blob.ErrExists) {
			return err
		}
	}
	s.addRegistered(spaceURI)
	if !s.ring().Owns(spaceURI) {
		return nil
	}
	// Have the background loop register for notifications, and index the
	// space now rather than at the next poll.
	select {
	case s.wake() <- struct{}{}:
	default:
	}
	s.Jobs.Add(1)
	go func() {
		defer s.Jobs.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		// Share the node's sync slots, so grants can't start unbounded
		// work. The background loop catches up if this gives up.
		releaseNode, err := s.acquireNodeSlot(ctx)
		if err != nil {
			return
		}
		defer releaseNode()
		s.syncSpaceOnce(ctx, spaceURI)
	}()
	return nil
}

// checkGrant gets a credential for the space with a new grant's session.
func (s *Server) checkGrant(ctx context.Context, spaceURI string, res *AuthResult) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	api, err := s.Grants.Auth.Resume(ctx, res.DID, res.SessionID)
	if err != nil {
		return err
	}
	c, err := spaceclient.NewDelegated(spaceclient.SessionDelegator{Session: api}, s.Indexer.Client.Dir, s.Indexer.Client.HTTP)
	if err != nil {
		return err
	}
	_, err = c.Credential(ctx, spaceURI)
	return err
}

func (s *Server) revoke(ctx context.Context, mode string, did syntax.DID, sessionID string) {
	if err := s.Grants.Auth.Revoke(ctx, mode, did, sessionID); err != nil {
		s.log().Warn("revoking a sign-in failed", "did", did, "err", err)
	}
}

// readsSpace reports whether granted OAuth scopes let did read the space.
// Authorization servers rewrite scopes when they issue a token (Cocoon
// resolves authority=self to the user's DID and writes each scope
// canonically), so this reads what each scope means.
func readsSpace(granted []string, did, spaceURI string) bool {
	ref, err := space.ParseRef(spaceURI)
	if err != nil {
		return false
	}
	want := scopes.SpaceMatch{Type: ref.Type, Authority: ref.Authority, Skey: ref.Skey, Action: "read"}
	for _, g := range granted {
		p := scopes.ParseSpacePermission(g)
		if p == nil {
			continue
		}
		if p.IsSelfAuthority() {
			p = p.WithResolvedAuthority(did)
		}
		if p.Matches(want) {
			return true
		}
	}
	return false
}

// allowedReturn checks a return URL is on one of ReturnOrigins.
func (s *Server) allowedReturn(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.User != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return false
	}
	origin := u.Scheme + "://" + u.Host
	for _, o := range s.ReturnOrigins {
		if strings.TrimSuffix(o, "/") == origin {
			return true
		}
	}
	return false
}

func (s *Server) mac(v string) string {
	m := hmac.New(sha256.New, s.CookieKey)
	m.Write([]byte(v))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

// startedHere checks the callback's state belongs to a grant this browser
// started.
func (s *Server) startedHere(r *http.Request, state string) bool {
	c, err := r.Cookie(grantCookie)
	if err != nil || state == "" || len(s.CookieKey) == 0 {
		return false
	}
	return hmac.Equal([]byte(c.Value), []byte(s.mac("grant."+state)))
}

// finish ends a grant or stop: back to the return URL with the outcome, or
// a page when there's none.
func (s *Server) finish(w http.ResponseWriter, r *http.Request, ret string, status int, outcome, errMsg string) {
	if ret != "" {
		u, _ := url.Parse(ret)
		q := u.Query()
		if errMsg != "" {
			q.Set("indexing_error", errMsg)
		} else {
			q.Set("indexing", outcome)
		}
		u.RawQuery = q.Encode()
		http.Redirect(w, r, u.String(), http.StatusSeeOther)
		return
	}
	switch {
	case errMsg != "":
		s.page(w, status, "Nothing changed", errMsg)
	case outcome == "stopped":
		s.page(w, status, "Indexing stopped", "The appview won't update this space's index any more.")
	default:
		s.page(w, status, "Indexing", "The appview will index this space and keep it up to date.")
	}
}

func (s *Server) writePageErr(w http.ResponseWriter, err error) {
	s.log().Error("grant failed", "err", err)
	s.page(w, http.StatusInternalServerError, "Something went wrong", "Try again.")
}

var pageTmpl = template.Must(template.New("page").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Title}} · Engram Garden</title>
<style>body{font:16px/1.5 system-ui,sans-serif;max-width:32rem;margin:4rem auto;padding:0 1rem;color:#1d2a22}h1{font-size:1.4rem}</style>
</head><body><h1>{{.Title}}</h1><p>{{.Message}}</p><p>You can close this tab.</p></body></html>`))

func (s *Server) page(w http.ResponseWriter, status int, title, msg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'")
	w.WriteHeader(status)
	_ = pageTmpl.Execute(w, struct{ Title, Message string }{title, msg})
}
