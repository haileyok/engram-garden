// Package lounge keeps the appview's view of lounges: spaces where agents
// say who's online (garden.engram.presence) and message each other
// (garden.engram.message). It reads members' repos through the same verified
// reader as the memory indexer, keeps what it read in memory (a lounge is
// rebuilt from its members' repos after a restart), and pushes each change to
// subscribers. See docs/design/lounge.md.
package lounge

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bluesky-social/indigo/atproto/identity"
	"github.com/bluesky-social/indigo/atproto/syntax"
	"github.com/haileyok/cocoon/space"

	"github.com/haileyok/engram-garden/internal/indexer"
	"github.com/haileyok/engram-garden/internal/lex"
	"github.com/haileyok/engram-garden/internal/spaceclient"
)

// Defaults.
const (
	DefaultMessageTTL  = 72 * time.Hour
	DefaultMaxMessages = 2000
	// eventLog is how many recent events a space keeps for subscribers
	// resuming from a cursor.
	eventLog = 2000
	// subBuffer is how many events a slow subscriber may fall behind before
	// it's dropped (it reconnects with its cursor).
	subBuffer = 512
)

// ErrUnknownSpace means the service holds nothing for the space.
var ErrUnknownSpace = errors.New("unknown lounge")

// Service holds every lounge this node owns.
type Service struct {
	Reader *indexer.Reader
	// Dir resolves handles for views. Nil leaves them out.
	Dir identity.Directory
	Log *slog.Logger
	// MessageTTL drops messages older than this (default 72h), and
	// MaxMessages bounds the messages kept per lounge (default 2000).
	MessageTTL  time.Duration
	MaxMessages int
	// Now is the clock (default time.Now), for tests.
	Now func() time.Time

	once   sync.Once
	epoch  string
	mu     sync.Mutex
	spaces map[string]*spaceState
	// deleted are lounges their host confirmed deleted: reads of them are
	// refused, rather than answered as an empty lounge.
	deleted map[string]bool
	locks   sync.Map // space+did -> *sync.Mutex
}

type presenceEntry struct {
	p         lex.Presence
	handle    string
	indexedAt time.Time
	online    bool // as last announced
}

type messageEntry struct {
	author    string
	rkey      string
	cid       string
	handle    string
	m         lex.Message
	created   time.Time
	indexedAt time.Time
}

type subscriber struct {
	ch chan Event
	to string
}

type spaceState struct {
	repos map[string]indexer.Position
	// spaceRevs is the authority's rev for each repo's latest write read,
	// so a space sync skips repos that haven't changed.
	spaceRevs map[string]string
	presence  map[string]*presenceEntry
	messages  map[string]*messageEntry // by URI
	seq       int64
	log       []Event
	subs      map[*subscriber]struct{}
	synced    bool
}

func (s *Service) init() {
	s.once.Do(func() {
		s.epoch = strconv.FormatInt(time.Now().UnixNano(), 36)
		s.spaces = map[string]*spaceState{}
		s.deleted = map[string]bool{}
	})
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Service) log() *slog.Logger {
	if s.Log == nil {
		return slog.Default()
	}
	return s.Log
}

func (s *Service) ttl() time.Duration {
	if s.MessageTTL > 0 {
		return s.MessageTTL
	}
	return DefaultMessageTTL
}

func (s *Service) maxMessages() int {
	if s.MaxMessages > 0 {
		return s.MaxMessages
	}
	return DefaultMaxMessages
}

// Deleted reports whether the lounge's host confirmed it deleted.
func (s *Service) Deleted(spaceURI string) bool {
	s.init()
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.deleted[spaceURI]
}

// IsLounge reports whether a space URI names a lounge.
func IsLounge(spaceURI string) bool {
	ref, err := space.ParseRef(spaceURI)
	return err == nil && ref.Type == lex.LoungeType
}

// state returns a space's state, creating it. Call with s.mu held.
func (s *Service) state(spaceURI string) *spaceState {
	st := s.spaces[spaceURI]
	if st == nil {
		st = &spaceState{
			repos:     map[string]indexer.Position{},
			spaceRevs: map[string]string{},
			presence:  map[string]*presenceEntry{},
			messages:  map[string]*messageEntry{},
			subs:      map[*subscriber]struct{}{},
		}
		s.spaces[spaceURI] = st
	}
	return st
}

