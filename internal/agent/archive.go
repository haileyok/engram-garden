package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/bluesky-social/indigo/atproto/syntax"
	"github.com/haileyok/cocoon/space"
	"github.com/ipfs/go-cid"

	"github.com/haileyok/engram-garden/internal/lex"
	"github.com/haileyok/engram-garden/internal/spaceclient"
)

// Archive is where agent sessions keep their transcripts: a
// garden.engram.archive space, each session writing its own
// garden.engram.session record (and the transcript's blobs) as its own
// identity. An Archive opened as an identity pushes its own sessions and
// reads everyone's; one opened as the principal (the archive's authority)
// only reads. See docs/design/sessions.md.
type Archive struct {
	// Client acts as the identity, or as the principal.
	Client *spaceclient.Client
	// Space is the archive's URI.
	Space string
	// AppviewURL and AppviewDID locate the appview that indexes the archive.
	AppviewURL string
	AppviewDID string
	// Identity is the session's own identity: its kind and parent go in
	// every record it writes. Zero for a reader.
	Identity Identity
	// Now is the clock (default time.Now).
	Now func() time.Time

	mu    sync.Mutex
	locks map[string]*sync.Mutex
	// tails holds each session's last chunk, which the next push grows,
	// so it isn't fetched back from the PDS every time.
	tails map[string]tailChunk
}

type tailChunk struct {
	link string
	data string
}

// Sizes.
const (
	// MaxChunkBytes is the most one blob holds. The last chunk grows until
	// it reaches this, so a session has about one chunk per MaxChunkBytes.
	MaxChunkBytes = 3 << 20
	// MaxPushBytes is the most one push carries; a harness sends more in
	// several pushes. It is also the longest line a transcript can have: a
	// line longer than MaxChunkBytes is a chunk of its own.
	MaxPushBytes = 8 << 20
)

// SessionInfo is a session as an agent sees it: the record, and who wrote it.
type SessionInfo struct {
	lex.Session
	URI    string `json:"uri"`
	Author string `json:"author"`
	CID    string `json:"cid,omitempty"`
}

type PushSessionIn struct {
	// Session names the session and says where the work stands. The archive
	// fills in kind, parent, head, chunks and updatedAt.
	Session lex.Session `json:"session"`
	// FromBytes is how much of the transcript the caller believes is
	// stored, so this push is the rest. When it isn't what the record says,
	// the answer asks for a resync and nothing is written.
	FromBytes int64 `json:"fromBytes,omitempty"`
	// Replace stores Data as the start of the transcript, discarding what
	// was stored: the answer to a resync.
	Replace bool `json:"replace,omitempty"`
	// Data is the transcript from FromBytes: complete JSONL lines, at most
	// MaxPushBytes. A trailing line without its newline is held back.
	Data string `json:"data"`
}

type PushSessionOut struct {
	// Session and URI say what was written.
	Session string `json:"session"`
	URI     string `json:"uri,omitempty"`
	// Bytes and Entries are how much of the transcript is stored now, and
	// Stored how much of this push was (a partial trailing line isn't).
	Bytes   int64 `json:"bytes"`
	Entries int   `json:"entries"`
	Chunks  int   `json:"chunks"`
	Stored  int64 `json:"storedBytes"`
	// Resync means the record holds something else: push the whole file,
	// replace first.
	Resync bool `json:"resync,omitempty"`
}

type GetSessionIn struct {
	// Session is the session's ID, or its record's at:// URI.
	Session string `json:"session" jsonschema:"the session's ID, or its record's at:// URI"`
	// Author picks whose session, when the ID alone could be several
	// (default: your own, else the newest).
	Author string `json:"author,omitempty" jsonschema:"the DID of the session's agent, when the ID isn't enough"`
}

type PullSessionOut struct {
	SessionInfo
	// Data is the whole transcript: complete JSONL lines.
	Data string `json:"data"`
}

type ListSessionsIn struct {
	// Author lists one agent's sessions only.
	Author string `json:"author,omitempty" jsonschema:"only this agent's sessions (a DID)"`
}

type ListSessionsOut struct {
	Sessions []SessionInfo `json:"sessions"`
	Note     string        `json:"note,omitempty"`
}

type DeleteSessionIn struct {
	Session string `json:"session"`
}

type DeleteSessionOut struct {
	Deleted string `json:"deleted"`
}

