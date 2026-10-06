// Package spacetest runs an in-memory Spaces network for tests: one HTTP
// server acting as every account's PDS and as the space authority, with
// tokens, credentials, commits and CARs built by the real space package.
package spacetest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bluesky-social/indigo/atproto/atclient"
	"github.com/bluesky-social/indigo/atproto/atcrypto"
	"github.com/bluesky-social/indigo/atproto/atdata"
	"github.com/bluesky-social/indigo/atproto/auth"
	"github.com/bluesky-social/indigo/atproto/identity"
	"github.com/bluesky-social/indigo/atproto/syntax"
	"github.com/haileyok/cocoon/space"
	"github.com/ipfs/go-cid"
)

// Account is a DID on the network with a space repo.
type Account struct {
	DID string
	Key *atcrypto.PrivateKeyK256

	records  map[string]space.SerializedRecord // path -> record
	ops      []op
	rev      string
	spaceRev string
}

type op struct {
	Rev        string
	Collection string
	Rkey       string
	Cid        *cid.Cid
	Prev       *cid.Cid
	Value      map[string]any
}

// Net is the fake network.
type Net struct {
	T         testing.TB
	Dir       *identity.MockDirectory
	Server    *httptest.Server
	Space     string
	Authority *Account

	mu       sync.Mutex
	accounts map[string]*Account
	members  map[string]bool
	clock    *syntax.TIDClock
	calls    map[string]int

	// Tamper knobs.
	CorruptCommits bool // sign commits over the wrong contents
	OmitValues     bool // leave values off listRepoOps entries
}

// New starts a network whose authority governs one space.
func New(t testing.TB) *Net {
	t.Helper()
	n := &Net{
		T:        t,
		accounts: map[string]*Account{},
		members:  map[string]bool{},
		clock:    syntax.NewTIDClock(0),
		calls:    map[string]int{},
	}
	n.Dir = identity.NewMockDirectory()
	n.Server = httptest.NewServer(http.HandlerFunc(n.serve))
	t.Cleanup(n.Server.Close)
	n.Authority = n.NewAccount("did:plc:authority")
	n.Space = "at://" + n.Authority.DID + "/space/garden.engram.space/memory"
	return n
}

// NewAccount registers a DID whose PDS is this network.
func (n *Net) NewAccount(did string) *Account {
	n.T.Helper()
	key, err := atcrypto.GeneratePrivateKeyK256()
	if err != nil {
		n.T.Fatal(err)
	}
	pub, err := key.PublicKey()
	if err != nil {
		n.T.Fatal(err)
	}
	a := &Account{DID: did, Key: key, records: map[string]space.SerializedRecord{}}
	n.mu.Lock()
	n.accounts[did] = a
	n.mu.Unlock()
	n.Dir.Insert(identity.Identity{
		DID:      syntax.DID(did),
		Handle:   syntax.HandleInvalid,
		Keys:     map[string]identity.VerificationMethod{"atproto": {Type: "Multikey", PublicKeyMultibase: pub.Multibase()}},
		Services: map[string]identity.ServiceEndpoint{"atproto_pds": {Type: "AtprotoPersonalDataServer", URL: n.Server.URL}},
	})
	return a
}

// RotateKey gives an account a new signing key and republishes its DID doc.
func (n *Net) RotateKey(a *Account) {
	n.T.Helper()
	key, err := atcrypto.GeneratePrivateKeyK256()
	if err != nil {
		n.T.Fatal(err)
	}
	pub, _ := key.PublicKey()
	n.mu.Lock()
	a.Key = key
	n.mu.Unlock()
	n.Dir.Insert(identity.Identity{
		DID:      syntax.DID(a.DID),
		Handle:   syntax.HandleInvalid,
		Keys:     map[string]identity.VerificationMethod{"atproto": {Type: "Multikey", PublicKeyMultibase: pub.Multibase()}},
		Services: map[string]identity.ServiceEndpoint{"atproto_pds": {Type: "AtprotoPersonalDataServer", URL: n.Server.URL}},
	})
}

