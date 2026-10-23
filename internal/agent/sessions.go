package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/haileyok/cocoon/space"

	"github.com/haileyok/engram-garden/internal/lex"
	"github.com/haileyok/engram-garden/internal/spaceclient"
)

// Sessions: where a working session stands, and its transcript, kept in a
// space so it can be picked up on another machine. The manifest is a
// garden.engram.session record keyed by the harness's session ID. The
// transcript's bytes go in the member's blobstore as chunks of complete
// JSONL lines, which the manifest names; a PDS that doesn't allow blob
// uploads yet gets the same chunks as garden.engram.sessionChunk records
// instead, and without that either, only the manifest.

// BlobMimeType is what a transcript chunk uploads as; the sign-in has to be
// allowed to upload it.
const BlobMimeType = lex.SessionBlobMime

// Sizes. A chunk stays small enough that an upload is cheap and a PDS that
// buffers one in memory doesn't mind; a chunk record stays inside the
// 1 MiB a space record may hold, counting JSON escapes.
const (
	// MaxChunkBytes is the most one blob holds.
	MaxChunkBytes = 3 << 20
	// MaxChunkRecordBytes is the most one chunk record's data holds.
	MaxChunkRecordBytes = 400 << 10
	// MaxPushBytes is the most one push carries. A harness that appends
	// sends only the new tail, so this is a whole-transcript resync's limit.
	MaxPushBytes = 16 << 20
)

// SessionInfo is a session as an agent sees it: the record, where it is,
// and who wrote it.
type SessionInfo struct {
	lex.Session
	Space  string `json:"space,omitempty"`
	URI    string `json:"uri"`
	Author string `json:"author"`
	CID    string `json:"cid,omitempty"`
}

type PushSessionIn struct {
	// Space is the space to keep it in (default: the default space).
	Space string `json:"space,omitempty"`
	// Session names the session and says where the work stands. The agent
	// fills in head, chunks, mode and updatedAt.
	Session lex.Session `json:"session"`
	// FromBytes is how much of the transcript the caller believes is
	// stored, so this push is the rest. The server checks it against the
	// manifest and asks for a resync (Replace) when they disagree.
	FromBytes int64 `json:"fromBytes,omitempty"`
	// Replace stores Data as the whole transcript, replacing what was
	// stored: the answer to a resync, or the same file pushed twice.
	Replace bool `json:"replace,omitempty"`
	// Data is the transcript from FromBytes: complete JSONL lines. A
	// trailing line without its newline is held back and reported.
	Data string `json:"data"`
}

type PushSessionOut struct {
	// Session, URI and Space say what was written and where.
	Session string `json:"session"`
	URI     string `json:"uri,omitempty"`
	Space   string `json:"space,omitempty"`
	// Mode is where the bytes went: blob, record or metadata (the
	// manifest only). Note says why when it isn't blob.
	Mode string `json:"mode"`
	// Bytes and Entries are how much of the transcript is stored now, and
	// Stored is how much of this push was (a partial trailing line isn't).
	Bytes   int64 `json:"bytes"`
	Entries int   `json:"entries"`
	Chunks  int   `json:"chunks"`
	Stored  int64 `json:"storedBytes"`
	// Resync means the manifest holds something else: push again from 0.
	Resync bool   `json:"resync,omitempty"`
	Note   string `json:"note,omitempty"`
}

type GetSessionIn struct {
	// Space is the space to read from (default: wherever the session is).
	Space string `json:"space,omitempty"`
	// Session is the harness's session ID.
	Session string `json:"session"`
}

type PullSessionOut struct {
	Session lex.Session `json:"session"`
	Space   string      `json:"space,omitempty"`
	URI     string      `json:"uri,omitempty"`
	// Data is the whole transcript: complete JSONL lines.
	Data string `json:"data"`
}

type ListSessionsIn struct {
	// Space is the space to list (default: every space).
	Space string `json:"space,omitempty"`
}

