package spacestore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/RoaringBitmap/roaring/v2"

	"github.com/haileyok/engram-garden/internal/blob"
	"github.com/haileyok/engram-garden/internal/metrics"
	"github.com/haileyok/engram-garden/internal/segment"
	"github.com/haileyok/engram-garden/internal/text"
)

// writeSegment writes docs to a local file, uploads it, and opens it from
// the disk cache.
func (n *Node) writeSegment(ctx context.Context, spaceURI string, docs []segment.Doc, dims int) (*seg, error) {
	dir := n.opt.CacheDir
	if dir == "" {
		dir = os.TempDir()
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	f, err := os.CreateTemp(dir, ".seg-*")
	if err != nil {
		return nil, err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	info, err := segment.Write(f, docs, segment.WriteOptions{Dims: dims, ClusterThreshold: n.opt.ClusterThreshold, Keyword: n.opt.KeywordWrite, TempDir: dir})
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return nil, err
	}
	si := SegmentInfo{
		ID: newSegmentID(), Count: info.Count, Bytes: info.Bytes, Clustered: info.Clustered,
		MinCreatedAt: info.MinCreatedAt, MaxCreatedAt: info.MaxCreatedAt, CreatedAt: n.now().UTC(),
		Format: info.Version,
	}
	if info.Version >= 2 {
		si.Analyzer = text.Version
	}
	key := SegmentKey(spaceURI, si.ID)
	rf, err := os.Open(tmp)
	if err != nil {
		return nil, err
	}
	err = n.opt.Blob.Put(ctx, key, rf, info.Bytes)
	rf.Close()
	if err != nil {
		return nil, fmt.Errorf("uploading segment: %w", err)
	}
	if err := n.cache.add(key, tmp); err != nil {
		n.log().Warn("caching new segment failed", "key", key, "err", err)
	}
	return n.openSegment(ctx, spaceURI, si, true)
}

// openSegment opens a segment's index and, unless streaming, its 1-bit
// section.
func (n *Node) openSegment(ctx context.Context, spaceURI string, si SegmentInfo, loadBits bool) (*seg, error) {
	src := &source{key: SegmentKey(spaceURI, si.ID), size: si.Bytes, blob: n.opt.Blob, cache: n.cache, remote: &n.remote}
	rd, err := segment.Open(ctx, src)
	if err != nil {
		return nil, fmt.Errorf("segment %s: %w", si.ID, err)
	}
	ix, err := rd.LoadIndex(ctx)
	if err != nil {
		return nil, fmt.Errorf("segment %s: %w", si.ID, err)
	}
	if ix.Count != si.Count {
		return nil, fmt.Errorf("segment %s holds %d memories, manifest says %d", si.ID, ix.Count, si.Count)
	}
	// The header, not the manifest's hint, says whether there's a keyword
	// index.
	kw, err := rd.LoadKeyword(ctx)
	if err != nil {
		return nil, fmt.Errorf("segment %s: %w", si.ID, err)
	}
	sg := &seg{info: si, src: src, rd: rd, ix: ix, kw: kw, rows: make(map[uint32]int, ix.Count)}
	for row := range ix.Count {
		sg.rows[ix.ID(row)] = row
	}
	if loadBits {
		if sg.bits, err = rd.LoadBits(ctx); err != nil {
			return nil, fmt.Errorf("segment %s: %w", si.ID, err)
		}
	}
	return sg, nil
}

func (sg *seg) ramBytes() int64 {
	n := sg.ix.RAMBytes() + int64(len(sg.bits)) + int64(len(sg.rows))*16
	if sg.kw != nil {
		n += sg.kw.RAMBytes()
	}
	return n
}

// putManifest writes a manifest under its token and generation. With
// conditional writes, a taken key means another writer holds the same
// token, so this node stops writing.
func (n *Node) putManifest(ctx context.Context, m *Manifest) error {
	raw, err := json.Marshal(m)
	if err != nil {
		return err
	}
	err = blob.PutBytes(ctx, n.opt.Blob, ManifestKey(m.Space, m.Token, m.Generation), raw, n.opt.ConditionalWrites)
	if errors.Is(err, blob.ErrExists) {
		return fmt.Errorf("%w: manifest generation %d already written", ErrStaleOwner, m.Generation)
	}
	return err
}

// checkLease confirms this node still owns the space under its token.
func (s *Space) checkLease() error {
	tok, ok := s.n.opt.Lease(s.uri)
	if !ok || tok != s.token {
		s.mu.Lock()
		s.readOnly = true
		s.mu.Unlock()
		return ErrNotOwner
	}
	return nil
}

// Flush publishes the space's buffered changes: new segments, then a
// manifest that lists them with the updated deletions and repo positions.
func (s *Space) Flush(ctx context.Context) error {
	s.flushMu.Lock()
	defer s.flushMu.Unlock()
	if err := s.flushLocked(ctx); err != nil {
		return err
	}
	return s.maintainLocked(ctx)
}

func (s *Space) flushLocked(ctx context.Context) (err error) {
	s.mu.Lock()
	if !s.dirty() || s.readOnly {
		s.mu.Unlock()
		return nil
	}
	start := time.Now()
	defer func() {
		flushes.WithLabelValues(metrics.Result(err)).Inc()
		flushDuration.Observe(time.Since(start).Seconds())
	}()
	// Snapshot. Entries are immutable; a change during the flush replaces
	// the map entry and stays buffered.
	entries := make(map[string]*bufEntry, len(s.buf))
	for path, e := range s.buf {
		entries[path] = e
		s.inflight[e.id] = struct{}{}
	}
	delSnap := make([]uint32, 0, len(s.pendingDel))
	for id := range s.pendingDel {
		delSnap = append(delSnap, id)
	}
	repoSnap := make(map[string]*pendingRepo, len(s.repos))
	for did, p := range s.repos {
		repoSnap[did] = p
	}
	next := s.man.clone()
	next.NextMemoryID = s.nextID
	next.Config = s.config
	next.SpaceDeleted = s.spaceGone
	next.Skipped = map[string][]string{}
	for a, rks := range s.skipped {
		for rk := range rks {
			next.Skipped[a] = append(next.Skipped[a], rk)
		}
		slices.Sort(next.Skipped[a])
	}
	var models [2]*ModelInfo
	for i, sl := range s.slots {
		if sl != nil {
			m := sl.model
			models[i] = &m
		}
	}
	dirtyAt := s.dirtySince
	published := s.deleted
	s.mu.Unlock()

	clearInflight := func() {
		s.mu.Lock()
		for _, e := range entries {
			delete(s.inflight, e.id)
		}
		s.mu.Unlock()
	}
	if err := s.checkLease(); err != nil {
		clearInflight()
		return err
	}

	// One segment per slot with buffered memories. Rows are ordered by id so
	// the file is deterministic.
	paths := make([]string, 0, len(entries))
	for p := range entries {
		paths = append(paths, p)
	}
	sort.Slice(paths, func(a, b int) bool { return entries[paths[a]].id < entries[paths[b]].id })
	var newSegs [2]*seg
	for i, m := range models {
		if m == nil {
			continue
		}
		var docs []segment.Doc
		for _, p := range paths {
			e := entries[p]
			if e.bits[i] == nil {
				continue
			}
			d := e.doc
			d.Bits, d.Int8 = e.bits[i], e.int8[i]
			docs = append(docs, d)
		}
		if len(docs) == 0 {
			continue
		}
		sg, err := s.n.writeSegment(ctx, s.uri, docs, m.Dims)
		if err != nil {
			clearInflight()
			return err
		}
		newSegs[i] = sg
	}

	// The manifest: this is the commit point.
	deleted := published.Clone()
	for _, id := range delSnap {
		deleted.Add(id)
	}
	deleted.RunOptimize()
	if next.Deleted, _ = deleted.ToBytes(); next.SpaceDeleted {
		next.Active, next.Building, next.Deleted = nil, nil, nil
	}
	setIndex := func(dst **IndexManifest, m *ModelInfo, add *seg, keep []*seg) {
		if m == nil || next.SpaceDeleted {
			*dst = nil
			return
		}
		im := &IndexManifest{ModelInfo: *m, Segments: []SegmentInfo{}}
		for _, sg := range keep {
			im.Segments = append(im.Segments, sg.info)
		}
		if add != nil {
			im.Segments = append(im.Segments, add.info)
		}
		*dst = im
	}
	s.mu.RLock()
	var keep [2][]*seg
	changed := false
	for i, sl := range s.slots {
		if sl != nil {
			keep[i] = slices.Clone(sl.segs)
		}
		changed = changed || (sl == nil) != (models[i] == nil) || (sl != nil && sl.model != *models[i])
	}
	s.mu.RUnlock()
	if changed {
		// The space's model changed during the flush. Publish nothing; the
		// changes are still buffered and the next flush writes them under
		// the new shape. The uploaded segments are collected later.
		clearInflight()
		return nil
	}
	setIndex(&next.Active, models[0], newSegs[0], keep[0])
	setIndex(&next.Building, models[1], newSegs[1], keep[1])
	for did, p := range repoSnap {
		if p.pos == nil {
			delete(next.Repos, did)
		} else {
			next.Repos[did] = *p.pos
		}
	}
	next.Generation++
	next.UpdatedAt = s.n.now().UTC()
	if err := s.n.putManifest(ctx, next); err != nil {
		clearInflight()
		if errors.Is(err, ErrStaleOwner) {
			s.mu.Lock()
			s.readOnly = true
			s.mu.Unlock()
		}
		return err
	}

	// Install.
	s.install(next, deleted, delSnap, repoSnap, newSegs, models, entries, dirtyAt)
	s.n.noteRAM()
	return nil
}

func (s *Space) install(next *Manifest, deleted *roaring.Bitmap, delSnap []uint32, repoSnap map[string]*pendingRepo,
	newSegs [2]*seg, models [2]*ModelInfo, entries map[string]*bufEntry, dirtyAt time.Time,
) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.man = next
	s.deleted = deleted
	for _, id := range delSnap {
		delete(s.pendingDel, id)
	}
	for did, p := range repoSnap {
		if s.repos[did] == p {
			delete(s.repos, did)
		}
	}
	for i, sg := range newSegs {
		if sg == nil {
			continue
		}
		if s.slots[i] == nil || s.slots[i].model != *models[i] {
			// The slot changed during the flush; the segment stays
			// unreferenced by the live state and is collected later.
			continue
		}
		s.slots[i].segs = append(s.slots[i].segs, sg)
		s.ramBytes += sg.ramBytes()
	}
	for path, e := range entries {
		delete(s.inflight, e.id)
		s.bufDrop(path, e)
		l := s.locs[path]
		if l == nil || l.buf != e {
			continue
		}
		l.buf = nil
		for i, sg := range newSegs {
			if sg == nil || s.slots[i] == nil {
				continue
			}
			if row, ok := sg.rows[e.id]; ok {
				l.seg[i], l.row[i] = sg, row
			}
		}
	}
	if len(s.buf) == 0 && len(s.pendingDel) == 0 && len(s.repos) == 0 && configEqual(s.config, next.Config) &&
		s.spaceGone == next.SpaceDeleted && s.nextID == next.NextMemoryID && !s.dirtySince.After(dirtyAt) {
		s.dirtySince = time.Time{}
	}
}

