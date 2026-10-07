package appview

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/haileyok/engram-garden/internal/blob"
	"github.com/haileyok/engram-garden/internal/spacestore"
)

// registrationPrefix holds one object per registered space. Objects are
// written once and never changed.
const registrationPrefix = "registered-spaces/"

// refreshEvery limits how often a request for an unknown space rereads the
// registered spaces from storage.
const refreshEvery = 10 * time.Second

type registration struct {
	Space        string    `json:"space"`
	RegisteredAt time.Time `json:"registeredAt"`
}

func registrationKey(spaceURI string) string {
	return registrationPrefix + spacestore.SpaceKey(spaceURI) + ".json"
}

// IndexedSpaces is every space this service indexes: the configured ones
// and the registered ones.
func (s *Server) IndexedSpaces() []string { return s.allSpaces() }

// allSpaces is IndexedSpaces.
func (s *Server) allSpaces() []string {
	s.regMu.RLock()
	defer s.regMu.RUnlock()
	seen := map[string]bool{}
	var out []string
	for _, sp := range s.Spaces {
		if !seen[sp] {
			seen[sp] = true
			out = append(out, sp)
		}
	}
	for sp := range s.registered {
		if !seen[sp] {
			seen[sp] = true
			out = append(out, sp)
		}
	}
	sort.Strings(out)
	return out
}

func (s *Server) addRegistered(spaceURI string) {
	s.regMu.Lock()
	defer s.regMu.Unlock()
	if s.registered == nil {
		s.registered = map[string]bool{}
	}
	s.registered[spaceURI] = true
}

// LoadRegistrations reads the registered spaces from storage.
func (s *Server) LoadRegistrations(ctx context.Context) error {
	if s.Blob == nil {
		return nil
	}
	objs, err := s.Blob.List(ctx, registrationPrefix)
	if err != nil {
		return err
	}
	for _, o := range objs {
		if !strings.HasSuffix(o.Key, ".json") {
			continue
		}
		raw, err := blob.GetBytes(ctx, s.Blob, o.Key)
		if err != nil {
			return err
		}
		var reg registration
		if err := json.Unmarshal(raw, &reg); err != nil || reg.Space == "" {
			s.log().Warn("skipping an unreadable space registration", "key", o.Key, "err", err)
			continue
		}
		if registrationKey(reg.Space) != o.Key {
			s.log().Warn("skipping a space registration stored under the wrong key", "key", o.Key, "space", reg.Space)
			continue
		}
		s.addRegistered(reg.Space)
	}
	return nil
}

// knows reports whether the space is indexed, rereading the registrations
// (at most every refreshEvery) when it isn't, since another node may have
// registered it.
func (s *Server) knows(ctx context.Context, spaceURI string) bool {
	if s.indexes(spaceURI) {
		return true
	}
	if s.Blob == nil {
		return false
	}
	s.regMu.Lock()
	due := time.Since(s.lastRefresh) >= refreshEvery
	if due {
		s.lastRefresh = time.Now()
	}
	s.regMu.Unlock()
	if !due {
		return false
	}
	if err := s.LoadRegistrations(ctx); err != nil {
		s.log().Warn("rereading space registrations failed", "err", err)
		return false
	}
	return s.indexes(spaceURI)
}

// handleDescribe tells clients how to have a space indexed: where its
// authority grants the appview access, and whether registration is open.
func (s *Server) handleDescribe(w http.ResponseWriter, r *http.Request) {
	out := map[string]any{"did": s.ServiceDID, "registration": "closed"}
	if s.OpenRegistration {
		out["registration"] = "open"
	}
	if u := s.GrantURL(); u != "" {
		out["grantUrl"] = u
	}
	writeJSON(w, http.StatusOK, out)
}