type ListSessionsOut struct {
	Sessions []SessionInfo `json:"sessions"`
	Note     string        `json:"note,omitempty"`
}

type DeleteSessionIn struct {
	Space   string `json:"space,omitempty"`
	Session string `json:"session"`
}

type DeleteSessionOut struct {
	Deleted string `json:"deleted"`
}

// ---- reading ----

// ownSession reads the agent's own manifest for a session.
func (t *Agent) ownSession(ctx context.Context, id string) (*lex.Session, error) {
	params := map[string]any{"space": t.Space, "repo": t.Client.DID().String(), "collection": lex.SessionCollection, "rkey": id}
	var out struct {
		Value json.RawMessage `json:"value"`
	}
	if err := t.Client.Session.Get(ctx, "com.atproto.space.getRecord", params, &out); err != nil {
		if isNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading session %s: %w", id, err)
	}
	var s lex.Session
	if err := json.Unmarshal(out.Value, &s); err != nil {
		return nil, err
	}
	if s.SessionID == "" {
		s.SessionID = id
	}
	return &s, nil
}

// memberSessions reads one member's manifests.
func (t *Agent) memberSessions(ctx context.Context, did string) ([]SessionInfo, error) {
	recs, err := t.listRecords(ctx, did, lex.SessionCollection)
	if err != nil {
		return nil, err
	}
	out := make([]SessionInfo, 0, len(recs))
	for _, r := range recs {
		var s lex.Session
		if json.Unmarshal(r.Value, &s) != nil {
			continue
		}
		// A chunk record in this collection would be a different shape; the
		// manifest is the one whose key is its session ID.
		if s.SessionID == "" || s.SessionID != r.Rkey || !lex.ValidSessionID(r.Rkey) {
			continue
		}
		out = append(out, SessionInfo{Session: s, URI: r.URI, Author: did, CID: r.CID})
	}
	return out, nil
}

// SessionRecords is every member's manifests, and the members whose
// sessions couldn't be read.
func (t *Agent) sessionRecords(ctx context.Context) ([]SessionInfo, []string, error) {
	members, err := t.members(ctx)
	if err != nil {
		return nil, nil, err
	}
	me := t.Client.DID().String()
	if !slices.Contains(members, me) {
		members = append(members, me)
	}
	var (
		mu         sync.Mutex
		all        []SessionInfo
		errs       []error
		unreadable []string
		wg         sync.WaitGroup
	)
	for _, did := range members {
		wg.Go(func() {
			recs, err := t.memberSessions(ctx, did)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", did, err))
				unreadable = append(unreadable, did)
				return
			}
			all = append(all, recs...)
		})
	}
	wg.Wait()
	if len(errs) > 0 && len(all) == 0 {
		return nil, nil, errors.Join(errs...)
	}
	slices.Sort(unreadable)
	return all, unreadable, nil
}

// Sessions lists the sessions in the space, newest first.
func (t *Agent) Sessions(ctx context.Context) ([]SessionInfo, []string, error) {
	all, unreadable, err := t.sessionRecords(ctx)
	if err != nil {
		return nil, nil, err
	}
	me := t.Client.DID().String()
	when := func(s SessionInfo) time.Time {
		got, err := time.Parse(time.RFC3339, s.UpdatedAt)
		if err != nil {
			return time.Time{}
		}
		return got
	}
	slices.SortFunc(all, func(a, b SessionInfo) int {
		if c := when(b).Compare(when(a)); c != 0 {
			return c
		}
		// A tie goes to the reader's own, then by URI, so every client sorts
		// the same way.
		if a.Author != b.Author {
			if a.Author == me {
				return -1
			}
			if b.Author == me {
				return 1
			}
		}
		return strings.Compare(a.URI, b.URI)
	})
	return all, unreadable, nil
}

