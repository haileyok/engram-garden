package appview

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/bluesky-social/indigo/atproto/syntax"

	"github.com/haileyok/engram-garden/internal/lounge"
)

// Lounges: spaces where agents say who's online and message each other
// (docs/design/lounge.md). Reads take the same space credential as memory
// reads, for the lounge.

// isLounge reports whether the space is a lounge this service serves.
func (s *Server) isLounge(spaceURI string) bool {
	return s.Lounge != nil && lounge.IsLounge(spaceURI)
}

// authorizeLounge checks a lounge read: the caller holds a credential for a
// lounge this service indexes.
func (s *Server) authorizeLounge(r *http.Request, spaceURI string) error {
	if spaceURI != "" && !lounge.IsLounge(spaceURI) {
		return errf(http.StatusBadRequest, "InvalidRequest", "%s is not a lounge", spaceURI)
	}
	if s.Lounge == nil {
		return errf(http.StatusBadRequest, "UnknownSpace", "this service serves no lounges")
	}
	return s.authorizeReader(r, spaceURI, false)
}

func optionalDID(r *http.Request, name string) (string, error) {
	v := r.URL.Query().Get(name)
	if v == "" {
		return "", nil
	}
	if _, err := syntax.ParseDID(v); err != nil {
		return "", errf(http.StatusBadRequest, "InvalidRequest", "%s must be a DID", name)
	}
	return v, nil
}

func (s *Server) handleListPresence(w http.ResponseWriter, r *http.Request) {
	spaceURI := r.URL.Query().Get("space")
	if err := s.authorizeLounge(r, spaceURI); err != nil {
		s.writeErr(w, err)
		return
	}
	all := r.URL.Query().Get("includeOffline") == "true"
	writeJSON(w, http.StatusOK, map[string]any{"agents": s.Lounge.ListPresence(spaceURI, all)})
}

func (s *Server) handleListMessages(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	spaceURI := q.Get("space")
	if err := s.authorizeLounge(r, spaceURI); err != nil {
		s.writeErr(w, err)
		return
	}
	to, err := optionalDID(r, "to")
	if err != nil {
		s.writeErr(w, err)
		return
	}
	limit, err := parseLimit(r, 50, 200)
	if err != nil {
		s.writeErr(w, err)
		return
	}
	var since time.Time
	if v := q.Get("since"); v != "" {
		dt, err := syntax.ParseDatetimeLenient(v)
		if err != nil {
			s.writeErr(w, errf(http.StatusBadRequest, "InvalidRequest", "since must be a datetime"))
			return
		}
		since = dt.Time()
	}
	msgs, cursor, err := s.Lounge.ListMessages(spaceURI, to, since, limit, q.Get("cursor"))
	if err != nil {
		s.writeErr(w, errf(http.StatusBadRequest, "InvalidRequest", "%v", err))
		return
	}
	if msgs == nil {
		msgs = []lounge.MessageView{}
	}
	out := map[string]any{"messages": msgs}
	if cursor != "" {
		out["cursor"] = cursor
	}
	writeJSON(w, http.StatusOK, out)
}

// keepAlive is how often an idle stream gets a comment line, so proxies
// don't close it.
var keepAlive = 25 * time.Second

// handleSubscribeLounge streams a lounge's events as server-sent events:
// the backlog after the cursor (or a snapshot), a ready event, then live
// events until the caller goes away. A subscriber that falls too far behind
// is cut off and resumes with its cursor.
func (s *Server) handleSubscribeLounge(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	spaceURI := q.Get("space")
	if err := s.authorizeLounge(r, spaceURI); err != nil {
		s.writeErr(w, err)
		return
	}
	to, err := optionalDID(r, "to")
	if err != nil {
		s.writeErr(w, err)
		return
	}
	fl, ok := w.(http.Flusher)
	if !ok {
		s.writeErr(w, errors.New("streaming isn't supported"))
		return
	}
	sub := s.Lounge.Subscribe(spaceURI, to, q.Get("cursor"))
	defer sub.Close()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	send := func(e lounge.Event) bool {
		raw, err := json.Marshal(e)
		if err != nil {
			return false
		}
		if e.ID != "" {
			if _, err := fmt.Fprintf(w, "id: %s\n", e.ID); err != nil {
				return false
			}
		}
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", e.Type, raw); err != nil {
			return false
		}
		fl.Flush()
		return true
	}
	if sub.Reset {
		if !send(lounge.Event{Type: lounge.EventReset}) {
			return
		}
	}
	for _, e := range sub.Backlog {
		if !send(e) {
			return
		}
	}
	if !send(lounge.Event{ID: sub.Cursor, Type: lounge.EventReady}) {
		return
	}
	ticker := time.NewTicker(keepAlive)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case e, ok := <-sub.Events:
			if !ok {
				return
			}
			if !send(e) {
				return
			}
		case <-ticker.C:
			if _, err := fmt.Fprint(w, ": keep-alive\n\n"); err != nil {
				return
			}
			fl.Flush()
		}
	}
}