func (a *Archive) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

func (a *Archive) self() string { return a.Client.DID().String() }

// reader reads the archive's members and records as a space like any other.
func (a *Archive) reader() *Agent { return &Agent{Client: a.Client, Space: a.Space} }

func (a *Archive) ref() (space.Ref, error) {
	ref, err := space.ParseRef(a.Space)
	if err != nil {
		return ref, err
	}
	if ref.Type != lex.ArchiveType {
		return ref, fmt.Errorf("%s isn't an archive (%s)", a.Space, lex.ArchiveType)
	}
	return ref, nil
}

// lock serializes pushes to one session: the record is read, changed and
// written back, and the space has no compare-and-swap for records.
func (a *Archive) lock(id string) func() {
	a.mu.Lock()
	if a.locks == nil {
		a.locks = map[string]*sync.Mutex{}
	}
	l := a.locks[id]
	if l == nil {
		l = &sync.Mutex{}
		a.locks[id] = l
	}
	a.mu.Unlock()
	l.Lock()
	return l.Unlock
}

// ---- reading ----

// getSession reads one member's session record, through the MAC'd path a
// member's PDS requires for reading another member's records.
func (a *Archive) getSession(ctx context.Context, did, id string) (*SessionInfo, error) {
	ref, err := a.ref()
	if err != nil {
		return nil, err
	}
	var out struct {
		CID   string          `json:"cid"`
		Value json.RawMessage `json:"value"`
	}
	if did == a.self() && a.Client.Session != nil {
		params := map[string]any{"space": a.Space, "repo": did, "collection": lex.SessionCollection, "rkey": id}
		err = a.Client.Session.Get(ctx, "com.atproto.space.getRecord", params, &out)
	} else {
		var host string
		if host, err = a.Client.PDSHost(ctx, did); err == nil {
			params := url.Values{"space": {a.Space}, "repo": {did}, "collection": {lex.SessionCollection}, "rkey": {id}}
			_, err = a.Client.QueryMac(ctx, host, a.Space, did, space.RecordMacProfile, "com.atproto.space.getRecord", params, &out)
		}
	}
	if err != nil {
		if isNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading session %s: %w", id, err)
	}
	var s lex.Session
	if err := json.Unmarshal(out.Value, &s); err != nil {
		return nil, err
	}
	s.SessionID = id
	return &SessionInfo{Session: s, URI: ref.RecordURI(did, lex.SessionCollection, id), Author: did, CID: out.CID}, nil
}

// memberSessions reads one member's session records.
func (a *Archive) memberSessions(ctx context.Context, did string) ([]SessionInfo, error) {
	recs, err := a.reader().listRecords(ctx, did, lex.SessionCollection)
	if err != nil {
		return nil, err
	}
	out := make([]SessionInfo, 0, len(recs))
	for _, r := range recs {
		var s lex.Session
		if json.Unmarshal(r.Value, &s) != nil || !lex.ValidSessionID(r.Rkey) {
			continue
		}
		s.SessionID = r.Rkey
		out = append(out, SessionInfo{Session: s, URI: r.URI, Author: did, CID: r.CID})
	}
	return out, nil
}

// ListSessions lists the archive's sessions, newest first.
func (a *Archive) ListSessions(ctx context.Context, in ListSessionsIn) (ListSessionsOut, error) {
	if _, err := a.ref(); err != nil {
		return ListSessionsOut{}, err
	}
	members := []string{in.Author}
	if in.Author == "" {
		var err error
		if members, err = a.reader().members(ctx); err != nil {
			return ListSessionsOut{}, err
		}
	}
	var (
		mu         sync.Mutex
		all        = []SessionInfo{}
		unreadable []string
		wg         sync.WaitGroup
		sem        = make(chan struct{}, 16)
	)
	for _, did := range members {
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			recs, err := a.memberSessions(ctx, did)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				unreadable = append(unreadable, did)
				return
			}
			all = append(all, recs...)
		})
	}
	wg.Wait()
	sortSessions(all)
	out := ListSessionsOut{Sessions: all}
	if len(unreadable) > 0 {
		slices.Sort(unreadable)
		out.Note = "couldn't read the sessions of " + strings.Join(unreadable, ", ")
	}
	return out, nil
}