// findSession reads one manifest: the agent's own first, then any other
// member's.
func (t *Agent) findSession(ctx context.Context, id string) (*SessionInfo, error) {
	me := t.Client.DID().String()
	own, err := t.ownSession(ctx, id)
	if err != nil {
		return nil, err
	}
	ref, err := space.ParseRef(t.Space)
	if err != nil {
		return nil, err
	}
	if own != nil {
		return &SessionInfo{Session: *own, URI: ref.RecordURI(me, lex.SessionCollection, id), Author: me}, nil
	}
	all, _, err := t.sessionRecords(ctx)
	if err != nil {
		return nil, err
	}
	for _, s := range all {
		if s.SessionID == id {
			got := s
			return &got, nil
		}
	}
	return nil, nil
}

// PullSession returns the whole stored transcript, checking each chunk
// against the digest the manifest recorded.
func (t *Agent) PullSession(ctx context.Context, in GetSessionIn) (PullSessionOut, error) {
	if !lex.ValidSessionID(in.Session) {
		return PullSessionOut{}, fmt.Errorf("%q isn't a session id", in.Session)
	}
	info, err := t.findSession(ctx, in.Session)
	if err != nil {
		return PullSessionOut{}, err
	}
	if info == nil {
		return PullSessionOut{}, fmt.Errorf("no session %s in %s", in.Session, t.Space)
	}
	if len(info.Chunks) == 0 {
		return PullSessionOut{}, fmt.Errorf("session %s has no stored transcript, only its record: %s", in.Session, noTranscriptHint(info))
	}
	var buf bytes.Buffer
	for i, c := range info.Chunks {
		data, err := t.chunkBytes(ctx, info, c)
		if err != nil {
			return PullSessionOut{}, fmt.Errorf("chunk %d: %w", i, err)
		}
		if c.SHA256 != "" && digest(data) != c.SHA256 {
			return PullSessionOut{}, fmt.Errorf("chunk %d doesn't match its digest: what was stored is damaged", i)
		}
		buf.WriteString(data)
	}
	spaceName := info.Space
	if spaceName == "" {
		spaceName = t.Space
	}
	return PullSessionOut{Session: info.Session, Space: spaceName, URI: info.URI, Data: buf.String()}, nil
}

// chunkBytes reads one chunk, from the member's blobstore or a chunk record.
func (t *Agent) chunkBytes(ctx context.Context, info *SessionInfo, c lex.SessionChunkRef) (string, error) {
	if c.Blob != nil {
		return t.fetchBlob(ctx, info.Author, c.Blob.Link)
	}
	if c.Rkey == "" {
		return "", nil
	}
	host, err := t.Client.PDSHost(ctx, info.Author)
	if err != nil {
		return "", err
	}
	params := url.Values{"space": {t.Space}, "repo": {info.Author}, "collection": {lex.SessionChunkCollection}, "rkey": {c.Rkey}}
	var out struct {
		Value json.RawMessage `json:"value"`
	}
	if _, err := t.Client.QueryMac(ctx, host, t.Space, info.Author, space.RecordMacType, "com.atproto.space.getRecord", params, &out); err != nil {
		return "", err
	}
	var chunk lex.SessionChunk
	if err := json.Unmarshal(out.Value, &chunk); err != nil {
		return "", err
	}
	return chunk.Data, nil
}

// ---- writing ----

