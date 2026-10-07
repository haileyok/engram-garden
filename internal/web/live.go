package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// seenLimit bounds how many memory URIs a live stream remembers.
const seenLimit = 2000

type liveMemory struct {
	URI string `json:"uri"`
}

// handleLive streams a space's new memories as server-sent events. It polls
// the appview with the user's credential, so a stream shows only what the
// user may read. Events:
//
//	ready      the stream is watching
//	memories   {"memories": [...]}: memories that weren't there before, newest first
//	status     the space's getSpaceStatus, when its memory count changes
//	error      {"error", "message"}: the stream stops
func (s *Server) handleLive(w http.ResponseWriter, r *http.Request, u *user) {
	ref, e := memorySpace(r.URL.Query().Get("space"))
	if e != nil {
		writeErr(w, e)
		return
	}
	fl, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, apiErr(http.StatusInternalServerError, "Unsupported", "streaming isn't supported"))
		return
	}
	s.mu.Lock()
	if u.live >= maxLivePerUser {
		s.mu.Unlock()
		writeErr(w, apiErr(http.StatusTooManyRequests, "TooManyStreams", "close another tab watching a space"))
		return
	}
	u.live++
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		u.live--
		u.used = time.Now()
		s.mu.Unlock()
	}()

	ctx := r.Context()
	params := url.Values{"space": {ref.String()}, "limit": {"50"}}
	type page struct {
		Memories []json.RawMessage `json:"memories"`
	}
	var status struct {
		Memories int `json:"memories"`
	}
	poll := func() ([]json.RawMessage, []string, json.RawMessage, error) {
		var p page
		if err := s.appview(ctx, u, ref, http.MethodGet, "garden.engram.listMemories", params, nil, &p); err != nil {
			return nil, nil, nil, err
		}
		uris := make([]string, len(p.Memories))
		for i, raw := range p.Memories {
			var m liveMemory
			_ = json.Unmarshal(raw, &m)
			uris[i] = m.URI
		}
		var st json.RawMessage
		if err := s.appview(ctx, u, ref, http.MethodGet, "garden.engram.getSpaceStatus", url.Values{"space": {ref.String()}}, nil, &st); err != nil {
			return nil, nil, nil, err
		}
		return p.Memories, uris, st, nil
	}

	send := func(event string, v any) {
		raw, _ := json.Marshal(v)
		_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, raw)
		fl.Flush()
	}
	sendErr := func(err error) {
		ue := upstreamErr(err)
		send("error", map[string]string{"error": ue.Name, "message": ue.Message})
	}

	_, uris, st, err := poll()
	if err != nil {
		s.fail(w, err)
		return
	}
	seen := map[string]bool{}
	order := []string{}
	remember := func(uri string) {
		if uri == "" || seen[uri] {
			return
		}
		seen[uri] = true
		order = append(order, uri)
		if len(order) > seenLimit {
			delete(seen, order[0])
			order = order[1:]
		}
	}
	for _, uri := range uris {
		remember(uri)
	}
	_ = json.Unmarshal(st, &status)
	count := status.Memories

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	send("ready", map[string]any{"space": ref.String()})
	send("status", st)

	every := s.LiveEvery
	if every <= 0 {
		every = 5 * time.Second
	}
	t := time.NewTicker(every)
	defer t.Stop()
	keepalive := time.NewTicker(25 * time.Second)
	defer keepalive.Stop()
	failures := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-keepalive.C:
			_, _ = fmt.Fprint(w, ": keepalive\n\n")
			fl.Flush()
		case <-t.C:
			ms, uris, st, err := poll()
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				// Ride out brief appview trouble; give up on anything else.
				if ue := upstreamErr(err); ue.Status >= 500 && failures < 5 {
					failures++
					continue
				}
				sendErr(err)
				return
			}
			failures = 0
			var fresh []json.RawMessage
			for i, uri := range uris {
				if uri != "" && !seen[uri] {
					fresh = append(fresh, ms[i])
				}
			}
			for i := len(uris) - 1; i >= 0; i-- {
				remember(uris[i])
			}
			if len(fresh) > 0 {
				send("memories", map[string]any{"memories": fresh})
			}
			_ = json.Unmarshal(st, &status)
			if status.Memories != count {
				count = status.Memories
				send("status", st)
			}
		}
	}
}
