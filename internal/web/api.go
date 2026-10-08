package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/bluesky-social/indigo/atproto/atclient"
	"github.com/bluesky-social/indigo/atproto/atdata"
	"github.com/bluesky-social/indigo/atproto/syntax"
	"github.com/haileyok/cocoon/space"

	"github.com/haileyok/engram-garden/internal/lex"
)

// memorySpace parses a space URI and checks it's a memory space.
func memorySpace(raw string) (space.Ref, *Error) {
	if raw == "" {
		return space.Ref{}, apiErr(http.StatusBadRequest, "InvalidRequest", "space is required")
	}
	ref, err := space.ParseRef(raw)
	if err != nil {
		return space.Ref{}, apiErr(http.StatusBadRequest, "InvalidRequest", "bad space URI: %v", err)
	}
	if ref.Type != lex.SpaceType {
		return space.Ref{}, apiErr(http.StatusBadRequest, "InvalidRequest", "not a memory space (type %s)", lex.SpaceType)
	}
	return ref, nil
}

// ownSpace is memorySpace for spaces the user governs.
func ownSpace(raw string, u *user) (space.Ref, *Error) {
	ref, e := memorySpace(raw)
	if e != nil {
		return ref, e
	}
	if ref.Authority != u.did.String() {
		return ref, apiErr(http.StatusForbidden, "NotSpaceOwner", "only the space's authority can do this")
	}
	return ref, nil
}

// appview calls the appview as the user, with a credential for the space.
func (s *Server) appview(ctx context.Context, u *user, ref space.Ref, method, nsid string, params url.Values, body, out any) error {
	if method == http.MethodGet {
		return u.space.Query(ctx, s.AppviewURL, ref.String(), s.AppviewDID, nsid, params, out)
	}
	return u.space.Procedure(ctx, s.AppviewURL, ref.String(), s.AppviewDID, nsid, body, out)
}

