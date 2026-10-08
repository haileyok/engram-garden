// Package indexer keeps the memory index in step with a space: it pulls each
// member's repo changes, verifies them against the member's signed commit,
// and hands the memories, with the vectors their records carry, to the
// index.
package indexer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/bluesky-social/indigo/atproto/atdata"
	"github.com/bluesky-social/indigo/atproto/identity"
	"github.com/bluesky-social/indigo/atproto/syntax"
	"github.com/haileyok/cocoon/space"
	"github.com/ipfs/go-cid"

	"github.com/haileyok/engram-garden/internal/lex"
	"github.com/haileyok/engram-garden/internal/metrics"
	"github.com/haileyok/engram-garden/internal/spaceclient"
	"github.com/haileyok/engram-garden/internal/spacestore"
)

// Collection is the record type indexed as memories.
const Collection = lex.MemoryCollection

// Indexer syncs spaces into the index.
type Indexer struct {
	Store  *spacestore.Node
	Client *spaceclient.Client
	Dir    identity.Directory
	Log    *slog.Logger
	// PageSize is the listRepoOps page size.
	PageSize int

	locks sync.Map // space+did -> *sync.Mutex

	mu        sync.Mutex
	spaceRevs map[string]string // last spaceRev seen per space, for gap detection
	resync    map[string]bool   // spaces whose config change needs a full sync
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

func (ix *Indexer) markResync(spaceURI string) {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	if ix.resync == nil {
		ix.resync = map[string]bool{}
	}
	ix.resync[spaceURI] = true
}

func (ix *Indexer) takeResync(spaceURI string) bool {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	r := ix.resync[spaceURI]
	delete(ix.resync, spaceURI)
	return r
}

type listedRepo struct {
	DID      string `json:"did"`
	RepoRev  string `json:"repoRev"`
	SpaceRev string `json:"spaceRev"`
}

// SyncSpace lists the space's writers at the authority and syncs every repo
// whose latest write the index hasn't seen. The authority's repo goes
// first, since its config record says which vectors to index.
func (ix *Indexer) SyncSpace(ctx context.Context, spaceURI string) (err error) {
	start := time.Now()
	defer func() {
		spaceSyncs.WithLabelValues(metrics.Result(err)).Inc()
		spaceSyncDuration.Observe(time.Since(start).Seconds())
	}()
	// A config change met during the pass asks for a full pass; two passes
	// cover it.
	for range 2 {
		ix.takeResync(spaceURI)
		if err = ix.syncSpaceOnce(ctx, spaceURI); err != nil {
			return err
		}
		if !ix.takeResync(spaceURI) {
			return nil
		}
	}
	return err
}

func (ix *Indexer) syncSpaceOnce(ctx context.Context, spaceURI string) error {
	ref, err := space.ParseRef(spaceURI)
	if err != nil {
		return err
	}
	host, err := ix.Client.SpaceHost(ctx, ref.Authority)
	if err != nil {
		return err
	}
	var listed []listedRepo
	cursor := ""
	for {
		params := url.Values{"space": {spaceURI}, "limit": {"100"}}
		if cursor != "" {
			params.Set("cursor", cursor)
		}
		var out struct {
			Repos  []listedRepo `json:"repos"`
			Cursor string       `json:"cursor"`
		}
		if err := ix.Client.Query(ctx, host, spaceURI, ref.Authority, "com.atproto.space.listRepos", params, &out); err != nil {
			if spaceclient.IsError(err, "SpaceDeleted") {
				return ix.Store.MarkSpaceDeleted(ctx, spaceURI)
			}
			return fmt.Errorf("listRepos: %w", err)
		}
		listed = append(listed, out.Repos...)
		if out.Cursor == "" || len(out.Repos) == 0 {
			break
		}
		cursor = out.Cursor
	}
	sort.SliceStable(listed, func(i, j int) bool { return listed[i].DID == ref.Authority && listed[j].DID != ref.Authority })

	known, err := ix.Store.Repos(ctx, spaceURI)
	if err != nil {
		return err
	}
	var errs []error
	maxRev := ""
	isListed := map[string]bool{}
	for _, r := range listed {
		isListed[r.DID] = true
		maxRev = max(maxRev, r.SpaceRev)
		if p, ok := known[r.DID]; ok && p.SpaceRev >= r.SpaceRev {
			continue
		}
		if err := ix.SyncRepo(ctx, spaceURI, r.DID, r.SpaceRev); err != nil {
			ix.log().Warn("repo sync failed", "space", spaceURI, "repo", r.DID, "err", err)
			errs = append(errs, fmt.Errorf("%s: %w", r.DID, err))
		}
	}
	// The listing completed: a repo the authority no longer lists has left
	// the space, so its memories go too.
	for did := range known {
		if isListed[did] {
			continue
		}
		unlock := ix.lock(spaceURI, did)
		err := ix.Store.RemoveRepo(ctx, spaceURI, did)
		unlock()
		if err != nil {
			errs = append(errs, fmt.Errorf("removing %s: %w", did, err))
			continue
		}
		ix.log().Info("repo left the space, removed its memories", "space", spaceURI, "repo", did)
	}
	if len(errs) == 0 {
		ix.noteSpaceRev(spaceURI, maxRev)
	}
	if st, err := ix.Store.Status(ctx, spaceURI); err == nil {
		for author, n := range st.Skipped {
			ix.log().Warn("memories not indexed: their vectors don't match the space's model", "space", spaceURI, "author", author, "count", n)
		}
	}
	return errors.Join(errs...)
}

// Register asks the space's authority to forward write notifications to the
// service identifier (a DID with a fragment naming the DID document entry to
// deliver to). Registrations lapse after a day, so call it periodically.
func (ix *Indexer) Register(ctx context.Context, spaceURI, service string) (time.Time, error) {
	ref, err := space.ParseRef(spaceURI)
	if err != nil {
		return time.Time{}, err
	}
	host, err := ix.Client.SpaceHost(ctx, ref.Authority)
	if err != nil {
		return time.Time{}, err
	}
	var out struct {
		ExpiresAt string `json:"expiresAt"`
	}
	body := map[string]string{"space": spaceURI, "service": service}
	if err := ix.Client.Procedure(ctx, host, spaceURI, ref.Authority, "com.atproto.space.registerNotify", body, &out); err != nil {
		return time.Time{}, fmt.Errorf("registerNotify: %w", err)
	}
	exp, err := syntax.ParseDatetimeLenient(out.ExpiresAt)
	if err != nil {
		return time.Time{}, nil
	}
	return exp.Time(), nil
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
	Space   string `json:"space"`
	Repo    string `json:"repo"`
	RepoRev string `json:"repoRev"`
	// Hash is the repo's set-hash digest, lexicon bytes ({"$bytes": …}).
	Hash         space.LexBytes `json:"hash"`
	SpaceRev     string         `json:"spaceRev,omitempty"`
	PrevSpaceRev string         `json:"prevSpaceRev,omitempty"`
}

// HandleWrite syncs the repo a notification names. It reports whether the
// caller should run SyncSpace: a notification went missing, or the space's
// model changed and every repo needs a full sync.
func (ix *Indexer) HandleWrite(ctx context.Context, n Notification) (needSpaceSync bool, err error) {
	if n.SpaceRev != "" {
		prev := ix.noteSpaceRev(n.Space, n.SpaceRev)
		// prevSpaceRev names the write before this one in the space. If it
		// isn't the newest one we saw, a notification went missing.
		needSpaceSync = prev != "" && n.PrevSpaceRev != prev && n.PrevSpaceRev > prev
	}
	err = ix.SyncRepo(ctx, n.Space, n.Repo, n.SpaceRev)
	if ix.takeResync(n.Space) {
		needSpaceSync = true
	}
	// Without the authority's config nothing can be indexed; a space sync
	// reads it first.
	if cfg, cerr := ix.Store.Config(ctx, n.Space); cerr == nil && cfg == nil {
		needSpaceSync = true
	}
	return needSpaceSync, err
}

// SyncRepo brings one member's repo up to date: incrementally from the
// oplog when it has synced before, else (or if verification fails) with a
// full verified export.
func (ix *Indexer) SyncRepo(ctx context.Context, spaceURI, did, spaceRev string) (err error) {
	if _, err := syntax.ParseDID(did); err != nil {
		return fmt.Errorf("bad repo DID %q", did)
	}
	unlock := ix.lock(spaceURI, did)
	defer unlock()

	start, mode := time.Now(), "full"
	defer func() {
		repoSyncs.WithLabelValues(mode, metrics.Result(err)).Inc()
		repoSyncDuration.WithLabelValues(mode).Observe(time.Since(start).Seconds())
	}()
	st, err := ix.Store.RepoState(ctx, spaceURI, did)
	if err != nil {
		return err
	}
	if st != nil && st.SetHash != nil {
		mode = "incremental"
		err := ix.syncIncremental(ctx, spaceURI, did, *st, spaceRev)
		if err == nil {
			return nil
		}
		if !errors.Is(err, errResync) {
			return err
		}
		ix.log().Info("incremental sync can't be applied, re-exporting repo", "space", spaceURI, "repo", did, "reason", err)
		reexports.Inc()
		mode = "full"
	}
	return ix.syncFull(ctx, spaceURI, did, spaceRev)
}

// errResync marks an incremental sync that can't be trusted or applied; a
// full export replaces it.
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

func (ix *Indexer) syncIncremental(ctx context.Context, spaceURI, did string, st spacestore.RepoPosition, spaceRev string) error {
	host, err := ix.Client.PDSHost(ctx, did)
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
		params := url.Values{"space": {spaceURI}, "repo": {did}, "limit": {fmt.Sprint(pageSize)}}
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
		if err := ix.Client.Query(ctx, host, spaceURI, did, "com.atproto.space.listRepoOps", params, &out); err != nil {
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
	if err := ix.verifyCommit(ctx, spaceURI, did, *commit, repo); err != nil {
		return err
	}

	ref, _ := space.ParseRef(spaceURI)
	var upserts []spacestore.Memory
	var deletes []string
	for _, path := range order {
		ps := final[path]
		coll, rkey, err := space.ParseRecordPath(path)
		if err != nil {
			continue
		}
		isConfig := did == ref.Authority && coll == lex.ConfigCollection && rkey == lex.ConfigRkey
		if coll != Collection && !isConfig {
			continue
		}
		var rec map[string]any
		if ps.cid != nil {
			if len(ps.value) == 0 {
				return fmt.Errorf("%w: no value for the latest version of %s", errResync, path)
			}
			if rec, err = atdata.UnmarshalJSON(ps.value); err != nil {
				return fmt.Errorf("%w: %s: %v", errResync, path, err)
			}
			ser, err := space.SerializeRecord(coll, rkey, rec)
			if err != nil || !ser.Cid.Equals(*ps.cid) {
				return fmt.Errorf("%w: %s: value does not match its cid", errResync, path)
			}
		}
		if isConfig {
			resync, err := ix.applyConfig(ctx, spaceURI, rec)
			if err != nil {
				return err
			}
			if resync {
				// The space now needs vectors from records this sync didn't
				// read; export this repo, and the others on the next pass.
				return fmt.Errorf("%w: the space's model changed", errResync)
			}
			continue
		}
		if ps.cid == nil {
			deletes = append(deletes, rkey)
			continue
		}
		if m, ok := memoryFromRecord(did, rkey, ps.cid.String(), rec); ok {
			upserts = append(upserts, m)
		} else {
			// Not a usable memory: make sure no older version lingers.
			deletes = append(deletes, rkey)
		}
	}
	next := spacestore.RepoPosition{Rev: commit.Rev, SetHash: repo.SetHash.State(), SpaceRev: max(st.SpaceRev, spaceRev)}
	if err := ix.apply(ctx, spaceURI, did, next, upserts, deletes, false); err != nil {
		return err
	}
	if len(upserts)+len(deletes) > 0 {
		observeLag(commit.Rev, time.Now())
		ix.log().Info("synced repo", "space", spaceURI, "repo", did, "upserts", len(upserts), "deletes", len(deletes), "rev", commit.Rev)
	}
	return nil
}

func (ix *Indexer) apply(ctx context.Context, spaceURI, did string, pos spacestore.RepoPosition, upserts []spacestore.Memory, deletes []string, replace bool) error {
	err := ix.Store.ApplyRepoChanges(ctx, spaceURI, did, pos, upserts, deletes, replace)
	if err == nil || errors.Is(err, spacestore.ErrOverLimit) {
		recordsIndexed.WithLabelValues("upsert").Add(float64(len(upserts)))
		recordsIndexed.WithLabelValues("delete").Add(float64(len(deletes)))
	}
	if errors.Is(err, spacestore.ErrOverLimit) {
		overLimit.Inc()
		ix.log().Warn("space is over a limit; some memories were not indexed", "space", spaceURI, "repo", did)
		return nil
	}
	return err
}

// applyConfig records the authority's config record (nil when deleted).
func (ix *Indexer) applyConfig(ctx context.Context, spaceURI string, rec map[string]any) (bool, error) {
	var cfg *lex.Config
	if rec != nil {
		c, err := lex.ParseConfig(rec)
		if err != nil {
			ix.log().Warn("ignoring invalid space config", "space", spaceURI, "err", err)
			return false, nil
		}
		cfg = c
	}
	resync, err := ix.Store.SetConfig(ctx, spaceURI, cfg)
	if err != nil {
		return false, err
	}
	if resync {
		ix.log().Info("space model changed; syncing every repo again", "space", spaceURI, "model", cfg.ModelInfo, "next", cfg.Next)
		ix.markResync(spaceURI)
	}
	return resync, nil
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
	if did == ref.Authority {
		var cfgRec map[string]any
		for _, r := range vr.Records {
			if r.Collection == lex.ConfigCollection && r.Rkey == lex.ConfigRkey {
				cfgRec = r.Record
			}
		}
		if _, err := ix.applyConfig(ctx, spaceURI, cfgRec); err != nil {
			return err
		}
	}
	var upserts []spacestore.Memory
	for _, r := range vr.Records {
		if r.Collection != Collection {
			continue
		}
		if m, ok := memoryFromRecord(did, r.Rkey, r.Cid.String(), r.Record); ok {
			upserts = append(upserts, m)
		}
	}
	next := spacestore.RepoPosition{Rev: vr.Commit.Rev, SetHash: vr.Repo.SetHash.State(), SpaceRev: spaceRev}
	if err := ix.apply(ctx, spaceURI, did, next, upserts, nil, true); err != nil {
		return err
	}
	ix.log().Info("exported repo", "space", spaceURI, "repo", did, "memories", len(upserts), "rev", vr.Commit.Rev)
	return nil
}

// memoryFromRecord reads a garden.engram.memory record. Records without
// text are not indexed. Its vectors are passed on keyed by model; the index
// decides which match the space's model.
func memoryFromRecord(did, rkey, cidStr string, rec map[string]any) (spacestore.Memory, bool) {
	text, _ := rec["text"].(string)
	if strings.TrimSpace(text) == "" {
		return spacestore.Memory{}, false
	}
	m := spacestore.Memory{Author: did, Rkey: rkey, CID: cidStr, Text: text, Vectors: map[string][]float32{}}
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
	for _, e := range lex.ParseEmbeddings(rec) {
		m.Vectors[e.Key()] = e.Vector
	}
	return m, true
}
