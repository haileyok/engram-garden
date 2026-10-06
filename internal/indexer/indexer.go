// Package indexer keeps the memory index in step with a space: it pulls each
// member's repo changes, verifies them against the member's signed commit,
// embeds new memories and writes them to the store.
package indexer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/bluesky-social/indigo/atproto/atdata"
	"github.com/bluesky-social/indigo/atproto/identity"
	"github.com/bluesky-social/indigo/atproto/syntax"
	"github.com/haileyok/cocoon/space"
	"github.com/ipfs/go-cid"

	"github.com/haileyok/engram-garden/internal/embed"
	"github.com/haileyok/engram-garden/internal/spaceclient"
	"github.com/haileyok/engram-garden/internal/store"
)

// Collection is the record type indexed as memories.
const Collection = "garden.engram.memory"

// maxEmbedChars bounds the text sent to the embedder, to stay inside common
// models' input limits.
const maxEmbedChars = 24000

// Indexer syncs spaces into the store.
type Indexer struct {
	Store    *store.Store
	Embedder embed.Embedder
	Client   *spaceclient.Client
	Dir      identity.Directory
	Log      *slog.Logger
	// PageSize is the listRepoOps page size.
	PageSize int

	locks sync.Map // space+did -> *sync.Mutex

	mu        sync.Mutex
	spaceRevs map[string]string // last spaceRev seen per space, for gap detection
}

func (ix *Indexer) log() *slog.Logger {
	if ix.Log == nil {
		return slog.Default()
	}
	return ix.Log
}