// relay writes an upstream JSON response through unchanged.
func (s *Server) relay(w http.ResponseWriter, err error, out json.RawMessage) {
	if err != nil {
		s.fail(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(out)
}

// ---- service ----

// handleService describes the appview: the account a space must add as a
// member to be indexed, and whether spaces can register themselves.
func (s *Server) handleService(w http.ResponseWriter, r *http.Request, _ *user) {
	s.svcMu.Lock()
	defer s.svcMu.Unlock()
	if s.svc == nil || time.Since(s.svcAt) > 10*time.Minute {
		c := atclient.NewAPIClient(s.AppviewURL)
		if s.HTTP != nil {
			c.Client = s.HTTP
		}
		var out map[string]any
		if err := c.Get(r.Context(), "garden.engram.describeService", nil, &out); err != nil {
			s.fail(w, err)
			return
		}
		s.svc, s.svcAt = out, time.Now()
	}
	writeJSON(w, http.StatusOK, s.svc)
}

// handleProfiles resolves DIDs to handles for display.
func (s *Server) handleProfiles(w http.ResponseWriter, r *http.Request, _ *user) {
	dids := r.URL.Query()["did"]
	if len(dids) > 100 {
		writeErr(w, apiErr(http.StatusBadRequest, "InvalidRequest", "at most 100 DIDs"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	out := map[string]string{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)
	for _, raw := range dids {
		did, err := syntax.ParseDID(raw)
		if err != nil {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			ident, err := s.Dir.LookupDID(ctx, did)
			if err != nil || ident.Handle.IsInvalidHandle() {
				return
			}
			mu.Lock()
			out[did.String()] = ident.Handle.String()
			mu.Unlock()
		}()
	}
	wg.Wait()
	writeJSON(w, http.StatusOK, map[string]any{"handles": out})
}

// ---- spaces and memories ----

// handleSpaces lists the memory spaces the user's PDS knows them in: the
// ones they govern and the ones they've written to.
func (s *Server) handleSpaces(w http.ResponseWriter, r *http.Request, u *user) {
	var out struct {
		Spaces []struct {
			URI string `json:"uri"`
		} `json:"spaces"`
	}
	if err := u.api.Get(r.Context(), "com.atproto.space.listSpaces", map[string]any{"spaceType": lex.SpaceType, "limit": 100}, &out); err != nil {
		s.fail(w, err)
		return
	}
	spaces := []map[string]any{}
	for _, sp := range out.Spaces {
		ref, err := space.ParseRef(sp.URI)
		if err != nil || ref.Type != lex.SpaceType {
			continue
		}
		spaces = append(spaces, map[string]any{
			"uri": ref.String(), "authority": ref.Authority, "name": ref.Skey,
			"isAuthority": ref.Authority == u.did.String(),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"spaces": spaces})
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request, u *user) {
	ref, e := memorySpace(r.URL.Query().Get("space"))
	if e != nil {
		writeErr(w, e)
		return
	}
	var out json.RawMessage
	err := s.appview(r.Context(), u, ref, http.MethodGet, "garden.engram.getSpaceStatus", url.Values{"space": {ref.String()}}, nil, &out)
	s.relay(w, err, out)
}

func (s *Server) handleMemories(w http.ResponseWriter, r *http.Request, u *user) {
	q := r.URL.Query()
	ref, e := memorySpace(q.Get("space"))
	if e != nil {
		writeErr(w, e)
		return
	}
	params := url.Values{"space": {ref.String()}}
	for _, k := range []string{"author", "since", "cursor", "limit"} {
		if v := q.Get(k); v != "" {
			params.Set(k, v)
		}
	}
	if tags := q["tags"]; len(tags) > 0 {
		params["tags"] = tags
	}
	var out json.RawMessage
	err := s.appview(r.Context(), u, ref, http.MethodGet, "garden.engram.listMemories", params, nil, &out)
	s.relay(w, err, out)
}

func (s *Server) handleMemory(w http.ResponseWriter, r *http.Request, u *user) {
	q := r.URL.Query()
	ref, e := memorySpace(q.Get("space"))
	if e != nil {
		writeErr(w, e)
		return
	}
	var out json.RawMessage
	err := s.appview(r.Context(), u, ref, http.MethodGet, "garden.engram.getMemory", url.Values{"space": {ref.String()}, "uri": {q.Get("uri")}}, nil, &out)
	s.relay(w, err, out)
}

// recordURI splits a memory's URI in a space into author and rkey.
func recordURI(ref space.Ref, uri string) (string, string, bool) {
	rest, ok := strings.CutPrefix(uri, ref.String()+"/")
	parts := strings.Split(rest, "/")
	if !ok || len(parts) != 3 || parts[1] != lex.MemoryCollection {
		return "", "", false
	}
	if _, err := syntax.ParseDID(parts[0]); err != nil {
		return "", "", false
	}
	if _, err := syntax.ParseRecordKey(parts[2]); err != nil {
		return "", "", false
	}
	return parts[0], parts[2], true
}

// handleDeleteMemory deletes one of the user's own memories from their repo.
func (s *Server) handleDeleteMemory(w http.ResponseWriter, r *http.Request, u *user) {
	var in struct {
		Space string `json:"space"`
		URI   string `json:"uri"`
	}
	if e := decode(r, &in); e != nil {
		writeErr(w, e)
		return
	}
	ref, e := memorySpace(in.Space)
	if e != nil {
		writeErr(w, e)
		return
	}
	author, rkey, ok := recordURI(ref, in.URI)
	if !ok {
		writeErr(w, apiErr(http.StatusBadRequest, "InvalidRequest", "not a memory in this space"))
		return
	}
	if author != u.did.String() {
		writeErr(w, apiErr(http.StatusForbidden, "NotYourMemory", "you can only delete your own memories"))
		return
	}
	body := map[string]any{"space": ref.String(), "repo": author, "collection": lex.MemoryCollection, "rkey": rkey}
	if err := u.api.Post(r.Context(), "com.atproto.space.deleteRecord", body, nil); err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": in.URI})
}

// ---- managing a space (authority only) ----

func (s *Server) handleCreateSpace(w http.ResponseWriter, r *http.Request, u *user) {
	var in struct {
		Name string `json:"name"`
	}
	if e := decode(r, &in); e != nil {
		writeErr(w, e)
		return
	}
	if _, err := syntax.ParseRecordKey(in.Name); err != nil || in.Name == "." || in.Name == ".." {
		writeErr(w, apiErr(http.StatusBadRequest, "InvalidRequest", "names use letters, digits and . _ ~ : - (up to 512)"))
		return
	}
	member := map[string]string{"$type": "com.atproto.simplespace.defs#memberListPolicy"}
	body := map[string]any{"spaceType": lex.SpaceType, "skey": in.Name, "readPolicy": member, "writePolicy": member}
	var out struct {
		URI string `json:"uri"`
	}
	if err := u.api.Post(r.Context(), "com.atproto.simplespace.createSpace", body, &out); err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"uri": out.URI})
}

func (s *Server) handleGetSpace(w http.ResponseWriter, r *http.Request, u *user) {
	ref, e := ownSpace(r.URL.Query().Get("space"), u)
	if e != nil {
		writeErr(w, e)
		return
	}
	var out json.RawMessage
	err := u.api.Get(r.Context(), "com.atproto.simplespace.getSpace", map[string]any{"space": ref.String()}, &out)
	s.relay(w, err, out)
}

func (s *Server) handleMembers(w http.ResponseWriter, r *http.Request, u *user) {
	ref, e := ownSpace(r.URL.Query().Get("space"), u)
	if e != nil {
		writeErr(w, e)
		return
	}
	type member struct {
		DID   string `json:"did"`
		Read  bool   `json:"read"`
		Write bool   `json:"write"`
	}
	members := []member{}
	cursor := ""
	for range 20 {
		params := map[string]any{"space": ref.String(), "limit": 1000}
		if cursor != "" {
			params["cursor"] = cursor
		}
		var page struct {
			Members []member `json:"members"`
			Cursor  string   `json:"cursor"`
		}
		if err := u.api.Get(r.Context(), "com.atproto.simplespace.listMembers", params, &page); err != nil {
			s.fail(w, err)
			return
		}
		members = append(members, page.Members...)
		if page.Cursor == "" || len(page.Members) == 0 {
			break
		}
		cursor = page.Cursor
	}
	writeJSON(w, http.StatusOK, map[string]any{"members": members})
}

// resolveMember accepts a DID or a handle.
func (s *Server) resolveMember(ctx context.Context, raw string) (string, *Error) {
	raw = strings.TrimPrefix(strings.TrimSpace(raw), "@")
	id, err := syntax.ParseAtIdentifier(raw)
	if err != nil {
		return "", apiErr(http.StatusBadRequest, "InvalidRequest", "enter a handle or a DID")
	}
	if id.IsDID() {
		did, _ := id.AsDID()
		return did.String(), nil
	}
	ident, err := s.Dir.Lookup(ctx, id)
	if err != nil {
		return "", apiErr(http.StatusBadRequest, "UnknownHandle", "couldn't resolve %s", raw)
	}
	return ident.DID.String(), nil
}

func (s *Server) handlePutMember(w http.ResponseWriter, r *http.Request, u *user) {
	var in struct {
		Space  string `json:"space"`
		Member string `json:"member"`
		Read   *bool  `json:"read"`
		Write  *bool  `json:"write"`
	}
	if e := decode(r, &in); e != nil {
		writeErr(w, e)
		return
	}
	ref, e := ownSpace(in.Space, u)
	if e != nil {
		writeErr(w, e)
		return
	}
	did, e := s.resolveMember(r.Context(), in.Member)
	if e != nil {
		writeErr(w, e)
		return
	}
	read, write := true, true
	if in.Read != nil {
		read = *in.Read
	}
	if in.Write != nil {
		write = *in.Write
	}
	body := map[string]any{"space": ref.String(), "did": did, "read": read, "write": write}
	if err := u.api.Post(r.Context(), "com.atproto.simplespace.putMember", body, nil); err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"did": did, "read": read, "write": write})
}

func (s *Server) handleRemoveMember(w http.ResponseWriter, r *http.Request, u *user) {
	var in struct {
		Space string `json:"space"`
		DID   string `json:"did"`
	}
	if e := decode(r, &in); e != nil {
		writeErr(w, e)
		return
	}
	ref, e := ownSpace(in.Space, u)
	if e != nil {
		writeErr(w, e)
		return
	}
	if _, err := syntax.ParseDID(in.DID); err != nil {
		writeErr(w, apiErr(http.StatusBadRequest, "InvalidRequest", "did must be a DID"))
		return
	}
	if err := u.api.Post(r.Context(), "com.atproto.simplespace.removeMember", map[string]any{"space": ref.String(), "did": in.DID}, nil); err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{})
}

// currentConfig reads the space's model from the authority's own repo.
func (s *Server) currentConfig(ctx context.Context, u *user, ref space.Ref) (*lex.Config, error) {
	var out struct {
		Value json.RawMessage `json:"value"`
	}
	params := map[string]any{"space": ref.String(), "repo": u.did.String(), "collection": lex.ConfigCollection, "rkey": lex.ConfigRkey}
	if err := u.api.Get(ctx, "com.atproto.space.getRecord", params, &out); err != nil {
		var ae *atclient.APIError
		if errors.As(err, &ae) && (ae.Name == "RecordNotFound" || ae.Name == "RepoNotFound") {
			return nil, nil
		}
		return nil, err
	}
	rec, err := atdata.UnmarshalJSON(out.Value)
	if err != nil {
		return nil, err
	}
	return lex.ParseConfig(rec)
}

func configView(cfg *lex.Config) any {
	if cfg == nil {
		return nil
	}
	return cfg.Record(time.Now())
}

func (s *Server) handleGetConfig(w http.ResponseWriter, r *http.Request, u *user) {
	ref, e := ownSpace(r.URL.Query().Get("space"), u)
	if e != nil {
		writeErr(w, e)
		return
	}
	cfg, err := s.currentConfig(r.Context(), u, ref)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"config": configView(cfg)})
}