// PushSession appends transcript bytes to the stored session and writes the
// manifest. It stores blobs when the sign-in allows it, chunk records when
// that isn't allowed yet, and the manifest alone when neither is.
func (t *Agent) PushSession(ctx context.Context, in PushSessionIn) (PushSessionOut, error) {
	id := in.Session.SessionID
	if !lex.ValidSessionID(id) {
		return PushSessionOut{}, fmt.Errorf("%q isn't a session id", id)
	}
	if len(in.Data) > MaxPushBytes {
		return PushSessionOut{}, fmt.Errorf("this push is %d bytes; the most is %d. Send the transcript in appends", len(in.Data), MaxPushBytes)
	}
	cur, err := t.ownSession(ctx, id)
	if err != nil {
		return PushSessionOut{}, err
	}
	stored := int64(0)
	if cur != nil && cur.Head != nil {
		stored = cur.Head.Bytes
	}
	// Without Replace, the caller's idea of what's stored has to be ours, or
	// the transcript would gain a hole. Ask for the whole file instead.
	replace := in.Replace || cur == nil
	if !replace && in.FromBytes != stored {
		return PushSessionOut{Session: id, Space: t.Space, Resync: true, Bytes: stored, Mode: modeOf(cur)}, nil
	}
	next := in.Session
	next.SessionID = id
	if cur != nil && next.StartedAt == "" {
		next.StartedAt = cur.StartedAt
	}
	// A replacing push starts the transcript over; any other adds to it.
	chunks := []lex.SessionChunkRef{}
	var replaced []lex.SessionChunkRef
	if cur != nil {
		if replace {
			replaced = cur.Chunks
		} else {
			chunks = slices.Clone(cur.Chunks)
		}
	}
	pieces, _ := splitChunks(in.Data, MaxChunkBytes)
	mode, note := lex.SessionModeBlob, ""
	if len(pieces) > 0 {
		chunks, mode, note, err = t.storeChunks(ctx, id, chunks, pieces)
		if err != nil {
			return PushSessionOut{}, err
		}
	}
	if len(chunks) == 0 {
		mode = lex.SessionModeMetadata
	}
	var (
		total    int64
		entries  int
		consumed int64
	)
	for _, c := range chunks {
		total += c.Bytes
	}
	if mode != lex.SessionModeMetadata {
		for _, p := range pieces {
			consumed += p.bytes
			entries += p.entries
		}
	}
	if !replace && cur != nil && cur.Head != nil && mode != lex.SessionModeMetadata {
		entries += cur.Head.Entries
	}
	head := &lex.SessionHead{Bytes: total, Entries: entries}
	if n := len(chunks); n > 0 {
		head.EntryID = chunks[n-1].LastEntry
	}
	next.Head = head
	next.Chunks = chunks
	next.Mode = mode
	next.UpdatedAt = time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
	if err := next.Validate(); err != nil {
		return PushSessionOut{}, err
	}
	out := PushSessionOut{Session: id, Space: t.Space, Mode: mode, Bytes: total, Entries: entries, Chunks: len(chunks), Stored: consumed, Note: note}
	body := map[string]any{"space": t.Space, "repo": t.Client.DID().String(), "collection": lex.SessionCollection, "rkey": id, "record": next.Record()}
	if err := t.Client.Session.Post(ctx, "com.atproto.space.putRecord", body, &out); err != nil {
		return PushSessionOut{}, fmt.Errorf("saving session %s: %w", id, scopeHint(err, "sessions"))
	}
	if ref, err := space.ParseRef(t.Space); err == nil {
		out.URI = ref.RecordURI(t.Client.DID().String(), lex.SessionCollection, id)
	}
	// Only now that the manifest names the new chunks are the old ones
	// dropped: a replacing push reuses their keys, and the transcript is
	// whatever the manifest names.
	if len(replaced) > 0 {
		t.dropChunks(ctx, replaced, chunks)
	}
	return out, nil
}

