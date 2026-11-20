package indexer

import (
	"context"
	"errors"
	"fmt"
	"net/url"

	"github.com/bluesky-social/indigo/atproto/atdata"
	"github.com/bluesky-social/indigo/atproto/identity"
	"github.com/bluesky-social/indigo/atproto/syntax"
	"github.com/haileyok/cocoon/space"
	"github.com/ipfs/go-cid"

	"github.com/haileyok/engram-garden/internal/spaceclient"
)

// Reader reads members' repos in a space and verifies what it reads against
// the MAC the repo host returns for each commit. The memory indexer and the
// lounge both read through it; they differ only in what they keep.
type Reader struct {
	Client *spaceclient.Client
	Dir    identity.Directory
	// PageSize is the listRepoOps page size (default 500).
	PageSize int
}

// ErrResync marks an incremental read that can't be trusted or applied; a
// full export replaces it.
var ErrResync = errResync

// ErrKemRotated marks a commit or archive whose MAC didn't verify with the
// author's current #atproto_kem key: read again with fresh set.
var ErrKemRotated = errKemRotated

// Change is one record's latest state in a verified read of a repo's oplog.
// Record is nil when the record was deleted.
type Change struct {
	Collection string
	Rkey       string
	Cid        *cid.Cid
	Record     map[string]any
}

// Position is where a read of a repo stands: the last oplog rev applied and
// the set hash over every record applied so far.
type Position struct {
	Rev     string
	SetHash []byte
}

// purge drops a cached DID document, so the next read resolves the author's
// #atproto_kem key again.
func (rd *Reader) purge(ctx context.Context, did string) {
	d, err := syntax.ParseDID(did)
	if err != nil || rd.Dir == nil {
		return
	}
	_ = rd.Dir.Purge(ctx, d.AtIdentifier())
}

// ReadOps reads a repo's oplog since pos and verifies it against the repo's
// commit. It returns the latest state of each changed record that want
// accepts (nil accepts all), in the order they first changed, and the new
// position. Values are checked against their CIDs. fresh refetches the
// author's DID document first.
func (rd *Reader) ReadOps(ctx context.Context, spaceURI, did string, pos Position, fresh bool, want func(collection, rkey string) bool) ([]Change, Position, *space.Commit, error) {
	host, err := rd.Client.PDSHost(ctx, did)
	if err != nil {
		return nil, pos, nil, err
	}
	if fresh {
		rd.purge(ctx, did)
	}
	repo, err := space.RepoCommitFromState(pos.SetHash)
	if err != nil {
		return nil, pos, nil, fmt.Errorf("%w: stored set hash: %v", errResync, err)
	}
	pageSize := rd.PageSize
	if pageSize <= 0 {
		pageSize = 500
	}
	final := map[string]*pathState{}
	var order []string
	var (
		commit *space.Commit
		mac    *spaceclient.Mac
	)
	cursor := ""
	for {
		params := url.Values{"space": {spaceURI}, "repo": {did}, "limit": {fmt.Sprint(pageSize)}}
		if cursor != "" {
			params.Set("cursor", cursor)
		} else if pos.Rev != "" {
			params.Set("since", pos.Rev)
		}
		var out struct {
			Ops    []opEntry     `json:"ops"`
			Cursor string        `json:"cursor"`
			Commit *space.Commit `json:"commit"`
		}
		pageMac, err := rd.Client.QueryMac(ctx, host, spaceURI, did, space.CommitMacProfile, "com.atproto.space.listRepoOps", params, &out)
		if err != nil {
			return nil, pos, nil, fmt.Errorf("listRepoOps: %w", err)
		}
		for _, op := range out.Ops {
			rop, err := parseOp(op)
			if err != nil {
				return nil, pos, nil, fmt.Errorf("%w: %v", errResync, err)
			}
			repo.ApplyOp(rop)
			path := space.FormatRecordPath(op.Collection, op.Rkey)
			if _, seen := final[path]; !seen {
				order = append(order, path)
			}
			final[path] = &pathState{cid: rop.Cid, value: op.Value}
		}
		// Every page carries the repo's current commit, but only the last
		// page's, which has no cursor, says what the ops should add up to.
		if out.Cursor == "" {
			commit, mac = out.Commit, pageMac
			break
		}
		if out.Cursor == cursor {
			return nil, pos, nil, fmt.Errorf("%w: oplog cursor did not advance", errResync)
		}
		cursor = out.Cursor
	}
	if commit == nil {
		return nil, pos, nil, fmt.Errorf("%w: oplog ended without a commit", errResync)
	}
	if err := verifyCommit(spaceURI, did, *commit, repo, mac); err != nil {
		return nil, pos, nil, err
	}

	var changes []Change
	for _, path := range order {
		ps := final[path]
		coll, rkey, err := space.ParseRecordPath(path)
		if err != nil {
			continue
		}
		if want != nil && !want(coll, rkey) {
			continue
		}
		c := Change{Collection: coll, Rkey: rkey, Cid: ps.cid}
		if ps.cid != nil {
			if len(ps.value) == 0 {
				return nil, pos, nil, fmt.Errorf("%w: no value for the latest version of %s", errResync, path)
			}
			rec, err := atdata.UnmarshalJSON(ps.value)
			if err != nil {
				return nil, pos, nil, fmt.Errorf("%w: %s: %v", errResync, path, err)
			}
			ser, err := space.SerializeRecord(coll, rkey, rec)
			if err != nil || !ser.Cid.Equals(*ps.cid) {
				return nil, pos, nil, fmt.Errorf("%w: %s: value does not match its cid", errResync, path)
			}
			c.Record = rec
		}
		changes = append(changes, c)
	}
	return changes, Position{Rev: commit.Rev, SetHash: repo.SetHash.State()}, commit, nil
}