// AddMember lets an account obtain credentials for the space.
func (n *Net) AddMember(did string) {
	n.mu.Lock()
	n.members[did] = true
	n.mu.Unlock()
}

// RemoveMember revokes an account's ability to obtain new credentials.
func (n *Net) RemoveMember(did string) {
	n.mu.Lock()
	delete(n.members, did)
	n.mu.Unlock()
}

// Calls reports how often an XRPC method was served.
func (n *Net) Calls(nsid string) int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.calls[nsid]
}

// Put creates or updates a record in an account's repo, returning its URI
// and the write's spaceRev.
func (n *Net) Put(a *Account, collection, rkey string, value map[string]any) (string, string) {
	n.T.Helper()
	// Round-trip through JSON so values match what a client would send.
	raw, _ := json.Marshal(value)
	val, err := atdata.UnmarshalJSON(raw)
	if err != nil {
		n.T.Fatal(err)
	}
	ser, err := space.SerializeRecord(collection, rkey, val)
	if err != nil {
		n.T.Fatal(err)
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	path := space.FormatRecordPath(collection, rkey)
	o := op{Rev: n.clock.Next().String(), Collection: collection, Rkey: rkey, Cid: &ser.Cid, Value: val}
	if prev, ok := a.records[path]; ok {
		p := prev.Cid
		o.Prev = &p
	}
	a.records[path] = ser
	a.ops = append(a.ops, o)
	a.rev = o.Rev
	a.spaceRev = n.clock.Next().String()
	ref, _ := space.ParseRef(n.Space)
	return ref.RecordURI(a.DID, collection, rkey), a.spaceRev
}

// Delete removes a record, returning the write's spaceRev.
func (n *Net) Delete(a *Account, collection, rkey string) string {
	n.T.Helper()
	n.mu.Lock()
	defer n.mu.Unlock()
	path := space.FormatRecordPath(collection, rkey)
	prev, ok := a.records[path]
	if !ok {
		n.T.Fatalf("no record %s", path)
	}
	p := prev.Cid
	delete(a.records, path)
	o := op{Rev: n.clock.Next().String(), Collection: collection, Rkey: rkey, Prev: &p}
	a.ops = append(a.ops, o)
	a.rev = o.Rev
	a.spaceRev = n.clock.Next().String()
	return a.spaceRev
}

// Session returns an API client authenticated as the account on its PDS.
func (n *Net) Session(a *Account) *atclient.APIClient {
	did := syntax.DID(a.DID)
	c := atclient.NewAPIClient(n.Server.URL)
	c.Auth = testAuth{did: a.DID}
	c.AccountDID = &did
	return c
}

type testAuth struct{ did string }

func (t testAuth) DoWithAuth(c *http.Client, req *http.Request, _ syntax.NSID) (*http.Response, error) {
	req.Header.Set("X-Test-Did", t.did)
	return c.Do(req)
}

// ServiceAuth mints a service-auth JWT from the authority, as it signs the
// notifications it forwards.
func (n *Net) ServiceAuth(aud, lxm string) string {
	n.T.Helper()
	tok, err := serviceJWT(n.Authority, aud, lxm)
	if err != nil {
		n.T.Fatal(err)
	}
	return tok
}

func serviceJWT(a *Account, aud, lxm string) (string, error) {
	nsid, err := syntax.ParseNSID(lxm)
	if err != nil {
		return "", err
	}
	return auth.SignServiceAuth(syntax.DID(a.DID), aud, time.Minute, &nsid, a.Key)
}

// ---- HTTP ----

type xerr struct {
	status int
	name   string
	msg    string
}

func (n *Net) serve(w http.ResponseWriter, r *http.Request) {
	nsid := strings.TrimPrefix(r.URL.Path, "/xrpc/")
	n.mu.Lock()
	n.calls[nsid]++
	n.mu.Unlock()
	out, err := n.route(r, nsid, w)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(err.status)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": err.name, "message": err.msg})
		return
	}
	if out != nil {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	}
}