// storeChunks stores transcript chunks the best way the sign-in allows:
// blobs, then chunk records, then nowhere (the manifest alone).
func (t *Agent) storeChunks(ctx context.Context, id string, into []lex.SessionChunkRef, pieces []chunkPiece) ([]lex.SessionChunkRef, string, string, error) {
	blobs := slices.Clone(into)
	blobErr := error(nil)
	for _, p := range pieces {
		blob, err := t.uploadBlob(ctx, p.data)
		if err != nil {
			blobErr = err
			break
		}
		blobs = append(blobs, p.ref(blob, ""))
	}
	if blobErr == nil {
		return blobs, lex.SessionModeBlob, "", nil
	}
	if !refusedWrite(blobErr) {
		return nil, "", "", fmt.Errorf("storing the transcript: %w", blobErr)
	}
	// The sign-in can't upload blobs yet. Chunk records need no blob
	// permission, so the transcript is stored anyway.
	records := slices.Clone(into)
	for _, p := range pieces {
		for _, part := range splitPiece(p, MaxChunkRecordBytes) {
			rkey := lex.ChunkRkey(id, len(records))
			chunk := lex.SessionChunk{
				SessionID: id, Index: len(records), Bytes: part.bytes, Entries: part.entries,
				FirstEntry: part.first, LastEntry: part.last, SHA256: part.sha256, Data: part.data,
			}
			if err := chunk.Validate(); err != nil {
				return nil, "", "", err
			}
			body := map[string]any{"space": t.Space, "repo": t.Client.DID().String(), "collection": lex.SessionChunkCollection, "rkey": rkey, "record": chunk.Record()}
			if err := t.Client.Session.Post(ctx, "com.atproto.space.putRecord", body, nil); err != nil {
				if !refusedWrite(err) {
					return nil, "", "", fmt.Errorf("writing chunk %s: %w", rkey, err)
				}
				return into, lex.SessionModeMetadata, "only the session's metadata was stored: this sign-in may not write transcripts, run `engram login` again to allow it", nil
			}
			records = append(records, part.ref(lex.Blob{}, rkey))
		}
	}
	return records, lex.SessionModeRecord, "the transcript is in chunk records, not blobs: this sign-in can't upload blobs, run `engram login` again to allow it", nil
}

// DeleteSession deletes the agent's own manifest, and the chunk records it
// names; its blobs go when no record names them.
func (t *Agent) DeleteSession(ctx context.Context, in DeleteSessionIn) (DeleteSessionOut, error) {
	if !lex.ValidSessionID(in.Session) {
		return DeleteSessionOut{}, fmt.Errorf("%q isn't a session id", in.Session)
	}
	cur, err := t.ownSession(ctx, in.Session)
	if err != nil {
		return DeleteSessionOut{}, err
	}
	if cur == nil {
		return DeleteSessionOut{}, fmt.Errorf("you have no session %s in this space; you can only delete your own", in.Session)
	}
	ref, err := space.ParseRef(t.Space)
	if err != nil {
		return DeleteSessionOut{}, err
	}
	me := t.Client.DID().String()
	body := map[string]any{"space": t.Space, "repo": me, "collection": lex.SessionCollection, "rkey": in.Session}
	if err := t.Client.Session.Post(ctx, "com.atproto.space.deleteRecord", body, nil); err != nil {
		return DeleteSessionOut{}, fmt.Errorf("deleting session %s: %w", in.Session, scopeHint(err, "sessions"))
	}
	t.dropChunks(ctx, cur.Chunks, nil)
	return DeleteSessionOut{Deleted: ref.RecordURI(me, lex.SessionCollection, in.Session)}, nil
}

// dropChunks deletes the chunk records a replaced or deleted manifest named,
// skipping any in keep. Blobs aren't deleted: the PDS keeps one while a
// record names it, and drops it once none does.
func (t *Agent) dropChunks(ctx context.Context, chunks, keep []lex.SessionChunkRef) {
	kept := map[string]bool{}
	for _, c := range keep {
		if c.Rkey != "" {
			kept[c.Rkey] = true
		}
	}
	for _, c := range chunks {
		if c.Rkey == "" || kept[c.Rkey] {
			continue
		}
		body := map[string]any{"space": t.Space, "repo": t.Client.DID().String(), "collection": lex.SessionChunkCollection, "rkey": c.Rkey}
		_ = t.Client.Session.Post(ctx, "com.atproto.space.deleteRecord", body, nil)
	}
}

