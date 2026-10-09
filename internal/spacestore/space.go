package spacestore

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/RoaringBitmap/roaring/v2"
	"golang.org/x/time/rate"

	"github.com/haileyok/engram-garden/internal/segment"
	"github.com/haileyok/engram-garden/internal/text"
	"github.com/haileyok/engram-garden/internal/vec"
)

var (
	// ErrNotFound reports a missing memory.
	ErrNotFound = errors.New("not found")
	// ErrNotOwner reports a space this node doesn't own (or no longer owns).
	ErrNotOwner = errors.New("this node does not own the space")
	// ErrStaleOwner reports that a newer owner has already written the
	// space, so this node must not.
	ErrStaleOwner = errors.New("a newer owner has written this space")
	// ErrSpaceDeleted reports a space its authority deleted.
	ErrSpaceDeleted = errors.New("space was deleted")
	// ErrNoModel reports a space whose authority hasn't declared a model.
	ErrNoModel = errors.New("the space has not declared an embedding model")
	// ErrOverLimit reports changes held back by a per-space limit.
	ErrOverLimit = errors.New("space is over a limit; some memories were not indexed")
	// ErrRateLimited reports too many searches for one space.
	ErrRateLimited = errors.New("too many searches for this space")
	// ErrRetryable reports a search that couldn't read the index in time.
	// The load carries on in the background.
	ErrRetryable = errors.New("the space's index is still loading; retry shortly")
)

// ModelMismatchError reports a query or vector from a model other than the
// space's.
type ModelMismatchError struct{ Want, Got ModelInfo }

func (e *ModelMismatchError) Error() string {
	return fmt.Sprintf("the space uses %s, not %s", e.Want, e.Got)
}

// Memory is one verified memory record to index, with its vectors keyed by
// ModelInfo.Key.
type Memory struct {
	Author    string
	Rkey      string
	CID       string
	Text      string
	Source    string
	Tags      []string
	CreatedAt time.Time
	Vectors   map[string][]float32
}

// Hit is an indexed memory as reads return it.
type Hit struct {
	Author    string
	Rkey      string
	CID       string
	Text      string
	Source    string
	Tags      []string
	CreatedAt time.Time
	IndexedAt time.Time
	// Similarity is the cosine similarity to the query, for search results.
	Similarity float64
}

// Filter narrows searches and listings.
type Filter struct {
	Author string
	// Tags must all be present.
	Tags  []string
	Since time.Time
}

// slot is one model's index: the active one (slot 0) or the one being
// built during a model change (slot 1).
type slot struct {
	model ModelInfo
	segs  []*seg
}

type seg struct {
	info SegmentInfo
	src  *source
	rd   *segment.Reader
	ix   *segment.Index
	// bits is the 1-bit section, or nil when the space is too large to pin
	// it in RAM and scans stream it instead.
	bits []byte
	// kw is the keyword index's in-RAM part (lengths and term index), nil
	// for a version 1 segment.
	kw   *segment.Keyword
	rows map[uint32]int // memory id -> row
}

// bufEntry is a memory in the write buffer.
type bufEntry struct {
	id   uint32
	doc  segment.Doc
	bits [2][]byte
	int8 [2][]byte
	// tf and norm are the memory's keyword terms and length code, as a
	// segment would store them.
	tf   map[string]uint32
	norm byte
}

// bufKeyword is the write buffer's keyword index: for each term, the
// buffered memories containing it. It changes only through bufPut and
// bufDrop, under the space's write lock.
type bufKeyword struct {
	terms map[string]map[*bufEntry]uint32
	// length is the buffered memories' total decoded length.
	length uint64
}

// bufPut adds a memory to the write buffer and its keyword index.
func (s *Space) bufPut(path string, e *bufEntry) {
	if e.tf == nil {
		a := text.AnalyzeMemory(e.doc.Text, e.doc.Tags, e.doc.Source)
		e.tf, e.norm = a.TF, text.EncodeLength(a.Length)
	}
	s.buf[path] = e
	if s.bufKW.terms == nil {
		s.bufKW.terms = map[string]map[*bufEntry]uint32{}
	}
	for t, tf := range e.tf {
		m := s.bufKW.terms[t]
		if m == nil {
			m = map[*bufEntry]uint32{}
			s.bufKW.terms[t] = m
		}
		m[e] = tf
	}
	s.bufKW.length += uint64(text.DecodeLength(e.norm))
}