// maintainLocked merges segments and collects garbage, at most once per
// interval.
func (s *Space) maintainLocked(ctx context.Context) error {
	opt := s.n.opt
	s.mu.RLock()
	due := s.n.now().Sub(s.man.LastMergeAt) >= opt.MergeInterval && !s.readOnly && !s.spaceGone
	gcDue := s.n.now().Sub(s.man.LastGCAt) >= opt.MergeInterval && !s.readOnly
	s.mu.RUnlock()
	if due {
		start := time.Now()
		err := s.mergeLocked(ctx)
		observeMaintenance("merge", start, err)
		if err != nil {
			return fmt.Errorf("merging: %w", err)
		}
	}
	if gcDue {
		start := time.Now()
		err := s.gcLocked(ctx)
		observeMaintenance("gc", start, err)
		if err != nil {
			return fmt.Errorf("collecting garbage: %w", err)
		}
	}
	return nil
}

// mergeInputs picks the segments of one slot to merge, or nil.
func (s *Space) mergeInputs(segs []*seg, deleted *roaring.Bitmap) []*seg {
	opt := s.n.opt
	total, dead := 0, 0
	for _, sg := range segs {
		total += sg.info.Count
		for id := range sg.rows {
			if deleted.Contains(id) {
				dead++
			}
		}
	}
	if len(segs) <= opt.MaxSegments && (total == 0 || float64(dead)/float64(total) <= opt.MaxDeletedRatio) {
		return nil
	}
	// Prefer segments past the provider's minimum retention: deleting them
	// costs nothing extra.
	var old []*seg
	for _, sg := range segs {
		if s.n.now().Sub(sg.info.CreatedAt) >= opt.MinRetention {
			old = append(old, sg)
		}
	}
	if len(old) >= 2 && (len(segs)-len(old)+1) <= opt.MaxSegments {
		return old
	}
	if len(segs) < 2 && dead == 0 {
		return nil
	}
	return segs
}

