// Package sessionindex indexes an archive: it turns the session records
// the indexer reads (verified against their authors' commits) into passages
// of their transcripts, embedded with the archive's model. The transcript's
// chunks are blobs the records name, checked against their CIDs as they're
// read, so everything indexed is covered by the author's signed commit.
//
// Each repo's progress (per session: which chunks were read, and the
// transcript cutter's state) rides in the repo's sync position, so the two
// are always published together and a sync that's cut short carries on
// where it stopped. See docs/design/sessions.md.
package sessionindex

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/haileyok/cocoon/space"

	"github.com/haileyok/engram-garden/internal/embed"
	"github.com/haileyok/engram-garden/internal/lex"
	"github.com/haileyok/engram-garden/internal/redact"
	"github.com/haileyok/engram-garden/internal/spacestore"
	"github.com/haileyok/engram-garden/internal/transcript"
)

// KindSession is the passage made of a session record's title and summary.
const KindSession = "session"

// Tag prefixes passages carry, for filtering searches.
const (
	TagSession = "session:"
	TagKind    = "kind:"
	TagRepo    = "repo:"
	TagBranch  = "branch:"
	TagFile    = "file:"
	TagCmd     = "cmd:"
	TagAgent   = "agent:" // session or subagent
	TagParent  = "parent:"
)

// Blobs reads a blob a space record names, checked against its CID.
type Blobs interface {
	FetchBlob(ctx context.Context, spaceURI, author, link string) ([]byte, error)
}

// Indexer derives an archive's passages.
type Indexer struct {
	Store *spacestore.Node
	Blobs Blobs
	// Embed embeds passages with the archive's declared model.
	Embed embed.Provider
	Log   *slog.Logger
	// BatchBytes is how much transcript one batch reads before its
	// passages are embedded and stored (default 4 MiB).
	BatchBytes int64
}

func (x *Indexer) log() *slog.Logger {
	if x.Log == nil {
		return slog.Default()
	}
	return x.Log
}

// ErrNeedFull means the repo's passages were cut by another version of
// the transcript format: the indexer reads the whole repo instead.
var ErrNeedFull = errors.New("the repo's sessions need cutting again")

// ErrNoModel means the archive's authority hasn't declared a model yet, so
// nothing can be embedded.
var ErrNoModel = errors.New("the archive has no declared model")

// Change is a session record as the indexer read it: changed, or deleted
// (Record nil).
type Change struct {
	Rkey   string
	CID    string
	Record map[string]any
}

// state is a repo's progress, in its sync position.
type state struct {
	Version int `json:"v"`
	// Models are the model keys the passages were embedded with.
	Models   string                   `json:"models"`
	Sessions map[string]*sessionState `json:"sessions,omitempty"`
}

type sessionState struct {
	// Chunks were read through the end of chunk Read-1; Prefix is a digest
	// of their CIDs, so a rewritten transcript is noticed.
	Read   int    `json:"read"`
	Prefix string `json:"prefix"`
	// Cut is the transcript cutter's state: how many bytes were read, and
	// what's still open.
	Cut transcript.State `json:"cut"`
	// Meta is a digest of the record's title, summary and facets, so the
	// session passage is redone when they change.
	Meta  string `json:"meta,omitempty"`
	Ended bool   `json:"ended,omitempty"`
}

// Batch is one step's changes: passages to store and delete, and the
// repo's state after them.
type Batch struct {
	Upserts []spacestore.Memory
	Deletes []string
	State   json.RawMessage
	// Done is set on the last batch.
	Done bool
}

// loadState reads a repo's state, or starts a new one when there's none or
// it was made by another version or with other models.
func loadState(raw json.RawMessage, models string) (*state, bool) {
	st := &state{}
	if len(raw) > 0 && json.Unmarshal(raw, st) == nil && st.Version == transcript.Version && st.Models == models {
		if st.Sessions == nil {
			st.Sessions = map[string]*sessionState{}
		}
		return st, true
	}
	return &state{Version: transcript.Version, Models: models, Sessions: map[string]*sessionState{}}, len(raw) == 0
}

func modelsKey(cfg *lex.Config) string {
	k := cfg.ModelInfo.Key()
	if cfg.Next != nil && cfg.Next.Valid() {
		k += " " + cfg.Next.Key()
	}
	return k
}

