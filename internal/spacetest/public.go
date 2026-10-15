package spacetest

import (
	"encoding/json"
	"net/http"
	"sort"
	"strconv"

	"github.com/bluesky-social/indigo/atproto/atdata"
	"github.com/haileyok/cocoon/space"
)

// publicRecord is a record in an account's public repo.
type publicRecord struct {
	Collection string
	Rkey       string
	Cid        string
	Raw        json.RawMessage
}

// PutPublic creates or replaces a record in an account's public repo,
// returning its at:// URI and CID.
func (n *Net) PutPublic(a *Account, collection, rkey string, value map[string]any) (string, string) {
	n.T.Helper()
	rec, err := n.publicRecord(collection, rkey, value)
	if err != nil {
		n.T.Fatal(err)
	}
	n.mu.Lock()
	a.public[space.FormatRecordPath(collection, rkey)] = rec
	n.mu.Unlock()
	return publicURI(a.DID, collection, rkey), rec.Cid
}

func (n *Net) publicRecord(collection, rkey string, value map[string]any) (publicRecord, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return publicRecord{}, err
	}
	val, err := atdata.UnmarshalJSON(raw)
	if err != nil {
		return publicRecord{}, err
	}
	ser, err := space.SerializeRecord(collection, rkey, val)
	if err != nil {
		return publicRecord{}, err
	}
	return publicRecord{Collection: collection, Rkey: rkey, Cid: ser.Cid.String(), Raw: raw}, nil
}

func publicURI(did, collection, rkey string) string {
	return "at://" + did + "/" + collection + "/" + rkey
}

// routePublic serves the public repo methods: reads need no auth, writes
// need the account's session. There are no commits or sync.
func (n *Net) routePublic(r *http.Request, nsid string) (any, *xerr) {
	q := r.URL.Query()
	switch nsid {
	case "com.atproto.repo.getRecord":
		n.mu.Lock()
		defer n.mu.Unlock()
		a := n.accounts[q.Get("repo")]
		if a == nil {
			return nil, &xerr{400, "RepoNotFound", "no repo"}
		}
		rec, ok := a.public[space.FormatRecordPath(q.Get("collection"), q.Get("rkey"))]
		if !ok {
			return nil, &xerr{400, "RecordNotFound", "no record"}
		}
		return map[string]any{"uri": publicURI(a.DID, rec.Collection, rec.Rkey), "cid": rec.Cid, "value": rec.Raw}, nil

	case "com.atproto.repo.listRecords":
		n.mu.Lock()
		defer n.mu.Unlock()
		a := n.accounts[q.Get("repo")]
		if a == nil {
			return nil, &xerr{400, "RepoNotFound", "no repo"}
		}
		coll := q.Get("collection")
		var paths []string
		for p, rec := range a.public {
			if rec.Collection == coll {
				paths = append(paths, p)
			}
		}
		sort.Strings(paths)
		limit := 50
		if l, err := strconv.Atoi(q.Get("limit")); err == nil && l > 0 {
			limit = l
		}
		start := 0
		if c := q.Get("cursor"); c != "" {
			start = sort.SearchStrings(paths, coll+"/"+c)
			if start < len(paths) && paths[start] == coll+"/"+c {
				start++
			}
		}
		end := min(start+limit, len(paths))
		records := []map[string]any{}
		for _, p := range paths[start:end] {
			rec := a.public[p]
			records = append(records, map[string]any{"uri": publicURI(a.DID, rec.Collection, rec.Rkey), "cid": rec.Cid, "value": rec.Raw})
		}
		res := map[string]any{"records": records}
		if end < len(paths) {
			res["cursor"] = a.public[paths[end-1]].Rkey
		}
		return res, nil

	case "com.atproto.repo.createRecord", "com.atproto.repo.putRecord", "com.atproto.repo.deleteRecord":
		a := n.account(r.Header.Get("X-Test-Did"))
		if a == nil {
			return nil, &xerr{401, "AuthRequired", "no session"}
		}
		var body struct {
			Repo       string         `json:"repo"`
			Collection string         `json:"collection"`
			Rkey       string         `json:"rkey"`
			Record     map[string]any `json:"record"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			return nil, &xerr{400, "InvalidRequest", err.Error()}
		}
		if body.Repo != a.DID {
			return nil, &xerr{403, "Forbidden", "can only write your own repo"}
		}
		path := space.FormatRecordPath(body.Collection, body.Rkey)
		n.mu.Lock()
		_, exists := a.public[path]
		n.mu.Unlock()
		if nsid == "com.atproto.repo.deleteRecord" {
			n.mu.Lock()
			delete(a.public, path)
			n.mu.Unlock()
			return map[string]any{}, nil
		}
		if body.Rkey == "" {
			n.mu.Lock()
			body.Rkey = n.clock.Next().String()
			n.mu.Unlock()
		} else if exists && nsid == "com.atproto.repo.createRecord" {
			return nil, &xerr{400, "RecordAlreadyExists", "Record already exists"}
		}
		if body.Record["$type"] == nil {
			body.Record["$type"] = body.Collection
		}
		rec, err := n.publicRecord(body.Collection, body.Rkey, body.Record)
		if err != nil {
			return nil, &xerr{400, "InvalidRecord", err.Error()}
		}
		n.mu.Lock()
		a.public[space.FormatRecordPath(body.Collection, body.Rkey)] = rec
		n.mu.Unlock()
		return map[string]any{"uri": publicURI(a.DID, body.Collection, body.Rkey), "cid": rec.Cid}, nil
	}
	return nil, &xerr{404, "MethodNotImplemented", nsid}
}