// sortSessions puts the newest first, ties by URI so every client agrees.
func sortSessions(all []SessionInfo) {
	slices.SortFunc(all, func(x, y SessionInfo) int {
		if c := strings.Compare(y.UpdatedAt, x.UpdatedAt); c != 0 {
			return c
		}
		return strings.Compare(x.URI, y.URI)
	})
}

// findSession finds a session by URI, by author and ID, or by ID: the
// reader's own first, else the newest of that ID.
func (a *Archive) findSession(ctx context.Context, in GetSessionIn) (*SessionInfo, error) {
	id, author := in.Session, in.Author
	if strings.HasPrefix(id, "at://") {
		did, rkey, ok := a.parseSessionURI(id)
		if !ok {
			return nil, fmt.Errorf("%s isn't a session in %s", id, a.Space)
		}
		id, author = rkey, did
	}
	if !lex.ValidSessionID(id) {
		return nil, fmt.Errorf("%q isn't a session id", id)
	}
	if author != "" {
		if _, err := syntax.ParseDID(author); err != nil {
			return nil, fmt.Errorf("author %q isn't a DID", author)
		}
		return a.getSession(ctx, author, id)
	}
	if !a.Identity.zero() {
		own, err := a.getSession(ctx, a.self(), id)
		if err != nil || own != nil {
			return own, err
		}
	}
	all, err := a.ListSessions(ctx, ListSessionsIn{})
	if err != nil {
		return nil, err
	}
	for _, s := range all.Sessions {
		if s.SessionID == id {
			return &s, nil
		}
	}
	return nil, nil
}

// PullSession returns a session's record and whole transcript, each chunk
// checked against its CID.
func (a *Archive) PullSession(ctx context.Context, in GetSessionIn) (PullSessionOut, error) {
	info, err := a.findSession(ctx, in)
	if err != nil {
		return PullSessionOut{}, err
	}
	if info == nil {
		return PullSessionOut{}, fmt.Errorf("no session %s in the archive", in.Session)
	}
	var b strings.Builder
	for i, c := range info.Chunks {
		data, err := a.FetchChunk(ctx, info.Author, c)
		if err != nil {
			return PullSessionOut{}, fmt.Errorf("chunk %d: %w", i, err)
		}
		b.WriteString(data)
	}
	return PullSessionOut{SessionInfo: *info, Data: b.String()}, nil
}

// FetchChunk reads one chunk from its author's blobstore and checks it
// against its CID: the record, signed by the author, names the CID, so what
// comes back is what the author wrote.
func (a *Archive) FetchChunk(ctx context.Context, author string, c lex.SessionChunkRef) (string, error) {
	return FetchBlob(ctx, a.Client, a.Space, author, c.Blob.Link)
}

// FetchBlob reads a space blob and checks its bytes against its CID.
func FetchBlob(ctx context.Context, c *spaceclient.Client, spaceURI, author, link string) (string, error) {
	want, err := cid.Decode(link)
	if err != nil {
		return "", fmt.Errorf("blob %q: %w", link, err)
	}
	host, err := c.PDSHost(ctx, author)
	if err != nil {
		return "", err
	}
	body, err := c.QueryRaw(ctx, host, spaceURI, author, "com.atproto.space.getBlob", url.Values{"space": {spaceURI}, "repo": {author}, "cid": {link}})
	if err != nil {
		return "", err
	}
	defer body.Close()
	b, err := io.ReadAll(io.LimitReader(body, MaxPushBytes+1))
	if err != nil {
		return "", err
	}
	if len(b) > MaxPushBytes {
		return "", fmt.Errorf("blob %s is larger than %d bytes", link, MaxPushBytes)
	}
	got, err := want.Prefix().Sum(b)
	if err != nil {
		return "", err
	}
	if !got.Equals(want) {
		return "", fmt.Errorf("blob %s doesn't match its CID: what was served isn't what was stored", link)
	}
	return string(b), nil
}

// parseSessionURI splits a session record's URI in this archive into its
// author and session ID.
func (a *Archive) parseSessionURI(uri string) (string, string, bool) {
	rest, ok := strings.CutPrefix(uri, a.Space+"/")
	parts := strings.Split(rest, "/")
	if !ok || len(parts) != 3 || parts[1] != lex.SessionCollection || parts[0] == "" || parts[2] == "" {
		return "", "", false
	}
	return parts[0], parts[2], true
}