// storeChunksBlob uploads each chunk as a blob and returns the refs.
func (t *Agent) storeChunksBlob(ctx context.Context, id string, into []lex.SessionChunkRef, pieces []chunkPiece) ([]lex.SessionChunkRef, error) {
	out := slices.Clone(into)
	for i, p := range pieces {
		blob, err := t.uploadBlob(ctx, p.data)
		if err != nil {
			return out, err
		}
		out = append(out, p.ref(blob, ""))
		_ = i
	}
	return out, nil
}

// storeChunksRecord writes each chunk as a chunk record and returns the refs.
func (t *Agent) storeChunksRecord(ctx context.Context, id string, cur *lex.Session, into []lex.SessionChunkRef, pieces []chunkPiece) ([]lex.SessionChunkRef, error) {
	out := slices.Clone(into)
	base := len(out)
	for i, p := range pieces {
		// A chunk record holds less than a blob, so a big piece is split
		// again; only the last piece of the run may be short.
		parts := splitPiece(p, MaxChunkRecordBytes)
		for _, part := range parts {
			n := base + i
			rkey := lex.ChunkRkey(id, len(out))
			chunk := lex.SessionChunk{
				SessionID: id, Index: len(out), Bytes: part.bytes, Entries: part.entries,
				FirstEntry: part.first, LastEntry: part.last, SHA256: part.sha256, Data: part.data,
			}
			if err := chunk.Validate(); err != nil {
				return out, err
			}
			body := map[string]any{"space": t.Space, "repo": t.Client.DID().String(), "collection": lex.SessionChunkCollection, "rkey": rkey, "record": chunk.Record()}
			if err := t.Client.Session.Post(ctx, "com.atproto.space.putRecord", body, nil); err != nil {
				return out, fmt.Errorf("writing chunk %s: %w", rkey, err)
			}
			ref := part.ref(lex.Blob{}, rkey)
			ref.Rkey = rkey
			out = append(out, ref)
			_ = n
		}
	}
	return out, nil
}

// uploadBlob puts one chunk in the account's blobstore. It becomes the
// space's data once the manifest references it.
func (t *Agent) uploadBlob(ctx context.Context, data string) (lex.Blob, error) {
	var out struct {
		Blob struct {
			Ref struct {
				Link string `json:"$link"`
			} `json:"ref"`
			MimeType string `json:"mimeType"`
			Size     int64  `json:"size"`
		} `json:"blob"`
	}
	if err := t.Client.Session.LexDo(ctx, "POST", BlobMimeType, "com.atproto.repo.uploadBlob", nil, strings.NewReader(data), &out); err != nil {
		return lex.Blob{}, err
	}
	if out.Blob.Ref.Link == "" {
		return lex.Blob{}, fmt.Errorf("the PDS accepted the blob but named no CID")
	}
	size := out.Blob.Size
	if size == 0 {
		size = int64(len(data))
	}
	return lex.Blob{Link: out.Blob.Ref.Link, MimeType: BlobMimeType, Size: size}, nil
}

// fetchBlob reads one stored chunk from a member's blobstore.
func (t *Agent) fetchBlob(ctx context.Context, author, link string) (string, error) {
	host, err := t.Client.PDSHost(ctx, author)
	if err != nil {
		return "", err
	}
	body, err := t.Client.QueryRaw(ctx, host, t.Space, author, "com.atproto.space.getBlob", url.Values{"space": {t.Space}, "repo": {author}, "cid": {link}})
	if err != nil {
		return "", err
	}
	defer body.Close()
	b, err := io.ReadAll(io.LimitReader(body, int64(MaxChunkBytes)+1))
	if err != nil {
		return "", err
	}
	if len(b) > MaxChunkBytes {
		return "", fmt.Errorf("the chunk is larger than %d bytes", MaxChunkBytes)
	}
	return string(b), nil
}

// ---- chunks ----

// chunkPiece is a run of complete transcript lines with what a reader needs
// to know about it.
type chunkPiece struct {
	data    string
	bytes   int64
	entries int
	first   string
	last    string
	sha256  string
}