// bufDrop removes path from the write buffer if it still holds e.
func (s *Space) bufDrop(path string, e *bufEntry) {
	if s.buf[path] != e {
		return
	}
	delete(s.buf, path)
	for t := range e.tf {
		if m := s.bufKW.terms[t]; m != nil {
			delete(m, e)
			if len(m) == 0 {
				delete(s.bufKW.terms, t)
			}
		}
	}
	s.bufKW.length -= uint64(text.DecodeLength(e.norm))
}

// loc is where a live memory is: in the buffer, or in a segment of either
// slot.
type loc struct {
	id        uint32
	author    string
	rkey      string
	cidHash   uint64
	tags      []string
	createdAt int64
	size      int64
	cover     uint8 // bit s set: indexed in slot s
	seg       [2]*seg
	row       [2]int
	buf       *bufEntry
}

type pendingRepo struct {
	pos *RepoPosition // nil: forget the position (full resync next time)
}

// Space is one space's index on its owning node.
type Space struct {
	n     *Node
	uri   string
	token uint64

	// flushMu serializes flushes and merges.
	flushMu sync.Mutex

	mu    sync.RWMutex
	man   *Manifest // the published manifest
	slots [2]*slot
	// deleted is the published deleted set. It's replaced, never modified,
	// so searches can read it without the lock.
	deleted    *roaring.Bitmap
	pendingDel map[uint32]struct{}
	inflight   map[uint32]struct{}
	buf        map[string]*bufEntry
	bufKW      bufKeyword
	locs       map[string]*loc
	repos      map[string]*pendingRepo
	nextID     uint32
	config     *SpaceConfig
	skipped    map[string]map[string]struct{}
	spaceGone  bool
	dirtySince time.Time
	liveBytes  int64
	writesDay  string
	writes     int
	readOnly   bool
	streamed   bool
	ramBytes   int64

	limiter   *rate.Limiter
	searchSem chan struct{}
	lastUsed  atomic.Int64
}

func pathKey(author, rkey string) string { return author + "\x00" + rkey }

func (s *Space) touch() { s.lastUsed.Store(s.n.now().UnixNano()) }

func (s *Space) markDirty() {
	if s.dirtySince.IsZero() {
		s.dirtySince = s.n.now()
	}
}

func (s *Space) dirty() bool { return !s.dirtySince.IsZero() }

func (s *Space) isDeleted(id uint32, pending map[uint32]struct{}, published *roaring.Bitmap) bool {
	if _, ok := pending[id]; ok {
		return true
	}
	return published.Contains(id)
}

// quantize returns a memory's 1-bit and int8 forms for a slot's model, or
// nil when the memory has no usable vector for it.
func quantize(m Memory, model ModelInfo) ([]byte, []byte) {
	v, ok := m.Vectors[model.Key()]
	if !ok || len(v) != model.Dims {
		return nil, nil
	}
	v = slices.Clone(v)
	if !vec.Normalize(v) {
		return nil, nil
	}
	return vec.AppendBits(nil, v), vec.AppendInt8(nil, v)
}

// deleteLoc removes a live memory. Called with mu held.
func (s *Space) deleteLoc(path string) {
	l := s.locs[path]
	if l == nil {
		return
	}
	delete(s.locs, path)
	s.liveBytes -= l.size
	if l.buf != nil {
		s.bufDrop(path, l.buf)
		// An id being flushed right now will be published; mark it deleted
		// for the next manifest. Otherwise it was never published.
		if _, flying := s.inflight[l.id]; !flying {
			return
		}
	}
	s.pendingDel[l.id] = struct{}{}
}

func (s *Space) skip(author, rkey string) {
	m := s.skipped[author]
	if m == nil {
		m = map[string]struct{}{}
		s.skipped[author] = m
	}
	m[rkey] = struct{}{}
}

func (s *Space) unskip(author, rkey string) {
	if m := s.skipped[author]; m != nil {
		delete(m, rkey)
		if len(m) == 0 {
			delete(s.skipped, author)
		}
	}
}