// ---- writing ----

// PushSession adds transcript lines to the identity's session and rewrites
// its record. New lines go into the last chunk until it holds
// MaxChunkBytes, then into new ones.
func (a *Archive) PushSession(ctx context.Context, in PushSessionIn) (PushSessionOut, error) {
	if a.Identity.zero() {
		return PushSessionOut{}, errors.New("only a session's own identity can store it")
	}
	if _, err := a.ref(); err != nil {
		return PushSessionOut{}, err
	}
	id := in.Session.SessionID
	if !lex.ValidSessionID(id) {
		return PushSessionOut{}, fmt.Errorf("%q isn't a session id", id)
	}
	if len(in.Data) > MaxPushBytes {
		return PushSessionOut{}, fmt.Errorf("this push is %d bytes; the most is %d, so send it in several", len(in.Data), MaxPushBytes)
	}
	defer a.lock(id)()

	cur, err := a.getSession(ctx, a.self(), id)
	if err != nil {
		return PushSessionOut{}, err
	}
	stored := int64(0)
	if cur != nil && cur.Head != nil {
		stored = cur.Head.Bytes
	}
	replace := in.Replace || cur == nil
	if !replace && in.FromBytes != stored {
		return PushSessionOut{Session: id, Resync: true, Bytes: stored}, nil
	}

	// Only complete lines are stored.
	data := in.Data[:strings.LastIndexByte(in.Data, '\n')+1]
	var chunks []lex.SessionChunkRef
	if !replace {
		chunks = slices.Clone(cur.Chunks)
	}
	if data != "" && len(chunks) > 0 {
		// Grow the last chunk while it has room: it's uploaded again with the
		// new lines (at most MaxChunkBytes more per push), and every chunk
		// before it is full.
		last := chunks[len(chunks)-1]
		if last.Bytes < MaxChunkBytes {
			tail, err := a.tail(ctx, id, last)
			if err != nil {
				return PushSessionOut{}, fmt.Errorf("reading the last chunk back: %w", err)
			}
			chunks = chunks[:len(chunks)-1]
			data = tail + data
		}
	}
	var lastData string
	for _, p := range splitChunks(data, MaxChunkBytes) {
		blob, err := a.uploadBlob(ctx, p.data)
		if err != nil {
			return PushSessionOut{}, fmt.Errorf("storing the transcript: %w", err)
		}
		chunks = append(chunks, p.ref(blob))
		lastData = p.data
	}

	next := in.Session
	next.SessionID = id
	next.Kind, next.Parent = a.Identity.Kind, a.Identity.Parent
	if next.Kind == "" {
		next.Kind = lex.SessionKindSession
	}
	if cur != nil && next.StartedAt == "" {
		next.StartedAt = cur.StartedAt
	}
	head := &lex.SessionHead{}
	for _, c := range chunks {
		head.Bytes += c.Bytes
		head.Entries += c.Entries
	}
	if n := len(chunks); n > 0 {
		head.EntryID = chunks[n-1].LastEntry
	}
	next.Head, next.Chunks = head, chunks
	next.UpdatedAt = a.now().UTC().Format("2006-01-02T15:04:05.000Z")
	if err := next.Validate(); err != nil {
		return PushSessionOut{}, err
	}
	body := map[string]any{"space": a.Space, "repo": a.self(), "collection": lex.SessionCollection, "rkey": id, "record": next.Record()}
	if err := a.Client.Session.Post(ctx, "com.atproto.space.putRecord", body, nil); err != nil {
		return PushSessionOut{}, fmt.Errorf("saving session %s: %w", id, err)
	}
	if n := len(chunks); n > 0 && lastData != "" {
		a.setTail(id, tailChunk{link: chunks[n-1].Blob.Link, data: lastData})
	}
	ref, _ := a.ref()
	return PushSessionOut{
		Session: id, URI: ref.RecordURI(a.self(), lex.SessionCollection, id),
		Bytes: head.Bytes, Entries: head.Entries, Chunks: len(chunks),
		Stored: int64(len(in.Data[:strings.LastIndexByte(in.Data, '\n')+1])),
	}, nil
}

