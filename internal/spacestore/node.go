// Package spacestore is the index: each space's memories live in immutable
// segment files in object storage, listed by the space's newest manifest.
// One node owns a space at a time and keeps its hot parts in RAM and on
// local disk, serving both its searches and its sync.
package spacestore

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/RoaringBitmap/roaring/v2"
	"golang.org/x/sync/errgroup"
	"golang.org/x/sync/singleflight"
	"golang.org/x/time/rate"

	"github.com/haileyok/engram-garden/internal/blob"
)

// Limits bound what one space may use.
type Limits struct {
	MaxMemories int
	// MaxBytes bounds the text and source stored for a space.
	MaxBytes              int64
	SearchesPerSecond     float64
	WritesPerDay          int
	MaxConcurrentSearches int
}

// Options configure a node.
type Options struct {
	Blob blob.Store
	// CacheDir holds whole segment files on local disk. Empty disables the
	// disk cache.
	CacheDir   string
	CacheBytes int64
	// RAMBytes is the node's budget for loaded spaces' indexes and 1-bit
	// sections. SpaceRAMShare caps one space's 1-bit sections in RAM;
	// larger spaces stream them from disk or object storage.
	RAMBytes      int64
	SpaceRAMShare int64
	// Lease reports this node's fencing token for a space, and whether it
	// owns the space at all.
	Lease func(spaceURI string) (token uint64, ok bool)
	// ConditionalWrites writes manifests with If-None-Match. Enable it only
	// when blob.Probe says the store honors it.
	ConditionalWrites bool

	FlushCount       int
	FlushAge         time.Duration
	MaxSegments      int
	MaxDeletedRatio  float64
	MergeInterval    time.Duration
	MinRetention     time.Duration
	Candidates       int
	Deadline         time.Duration
	HardLimit        time.Duration
	ClusterThreshold int
	Limits           Limits

	Log *slog.Logger
	// Now overrides the clock (tests).
	Now func() time.Time
}

func (o *Options) defaults() {
	if o.Lease == nil {
		o.Lease = func(string) (uint64, bool) { return 1, true }
	}
	def := func(p *int, v int) {
		if *p <= 0 {
			*p = v
		}
	}
	defD := func(p *time.Duration, v time.Duration) {
		if *p <= 0 {
			*p = v
		}
	}
	def(&o.FlushCount, 1000)
	def(&o.MaxSegments, 8)
	def(&o.Candidates, 200)
	def(&o.ClusterThreshold, 250_000)
	defD(&o.FlushAge, time.Hour)
	defD(&o.MergeInterval, 24*time.Hour)
	defD(&o.MinRetention, 90*24*time.Hour)
	defD(&o.Deadline, 3*time.Second)
	defD(&o.HardLimit, 10*time.Second)
	if o.MaxDeletedRatio <= 0 {
		o.MaxDeletedRatio = 0.25
	}
	if o.RAMBytes <= 0 {
		o.RAMBytes = 8 << 30
	}
	if o.SpaceRAMShare <= 0 {
		o.SpaceRAMShare = 1 << 30
	}
	if o.CacheBytes <= 0 {
		o.CacheBytes = 100 << 30
	}
	if o.Now == nil {
		o.Now = time.Now
	}
}

// Node holds the spaces this appview node owns.
type Node struct {
	opt   Options
	cache *diskCache

	mu     sync.Mutex
	spaces map[string]*Space
	loads  singleflight.Group
	remote counter

	flushing sync.Map // *Space -> struct{}: a background flush is queued
	bg       sync.WaitGroup
	closed   chan struct{}
}

// New starts a node.
func New(opt Options) (*Node, error) {
	if opt.Blob == nil {
		return nil, errors.New("spacestore: Blob is required")
	}
	opt.defaults()
	n := &Node{opt: opt, spaces: map[string]*Space{}, closed: make(chan struct{})}
	if opt.CacheDir != "" {
		n.cache = newDiskCache(opt.CacheDir, opt.CacheBytes)
	}
	return n, nil
}

func (n *Node) now() time.Time { return n.opt.Now() }

func (n *Node) log() *slog.Logger {
	if n.opt.Log == nil {
		return slog.Default()
	}
	return n.opt.Log
}

// RemoteReads reports how many range reads, and bytes, went to object
// storage.
func (n *Node) RemoteReads() (reads, bytes int64) {
	n.remote.mu.Lock()
	defer n.remote.mu.Unlock()
	return n.remote.reads, n.remote.byts
}