func (n *Net) route(r *http.Request, nsid string, w http.ResponseWriter) (any, *xerr) {
	q := r.URL.Query()
	switch nsid {
	case "com.atproto.space.getDelegationToken":
		a := n.account(r.Header.Get("X-Test-Did"))
		if a == nil {
			return nil, &xerr{401, "AuthRequired", "no session"}
		}
		ref, err := space.ParseRef(q.Get("space"))
		if err != nil {
			return nil, &xerr{400, "InvalidRequest", err.Error()}
		}
		tok, err := space.CreateSpaceToken(space.TokenDelegation, space.CreateTokenOpts{Iss: a.DID, Sub: ref.String(), Aud: ref.HostAud()}, a.Key)
		if err != nil {
			return nil, &xerr{500, "InternalError", err.Error()}
		}
		return map[string]string{"token": tok}, nil

	case "com.atproto.space.getSpaceCredential":
		var body struct {
			Space string `json:"space"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		jwt, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok {
			return nil, &xerr{401, "MissingJwt", "missing delegation token"}
		}
		tok, err := space.VerifySpaceToken(space.TokenDelegation, jwt, space.VerifyTokenOpts{GetSigningKey: n.keyFunc(r.Context())})
		if err != nil {
			return nil, &xerr{401, "BadJwt", err.Error()}
		}
		keyID, err := space.VerifySpaceSignature(r.Header, "")
		if err != nil {
			return nil, &xerr{401, "BadSpaceSignature", err.Error()}
		}
		if tok.Payload.Sub != body.Space || body.Space != n.Space {
			return nil, &xerr{400, "InvalidDelegationToken", "wrong space"}
		}
		n.mu.Lock()
		member := n.members[tok.Payload.Iss]
		n.mu.Unlock()
		if !member {
			return nil, &xerr{403, "Forbidden", "not a member"}
		}
		cred, err := space.CreateSpaceToken(space.TokenCredential, space.CreateTokenOpts{Iss: n.Authority.DID, Sub: n.Space, KeyID: keyID}, n.Authority.Key)
		if err != nil {
			return nil, &xerr{500, "InternalError", err.Error()}
		}
		return map[string]string{"credential": cred}, nil

	case "com.atproto.space.listRepos":
		if e := n.checkCredential(r, n.Authority.DID); e != nil {
			return nil, e
		}
		n.mu.Lock()
		defer n.mu.Unlock()
		repos := []map[string]string{}
		for _, a := range n.sortedAccounts() {
			if a.rev == "" {
				continue
			}
			repos = append(repos, map[string]string{"did": a.DID, "repoRev": a.rev, "hash": "x", "spaceRev": a.spaceRev})
		}
		return map[string]any{"repos": repos}, nil

	case "com.atproto.space.listRepoOps":
		repo := q.Get("repo")
		if e := n.checkCredential(r, repo); e != nil {
			return nil, e
		}
		return n.listRepoOps(repo, q.Get("since"), q.Get("cursor"), q.Get("limit"))

	case "com.atproto.space.getRepo":
		repo := q.Get("repo")
		if e := n.checkCredential(r, repo); e != nil {
			return nil, e
		}
		n.mu.Lock()
		defer n.mu.Unlock()
		a := n.accounts[repo]
		if a == nil || a.rev == "" {
			return nil, &xerr{404, "RepoNotFound", "no repo"}
		}
		commit, records, e := n.commit(a)
		if e != nil {
			return nil, e
		}
		var buf bytes.Buffer
		if err := space.SerializeRepo(&buf, commit, records, false); err != nil {
			return nil, &xerr{500, "InternalError", err.Error()}
		}
		w.Header().Set("Content-Type", "application/vnd.ipld.car")
		_, _ = w.Write(buf.Bytes())
		return nil, nil
	}
	return nil, &xerr{404, "MethodNotImplemented", nsid}
}

func (n *Net) account(did string) *Account {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.accounts[did]
}

func (n *Net) sortedAccounts() []*Account {
	out := make([]*Account, 0, len(n.accounts))
	for _, a := range n.accounts {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].DID < out[j].DID })
	return out
}

func (n *Net) keyFunc(ctx context.Context) space.SigningKeyFunc {
	return func(iss, kid string, fresh bool) (string, error) {
		ident, err := n.Dir.LookupDID(ctx, syntax.DID(iss))
		if err != nil {
			return "", err
		}
		pub, err := ident.GetPublicKey(strings.TrimPrefix(kid, "#"))
		if err != nil {
			return "", err
		}
		return pub.DIDKey(), nil
	}
}

func (n *Net) checkCredential(r *http.Request, audience string) *xerr {
	jwt, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Atproto-Space ")
	if !ok {
		return &xerr{401, "MissingJwt", "missing space credential"}
	}
	tok, err := space.VerifySpaceToken(space.TokenCredential, jwt, space.VerifyTokenOpts{GetSigningKey: n.keyFunc(r.Context())})
	if err != nil {
		return &xerr{401, "BadJwt", err.Error()}
	}
	if tok.Payload.Sub != n.Space || tok.Payload.Iss != n.Authority.DID {
		return &xerr{401, "BadJwt", "credential for another space"}
	}
	if _, err := space.VerifySpaceSignature(r.Header, tok.Payload.Cnf.Kid); err != nil {
		return &xerr{401, "BadSpaceSignature", err.Error()}
	}
	if got := r.Header.Get(space.HeaderSpaceAudience); got != audience {
		return &xerr{401, "BadSpaceSignature", fmt.Sprintf("audience %q, want %q", got, audience)}
	}
	return nil
}

func (n *Net) commit(a *Account) (space.SignedCommit, []space.SerializedRecord, *xerr) {
	var refs []space.RecordRef
	var records []space.SerializedRecord
	paths := make([]string, 0, len(a.records))
	for p := range a.records {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		r := a.records[p]
		refs = append(refs, space.RecordRef{Collection: r.Collection, Rkey: r.Rkey, Cid: r.Cid})
		records = append(records, r)
	}
	repo := space.RepoCommitFromRecords(refs)
	if n.CorruptCommits {
		repo.SetHash.Add("bogus")
	}
	c, err := repo.Sign(space.CommitCtx{Space: n.Space, Author: a.DID, Rev: a.rev}, a.Key)
	if err != nil {
		return c, nil, &xerr{500, "InternalError", err.Error()}
	}
	return c, records, nil
}

func (n *Net) listRepoOps(repo, since, cursor, limitStr string) (any, *xerr) {
	n.mu.Lock()
	defer n.mu.Unlock()
	a := n.accounts[repo]
	if a == nil || a.rev == "" {
		return nil, &xerr{404, "RepoNotFound", "no repo"}
	}
	limit := 100
	if limitStr != "" {
		limit, _ = strconv.Atoi(limitStr)
	}
	start := 0
	if cursor != "" {
		start, _ = strconv.Atoi(cursor)
	} else if since != "" {
		for start < len(a.ops) && a.ops[start].Rev <= since {
			start++
		}
	}
	end := min(start+limit, len(a.ops))
	// A value is inlined only for the latest version of a record.
	latest := map[string]int{}
	for i, o := range a.ops {
		latest[space.FormatRecordPath(o.Collection, o.Rkey)] = i
	}
	ops := []map[string]any{}
	for i := start; i < end; i++ {
		o := a.ops[i]
		m := map[string]any{"rev": o.Rev, "collection": o.Collection, "rkey": o.Rkey, "cid": nil, "prev": nil}
		if o.Cid != nil {
			m["cid"] = o.Cid.String()
		}
		if o.Prev != nil {
			m["prev"] = o.Prev.String()
		}
		if o.Value != nil && !n.OmitValues && latest[space.FormatRecordPath(o.Collection, o.Rkey)] == i {
			m["value"] = o.Value
		}
		ops = append(ops, m)
	}
	res := map[string]any{"ops": ops}
	if end-start < limit {
		c, _, e := n.commit(a)
		if e != nil {
			return nil, e
		}
		res["commit"] = c
	} else {
		res["cursor"] = strconv.Itoa(end)
	}
	return res, nil
}