// ErrRepoNotFound means the member has no repo in the space (any more).
var ErrRepoNotFound = errors.New("repo not found")

// ReadRepo exports a member's whole repo and verifies it. It returns every
// record and the position the export stands at. fresh refetches the
// author's DID document first.
func (rd *Reader) ReadRepo(ctx context.Context, spaceURI, did string, fresh bool) ([]space.VerifiedRecord, Position, *space.Commit, error) {
	host, err := rd.Client.PDSHost(ctx, did)
	if err != nil {
		return nil, Position{}, nil, err
	}
	if fresh {
		rd.purge(ctx, did)
	}
	mac, body, err := rd.Client.QueryRawMac(ctx, host, spaceURI, did, space.CommitMacProfile, "com.atproto.space.getRepo", url.Values{"space": {spaceURI}, "repo": {did}})
	if err != nil {
		if spaceclient.IsError(err, "RepoNotFound") {
			return nil, Position{}, nil, ErrRepoNotFound
		}
		return nil, Position{}, nil, fmt.Errorf("getRepo: %w", err)
	}
	defer body.Close()
	vr, err := space.VerifyRepoStarFull(body, space.VerifyRepoParams{Space: spaceURI, Repo: did, VerifyMacParams: mac.Params()})
	if spaceclient.IsError(err, "RepoNotFound") {
		return nil, Position{}, nil, ErrRepoNotFound
	}
	if err != nil {
		var (
			verr *space.RepoVerificationError
			merr *space.MacError
		)
		if (errors.As(err, &verr) || errors.As(err, &merr)) && !fresh {
			return nil, Position{}, nil, fmt.Errorf("%w: %v", errKemRotated, err)
		}
		return nil, Position{}, nil, fmt.Errorf("verifying %s's repo: %w", did, err)
	}
	refs := make([]space.RecordRef, 0, len(vr.Records))
	for _, r := range vr.Records {
		refs = append(refs, space.RecordRef{Collection: r.Collection, Rkey: r.Rkey, Cid: r.Cid})
	}
	state := space.RepoCommitFromRecords(refs).SetHash.State()
	return vr.Records, Position{Rev: vr.Commit.Rev, SetHash: state}, &vr.Commit, nil
}

// Writer is a listWriters entry: a repo's latest write in the space, or its
// removal from the writer set.
type Writer struct {
	DID      string
	SpaceRev string
	Removed  bool
}

// ListWriters lists the space's whole writer set at its authority.
func (rd *Reader) ListWriters(ctx context.Context, spaceURI string) ([]Writer, error) {
	ref, err := space.ParseRef(spaceURI)
	if err != nil {
		return nil, err
	}
	host, err := rd.Client.SpaceHost(ctx, ref.Authority)
	if err != nil {
		return nil, err
	}
	var out []Writer
	cursor := ""
	for {
		const pageSize = 100
		params := url.Values{"space": {spaceURI}, "limit": {fmt.Sprint(pageSize)}}
		if cursor != "" {
			params.Set("cursor", cursor)
		}
		var page struct {
			Repos  []listedRepo `json:"repos"`
			Cursor string       `json:"cursor"`
		}
		if err := rd.Client.Query(ctx, host, spaceURI, ref.Authority, "com.atproto.space.listWriters", params, &page); err != nil {
			return nil, err
		}
		for _, r := range page.Repos {
			out = append(out, Writer{DID: r.DID, SpaceRev: r.SpaceRev, Removed: r.removed()})
		}
		if page.Cursor == "" || len(page.Repos) < pageSize {
			return out, nil
		}
		cursor = page.Cursor
	}
}