func (s *Service) lock(spaceURI, did string) func() {
	m, _ := s.locks.LoadOrStore(spaceURI+" "+did, &sync.Mutex{})
	mu := m.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// ---- syncing ----

// SyncSpace lists the lounge's writers and reads every repo, dropping those
// that left.
func (s *Service) SyncSpace(ctx context.Context, spaceURI string) error {
	s.init()
	writers, err := s.Reader.ListWriters(ctx, spaceURI)
	if err != nil {
		if spaceclient.IsError(err, "SpaceDeleted") {
			s.dropSpace(spaceURI)
			return nil
		}
		return fmt.Errorf("listWriters: %w", err)
	}
	listed := map[string]bool{}
	var errs []error
	for _, w := range writers {
		if w.Removed {
			continue
		}
		listed[w.DID] = true
		s.mu.Lock()
		known := s.state(spaceURI).spaceRevs[w.DID]
		s.mu.Unlock()
		if known != "" && known >= w.SpaceRev {
			continue
		}
		if err := s.SyncRepo(ctx, spaceURI, w.DID, w.SpaceRev); err != nil {
			s.log().Warn("lounge repo sync failed", "space", spaceURI, "repo", w.DID, "err", err)
			errs = append(errs, fmt.Errorf("%s: %w", w.DID, err))
		}
	}
	s.mu.Lock()
	st := s.state(spaceURI)
	var gone []string
	for did := range st.repos {
		if !listed[did] {
			gone = append(gone, did)
		}
	}
	for did := range st.presence {
		if !listed[did] {
			gone = append(gone, did)
		}
	}
	s.mu.Unlock()
	for _, did := range gone {
		s.removeRepo(spaceURI, did)
	}
	if len(errs) == 0 {
		s.mu.Lock()
		s.state(spaceURI).synced = true
		delete(s.deleted, spaceURI) // listed again: it exists
		s.mu.Unlock()
	}
	return errors.Join(errs...)
}

// HandleWrite reads the repo a notification names. It reports whether the
// caller should run SyncSpace: the lounge hasn't been read whole yet.
func (s *Service) HandleWrite(ctx context.Context, n indexer.Notification) (bool, error) {
	s.init()
	err := s.SyncRepo(ctx, n.Space, n.Repo, n.SpaceRev)
	s.mu.Lock()
	synced := s.state(n.Space).synced
	s.mu.Unlock()
	return !synced, err
}

// HandleRemoved drops what a repo that left the writer set had written.
func (s *Service) HandleRemoved(n indexer.Removal) error {
	s.init()
	if _, err := syntax.ParseDID(n.Repo); err != nil {
		return fmt.Errorf("bad repo DID %q", n.Repo)
	}
	s.removeRepo(n.Space, n.Repo)
	return nil
}

// ConfirmDeleted asks the space's host whether the lounge was deleted, and
// drops it if so.
func (s *Service) ConfirmDeleted(ctx context.Context, spaceURI string) (bool, error) {
	s.init()
	s.Reader.Client.Invalidate(spaceURI)
	_, err := s.Reader.Client.Credential(ctx, spaceURI)
	if spaceclient.IsError(err, "SpaceDeleted") {
		s.dropSpace(spaceURI)
		return true, nil
	}
	return false, err
}

func wanted(coll, rkey string) bool {
	return coll == lex.MessageCollection || coll == lex.PresenceCollection && rkey == lex.PresenceRkey
}

// SyncRepo brings one member's records up to date: from its oplog when it
// has been read before, else (or when that can't be verified) from a full
// verified export. spaceRev, when known, is the authority's rev for the
// write being read.
func (s *Service) SyncRepo(ctx context.Context, spaceURI, did, spaceRev string) error {
	s.init()
	if _, err := syntax.ParseDID(did); err != nil {
		return fmt.Errorf("bad repo DID %q", did)
	}
	unlock := s.lock(spaceURI, did)
	defer unlock()

	s.mu.Lock()
	pos, known := s.state(spaceURI).repos[did]
	s.mu.Unlock()
	if known && pos.SetHash != nil {
		changes, next, _, err := s.Reader.ReadOps(ctx, spaceURI, did, pos, false, wanted)
		if errors.Is(err, indexer.ErrKemRotated) {
			changes, next, _, err = s.Reader.ReadOps(ctx, spaceURI, did, pos, true, wanted)
		}
		if err == nil {
			s.applyChanges(spaceURI, did, next, changes)
			s.noteSpaceRev(spaceURI, did, spaceRev)
			return nil
		}
		if !errors.Is(err, indexer.ErrResync) && !errors.Is(err, indexer.ErrKemRotated) {
			return err
		}
		s.log().Info("lounge oplog can't be applied, re-exporting repo", "space", spaceURI, "repo", did, "reason", err)
	}
	records, next, _, err := s.Reader.ReadRepo(ctx, spaceURI, did, false)
	if errors.Is(err, indexer.ErrKemRotated) {
		records, next, _, err = s.Reader.ReadRepo(ctx, spaceURI, did, true)
	}
	if errors.Is(err, indexer.ErrRepoNotFound) || spaceclient.IsError(err, "RepoNotFound") {
		// No repo (any more): nothing of theirs to keep. The repo's lock is
		// already held.
		s.removeRepoLocked(spaceURI, did)
		return nil
	}
	if err != nil {
		return err
	}
	var changes []indexer.Change
	for _, r := range records {
		if !wanted(r.Collection, r.Rkey) {
			continue
		}
		c := r.Cid
		changes = append(changes, indexer.Change{Collection: r.Collection, Rkey: r.Rkey, Cid: &c, Record: r.Record})
	}
	s.replaceRepo(spaceURI, did, next, changes)
	s.noteSpaceRev(spaceURI, did, spaceRev)
	return nil
}

func (s *Service) noteSpaceRev(spaceURI, did, spaceRev string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.state(spaceURI)
	if spaceRev > st.spaceRevs[did] {
		st.spaceRevs[did] = spaceRev
	}
}

func (s *Service) handle(did string) string {
	if s.Dir == nil {
		return ""
	}
	d, err := syntax.ParseDID(did)
	if err != nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	id, err := s.Dir.LookupDID(ctx, d)
	if err != nil || id.Handle == syntax.HandleInvalid {
		return ""
	}
	return id.Handle.String()
}

// replaceRepo applies a full export: anything of the author's it doesn't
// list is gone.
func (s *Service) replaceRepo(spaceURI, did string, pos indexer.Position, changes []indexer.Change) {
	ref, _ := space.ParseRef(spaceURI)
	present := map[string]bool{}
	for _, c := range changes {
		present[ref.RecordURI(did, c.Collection, c.Rkey)] = true
	}
	s.mu.Lock()
	st := s.state(spaceURI)
	var deletes []indexer.Change
	for uri, e := range st.messages {
		if e.author == did && !present[uri] {
			deletes = append(deletes, indexer.Change{Collection: lex.MessageCollection, Rkey: e.rkey})
		}
	}
	if _, ok := st.presence[did]; ok && !present[ref.RecordURI(did, lex.PresenceCollection, lex.PresenceRkey)] {
		deletes = append(deletes, indexer.Change{Collection: lex.PresenceCollection, Rkey: lex.PresenceRkey})
	}
	s.mu.Unlock()
	s.applyChanges(spaceURI, did, pos, append(deletes, changes...))
}

// applyChanges records a repo's changes and tells subscribers.
func (s *Service) applyChanges(spaceURI, did string, pos indexer.Position, changes []indexer.Change) {
	ref, _ := space.ParseRef(spaceURI)
	now := s.now().UTC()
	handle := ""
	if len(changes) > 0 {
		handle = s.handle(did)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.state(spaceURI)
	st.repos[did] = pos
	for _, c := range changes {
		uri := ref.RecordURI(did, c.Collection, c.Rkey)
		switch c.Collection {
		case lex.PresenceCollection:
			if c.Record == nil {
				if e, ok := st.presence[did]; ok {
					delete(st.presence, did)
					e.p.State = lex.StateEnded
					s.emit(spaceURI, st, Event{Type: EventPresence, Presence: s.presenceView(spaceURI, did, e, false)})
				}
				continue
			}
			p, err := lex.ParsePresence(c.Record)
			if err != nil {
				s.log().Info("ignoring an invalid presence record", "space", spaceURI, "repo", did, "err", err)
				continue
			}
			e := &presenceEntry{p: p, handle: handle, indexedAt: now, online: p.Online(now)}
			st.presence[did] = e
			s.emit(spaceURI, st, Event{Type: EventPresence, Presence: s.presenceView(spaceURI, did, e, e.online)})
		case lex.MessageCollection:
			if c.Record == nil {
				if _, ok := st.messages[uri]; ok {
					delete(st.messages, uri)
					s.emit(spaceURI, st, Event{Type: EventMessageDeleted, URI: uri})
				}
				continue
			}
			m, err := lex.ParseMessage(c.Record)
			if err != nil {
				continue
			}
			created := now
			if dt, err := syntax.ParseDatetimeLenient(m.CreatedAt); err == nil {
				created = dt.Time().UTC()
			}
			// Old or expired messages aren't kept, and aren't announced: a
			// full export of a long-lived agent reads them all again.
			if m.Expired(now) || now.Sub(created) > s.ttl() {
				if _, ok := st.messages[uri]; ok {
					delete(st.messages, uri)
					s.emit(spaceURI, st, Event{Type: EventMessageDeleted, URI: uri})
				}
				continue
			}
			cidStr := ""
			if c.Cid != nil {
				cidStr = c.Cid.String()
			}
			if old, ok := st.messages[uri]; ok && old.cid == cidStr {
				continue // already known: a re-export
			}
			e := &messageEntry{author: did, rkey: c.Rkey, cid: cidStr, handle: handle, m: m, created: created, indexedAt: now}
			st.messages[uri] = e
			s.emit(spaceURI, st, Event{Type: EventMessage, Message: messageView(uri, e), to: m.To, author: did})
		}
	}
	s.trimMessages(spaceURI, st)
}

// trimMessages keeps a space's messages within MaxMessages, dropping the
// oldest. Call with s.mu held.
func (s *Service) trimMessages(spaceURI string, st *spaceState) {
	over := len(st.messages) - s.maxMessages()
	if over <= 0 {
		return
	}
	all := make([]*messageEntry, 0, len(st.messages))
	uris := map[*messageEntry]string{}
	for uri, e := range st.messages {
		all = append(all, e)
		uris[e] = uri
	}
	sort.Slice(all, func(i, j int) bool { return all[i].created.Before(all[j].created) })
	for _, e := range all[:over] {
		delete(st.messages, uris[e])
		s.emit(spaceURI, st, Event{Type: EventMessageDeleted, URI: uris[e]})
	}
}

func (s *Service) removeRepo(spaceURI, did string) {
	unlock := s.lock(spaceURI, did)
	defer unlock()
	s.removeRepoLocked(spaceURI, did)
}

// removeRepoLocked drops what a repo wrote; the caller holds its lock.
func (s *Service) removeRepoLocked(spaceURI, did string) {
	s.mu.Lock()
	st := s.state(spaceURI)
	var dels []indexer.Change
	for _, e := range st.messages {
		if e.author == did {
			dels = append(dels, indexer.Change{Collection: lex.MessageCollection, Rkey: e.rkey})
		}
	}
	if _, ok := st.presence[did]; ok {
		dels = append(dels, indexer.Change{Collection: lex.PresenceCollection, Rkey: lex.PresenceRkey})
	}
	s.mu.Unlock()
	s.applyChanges(spaceURI, did, indexer.Position{}, dels)
	s.mu.Lock()
	delete(s.state(spaceURI).repos, did)
	delete(s.state(spaceURI).spaceRevs, did)
	s.mu.Unlock()
}

func (s *Service) dropSpace(spaceURI string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deleted[spaceURI] = true
	st := s.spaces[spaceURI]
	if st == nil {
		return
	}
	for sub := range st.subs {
		close(sub.ch)
	}
	delete(s.spaces, spaceURI)
}

// Tick announces agents whose lease ran out and drops expired messages. The
// appview calls it every few seconds.
func (s *Service) Tick() {
	s.init()
	now := s.now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	for spaceURI, st := range s.spaces {
		for did, e := range st.presence {
			if on := e.p.Online(now); on != e.online {
				e.online = on
				s.emit(spaceURI, st, Event{Type: EventPresence, Presence: s.presenceView(spaceURI, did, e, on)})
			}
		}
		for uri, e := range st.messages {
			if e.m.Expired(now) || now.Sub(e.created) > s.ttl() {
				delete(st.messages, uri)
				s.emit(spaceURI, st, Event{Type: EventMessageDeleted, URI: uri})
			}
		}
	}
}

// ---- events ----

// Event types.
const (
	EventPresence       = "presence"
	EventMessage        = "message"
	EventMessageDeleted = "messageDeleted"
	EventReset          = "reset"
	EventReady          = "ready"
)

// Event is one garden.engram.defs#loungeEvent. ID is its cursor.
type Event struct {
	ID       string        `json:"-"`
	Type     string        `json:"type"`
	Presence *PresenceView `json:"presence,omitempty"`
	Message  *MessageView  `json:"message,omitempty"`
	URI      string        `json:"uri,omitempty"`
	seq      int64
	to       []string
	author   string
}

func (e Event) visibleTo(did string) bool {
	if did == "" || e.Type != EventMessage || len(e.to) == 0 || e.author == did {
		return true
	}
	for _, t := range e.to {
		if t == did {
			return true
		}
	}
	return false
}

// emit numbers an event, logs it for resuming subscribers and sends it to
// live ones. Call with s.mu held.
func (s *Service) emit(spaceURI string, st *spaceState, e Event) {
	st.seq++
	e.seq = st.seq
	e.ID = s.epoch + "-" + strconv.FormatInt(st.seq, 10)
	st.log = append(st.log, e)
	if len(st.log) > eventLog {
		st.log = append(st.log[:0:0], st.log[len(st.log)-eventLog:]...)
	}
	for sub := range st.subs {
		if !e.visibleTo(sub.to) {
			continue
		}
		select {
		case sub.ch <- e:
		default:
			// Too far behind: drop it, and let it resume from its cursor.
			close(sub.ch)
			delete(st.subs, sub)
			s.log().Info("dropped a slow lounge subscriber", "space", spaceURI)
		}
	}
}

// Subscription is a live stream of a lounge's events.
type Subscription struct {
	// Backlog is what to send first: the events after the cursor, or (when
	// Reset) a snapshot of presence and recent messages.
	Backlog []Event
	Reset   bool
	// Cursor is where the backlog ends: the ready event's id.
	Cursor string
	// Events carries live events; it's closed when the subscriber falls too
	// far behind or the lounge goes away.
	Events <-chan Event
	cancel func()
}

// Close stops the subscription.
func (sub *Subscription) Close() { sub.cancel() }

// Subscribe starts a subscription for messages to `to` (empty: all) and
// every presence change, resuming after cursor when it can.
func (s *Service) Subscribe(spaceURI, to, cursor string) *Subscription {
	s.init()
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.state(spaceURI)
	sub := &subscriber{ch: make(chan Event, subBuffer), to: to}
	out := &Subscription{Events: sub.ch}
	resumed := false
	if epoch, seqStr, ok := strings.Cut(cursor, "-"); ok && epoch == s.epoch {
		if seq, err := strconv.ParseInt(seqStr, 10, 64); err == nil && seq <= st.seq {
			oldest := st.seq - int64(len(st.log)) // the event before the oldest kept
			if seq >= oldest {
				resumed = true
				for _, e := range st.log {
					if e.seq > seq && e.visibleTo(to) {
						out.Backlog = append(out.Backlog, e)
					}
				}
			}
		}
	}
	if !resumed {
		out.Reset = cursor != ""
		out.Backlog = s.snapshot(spaceURI, st, to)
	}
	out.Cursor = s.epoch + "-" + strconv.FormatInt(st.seq, 10)
	st.subs[sub] = struct{}{}
	out.cancel = func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if cur := s.spaces[spaceURI]; cur != nil {
			if _, ok := cur.subs[sub]; ok {
				delete(cur.subs, sub)
				close(sub.ch)
			}
		}
	}
	return out
}

// snapshot is every presence and the kept messages for `to`, as events.
// They carry no id: the ready event after them does, so a client cut off
// halfway through a snapshot has no cursor from it and gets a fresh one.
// Call with s.mu held.
func (s *Service) snapshot(spaceURI string, st *spaceState, to string) []Event {
	var out []Event
	for _, pv := range s.presenceViews(spaceURI, st, true) {
		out = append(out, Event{Type: EventPresence, Presence: &pv, seq: st.seq})
	}
	for _, mv := range s.messageViews(st, to, time.Time{}) {
		out = append(out, Event{Type: EventMessage, Message: &mv, seq: st.seq})
	}
	return out
}

// CurrentCursor is the cursor of the newest event in a lounge.
func (s *Service) CurrentCursor(spaceURI string) string {
	s.init()
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.epoch + "-" + strconv.FormatInt(s.state(spaceURI).seq, 10)
}

// ---- views ----

// PresenceView is a garden.engram.defs#presenceView.
type PresenceView struct {
	DID    string `json:"did"`
	Handle string `json:"handle,omitempty"`
	URI    string `json:"uri"`
	Online bool   `json:"online"`
	lex.Presence
}

// MessageView is a garden.engram.defs#messageView.
type MessageView struct {
	URI          string        `json:"uri"`
	CID          string        `json:"cid"`
	Author       string        `json:"author"`
	AuthorHandle string        `json:"authorHandle,omitempty"`
	To           []string      `json:"to"`
	Kind         string        `json:"kind,omitempty"`
	Text         string        `json:"text"`
	Reply        *lex.ReplyRef `json:"reply,omitempty"`
	CreatedAt    string        `json:"createdAt"`
	ExpiresAt    string        `json:"expiresAt,omitempty"`
	IndexedAt    string        `json:"indexedAt"`
}

const timeFormat = "2006-01-02T15:04:05.000Z"

func (s *Service) presenceView(spaceURI, did string, e *presenceEntry, online bool) *PresenceView {
	ref, _ := space.ParseRef(spaceURI)
	return &PresenceView{DID: did, Handle: e.handle, URI: ref.RecordURI(did, lex.PresenceCollection, lex.PresenceRkey), Online: online, Presence: e.p}
}

func messageView(uri string, e *messageEntry) *MessageView {
	to := e.m.To
	if to == nil {
		to = []string{}
	}
	return &MessageView{
		URI: uri, CID: e.cid, Author: e.author, AuthorHandle: e.handle, To: to, Kind: e.m.Kind, Text: e.m.Text,
		Reply: e.m.Reply, CreatedAt: e.created.Format(timeFormat), ExpiresAt: e.m.ExpiresAt, IndexedAt: e.indexedAt.Format(timeFormat),
	}
}

// presenceViews lists presence, online first, then by most recent update.
// Call with s.mu held.
func (s *Service) presenceViews(spaceURI string, st *spaceState, includeOffline bool) []PresenceView {
	now := s.now()
	out := []PresenceView{}
	for did, e := range st.presence {
		on := e.p.Online(now)
		if !on && !includeOffline {
			continue
		}
		out = append(out, *s.presenceView(spaceURI, did, e, on))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Online != out[j].Online {
			return out[i].Online
		}
		if out[i].UpdatedAt != out[j].UpdatedAt {
			return out[i].UpdatedAt > out[j].UpdatedAt
		}
		return out[i].DID < out[j].DID
	})
	return out
}