// handlePutConfig declares or changes the space's model, the same changes
// engram-config makes.
func (s *Server) handlePutConfig(w http.ResponseWriter, r *http.Request, u *user) {
	var in struct {
		Space          string        `json:"space"`
		Action         string        `json:"action"`
		Model          lex.ModelInfo `json:"model"`
		DocumentPrefix string        `json:"documentPrefix"`
		QueryPrefix    string        `json:"queryPrefix"`
	}
	if e := decode(r, &in); e != nil {
		writeErr(w, e)
		return
	}
	ref, e := ownSpace(in.Space, u)
	if e != nil {
		writeErr(w, e)
		return
	}
	cur, err := s.currentConfig(r.Context(), u, ref)
	if err != nil {
		s.fail(w, err)
		return
	}
	in.Model.Model = strings.TrimSpace(in.Model.Model)
	in.Model.ModelDigest = strings.TrimSpace(in.Model.ModelDigest)
	cfg, err := lex.ChangeConfig(cur, lex.ConfigAction(in.Action), in.Model, in.DocumentPrefix, in.QueryPrefix)
	if err != nil {
		writeErr(w, apiErr(http.StatusBadRequest, "InvalidConfig", "%v", err))
		return
	}
	body := map[string]any{
		"space": ref.String(), "repo": u.did.String(), "collection": lex.ConfigCollection, "rkey": lex.ConfigRkey,
		"record": cfg.Record(time.Now()),
	}
	if err := u.api.Post(r.Context(), "com.atproto.space.putRecord", body, nil); err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"config": configView(&cfg)})
}
