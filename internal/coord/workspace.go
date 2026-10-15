package coord

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/haileyok/cocoon/space"

	"github.com/haileyok/engram-garden/internal/spaceclient"
)

// Workspace is one run of an agent in one workspace space.
type Workspace struct {
	// Client acts as the agent's account.
	Client *spaceclient.Client
	// Space is the workspace space's URI.
	Space string
	// Generator is stamped on every record this run writes. Run is
	// required (see NewRun).
	Generator Generator
	// RequireLease refuses writes unless this run holds the agent's
	// presence lease (see Acquire).
	RequireLease bool
	// Now is the clock for timestamps and leases (default time.Now).
	Now func() time.Time
}

func (w *Workspace) now() time.Time {
	if w.Now != nil {
		return w.Now()
	}
	return time.Now()
}

func (w *Workspace) me() string { return w.Client.DID().String() }

func (w *Workspace) ref() (space.Ref, error) { return space.ParseRef(w.Space) }

// Operator is the workspace's authority, whose approvals count.
func (w *Workspace) Operator() (string, error) {
	ref, err := w.ref()
	if err != nil {
		return "", err
	}
	return ref.Authority, nil
}

func (w *Workspace) generator() (*Generator, error) {
	if w.Generator.Run == "" {
		return nil, errors.New("no run: set Generator.Run (coord.NewRun) when the session starts")
	}
	g := w.Generator
	return &g, nil
}

// create writes a new record and never replaces one. rkey may be empty.
func (w *Workspace) create(ctx context.Context, collection, rkey string, rec any) (StrongRef, error) {
	if w.RequireLease && collection != HandoffCollection {
		ok, err := w.Active(ctx)
		if err != nil {
			return StrongRef{}, err
		}
		if !ok {
			return StrongRef{}, ErrNotActive
		}
	}
	body := map[string]any{"space": w.Space, "repo": w.me(), "collection": collection, "record": rec}
	if rkey != "" {
		body["rkey"] = rkey
	}
	var out StrongRef
	if err := w.Client.Session.Post(ctx, "com.atproto.space.createRecord", body, &out); err != nil {
		return StrongRef{}, fmt.Errorf("writing %s: %w", collection, err)
	}
	return out, nil
}

// Record is a record read from the workspace.
type Record struct {
	URI        string
	CID        string
	Author     string
	Collection string
	Rkey       string
	Value      json.RawMessage
}

// Ref is the record's strong ref.
func (r Record) Ref() StrongRef { return StrongRef{URI: r.URI, CID: r.CID} }

// Decode unmarshals the record's value.
func (r Record) Decode(v any) error { return json.Unmarshal(r.Value, v) }

// StaleError means a strong ref's record exists, but not at the cited CID:
// it changed or was replaced after it was cited.
type StaleError struct {
	Ref     StrongRef
	Current string // the record's CID now
}

func (e *StaleError) Error() string {
	return fmt.Sprintf("%s is at %s now, not the cited %s", e.Ref.URI, e.Current, e.Ref.CID)
}

// ErrNotFound means a record doesn't exist (any more).
var ErrNotFound = errors.New("record not found")

// parseRecordURI splits a space record URI in this workspace.
func (w *Workspace) parseRecordURI(uri string) (author, collection, rkey string, err error) {
	rest, ok := strings.CutPrefix(uri, w.Space+"/")
	parts := strings.Split(rest, "/")
	if !ok || len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return "", "", "", fmt.Errorf("%q is not a record in %s", uri, w.Space)
	}
	return parts[0], parts[1], parts[2], nil
}

// Get reads the record a strong ref points at, and fails with a
// *StaleError unless it's still at the cited CID.
func (w *Workspace) Get(ctx context.Context, ref StrongRef) (Record, error) {
	author, coll, rkey, err := w.parseRecordURI(ref.URI)
	if err != nil {
		return Record{}, err
	}
	rec, err := w.read(ctx, author, coll, rkey)
	if err != nil {
		return Record{}, err
	}
	if rec.CID != ref.CID {
		return rec, &StaleError{Ref: ref, Current: rec.CID}
	}
	return rec, nil
}