func (s *Space) mergeLocked(ctx context.Context) error {
	s.mu.RLock()
	published := s.deleted
	var plans [2][]*seg
	var models [2]*ModelInfo
	for i, sl := range s.slots {
		if sl == nil {
			continue
		}
		m := sl.model
		models[i] = &m
		plans[i] = s.mergeInputs(sl.segs, published)
	}
	s.mu.RUnlock()

	var merged [2]*seg
	dropped := roaring.New()
	any := false
	for i, inputs := range plans {
		if len(inputs) == 0 {
			continue
		}
		var docs []segment.Doc
		for _, sg := range inputs {
			all, err := sg.rd.ReadAll(ctx, sg.ix)
			if err != nil {
				return err
			}
			for _, d := range all {
				if published.Contains(d.ID) {
					dropped.Add(d.ID)
					continue
				}
				docs = append(docs, d)
			}
		}
		any = true
		if len(docs) == 0 {
			continue
		}
		sg, err := s.n.writeSegment(ctx, s.uri, docs, models[i].Dims)
		if err != nil {
			return err
		}
		merged[i] = sg
	}
	if err := s.checkLease(); err != nil {
		return err
	}

	s.mu.Lock()
	next := s.man.clone()
	next.LastMergeAt = s.n.now().UTC()
	next.Generation++
	next.UpdatedAt = next.LastMergeAt
	// Merged-away ids stay deleted only if a segment outside the merge
	// still holds them.
	deleted := published.Clone()
	stillHeld := roaring.New()
	for i, sl := range s.slots {
		if sl == nil {
			continue
		}
		for _, sg := range sl.segs {
			if !slices.Contains(plans[i], sg) {
				for id := range sg.rows {
					stillHeld.Add(id)
				}
			}
		}
	}
	gone := roaring.AndNot(dropped, stillHeld)
	deleted.AndNot(gone)
	// Pending deletions of dropped ids aren't needed any more either, but
	// they're harmless; keep them for simplicity.
	deleted.RunOptimize()
	next.Deleted, _ = deleted.ToBytes()
	newSlots := [2][]*seg{}
	for i, sl := range s.slots {
		if sl == nil || models[i] == nil || sl.model != *models[i] {
			if sl != nil {
				newSlots[i] = sl.segs
			}
			continue
		}
		for _, sg := range sl.segs {
			if !slices.Contains(plans[i], sg) {
				newSlots[i] = append(newSlots[i], sg)
			}
		}
		if merged[i] != nil {
			newSlots[i] = append(newSlots[i], merged[i])
		}
	}
	for i, im := range []**IndexManifest{&next.Active, &next.Building} {
		if *im == nil {
			continue
		}
		(*im).Segments = []SegmentInfo{}
		for _, sg := range newSlots[i] {
			(*im).Segments = append((*im).Segments, sg.info)
		}
	}
	s.mu.Unlock()
	if !any {
		// Nothing to merge: just record the check.
		s.mu.Lock()
		next = s.man.clone()
		next.LastMergeAt = s.n.now().UTC()
		next.Generation++
		s.mu.Unlock()
	}
	if err := s.n.putManifest(ctx, next); err != nil {
		return err
	}

	s.mu.Lock()
	s.man = next
	if !any {
		s.mu.Unlock()
		return nil
	}
	s.deleted = deleted
	var removed []*seg
	for i, sl := range s.slots {
		if sl == nil || models[i] == nil || sl.model != *models[i] {
			continue
		}
		removed = append(removed, plans[i]...)
		sl.segs = newSlots[i]
		if merged[i] != nil {
			s.ramBytes += merged[i].ramBytes()
		}
	}
	for _, sg := range removed {
		s.ramBytes -= sg.ramBytes()
	}
	// Point memories at their rows in the merged segments.
	for _, l := range s.locs {
		for i := range l.seg {
			if l.seg[i] == nil || !slices.Contains(plans[i], l.seg[i]) {
				continue
			}
			l.seg[i] = nil
			if merged[i] != nil {
				if row, ok := merged[i].rows[l.id]; ok {
					l.seg[i], l.row[i] = merged[i], row
				}
			}
		}
	}
	s.mu.Unlock()
	// The inputs stay in object storage until garbage collection, after the
	// provider's minimum retention: deleting them sooner saves nothing, and
	// a search that started before the merge may still be reading them.
	for _, sg := range removed {
		s.n.cache.remove(sg.src.key)
	}
	s.n.noteRAM()
	return nil
}