// Derive turns a repo's changed session records into passages, a batch at
// a time, calling apply after each. With full, changes are every session
// record the repo has: passages of sessions not among them are deleted.
// prev is the state from the repo's position, and carries on even through a
// full re-export, so a sync cut short resumes instead of starting over.
func (x *Indexer) Derive(ctx context.Context, spaceURI, did string, prev json.RawMessage, changes []Change, full bool, apply func(Batch) error) error {
	cfg, err := x.Store.Config(ctx, spaceURI)
	if err != nil {
		return err
	}
	if cfg == nil || !cfg.ModelInfo.Valid() {
		return ErrNoModel
	}
	ref, err := space.ParseRef(spaceURI)
	if err != nil {
		return err
	}
	st, current := loadState(prev, modelsKey(cfg))
	if !current && !full {
		// Every session has to be cut and embedded again, not only the
		// changed ones.
		return ErrNeedFull
	}
	var deletes []string
	if full {
		// Drop the passages of sessions that aren't there any more, and
		// with a state from another version or model, all of them.
		keep := map[string]bool{}
		for _, c := range changes {
			if c.Record != nil {
				keep[c.Rkey] = true
			}
		}
		stale, err := x.staleKeys(ctx, spaceURI, did, keep, current)
		if err != nil {
			return err
		}
		deletes = append(deletes, stale...)
		for id := range st.Sessions {
			if !keep[id] {
				delete(st.Sessions, id)
			}
		}
	}
	budget := x.BatchBytes
	if budget <= 0 {
		budget = 4 << 20
	}
	emb, err := x.embedders(ctx, cfg)
	if err != nil {
		return err
	}
	var pending []pendingPassage
	flush := func(done bool) error {
		ups, err := x.embed(ctx, cfg, emb, did, pending)
		if err != nil {
			return err
		}
		raw, err := json.Marshal(st)
		if err != nil {
			return err
		}
		if err := apply(Batch{Upserts: ups, Deletes: deletes, State: raw, Done: done}); err != nil {
			return err
		}
		pending, deletes = nil, nil
		return nil
	}
	for _, c := range changes {
		if !lex.ValidSessionID(c.Rkey) {
			continue
		}
		if c.Record == nil {
			if st.Sessions[c.Rkey] != nil || !full {
				keys, err := x.sessionKeys(ctx, spaceURI, did, c.Rkey)
				if err != nil {
					return err
				}
				deletes = append(deletes, keys...)
			}
			delete(st.Sessions, c.Rkey)
			continue
		}
		raw, err := json.Marshal(c.Record)
		if err != nil {
			continue
		}
		var rec lex.Session
		if json.Unmarshal(raw, &rec) != nil {
			continue
		}
		rec.SessionID = c.Rkey
		uri := ref.RecordURI(did, lex.SessionCollection, c.Rkey)
		ss := st.Sessions[c.Rkey]
		if ss != nil && !prefixHolds(ss, rec.Chunks) {
			// The transcript was rewritten (a resync): start it over.
			keys, err := x.sessionKeys(ctx, spaceURI, did, c.Rkey)
			if err != nil {
				return err
			}
			deletes = append(deletes, keys...)
			ss = nil
		}
		if ss == nil {
			ss = &sessionState{}
			st.Sessions[c.Rkey] = ss
		}
		facets := sessionTags(rec)
		if meta := metaDigest(rec); meta != ss.Meta {
			ss.Meta = meta
			if p, ok := sessionPassage(rec); ok {
				pending = append(pending, pendingPassage{session: rec.SessionID, uri: uri, facets: facets, p: p})
			}
		}
		// Read the chunks from the first one not wholly read.
		var start int64
		for i := 0; i < ss.Read && i < len(rec.Chunks); i++ {
			start += rec.Chunks[i].Bytes
		}
		read := int64(0)
		for i := ss.Read; i < len(rec.Chunks); i++ {
			ch := rec.Chunks[i]
			end := start + ch.Bytes
			if end <= ss.Cut.Offset {
				start = end
				continue
			}
			data, err := x.Blobs.FetchBlob(ctx, spaceURI, did, ch.Blob.Link)
			if err != nil {
				return fmt.Errorf("session %s chunk %d: %w", c.Rkey, i, err)
			}
			if int64(len(data)) != ch.Bytes {
				return fmt.Errorf("session %s chunk %d is %d bytes; its record says %d", c.Rkey, i, len(data), ch.Bytes)
			}
			skip := max(0, ss.Cut.Offset-start)
			for _, p := range ss.Cut.Feed(string(data[skip:])) {
				pending = append(pending, pendingPassage{session: rec.SessionID, uri: uri, facets: facets, p: p})
			}
			start = end
			read += ch.Bytes - skip
			// Every chunk but the last is full and won't change; the last
			// grows, so it's read again from where the cutter stopped.
			if i < len(rec.Chunks)-1 {
				ss.Read = i + 1
				ss.Prefix = prefixDigest(rec.Chunks[:ss.Read])
			}
			if read >= budget {
				if err := flush(false); err != nil {
					return err
				}
				read = 0
			}
		}
		if rec.Status == lex.SessionEnded && !ss.Ended && ss.Cut.Offset == start {
			ss.Ended = true
			for _, p := range ss.Cut.Finish() {
				pending = append(pending, pendingPassage{session: rec.SessionID, uri: uri, facets: facets, p: p})
			}
		} else if rec.Status != lex.SessionEnded {
			ss.Ended = false
		}
	}
	return flush(true)
}