// read gets a record's current version.
func (w *Workspace) read(ctx context.Context, author, coll, rkey string) (Record, error) {
	host, err := w.Client.PDSHost(ctx, author)
	if err != nil {
		return Record{}, err
	}
	var out struct {
		CID   string          `json:"cid"`
		Value json.RawMessage `json:"value"`
	}
	params := url.Values{"space": {w.Space}, "repo": {author}, "collection": {coll}, "rkey": {rkey}}
	if err := w.Client.Query(ctx, host, w.Space, author, "com.atproto.space.getRecord", params, &out); err != nil {
		if spaceclient.IsError(err, "RecordNotFound", "RepoNotFound") {
			return Record{}, ErrNotFound
		}
		return Record{}, err
	}
	ref, err := w.ref()
	if err != nil {
		return Record{}, err
	}
	return Record{URI: ref.RecordURI(author, coll, rkey), CID: out.CID, Author: author, Collection: coll, Rkey: rkey, Value: out.Value}, nil
}

// List returns every member's records in a collection.
func (w *Workspace) List(ctx context.Context, collection string) ([]Record, error) {
	members, err := w.members(ctx)
	if err != nil {
		return nil, err
	}
	var all []Record
	for _, did := range members {
		recs, err := w.listRepo(ctx, did, collection)
		if err != nil {
			return nil, err
		}
		all = append(all, recs...)
	}
	return all, nil
}

// members lists the DIDs with a repo in the workspace.
func (w *Workspace) members(ctx context.Context) ([]string, error) {
	ref, err := w.ref()
	if err != nil {
		return nil, err
	}
	host, err := w.Client.SpaceHost(ctx, ref.Authority)
	if err != nil {
		return nil, err
	}
	var dids []string
	cursor := ""
	for {
		params := url.Values{"space": {w.Space}, "limit": {"100"}}
		if cursor != "" {
			params.Set("cursor", cursor)
		}
		var out struct {
			Repos []struct {
				DID string `json:"did"`
			} `json:"repos"`
			Cursor string `json:"cursor"`
		}
		if err := w.Client.Query(ctx, host, w.Space, ref.Authority, "com.atproto.space.listRepos", params, &out); err != nil {
			return nil, fmt.Errorf("listRepos: %w", err)
		}
		for _, r := range out.Repos {
			dids = append(dids, r.DID)
		}
		if out.Cursor == "" || len(out.Repos) == 0 {
			return dids, nil
		}
		cursor = out.Cursor
	}
}

func (w *Workspace) listRepo(ctx context.Context, did, collection string) ([]Record, error) {
	ref, err := w.ref()
	if err != nil {
		return nil, err
	}
	host, err := w.Client.PDSHost(ctx, did)
	if err != nil {
		return nil, err
	}
	var recs []Record
	cursor := ""
	for {
		params := url.Values{"space": {w.Space}, "repo": {did}, "collection": {collection}, "limit": {"100"}}
		if cursor != "" {
			params.Set("cursor", cursor)
		}
		var out struct {
			Records []struct {
				CID        string          `json:"cid"`
				Collection string          `json:"collection"`
				Rkey       string          `json:"rkey"`
				Value      json.RawMessage `json:"value"`
			} `json:"records"`
			Cursor string `json:"cursor"`
		}
		if err := w.Client.Query(ctx, host, w.Space, did, "com.atproto.space.listRecords", params, &out); err != nil {
			if spaceclient.IsError(err, "RepoNotFound") {
				return recs, nil
			}
			return nil, fmt.Errorf("listRecords %s: %w", did, err)
		}
		for _, r := range out.Records {
			coll := r.Collection
			if coll == "" {
				coll = collection
			}
			recs = append(recs, Record{URI: ref.RecordURI(did, coll, r.Rkey), CID: r.CID, Author: did, Collection: coll, Rkey: r.Rkey, Value: r.Value})
		}
		if out.Cursor == "" || len(out.Records) == 0 {
			return recs, nil
		}
		cursor = out.Cursor
	}
}