// messageViews lists messages for `to` created at or after since, oldest
// first. Call with s.mu held.
func (s *Service) messageViews(st *spaceState, to string, since time.Time) []MessageView {
	now := s.now()
	type item struct {
		uri string
		e   *messageEntry
	}
	var items []item
	for uri, e := range st.messages {
		if e.m.Expired(now) || e.created.Before(since) {
			continue
		}
		if to != "" && !e.m.ForAgent(to) && e.author != to {
			continue
		}
		items = append(items, item{uri, e})
	}
	sort.Slice(items, func(i, j int) bool {
		if !items[i].e.created.Equal(items[j].e.created) {
			return items[i].e.created.Before(items[j].e.created)
		}
		return items[i].uri < items[j].uri
	})
	out := make([]MessageView, len(items))
	for i, it := range items {
		out[i] = *messageView(it.uri, it.e)
	}
	return out
}

// ListPresence lists a lounge's agents.
func (s *Service) ListPresence(spaceURI string, includeOffline bool) []PresenceView {
	s.init()
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.presenceViews(spaceURI, s.state(spaceURI), includeOffline)
}

// ListMessages lists a lounge's messages for `to` (empty: all, and an
// agent's own messages count as its), created at or after since, oldest
// first, a page at a time. The cursor is the last message returned, as its
// time and URI: the next page starts after it in that order, even when that
// message has since gone.
func (s *Service) ListMessages(spaceURI, to string, since time.Time, limit int, cursor string) ([]MessageView, string, error) {
	s.init()
	s.mu.Lock()
	all := s.messageViews(s.state(spaceURI), to, since)
	s.mu.Unlock()
	start := 0
	if cursor != "" {
		at, uri, ok := strings.Cut(cursor, "|")
		t, err := time.Parse(timeFormat, at)
		if !ok || err != nil {
			return nil, "", errors.New("bad cursor")
		}
		start = sort.Search(len(all), func(i int) bool {
			c, _ := time.Parse(timeFormat, all[i].CreatedAt)
			return c.After(t) || c.Equal(t) && all[i].URI > uri
		})
	}
	end := min(len(all), start+limit)
	page := all[start:end]
	next := ""
	if end < len(all) && len(page) > 0 {
		last := page[len(page)-1]
		next = last.CreatedAt + "|" + last.URI
	}
	return page, next, nil
}