// apply buffers one repo's verified changes and advances its position,
// unless a limit held some back.
func (s *Space) apply(did string, pos RepoPosition, upserts []Memory, deletes []string, replace bool) error {
	s.mu.Lock()
	if err := s.writable(); err != nil {
		s.mu.Unlock()
		return err
	}
	lim := s.n.opt.Limits
	day := s.n.now().UTC().Format("2006-01-02")
	if day != s.writesDay {
		s.writesDay, s.writes = day, 0
	}
	if replace {
		keep := make(map[string]bool, len(upserts))
		for _, m := range upserts {
			keep[m.Rkey] = true
		}
		for path, l := range s.locs {
			if l.author == did && !keep[l.rkey] {
				s.deleteLoc(path)
			}
		}
		delete(s.skipped, did)
	}
	for _, rkey := range deletes {
		s.deleteLoc(pathKey(did, rkey))
		s.unskip(did, rkey)
	}
	held := false
	for _, m := range upserts {
		if m.Author != did {
			s.mu.Unlock()
			return fmt.Errorf("memory %s is not in repo %s", m.Rkey, did)
		}
		path := pathKey(did, m.Rkey)
		e := &bufEntry{doc: segment.Doc{
			Author: did, Rkey: m.Rkey, CID: m.CID, Text: m.Text, Source: m.Source, Tags: m.Tags,
			CreatedAt: m.CreatedAt.UTC(), IndexedAt: s.n.now().UTC(),
		}}
		var cover uint8
		for i, sl := range s.slots {
			if sl == nil {
				continue
			}
			if b, iv := quantize(m, sl.model); b != nil {
				e.bits[i], e.int8[i] = b, iv
				cover |= 1 << i
			}
		}
		if cover == 0 {
			// No vector from the space's model: an old version can't stay
			// either, since the record changed.
			s.deleteLoc(path)
			s.skip(did, m.Rkey)
			continue
		}
		s.unskip(did, m.Rkey)
		old := s.locs[path]
		if old != nil && old.cidHash == segment.CIDHash(m.CID) && old.cover&cover == cover {
			continue // already indexed
		}
		size := int64(len(m.Text) + len(m.Source))
		var oldSize int64
		if old != nil {
			oldSize = old.size
		}
		// New memories count against the memory limit; any write that
		// grows the space counts against the byte limit.
		if (old == nil && lim.MaxMemories > 0 && len(s.locs) >= lim.MaxMemories) ||
			(lim.MaxBytes > 0 && size > oldSize && s.liveBytes-oldSize+size > lim.MaxBytes) {
			held = true
			continue
		}
		if lim.WritesPerDay > 0 && s.writes >= lim.WritesPerDay {
			held = true
			continue
		}
		s.writes++
		s.deleteLoc(path)
		e.id = s.nextID
		s.nextID++
		e.doc.ID = e.id
		s.bufPut(path, e)
		s.locs[path] = &loc{
			id: e.id, author: did, rkey: m.Rkey, cidHash: segment.CIDHash(m.CID), tags: m.Tags,
			createdAt: e.doc.CreatedAt.UnixMicro(), size: size, cover: cover, buf: e,
		}
		s.liveBytes += size
	}
	// Positions advance only when every change was taken, so held-back
	// memories are pulled again on the next sync.
	if !held {
		p := pos
		s.repos[did] = &pendingRepo{pos: &p}
	}
	s.markDirty()
	full := len(s.buf) >= s.n.opt.FlushCount
	s.mu.Unlock()
	if full {
		s.n.flushSoon(s)
	}
	if held {
		return ErrOverLimit
	}
	return nil
}

func (s *Space) writable() error {
	if s.readOnly {
		return ErrNotOwner
	}
	if s.spaceGone {
		return ErrSpaceDeleted
	}
	return nil
}

func (s *Space) repoState(did string) *RepoPosition {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if p, ok := s.repos[did]; ok {
		if p.pos == nil {
			return nil
		}
		c := *p.pos
		return &c
	}
	if p, ok := s.man.Repos[did]; ok {
		return &p
	}
	return nil
}

// knownRepos lists every repo with a position, pending or published.
func (s *Space) knownRepos() map[string]RepoPosition {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := map[string]RepoPosition{}
	for did, p := range s.man.Repos {
		out[did] = p
	}
	for did, p := range s.repos {
		if p.pos == nil {
			delete(out, did)
		} else {
			out[did] = *p.pos
		}
	}
	return out
}

func (s *Space) removeRepo(did string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.writable(); err != nil {
		return err
	}
	for path, l := range s.locs {
		if l.author == did {
			s.deleteLoc(path)
		}
	}
	delete(s.skipped, did)
	s.repos[did] = &pendingRepo{}
	s.markDirty()
	return nil
}