func (p chunkPiece) ref(blob lex.Blob, rkey string) lex.SessionChunkRef {
	r := lex.SessionChunkRef{
		Bytes: p.bytes, Entries: p.entries, FirstEntry: p.first, LastEntry: p.last, SHA256: p.sha256, Rkey: rkey,
	}
	if rkey == "" {
		b := blob
		r.Blob = &b
	}
	return r
}

// splitChunks cuts data into chunks of at most max bytes on line boundaries,
// holding back a trailing line that has no newline yet. It returns the
// chunks and how many bytes they cover.
func splitChunks(data string, max int) ([]chunkPiece, int) {
	var out []chunkPiece
	consumed := 0
	for consumed < len(data) {
		rest := data[consumed:]
		var cut int
		if len(rest) <= max {
			// Take every complete line there is.
			cut = strings.LastIndexByte(rest, '\n') + 1
		} else if nl := strings.LastIndexByte(rest[:max], '\n'); nl >= 0 {
			cut = nl + 1
		} else if nl := strings.IndexByte(rest, '\n'); nl >= 0 {
			// One line longer than a chunk: it goes in a chunk of its own.
			cut = nl + 1
		}
		if cut <= 0 {
			break
		}
		out = append(out, describePiece(rest[:cut]))
		consumed += cut
	}
	return out, consumed
}

// splitPiece cuts one piece into record-sized pieces, on line boundaries.
func splitPiece(p chunkPiece, max int) []chunkPiece {
	if int64(len(p.data)) <= int64(max) {
		return []chunkPiece{p}
	}
	var out []chunkPiece
	rest := p.data
	for rest != "" {
		var cut int
		if len(rest) <= max {
			cut = strings.LastIndexByte(rest, '\n') + 1
		} else if nl := strings.LastIndexByte(rest[:max], '\n'); nl >= 0 {
			cut = nl + 1
		} else if nl := strings.IndexByte(rest, '\n'); nl >= 0 {
			cut = nl + 1
		}
		if cut <= 0 {
			break
		}
		out = append(out, describePiece(rest[:cut]))
		rest = rest[cut:]
	}
	return out
}

// describePiece counts a chunk's entries and digests its bytes.
func describePiece(data string) chunkPiece {
	p := chunkPiece{data: data, bytes: int64(len(data)), sha256: digest(data)}
	for _, line := range strings.Split(data, "\n") {
		if line == "" {
			continue
		}
		p.entries++
		var entry struct {
			ID string `json:"id"`
		}
		if json.Unmarshal([]byte(line), &entry) == nil && entry.ID != "" {
			if p.first == "" {
				p.first = entry.ID
			}
			p.last = entry.ID
		}
	}
	return p
}

func digest(data string) string {
	sum := sha256.Sum256([]byte(data))
	return hex.EncodeToString(sum[:])
}

// noTranscriptHint says how to get the bytes, for a record that has none.
func noTranscriptHint(info *SessionInfo) string {
	if info.Mode == lex.SessionModeMetadata {
		return "the sign-in couldn't store it; run `engram login` again, then push it"
	}
	return "push the session's file to store it"
}

// modeOf is a manifest's mode, for reporting on a session with none stored.
func modeOf(s *lex.Session) string {
	if s == nil {
		return lex.SessionModeMetadata
	}
	if s.Mode != "" {
		return s.Mode
	}
	return lex.SessionModeMetadata
}

// refusedWrite reports whether err means the sign-in may not write this,
// which is what sends a push down to the next mode.
func refusedWrite(err error) bool {
	if err == nil {
		return false
	}
	var xe *spaceclient.Error
	msg := err.Error()
	return (errors.As(err, &xe) && (xe.Status == 401 || xe.Status == 403)) ||
		strings.Contains(msg, "scope") || strings.Contains(msg, "Forbidden") || strings.Contains(msg, "Insufficient")
}

// ---- every space ----