// tail is the bytes of a session's last chunk: from memory when this
// process wrote it, else from the PDS.
func (a *Archive) tail(ctx context.Context, id string, last lex.SessionChunkRef) (string, error) {
	a.mu.Lock()
	t, ok := a.tails[id]
	a.mu.Unlock()
	if ok && t.link == last.Blob.Link {
		return t.data, nil
	}
	return a.FetchChunk(ctx, a.self(), last)
}

func (a *Archive) setTail(id string, t tailChunk) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.tails == nil {
		a.tails = map[string]tailChunk{}
	}
	a.tails[id] = t
}

// DeleteSession deletes the identity's own session record; the PDS drops
// its blobs once no record names them.
func (a *Archive) DeleteSession(ctx context.Context, in DeleteSessionIn) (DeleteSessionOut, error) {
	if a.Identity.zero() {
		return DeleteSessionOut{}, errors.New("only a session's own identity can delete it")
	}
	ref, err := a.ref()
	if err != nil {
		return DeleteSessionOut{}, err
	}
	if !lex.ValidSessionID(in.Session) {
		return DeleteSessionOut{}, fmt.Errorf("%q isn't a session id", in.Session)
	}
	defer a.lock(in.Session)()
	cur, err := a.getSession(ctx, a.self(), in.Session)
	if err != nil {
		return DeleteSessionOut{}, err
	}
	if cur == nil {
		return DeleteSessionOut{}, fmt.Errorf("%s has no session %s in the archive", a.self(), in.Session)
	}
	body := map[string]any{"space": a.Space, "repo": a.self(), "collection": lex.SessionCollection, "rkey": in.Session}
	if err := a.Client.Session.Post(ctx, "com.atproto.space.deleteRecord", body, nil); err != nil {
		return DeleteSessionOut{}, fmt.Errorf("deleting session %s: %w", in.Session, err)
	}
	a.mu.Lock()
	delete(a.tails, in.Session)
	a.mu.Unlock()
	return DeleteSessionOut{Deleted: ref.RecordURI(a.self(), lex.SessionCollection, in.Session)}, nil
}

// uploadBlob puts one chunk in the identity's blobstore. It becomes the
// archive's once the session record names it.
func (a *Archive) uploadBlob(ctx context.Context, data string) (lex.Blob, error) {
	var out struct {
		Blob lex.Blob `json:"blob"`
	}
	if err := a.Client.Session.LexDo(ctx, "POST", lex.SessionBlobMime, "com.atproto.repo.uploadBlob", nil, strings.NewReader(data), &out); err != nil {
		return lex.Blob{}, err
	}
	if out.Blob.Link == "" {
		return lex.Blob{}, errors.New("the PDS accepted the blob but named no CID")
	}
	return lex.Blob{Link: out.Blob.Link, MimeType: lex.SessionBlobMime, Size: int64(len(data))}, nil
}

// ---- chunks ----

// chunkPiece is a run of complete transcript lines.
type chunkPiece struct {
	data    string
	entries int
	first   string
	last    string
}

func (p chunkPiece) ref(blob lex.Blob) lex.SessionChunkRef {
	return lex.SessionChunkRef{Blob: blob, Bytes: int64(len(p.data)), Entries: p.entries, FirstEntry: p.first, LastEntry: p.last}
}

// splitChunks cuts complete lines into chunks of at most max bytes, on line
// boundaries; a single line longer than max is a chunk of its own.
func splitChunks(data string, max int) []chunkPiece {
	var out []chunkPiece
	for data != "" {
		cut := len(data)
		if cut > max {
			if nl := strings.LastIndexByte(data[:max], '\n'); nl >= 0 {
				cut = nl + 1
			} else if nl := strings.IndexByte(data, '\n'); nl >= 0 {
				cut = nl + 1
			}
		}
		out = append(out, describePiece(data[:cut]))
		data = data[cut:]
	}
	return out
}

// describePiece counts a chunk's entries and finds its first and last IDs.
func describePiece(data string) chunkPiece {
	p := chunkPiece{data: data}
	for line := range strings.SplitSeq(data, "\n") {
		if line == "" {
			continue
		}
		p.entries++
		var entry struct {
			ID string `json:"id"`
		}
		if json.Unmarshal([]byte(line), &entry) == nil && entry.ID != "" && len(entry.ID) <= lex.MaxSessionRefField {
			if p.first == "" {
				p.first = entry.ID
			}
			p.last = entry.ID
		}
	}
	return p
}

func (id Identity) zero() bool { return id.DID == "" }