// setConfig records the authority's declared model and reshapes the slots
// to match. It reports whether every repo must be synced again from
// scratch, because the space now needs vectors it hasn't seen.
func (s *Space) setConfig(cfg *SpaceConfig) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.writable(); err != nil {
		return false, err
	}
	if configEqual(s.config, cfg) {
		return false, nil
	}
	s.config = cfg
	s.markDirty()
	if cfg == nil {
		return false, nil
	}
	resync := false
	switch {
	case s.slots[0] == nil:
		s.slots[0] = &slot{model: cfg.ModelInfo}
		resync = true
	case s.slots[0].model == cfg.ModelInfo:
	case s.slots[1] != nil && s.slots[1].model == cfg.ModelInfo:
		// Promote the building index in one step.
		s.dropSlot(0)
		s.slots[0], s.slots[1] = s.slots[1], nil
		for _, l := range s.locs {
			l.cover >>= 1
			l.seg[0], l.row[0], l.seg[1] = l.seg[1], l.row[1], nil
		}
		for _, e := range s.buf {
			e.bits[0], e.int8[0], e.bits[1], e.int8[1] = e.bits[1], e.int8[1], nil, nil
		}
	default:
		s.dropSlot(0)
		s.slots[0] = &slot{model: cfg.ModelInfo}
		resync = true
	}
	switch {
	case cfg.Next == nil || *cfg.Next == cfg.ModelInfo:
		if s.slots[1] != nil {
			s.dropSlot(1)
		}
	case s.slots[1] == nil || s.slots[1].model != *cfg.Next:
		if s.slots[1] != nil {
			s.dropSlot(1)
		}
		s.slots[1] = &slot{model: *cfg.Next}
		resync = true
	}
	if resync {
		for did := range s.man.Repos {
			s.repos[did] = &pendingRepo{}
		}
		for did := range s.repos {
			s.repos[did] = &pendingRepo{}
		}
	}
	return resync, nil
}

// dropSlot empties a slot: memories indexed only there are deleted.
// Called with mu held.
func (s *Space) dropSlot(i int) {
	s.slots[i] = nil
	bit := uint8(1) << i
	for path, l := range s.locs {
		l.cover &^= bit
		l.seg[i] = nil
		if l.cover == 0 {
			s.deleteLoc(path)
		}
	}
	for _, e := range s.buf {
		e.bits[i], e.int8[i] = nil, nil
	}
}

func configEqual(a, b *SpaceConfig) bool {
	if a == nil || b == nil {
		return a == b
	}
	if a.ModelInfo != b.ModelInfo || a.DocumentPrefix != b.DocumentPrefix || a.QueryPrefix != b.QueryPrefix {
		return false
	}
	if a.Next == nil || b.Next == nil {
		return a.Next == b.Next
	}
	return *a.Next == *b.Next
}

func (s *Space) markSpaceDeleted() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.spaceGone {
		return
	}
	s.spaceGone = true
	for path := range s.locs {
		s.deleteLoc(path)
	}
	s.repos = map[string]*pendingRepo{}
	for did := range s.man.Repos {
		s.repos[did] = &pendingRepo{}
	}
	s.skipped = map[string]map[string]struct{}{}
	s.markDirty()
}

// ---- reads ----

// docRef locates one memory's document.
type docRef struct {
	seg *seg
	row int
	buf *bufEntry
}

func (l *loc) ref() docRef {
	if l.buf != nil {
		return docRef{buf: l.buf}
	}
	for i := range l.seg {
		if l.seg[i] != nil {
			return docRef{seg: l.seg[i], row: l.row[i]}
		}
	}
	return docRef{}
}

// fetchDocs reads documents, grouping reads by segment.
func fetchDocs(ctx context.Context, refs []docRef) ([]segment.Body, error) {
	out := make([]segment.Body, len(refs))
	bySeg := map[*seg][]int{}
	for i, r := range refs {
		switch {
		case r.buf != nil:
			d := r.buf.doc
			out[i] = segment.Body{Text: d.Text, Source: d.Source, CID: d.CID, IndexedAt: d.IndexedAt}
		case r.seg != nil:
			bySeg[r.seg] = append(bySeg[r.seg], i)
		default:
			return nil, errors.New("memory has no location")
		}
	}
	var mu sync.Mutex
	var wg sync.WaitGroup
	var firstErr error
	for sg, idxs := range bySeg {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rows := make([]int, len(idxs))
			for k, i := range idxs {
				rows[k] = refs[i].row
			}
			bodies, err := sg.rd.ReadDocs(ctx, sg.ix, rows)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if firstErr == nil {
					firstErr = err
				}
				return
			}
			for k, i := range idxs {
				out[i] = bodies[k]
			}
		}()
	}
	wg.Wait()
	return out, firstErr
}

func (s *Space) get(ctx context.Context, author, rkey string) (Hit, error) {
	s.mu.RLock()
	l := s.locs[pathKey(author, rkey)]
	var ref docRef
	var h Hit
	if l != nil {
		ref = l.ref()
		h = Hit{Author: l.author, Rkey: l.rkey, Tags: l.tags, CreatedAt: time.UnixMicro(l.createdAt).UTC()}
	}
	s.mu.RUnlock()
	if l == nil {
		return Hit{}, ErrNotFound
	}
	bodies, err := fetchDocs(ctx, []docRef{ref})
	if err != nil {
		return Hit{}, err
	}
	h.Text, h.Source, h.CID, h.IndexedAt = bodies[0].Text, bodies[0].Source, bodies[0].CID, bodies[0].IndexedAt
	return h, nil
}