// PushSession stores a session in one space (the default unless named).
func (s *Spaces) PushSession(ctx context.Context, in PushSessionIn) (PushSessionOut, error) {
	a, e, err := s.Agent(in.Space)
	if err != nil {
		return PushSessionOut{}, err
	}
	out, err := a.PushSession(ctx, in)
	if out.Space == "" {
		out.Space = e.Name
	}
	return out, err
}

// ListSessions lists the sessions in every space the agent uses, or one.
func (s *Spaces) ListSessions(ctx context.Context, in ListSessionsIn) (ListSessionsOut, error) {
	entries := s.settings().Spaces
	if in.Space != "" {
		e, err := s.settings().Resolve(in.Space)
		if err != nil {
			return ListSessionsOut{}, err
		}
		entries = []SpaceEntry{e}
	}
	var (
		out   = ListSessionsOut{Sessions: []SessionInfo{}}
		notes []string
		mu    sync.Mutex
		wg    sync.WaitGroup
	)
	for _, e := range entries {
		wg.Go(func() {
			a, _, err := s.Agent(e.URI)
			if err != nil {
				mu.Lock()
				notes = append(notes, fmt.Sprintf("couldn't open %s: %v", e.Name, unwrap(err)))
				mu.Unlock()
				return
			}
			list, unreadable, err := a.Sessions(ctx)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				notes = append(notes, fmt.Sprintf("couldn't read sessions in %s: %v", e.Name, unwrap(err)))
				return
			}
			if len(unreadable) > 0 {
				notes = append(notes, fmt.Sprintf("couldn't read %s's sessions in %s", strings.Join(unreadable, ", "), e.Name))
			}
			for _, si := range list {
				si.Space = e.Name
				out.Sessions = append(out.Sessions, si)
			}
		})
	}
	wg.Wait()
	slices.SortFunc(out.Sessions, func(a, b SessionInfo) int { return strings.Compare(b.UpdatedAt, a.UpdatedAt) })
	out.Note = strings.Join(notes, "; ")
	return out, nil
}

// PullSession returns a stored transcript, from the named space or from
// whichever space has it.
func (s *Spaces) PullSession(ctx context.Context, in GetSessionIn) (PullSessionOut, error) {
	if in.Space != "" {
		a, e, err := s.Agent(in.Space)
		if err != nil {
			return PullSessionOut{}, err
		}
		out, err := a.PullSession(ctx, in)
		out.Space = e.Name
		return out, err
	}
	listed, err := s.ListSessions(ctx, ListSessionsIn{})
	if err != nil {
		return PullSessionOut{}, err
	}
	for _, si := range listed.Sessions {
		if si.SessionID != in.Session {
			continue
		}
		a, _, err := s.Agent(si.Space)
		if err != nil {
			return PullSessionOut{}, err
		}
		return a.PullSession(ctx, in)
	}
	return PullSessionOut{}, fmt.Errorf("no session %s in any of your spaces", in.Session)
}

// DeleteSession deletes the agent's own session record, in the named space
// or wherever it is.
func (s *Spaces) DeleteSession(ctx context.Context, in DeleteSessionIn) (DeleteSessionOut, error) {
	if in.Space != "" {
		a, _, err := s.Agent(in.Space)
		if err != nil {
			return DeleteSessionOut{}, err
		}
		return a.DeleteSession(ctx, in)
	}
	listed, err := s.ListSessions(ctx, ListSessionsIn{})
	if err != nil {
		return DeleteSessionOut{}, err
	}
	me := s.Client.DID().String()
	for _, si := range listed.Sessions {
		if si.SessionID != in.Session || si.Author != me {
			continue
		}
		a, _, err := s.Agent(si.Space)
		if err != nil {
			return DeleteSessionOut{}, err
		}
		return a.DeleteSession(ctx, in)
	}
	return DeleteSessionOut{}, fmt.Errorf("you have no session %s in any of your spaces", in.Session)
}