// space returns a loaded space, loading it if needed. Concurrent callers
// share one load, which continues in the background if they give up.
func (n *Node) space(ctx context.Context, spaceURI string) (*Space, error) {
	n.mu.Lock()
	s := n.spaces[spaceURI]
	n.mu.Unlock()
	if s != nil {
		if s.readOnly {
			return nil, ErrNotOwner
		}
		s.touch()
		return s, nil
	}
	ch := n.loads.DoChan(spaceURI, func() (any, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		return n.load(ctx, spaceURI)
	})
	select {
	case r := <-ch:
		if r.Err != nil {
			return nil, r.Err
		}
		s := r.Val.(*Space)
		s.touch()
		return s, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (n *Node) load(ctx context.Context, spaceURI string) (*Space, error) {
	token, ok := n.opt.Lease(spaceURI)
	if !ok {
		return nil, ErrNotOwner
	}
	man, err := LoadLatestManifest(ctx, n.opt.Blob, spaceURI)
	switch {
	case errors.Is(err, ErrNoManifest):
		man = &Manifest{Format: ManifestFormat, Space: spaceURI, Token: token, NextMemoryID: 1, Repos: map[string]RepoPosition{}}
	case err != nil:
		return nil, err
	case man.Token > token:
		return nil, fmt.Errorf("%w (manifest token %d, ours %d)", ErrStaleOwner, man.Token, token)
	case man.Token < token:
		// Handover: publish the state under our own token before serving,
		// so the previous owner's later uploads are never chosen.
		man.Token, man.Generation = token, 1
		man.UpdatedAt = n.now().UTC()
		if err := n.putManifest(ctx, man); err != nil {
			return nil, err
		}
	}
	s := &Space{
		n: n, uri: spaceURI, token: token, man: man,
		pendingDel: map[uint32]struct{}{}, inflight: map[uint32]struct{}{},
		buf: map[string]*bufEntry{}, locs: map[string]*loc{}, repos: map[string]*pendingRepo{},
		skipped: map[string]map[string]struct{}{}, nextID: max(man.NextMemoryID, 1),
		config: man.Config, spaceGone: man.SpaceDeleted,
		searchSem: make(chan struct{}, max(1, n.opt.Limits.MaxConcurrentSearches, 8)),
	}
	if n.opt.Limits.SearchesPerSecond > 0 {
		s.limiter = rate.NewLimiter(rate.Limit(n.opt.Limits.SearchesPerSecond), max(1, int(n.opt.Limits.SearchesPerSecond)))
	}
	if n.opt.Limits.MaxConcurrentSearches > 0 {
		s.searchSem = make(chan struct{}, n.opt.Limits.MaxConcurrentSearches)
	}
	for a, rks := range man.Skipped {
		for _, rk := range rks {
			s.skip(a, rk)
		}
	}
	s.deleted = roaring.New()
	if len(man.Deleted) > 0 {
		if _, err := s.deleted.FromUnsafeBytes(append([]byte(nil), man.Deleted...)); err != nil {
			return nil, fmt.Errorf("manifest deleted set: %w", err)
		}
	}

	// Large spaces stream their 1-bit sections instead of pinning them.
	var bitBytes int64
	for _, im := range []*IndexManifest{man.Active, man.Building} {
		if im != nil {
			for _, si := range im.Segments {
				bitBytes += int64(si.Count) * int64((im.Dims+63)/64*8)
			}
		}
	}
	s.streamed = bitBytes > n.opt.SpaceRAMShare
	for i, im := range []*IndexManifest{man.Active, man.Building} {
		if im == nil {
			continue
		}
		sl := &slot{model: im.ModelInfo, segs: make([]*seg, len(im.Segments))}
		g, gctx := errgroup.WithContext(ctx)
		g.SetLimit(16)
		for k, si := range im.Segments {
			g.Go(func() error {
				sg, err := n.openSegment(gctx, spaceURI, si, !s.streamed)
				sl.segs[k] = sg
				return err
			})
		}
		if err := g.Wait(); err != nil {
			return nil, err
		}
		s.slots[i] = sl
		for _, sg := range sl.segs {
			s.ramBytes += sg.ramBytes()
			for row := range sg.ix.Count {
				id := sg.ix.ID(row)
				if s.deleted.Contains(id) {
					continue
				}
				m := sg.ix.Meta(row)
				path := pathKey(m.Author, m.Rkey)
				l := s.locs[path]
				if l != nil && l.id != id {
					// Two live versions (shouldn't happen): keep the newer.
					if l.id > id {
						s.pendingDel[id] = struct{}{}
						continue
					}
					s.pendingDel[l.id] = struct{}{}
					s.deleteLocQuiet(path)
					l = nil
				}
				if l == nil {
					l = &loc{
						id: id, author: m.Author, rkey: m.Rkey, tags: m.Tags, createdAt: m.CreatedAt.UnixMicro(),
						cidHash: sg.ix.CIDHash(row), size: sg.ix.DocSize(row),
					}
					s.locs[path] = l
				}
				l.cover |= 1 << i
				l.seg[i], l.row[i] = sg, row
			}
		}
	}
	for _, l := range s.locs {
		s.liveBytes += l.size
	}
	if len(s.pendingDel) > 0 {
		s.markDirty()
	}

	n.mu.Lock()
	n.spaces[spaceURI] = s
	n.mu.Unlock()
	s.touch()
	n.noteRAM()
	// Pull the rest of the space onto local disk in the background.
	if n.cache != nil {
		var keys []string
		s.mu.RLock()
		for _, sl := range s.slots {
			if sl != nil {
				for _, sg := range sl.segs {
					keys = append(keys, sg.src.key)
				}
			}
		}
		s.mu.RUnlock()
		n.bg.Add(1)
		go func() {
			defer n.bg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
			defer cancel()
			for _, key := range keys {
				if err := n.cache.fetch(ctx, n.opt.Blob, key); err != nil {
					n.log().Debug("background segment fetch failed", "key", key, "err", err)
				}
			}
		}()
	}
	return s, nil
}

func (s *Space) deleteLocQuiet(path string) { delete(s.locs, path) }

// noteRAM evicts the least recently used clean spaces while over budget.
func (n *Node) noteRAM() {
	n.mu.Lock()
	var total int64
	spaces := make([]*Space, 0, len(n.spaces))
	for _, s := range n.spaces {
		s.mu.RLock()
		total += s.ramBytes
		s.mu.RUnlock()
		spaces = append(spaces, s)
	}
	if total <= n.opt.RAMBytes {
		n.mu.Unlock()
		return
	}
	sort.Slice(spaces, func(i, j int) bool { return spaces[i].lastUsed.Load() < spaces[j].lastUsed.Load() })
	for _, s := range spaces[:len(spaces)-1] { // never evict the most recent
		if total <= n.opt.RAMBytes {
			break
		}
		s.mu.RLock()
		clean, rb := !s.dirty(), s.ramBytes
		s.mu.RUnlock()
		if !clean {
			continue
		}
		delete(n.spaces, s.uri)
		total -= rb
	}
	n.mu.Unlock()
}

// Unload drops a space from memory, flushing it first.
func (n *Node) Unload(ctx context.Context, spaceURI string) error {
	n.mu.Lock()
	s := n.spaces[spaceURI]
	n.mu.Unlock()
	if s == nil {
		return nil
	}
	if err := s.Flush(ctx); err != nil && !errors.Is(err, ErrNotOwner) {
		return err
	}
	n.mu.Lock()
	if n.spaces[spaceURI] == s {
		delete(n.spaces, spaceURI)
	}
	n.mu.Unlock()
	return nil
}

// flushSoon queues a background flush of a space.
func (n *Node) flushSoon(s *Space) {
	if _, queued := n.flushing.LoadOrStore(s, struct{}{}); queued {
		return
	}
	n.bg.Add(1)
	go func() {
		defer n.bg.Done()
		defer n.flushing.Delete(s)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		if err := s.Flush(ctx); err != nil {
			n.log().Warn("flush failed", "space", s.uri, "err", err)
		}
	}()
}

// Tick flushes spaces whose buffers are full or old, and runs maintenance.
// Run calls it periodically.
func (n *Node) Tick(ctx context.Context) {
	n.mu.Lock()
	spaces := make([]*Space, 0, len(n.spaces))
	for _, s := range n.spaces {
		spaces = append(spaces, s)
	}
	n.mu.Unlock()
	for _, s := range spaces {
		s.mu.RLock()
		due := s.dirty() && (len(s.buf) >= n.opt.FlushCount || n.now().Sub(s.dirtySince) >= n.opt.FlushAge)
		s.mu.RUnlock()
		if due {
			if err := s.Flush(ctx); err != nil {
				n.log().Warn("flush failed", "space", s.uri, "err", err)
			}
		}
	}
	n.noteRAM()
}

// Run ticks until ctx ends, then flushes every space.
func (n *Node) Run(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			n.Tick(ctx)
		}
	}
}