// gcLocked deletes segments no manifest of this owner references once
// they're past the minimum retention, and superseded manifests past it.
func (s *Space) gcLocked(ctx context.Context) error {
	opt := s.n.opt
	objs, err := opt.Blob.List(ctx, spacePrefix(s.uri))
	if err != nil {
		return err
	}
	s.mu.RLock()
	current := ManifestKey(s.uri, s.man.Token, s.man.Generation)
	referenced := map[string]bool{}
	for _, im := range []*IndexManifest{s.man.Active, s.man.Building} {
		if im != nil {
			for _, si := range im.Segments {
				referenced[SegmentKey(s.uri, si.ID)] = true
			}
		}
	}
	// Segments installed but not yet in a manifest (a flush in progress)
	// are protected by the retention window.
	s.mu.RUnlock()
	for _, o := range objs {
		if s.n.now().Sub(o.Modified) < opt.MinRetention {
			continue
		}
		name := o.Key[strings.LastIndex(o.Key, "/")+1:]
		switch {
		case strings.HasPrefix(name, "seg-") && !referenced[o.Key]:
		case strings.HasPrefix(name, "manifest-") && o.Key < current:
		default:
			continue
		}
		if err := opt.Blob.Delete(ctx, o.Key); err != nil {
			return err
		}
		s.n.cache.remove(o.Key)
	}
	s.mu.Lock()
	next := s.man.clone()
	next.LastGCAt = s.n.now().UTC()
	next.Generation++
	s.mu.Unlock()
	if err := s.n.putManifest(ctx, next); err != nil {
		return err
	}
	s.mu.Lock()
	s.man = next
	s.mu.Unlock()
	return nil
}

// manifestBytes encodes a manifest for export.
func manifestBytes(m *Manifest) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	_ = enc.Encode(m)
	return buf.Bytes()
}