func (f Filter) matchLoc(l *loc) bool {
	if f.Author != "" && l.author != f.Author {
		return false
	}
	if !f.Since.IsZero() && l.createdAt < f.Since.UnixMicro() {
		return false
	}
	for _, t := range f.Tags {
		if !slices.Contains(l.tags, t) {
			return false
		}
	}
	return true
}

// ErrBadCursor reports an unparseable list cursor.
var ErrBadCursor = errors.New("bad cursor")

type listKey struct {
	createdAt    int64
	author, rkey string
}

func (a listKey) less(b listKey) bool {
	if a.createdAt != b.createdAt {
		return a.createdAt < b.createdAt
	}
	if a.author != b.author {
		return a.author < b.author
	}
	return a.rkey < b.rkey
}

func (s *Space) list(ctx context.Context, limit int, cursor string, f Filter) ([]Hit, string, error) {
	if limit <= 0 {
		return nil, "", errors.New("limit must be positive")
	}
	var after *listKey
	if cursor != "" {
		k, err := decodeCursor(cursor)
		if err != nil {
			return nil, "", err
		}
		after = &k
	}
	type item struct {
		key listKey
		l   *loc
		ref docRef
	}
	s.mu.RLock()
	var items []item
	for _, l := range s.locs {
		if !f.matchLoc(l) {
			continue
		}
		k := listKey{l.createdAt, l.author, l.rkey}
		if after != nil && !k.less(*after) {
			continue
		}
		items = append(items, item{key: k, l: l, ref: l.ref()})
	}
	s.mu.RUnlock()
	// Newest first.
	slices.SortFunc(items, func(a, b item) int {
		switch {
		case b.key.less(a.key):
			return -1
		case a.key.less(b.key):
			return 1
		}
		return 0
	})
	if len(items) > limit {
		items = items[:limit]
	}
	refs := make([]docRef, len(items))
	for i, it := range items {
		refs[i] = it.ref
	}
	bodies, err := fetchDocs(ctx, refs)
	if err != nil {
		return nil, "", err
	}
	out := make([]Hit, len(items))
	for i, it := range items {
		out[i] = Hit{
			Author: it.l.author, Rkey: it.l.rkey, Tags: it.l.tags, CreatedAt: time.UnixMicro(it.l.createdAt).UTC(),
			Text: bodies[i].Text, Source: bodies[i].Source, CID: bodies[i].CID, IndexedAt: bodies[i].IndexedAt,
		}
	}
	next := ""
	if len(items) == limit {
		next = encodeCursor(items[len(items)-1].key)
	}
	return out, next, nil
}

func encodeCursor(k listKey) string {
	return fmt.Sprintf("%d~%s~%s", k.createdAt, k.author, k.rkey)
}

func decodeCursor(c string) (listKey, error) {
	parts := strings.SplitN(c, "~", 3)
	if len(parts) != 3 {
		return listKey{}, ErrBadCursor
	}
	var t int64
	if _, err := fmt.Sscan(parts[0], &t); err != nil {
		return listKey{}, ErrBadCursor
	}
	return listKey{t, parts[1], parts[2]}, nil
}

// Status summarizes a space's index.
type Status struct {
	Config           *SpaceConfig
	Active           *ModelInfo
	Building         *ModelInfo
	Memories         int
	BuildingMemories int // memories with a vector for the building model
	Segments         int
	Buffered         int
	// Skipped counts, per author, memories whose vectors don't match.
	Skipped    map[string]int
	Generation uint64
	Token      uint64
	Deleted    bool
}

func (s *Space) status() Status {
	s.mu.RLock()
	defer s.mu.RUnlock()
	st := Status{Config: s.config, Memories: len(s.locs), Buffered: len(s.buf), Skipped: map[string]int{},
		Generation: s.man.Generation, Token: s.man.Token, Deleted: s.spaceGone}
	if s.slots[0] != nil {
		m := s.slots[0].model
		st.Active = &m
		st.Segments += len(s.slots[0].segs)
	}
	if s.slots[1] != nil {
		m := s.slots[1].model
		st.Building = &m
		st.Segments += len(s.slots[1].segs)
		for _, l := range s.locs {
			if l.cover&2 != 0 {
				st.BuildingMemories++
			}
		}
	}
	for a, rk := range s.skipped {
		st.Skipped[a] = len(rk)
	}
	return st
}