// Current reports whether a repo's stored state was made by this version
// of the transcript format and with the archive's models; a repo whose
// state isn't needs reading again in full.
func Current(raw json.RawMessage, cfg *lex.Config) bool {
	if len(raw) == 0 || cfg == nil {
		return true
	}
	_, ok := loadState(raw, modelsKey(cfg))
	return ok
}

// prefixHolds reports whether the chunks already read are still the
// record's first ones.
func prefixHolds(ss *sessionState, chunks []lex.SessionChunkRef) bool {
	if ss.Read > len(chunks) {
		return false
	}
	if ss.Read > 0 && prefixDigest(chunks[:ss.Read]) != ss.Prefix {
		return false
	}
	var total int64
	for _, c := range chunks {
		total += c.Bytes
	}
	return total >= ss.Cut.Offset
}

func prefixDigest(chunks []lex.SessionChunkRef) string {
	h := sha256.New()
	for _, c := range chunks {
		h.Write([]byte(c.Blob.Link))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:32]
}

func metaDigest(r lex.Session) string {
	h := sha256.New()
	for _, s := range []string{r.Title, r.Summary, r.Repo, r.Branch, r.Cwd, r.Kind, r.Parent, r.StartedAt} {
		h.Write([]byte(s))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:32]
}

// sessionPassage is a session's title and summary, as one passage.
func sessionPassage(r lex.Session) (transcript.Passage, bool) {
	if strings.TrimSpace(r.Title+r.Summary) == "" {
		return transcript.Passage{}, false
	}
	text := "Session: " + r.Title
	if r.Summary != "" {
		text += "\n\n" + r.Summary
	}
	var where []string
	for _, s := range []string{r.Repo, r.Branch, r.Cwd} {
		if s != "" {
			where = append(where, s)
		}
	}
	if len(where) > 0 {
		text += "\n\n" + strings.Join(where, " · ")
	}
	at, _ := time.Parse(time.RFC3339, r.StartedAt)
	if at.IsZero() {
		at, _ = time.Parse(time.RFC3339, r.UpdatedAt)
	}
	return transcript.Passage{Kind: KindSession, EntryID: "record", Time: at, Text: redact.Text(text)}, true
}

// sessionTags are the record's facets every passage of it carries.
func sessionTags(r lex.Session) []string {
	tags := []string{TagSession + r.SessionID}
	if r.Kind != "" {
		tags = append(tags, TagAgent+r.Kind)
	}
	if r.Parent != "" {
		tags = append(tags, TagParent+r.Parent)
	}
	if r.Repo != "" {
		tags = append(tags, TagRepo+NormalizeRepo(r.Repo))
	}
	if r.Branch != "" {
		tags = append(tags, TagBranch+r.Branch)
	}
	return tags
}

// NormalizeRepo names a repository the same way whichever remote URL
// form it came as: host/owner/name.
func NormalizeRepo(u string) string {
	s := strings.TrimSpace(u)
	s = strings.TrimSuffix(s, "/")
	s = strings.TrimSuffix(s, ".git")
	for _, p := range []string{"https://", "http://", "ssh://", "git://"} {
		s = strings.TrimPrefix(s, p)
	}
	if at := strings.Index(s, "@"); at >= 0 && at < strings.IndexAny(s+":/", ":/") {
		s = s[at+1:]
	}
	s = strings.Replace(s, ":", "/", 1)
	return strings.ToLower(s)
}

type pendingPassage struct {
	session string
	uri     string
	facets  []string
	p       transcript.Passage
}

// Rkey is a passage's key in the index.
func Rkey(session string, p transcript.Passage) string {
	return session + ":" + p.Kind + ":" + p.EntryID
}

// Source is where a passage came from: the session record's URI, the
// entry, and the entry's byte offset in the transcript.
func Source(uri string, p transcript.Passage) string {
	return uri + "#" + p.EntryID + "@" + strconv.FormatInt(p.Offset, 10)
}

