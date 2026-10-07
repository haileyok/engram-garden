package appview

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/haileyok/cocoon/space"

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

// handleDescribe tells clients how to have a space indexed: the account to
// add as a member, and whether registration is open.
func (s *Server) handleDescribe(w http.ResponseWriter, r *http.Request) {
	out := map[string]any{"did": s.ServiceDID, "registration": "closed"}
	if s.OpenRegistration {
		out["registration"] = "open"
	}
	if s.Indexer != nil && s.Indexer.Client != nil {
		out["account"] = s.Indexer.Client.DID().String()
	}
	writeJSON(w, http.StatusOK, out)
}

// handleRegister indexes a new space. Any member may ask, with a credential
// for the space. The appview's own account must be a member: adding it is
// how the authority agrees to have the space indexed.
func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Space string `json:"space"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Space == "" {
		s.writeErr(w, errf(http.StatusBadRequest, "InvalidRequest", "space is required"))
		return
	}
	ref, err := space.ParseRef(body.Space)
	if err != nil {
		s.writeErr(w, errf(http.StatusBadRequest, "InvalidRequest", "bad space: %v", err))
		return
	}
	spaceURI := ref.String()
	if spaceURI != body.Space {
		// Requests are routed by the URI as sent, so only accept one spelling.
		s.writeErr(w, errf(http.StatusBadRequest, "InvalidRequest", "write the space as %s", spaceURI))
		return
	}
	if !s.OpenRegistration && !s.indexes(spaceURI) {
		s.writeErr(w, errf(http.StatusForbidden, "RegistrationClosed", "this service indexes only the spaces its operator configures"))
		return
	}
	if err := s.verifyCredential(r, spaceURI); err != nil {
		s.writeErr(w, err)
		return
	}
	if s.indexes(spaceURI) {
		writeJSON(w, http.StatusOK, map[string]any{"space": spaceURI})
		return
	}
	if _, err := s.Indexer.Client.Credential(r.Context(), spaceURI); err != nil {
		s.log().Info("refused a space registration: the appview can't read the space", "space", spaceURI, "err", err)
		s.writeErr(w, errf(http.StatusForbidden, "NotAMember",
			"add this service's account (%s) to the space as a member first", s.Indexer.Client.DID()))
		return
	}
	if s.Blob != nil {
		raw, _ := json.Marshal(registration{Space: spaceURI, RegisteredAt: time.Now().UTC()})
		if err := blob.PutBytes(r.Context(), s.Blob, registrationKey(spaceURI), raw, true); err != nil && !errors.Is(err, blob.ErrExists) {
			s.writeErr(w, err)
			return
		}
	}
	s.addRegistered(spaceURI)
	s.log().Info("space registered", "space", spaceURI)

	// Index it now rather than at the next poll, and have the background
	// loop register for notifications.
	select {
	case s.wake() <- struct{}{}:
	default:
	}
	s.Jobs.Add(1)
	go func() {
		defer s.Jobs.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		s.syncSpaceOnce(ctx, spaceURI)
	}()
	writeJSON(w, http.StatusOK, map[string]any{"space": spaceURI})
}