// Close flushes every dirty space and waits for background work.
func (n *Node) Close(ctx context.Context) error {
	n.bg.Wait()
	n.mu.Lock()
	spaces := make([]*Space, 0, len(n.spaces))
	for _, s := range n.spaces {
		spaces = append(spaces, s)
	}
	n.mu.Unlock()
	var errs []error
	for _, s := range spaces {
		if err := s.Flush(ctx); err != nil && !errors.Is(err, ErrNotOwner) {
			errs = append(errs, fmt.Errorf("%s: %w", s.uri, err))
		}
	}
	return errors.Join(errs...)
}

// ---- public API, by space URI ----

// Warm starts loading a space without waiting, as when an agent's session
// starts.
func (n *Node) Warm(spaceURI string) {
	n.bg.Add(1)
	go func() {
		defer n.bg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		if _, err := n.space(ctx, spaceURI); err != nil {
			n.log().Debug("warming space failed", "space", spaceURI, "err", err)
		}
	}()
}

// RepoState returns a repo's sync position, or nil to sync it from
// scratch.
func (n *Node) RepoState(ctx context.Context, spaceURI, did string) (*RepoPosition, error) {
	s, err := n.space(ctx, spaceURI)
	if err != nil {
		return nil, err
	}
	return s.repoState(did), nil
}