func (ix *Indexer) lock(spaceURI, did string) func() {
	m, _ := ix.locks.LoadOrStore(spaceURI+" "+did, &sync.Mutex{})
	mu := m.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// SyncSpace lists the space's writers at the authority and syncs every repo
// whose latest write the index hasn't seen.
func (ix *Indexer) SyncSpace(ctx context.Context, spaceURI string) error {
	ref, err := space.ParseRef(spaceURI)
	if err != nil {
		return err
	}
	host, err := ix.Client.SpaceHost(ctx, ref.Authority)
	if err != nil {
		return err
	}
	known := map[string]string{}
	states, err := ix.Store.Repos(ctx, spaceURI)
	if err != nil {
		return err
	}
	for _, st := range states {
		known[st.DID] = st.SpaceRev
	}
	var errs []error
	cursor := ""
	maxRev := ""
	for {
		params := url.Values{"space": {spaceURI}, "limit": {"100"}}
		if cursor != "" {
			params.Set("cursor", cursor)
		}
		var out struct {
			Repos []struct {
				DID      string `json:"did"`
				RepoRev  string `json:"repoRev"`
				SpaceRev string `json:"spaceRev"`
			} `json:"repos"`
			Cursor string `json:"cursor"`
		}
		if err := ix.Client.Query(ctx, host, spaceURI, ref.Authority, "com.atproto.space.listRepos", params, &out); err != nil {
			if spaceclient.IsError(err, "SpaceDeleted") {
				return ix.Store.MarkSpaceDeleted(ctx, spaceURI)
			}
			return fmt.Errorf("listRepos: %w", err)
		}
		for _, r := range out.Repos {
			maxRev = max(maxRev, r.SpaceRev)
			if rev, ok := known[r.DID]; ok && rev >= r.SpaceRev {
				continue
			}
			if err := ix.SyncRepo(ctx, spaceURI, r.DID, r.SpaceRev); err != nil {
				ix.log().Warn("repo sync failed", "space", spaceURI, "repo", r.DID, "err", err)
				errs = append(errs, fmt.Errorf("%s: %w", r.DID, err))
			}
		}
		if out.Cursor == "" || len(out.Repos) == 0 {
			break
		}
		cursor = out.Cursor
	}
	if len(errs) == 0 {
		ix.noteSpaceRev(spaceURI, maxRev)
	}
	return errors.Join(errs...)
}

// noteSpaceRev records the newest spaceRev seen and returns the previous one.
func (ix *Indexer) noteSpaceRev(spaceURI, rev string) string {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	if ix.spaceRevs == nil {
		ix.spaceRevs = map[string]string{}
	}
	prev := ix.spaceRevs[spaceURI]
	if rev > prev {
		ix.spaceRevs[spaceURI] = rev
	}
	return prev
}

// Notification is a com.atproto.space.notifyWrite body.
type Notification struct {
	Space        string `json:"space"`
	Repo         string `json:"repo"`
	RepoRev      string `json:"repoRev"`
	Hash         string `json:"hash"`
	SpaceRev     string `json:"spaceRev,omitempty"`
	PrevSpaceRev string `json:"prevSpaceRev,omitempty"`
}

// HandleWrite syncs the repo a notification names. It reports whether the
// notification shows a gap, meaning others were missed and the caller should
// run SyncSpace.
func (ix *Indexer) HandleWrite(ctx context.Context, n Notification) (gap bool, err error) {
	if n.SpaceRev != "" {
		prev := ix.noteSpaceRev(n.Space, n.SpaceRev)
		// prevSpaceRev names the write before this one in the space. If it
		// isn't the newest one we saw, a notification went missing.
		gap = prev != "" && n.PrevSpaceRev != prev && n.PrevSpaceRev > prev
	}
	return gap, ix.SyncRepo(ctx, n.Space, n.Repo, n.SpaceRev)
}

// SyncRepo brings one member's repo up to date: incrementally from the
// oplog when it has synced before, else (or if verification fails) with a
// full verified export.
func (ix *Indexer) SyncRepo(ctx context.Context, spaceURI, did, spaceRev string) error {
	if _, err := syntax.ParseDID(did); err != nil {
		return fmt.Errorf("bad repo DID %q", did)
	}
	unlock := ix.lock(spaceURI, did)
	defer unlock()

	st, err := ix.Store.RepoState(ctx, spaceURI, did)
	if err != nil {
		return err
	}
	if st != nil && st.SetHash != nil {
		err := ix.syncIncremental(ctx, *st, spaceRev)
		if err == nil {
			return nil
		}
		if !errors.Is(err, errResync) {
			return err
		}
		ix.log().Info("incremental sync did not verify, re-exporting repo", "space", spaceURI, "repo", did, "reason", err)
	}
	return ix.syncFull(ctx, spaceURI, did, spaceRev)
}

// errResync marks an incremental sync that can't be trusted; a full export
// replaces it.
var errResync = errors.New("resync needed")

type opEntry struct {
	Rev        string          `json:"rev"`
	Collection string          `json:"collection"`
	Rkey       string          `json:"rkey"`
	Cid        *string         `json:"cid"`
	Prev       *string         `json:"prev"`
	Value      json.RawMessage `json:"value,omitempty"`
}

type pathState struct {
	cid   *cid.Cid
	value json.RawMessage
}

func (ix *Indexer) syncIncremental(ctx context.Context, st store.RepoState, spaceRev string) error {
	host, err := ix.Client.PDSHost(ctx, st.DID)
	if err != nil {
		return err
	}
	repo, err := space.RepoCommitFromState(st.SetHash)
	if err != nil {
		return fmt.Errorf("%w: stored set hash: %v", errResync, err)
	}
	pageSize := ix.PageSize
	if pageSize <= 0 {
		pageSize = 500
	}
	final := map[string]*pathState{}
	var order []string
	var commit *space.SignedCommit
	cursor := ""
	for commit == nil {
		params := url.Values{"space": {st.Space}, "repo": {st.DID}, "limit": {fmt.Sprint(pageSize)}}
		if cursor != "" {
			params.Set("cursor", cursor)
		} else if st.Rev != "" {
			params.Set("since", st.Rev)
		}
		var out struct {
			Ops    []opEntry           `json:"ops"`
			Cursor string              `json:"cursor"`
			Commit *space.SignedCommit `json:"commit"`
		}
		if err := ix.Client.Query(ctx, host, st.Space, st.DID, "com.atproto.space.listRepoOps", params, &out); err != nil {
			return fmt.Errorf("listRepoOps: %w", err)
		}
		for _, op := range out.Ops {
			rop, err := parseOp(op)
			if err != nil {
				return fmt.Errorf("%w: %v", errResync, err)
			}
			repo.ApplyOp(rop)
			path := space.FormatRecordPath(op.Collection, op.Rkey)
			if _, seen := final[path]; !seen {
				order = append(order, path)
			}
			final[path] = &pathState{cid: rop.Cid, value: op.Value}
		}
		commit = out.Commit
		if commit == nil {
			if out.Cursor == "" || out.Cursor == cursor {
				return fmt.Errorf("%w: oplog page ended without a commit or cursor", errResync)
			}
			cursor = out.Cursor
		}
	}
	if err := ix.verifyCommit(ctx, st.Space, st.DID, *commit, repo); err != nil {
		return err
	}

	ref, _ := space.ParseRef(st.Space)
	var upserts []store.Memory
	var deletes []string
	for _, path := range order {
		ps := final[path]
		coll, rkey, err := space.ParseRecordPath(path)
		if err != nil || coll != Collection {
			continue
		}
		uri := ref.RecordURI(st.DID, coll, rkey)
		if ps.cid == nil {
			deletes = append(deletes, uri)
			continue
		}
		if len(ps.value) == 0 {
			return fmt.Errorf("%w: no value for the latest version of %s", errResync, path)
		}
		rec, err := atdata.UnmarshalJSON(ps.value)
		if err != nil {
			return fmt.Errorf("%w: %s: %v", errResync, path, err)
		}
		ser, err := space.SerializeRecord(coll, rkey, rec)
		if err != nil || !ser.Cid.Equals(*ps.cid) {
			return fmt.Errorf("%w: %s: value does not match its cid", errResync, path)
		}
		if m, ok := memoryFromRecord(st.Space, st.DID, rkey, ps.cid.String(), uri, rec); ok {
			upserts = append(upserts, m)
		} else {
			// Not a usable memory: make sure no older version lingers.
			deletes = append(deletes, uri)
		}
	}
	if err := ix.embedAll(ctx, upserts); err != nil {
		return err
	}
	next := store.RepoState{Space: st.Space, DID: st.DID, Rev: commit.Rev, SetHash: repo.SetHash.State(), SpaceRev: max(st.SpaceRev, spaceRev)}
	if err := ix.Store.ApplyRepoChanges(ctx, next, upserts, deletes, false); err != nil {
		return err
	}
	if len(upserts)+len(deletes) > 0 {
		ix.log().Info("synced repo", "space", st.Space, "repo", st.DID, "upserts", len(upserts), "deletes", len(deletes), "rev", commit.Rev)
	}
	return nil
}

func parseOp(op opEntry) (space.RepoOp, error) {
	out := space.RepoOp{Collection: op.Collection, Rkey: op.Rkey}
	if op.Cid != nil {
		c, err := cid.Decode(*op.Cid)
		if err != nil {
			return out, fmt.Errorf("op cid: %w", err)
		}
		out.Cid = &c
	}
	if op.Prev != nil {
		c, err := cid.Decode(*op.Prev)
		if err != nil {
			return out, fmt.Errorf("op prev: %w", err)
		}
		out.Prev = &c
	}
	if out.Cid == nil && out.Prev == nil {
		return out, errors.New("op has neither cid nor prev")
	}
	return out, nil
}

// verifyCommit checks a commit is signed by the author and matches the set
// hash the ops produced.
func (ix *Indexer) verifyCommit(ctx context.Context, spaceURI, did string, c space.SignedCommit, repo *space.RepoCommit) error {
	didKey, err := ix.authorKey(ctx, did, false)
	if err != nil {
		return err
	}
	cctx := space.CommitCtx{Space: spaceURI, Author: did, Rev: c.Rev}
	if !space.VerifyCommit(c, cctx, didKey) {
		// The key may have rotated.
		if didKey, err = ix.authorKey(ctx, did, true); err != nil {
			return err
		}
		if !space.VerifyCommit(c, cctx, didKey) {
			return fmt.Errorf("%w: commit signature did not verify", errResync)
		}
	}
	if !repo.Matches(c) {
		return fmt.Errorf("%w: ops do not add up to the signed commit", errResync)
	}
	return nil
}

func (ix *Indexer) authorKey(ctx context.Context, did string, fresh bool) (string, error) {
	d, err := syntax.ParseDID(did)
	if err != nil {
		return "", err
	}
	if fresh {
		_ = ix.Dir.Purge(ctx, d.AtIdentifier())
	}
	ident, err := ix.Dir.LookupDID(ctx, d)
	if err != nil {
		return "", err
	}
	pub, err := ident.PublicKey()
	if err != nil {
		return "", err
	}
	return pub.DIDKey(), nil
}

func (ix *Indexer) syncFull(ctx context.Context, spaceURI, did, spaceRev string) error {
	host, err := ix.Client.PDSHost(ctx, did)
	if err != nil {
		return err
	}
	verify := func(fresh bool) (*space.VerifiedRepo, error) {
		didKey, err := ix.authorKey(ctx, did, fresh)
		if err != nil {
			return nil, err
		}
		body, err := ix.Client.QueryRaw(ctx, host, spaceURI, did, "com.atproto.space.getRepo", url.Values{"space": {spaceURI}, "repo": {did}})
		if err != nil {
			return nil, fmt.Errorf("getRepo: %w", err)
		}
		defer body.Close()
		return space.VerifyRepoCarFull(body, space.VerifyRepoParams{Space: spaceURI, Author: did, DidKey: didKey})
	}
	vr, err := verify(false)
	if err != nil && !spaceclient.IsError(err, "RepoNotFound") {
		var verr *space.RepoVerificationError
		if errors.As(err, &verr) {
			vr, err = verify(true)
		}
	}
	if spaceclient.IsError(err, "RepoNotFound") {
		// The member has no repo (any more): nothing of theirs to index.
		return ix.Store.RemoveRepo(ctx, spaceURI, did)
	}
	if err != nil {
		return fmt.Errorf("verifying %s's repo: %w", did, err)
	}

	ref, _ := space.ParseRef(spaceURI)
	var upserts []store.Memory
	for _, r := range vr.Records {
		if r.Collection != Collection {
			continue
		}
		uri := ref.RecordURI(did, r.Collection, r.Rkey)
		if m, ok := memoryFromRecord(spaceURI, did, r.Rkey, r.Cid.String(), uri, r.Record); ok {
			upserts = append(upserts, m)
		}
	}
	if err := ix.embedAll(ctx, upserts); err != nil {
		return err
	}
	next := store.RepoState{Space: spaceURI, DID: did, Rev: vr.Commit.Rev, SetHash: vr.Repo.SetHash.State(), SpaceRev: spaceRev}
	if err := ix.Store.ApplyRepoChanges(ctx, next, upserts, nil, true); err != nil {
		return err
	}
	ix.log().Info("exported repo", "space", spaceURI, "repo", did, "memories", len(upserts), "rev", vr.Commit.Rev)
	return nil
}

// memoryFromRecord reads a garden.engram.memory record. Records without
// text are not indexed.
func memoryFromRecord(spaceURI, did, rkey, cidStr, uri string, rec map[string]any) (store.Memory, bool) {
	text, _ := rec["text"].(string)
	if strings.TrimSpace(text) == "" {
		return store.Memory{}, false
	}
	m := store.Memory{URI: uri, Space: spaceURI, Author: did, Rkey: rkey, CID: cidStr, Text: text}
	if tags, ok := rec["tags"].([]any); ok {
		for _, t := range tags {
			if s, ok := t.(string); ok && s != "" {
				m.Tags = append(m.Tags, s)
			}
		}
	}
	m.Source, _ = rec["source"].(string)
	if s, ok := rec["createdAt"].(string); ok {
		if dt, err := syntax.ParseDatetimeLenient(s); err == nil {
			m.CreatedAt = dt.Time()
		}
	}
	if m.CreatedAt.IsZero() {
		m.CreatedAt = time.Now().UTC()
	}
	return m, true
}

// EmbedText is what gets embedded for a memory: its text, plus its tags so
// they help retrieval.
func EmbedText(text string, tags []string) string {
	if len(text) > maxEmbedChars {
		text = text[:maxEmbedChars]
	}
	if len(tags) > 0 {
		text += "\n\nTags: " + strings.Join(tags, ", ")
	}
	return text
}

func (ix *Indexer) embedAll(ctx context.Context, ms []store.Memory) error {
	if len(ms) == 0 {
		return nil
	}
	texts := make([]string, len(ms))
	for i, m := range ms {
		texts[i] = EmbedText(m.Text, m.Tags)
	}
	vecs, err := ix.Embedder.Embed(ctx, texts)
	if err != nil {
		return fmt.Errorf("embedding: %w", err)
	}
	for i := range ms {
		ms[i].Embedding = vecs[i]
		ms[i].Model = ix.Embedder.Model()
	}
	return nil
}