// ParseSource splits a Source back up.
func ParseSource(src string) (uri, entry string, offset int64, ok bool) {
	uri, frag, found := strings.Cut(src, "#")
	if !found {
		return src, "", 0, false
	}
	entry, off, found := strings.Cut(frag, "@")
	if !found {
		return uri, entry, 0, false
	}
	n, err := strconv.ParseInt(off, 10, 64)
	return uri, entry, n, err == nil
}

type embedders map[string]embed.Embedder

func (x *Indexer) embedders(ctx context.Context, cfg *lex.Config) (embedders, error) {
	if x.Embed == nil {
		return nil, errors.New("no embedding provider for archives")
	}
	out := embedders{}
	models := []lex.ModelInfo{cfg.ModelInfo}
	if cfg.Next != nil && cfg.Next.Valid() {
		models = append(models, *cfg.Next)
	}
	for _, m := range models {
		e, err := x.Embed.For(ctx, m)
		if err != nil {
			return nil, fmt.Errorf("the archive's model %s: %w", m, err)
		}
		out[m.Key()] = e
	}
	return out, nil
}

// embed turns passages into index entries with their vectors.
func (x *Indexer) embed(ctx context.Context, cfg *lex.Config, emb embedders, did string, ps []pendingPassage) ([]spacestore.Memory, error) {
	if len(ps) == 0 {
		return nil, nil
	}
	// A passage seen twice in a batch (a turn whose reply grew) is stored
	// once, as last seen.
	last := map[string]int{}
	for i, pp := range ps {
		last[Rkey(pp.session, pp.p)] = i
	}
	var keep []pendingPassage
	for i, pp := range ps {
		if last[Rkey(pp.session, pp.p)] == i {
			keep = append(keep, pp)
		}
	}
	out := make([]spacestore.Memory, len(keep))
	texts := make([]string, len(keep))
	for i, pp := range keep {
		tags := append([]string{}, pp.facets...)
		tags = append(tags, TagKind+pp.p.Kind)
		for _, f := range pp.p.Files {
			tags = append(tags, TagFile+f)
		}
		for _, c := range pp.p.Commands {
			tags = append(tags, TagCmd+c)
		}
		sum := sha256.Sum256([]byte(fmt.Sprintf("%d\x00%s", transcript.Version, pp.p.Text)))
		at := pp.p.Time
		if at.IsZero() {
			at = time.Now().UTC()
		}
		out[i] = spacestore.Memory{
			Author: did, Rkey: Rkey(pp.session, pp.p), CID: "t" + hex.EncodeToString(sum[:16]),
			Text: pp.p.Text, Source: Source(pp.uri, pp.p), Tags: tags, CreatedAt: at,
			Vectors: map[string][]float32{},
		}
		texts[i] = pp.p.Text
	}
	for key, e := range emb {
		prefix := cfg.DocumentPrefix
		for i := 0; i < len(texts); i += 64 {
			j := min(i+64, len(texts))
			batch := make([]string, j-i)
			for k := range batch {
				batch[k] = lex.EmbedText(prefix, texts[i+k], nil)
			}
			vs, err := e.Embed(ctx, batch)
			if err != nil {
				return nil, fmt.Errorf("embedding passages: %w", err)
			}
			if len(vs) != len(batch) {
				return nil, fmt.Errorf("embedding passages: %d vectors for %d texts", len(vs), len(batch))
			}
			for k, v := range vs {
				out[i+k].Vectors[key] = v
			}
		}
	}
	return out, nil
}

// sessionKeys lists the keys of a session's passages in the index.
func (x *Indexer) sessionKeys(ctx context.Context, spaceURI, did, session string) ([]string, error) {
	var keys []string
	cursor := ""
	for {
		hits, next, err := x.Store.List(ctx, spaceURI, 1000, cursor, spacestore.Filter{Author: did, Tags: []string{TagSession + session}})
		if err != nil {
			return nil, err
		}
		for _, h := range hits {
			keys = append(keys, h.Rkey)
		}
		if next == "" || len(hits) == 0 {
			return keys, nil
		}
		cursor = next
	}
}

// staleKeys lists the keys of the author's passages whose session isn't
// in keep, and with a state from another version, all of them.
func (x *Indexer) staleKeys(ctx context.Context, spaceURI, did string, keep map[string]bool, current bool) ([]string, error) {
	var keys []string
	cursor := ""
	for {
		hits, next, err := x.Store.List(ctx, spaceURI, 1000, cursor, spacestore.Filter{Author: did})
		if err != nil {
			return nil, err
		}
		for _, h := range hits {
			session, _, _ := strings.Cut(h.Rkey, ":")
			if !current || !keep[session] {
				keys = append(keys, h.Rkey)
			}
		}
		if next == "" || len(hits) == 0 {
			return keys, nil
		}
		cursor = next
	}
}