// Repos lists the repos with sync positions.
func (n *Node) Repos(ctx context.Context, spaceURI string) (map[string]RepoPosition, error) {
	s, err := n.space(ctx, spaceURI)
	if err != nil {
		return nil, err
	}
	return s.knownRepos(), nil
}

// ApplyRepoChanges buffers one repo's verified changes and its new
// position. With replace, every other memory of the author is dropped, as
// after a full export. It returns ErrOverLimit when limits held changes
// back; the position then stays put so they're pulled again.
func (n *Node) ApplyRepoChanges(ctx context.Context, spaceURI, did string, pos RepoPosition, upserts []Memory, deleteRkeys []string, replace bool) error {
	s, err := n.space(ctx, spaceURI)
	if err != nil {
		return err
	}
	return s.apply(did, pos, upserts, deleteRkeys, replace)
}

// RemoveRepo drops a member's memories and position.
func (n *Node) RemoveRepo(ctx context.Context, spaceURI, did string) error {
	s, err := n.space(ctx, spaceURI)
	if err != nil {
		return err
	}
	return s.removeRepo(did)
}

// SetConfig records the space's declared model. When it returns true,
// every repo needs a full sync to supply vectors for a new model.
func (n *Node) SetConfig(ctx context.Context, spaceURI string, cfg *SpaceConfig) (bool, error) {
	s, err := n.space(ctx, spaceURI)
	if err != nil {
		return false, err
	}
	resync, err := s.setConfig(cfg)
	if err == nil {
		n.flushSoon(s)
	}
	return resync, err
}

// Config returns the space's declared model, if any.
func (n *Node) Config(ctx context.Context, spaceURI string) (*SpaceConfig, error) {
	s, err := n.space(ctx, spaceURI)
	if err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.config, nil
}

// MarkSpaceDeleted drops the space's index and remembers that it's gone.
func (n *Node) MarkSpaceDeleted(ctx context.Context, spaceURI string) error {
	s, err := n.space(ctx, spaceURI)
	if err != nil {
		return err
	}
	s.markSpaceDeleted()
	return s.Flush(ctx)
}

// SpaceDeleted reports whether the space was deleted.
func (n *Node) SpaceDeleted(ctx context.Context, spaceURI string) (bool, error) {
	s, err := n.space(ctx, spaceURI)
	if err != nil {
		return false, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.spaceGone, nil
}

// Search finds the memories nearest a query vector.
func (n *Node) Search(ctx context.Context, spaceURI string, q SearchQuery) (*SearchResult, error) {
	// Deadlines run on the real clock, whatever Options.Now says.
	start := time.Now()
	loadCtx, cancel := context.WithDeadline(ctx, start.Add(n.opt.HardLimit))
	defer cancel()
	s, err := n.space(loadCtx, spaceURI)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
			return nil, ErrRetryable
		}
		return nil, err
	}
	if s.limiter != nil && !s.limiter.Allow() {
		return nil, ErrRateLimited
	}
	select {
	case s.searchSem <- struct{}{}:
		defer func() { <-s.searchSem }()
	case <-loadCtx.Done():
		return nil, ErrRetryable
	}
	return s.search(ctx, q, start)
}

// Get returns one memory.
func (n *Node) Get(ctx context.Context, spaceURI, author, rkey string) (Hit, error) {
	s, err := n.space(ctx, spaceURI)
	if err != nil {
		return Hit{}, err
	}
	return s.get(ctx, author, rkey)
}

// List returns memories newest first, with a cursor for the next page.
func (n *Node) List(ctx context.Context, spaceURI string, limit int, cursor string, f Filter) ([]Hit, string, error) {
	s, err := n.space(ctx, spaceURI)
	if err != nil {
		return nil, "", err
	}
	return s.list(ctx, limit, cursor, f)
}

// Status summarizes a space's index.
func (n *Node) Status(ctx context.Context, spaceURI string) (Status, error) {
	s, err := n.space(ctx, spaceURI)
	if err != nil {
		return Status{}, err
	}
	return s.status(), nil
}

// Flush publishes a space's buffered changes now.
func (n *Node) Flush(ctx context.Context, spaceURI string) error {
	s, err := n.space(ctx, spaceURI)
	if err != nil {
		return err
	}
	return s.Flush(ctx)
}
