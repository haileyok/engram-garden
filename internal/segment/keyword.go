package segment

// The keyword sections (version 2). All integers little-endian.
//
//	[norms]       count bytes: text.EncodeLength of each memory's length
//	[term index]  uvarint terms, uvarint blocks, then per dictionary block:
//	              uvarint first-term length, first term, uvarint offset and
//	              uvarint length within [terms]
//	[terms]       blocks of up to 128 terms in byte order, front-coded:
//	              uvarint count, then per term: uvarint shared-prefix
//	              length, uvarint suffix length, suffix, uvarint df,
//	              uvarint total tf, then for df 1 the row (uvarint), else
//	              the postings' offset and length within [postings]
//	[postings]    per term with df ≥ 2: a skip table, then its blocks
//
// A term's postings hold its rows in increasing order with their term
// frequencies, in blocks of 128 (the last may be shorter). The skip table
// has one 10-byte entry per block: last row (u32), the block's end offset
// within the term's block data (u32), its largest tf (u8, 255 meaning 255
// or more) and its smallest norm byte (u8). BM25 rises with tf and falls
// with length, so scoring the largest tf at the smallest length bounds the
// block's score for any IDF and average length.
//
// A full block is: delta width (u8), 128 row deltas bit-packed at that
// width, tf width (u8), 128 values of tf−1 bit-packed. A short last block
// is uvarint (delta, tf) pairs. A delta is row − previous row − 1, where
// the previous row of a block's first entry is the last row of the block
// before (−1 for the first block), so any block decodes on its own.

import (
	"bufio"
	"container/heap"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"math/bits"
	"os"
	"runtime"
	"slices"
	"sort"
	"sync"

	"github.com/haileyok/engram-garden/internal/text"
)

const (
	postingBlock = 128
	termBlock    = 128
	skipEntry    = 10
	// NoRow marks an exhausted posting iterator.
	NoRow = ^uint32(0)
)

type keywordSections struct {
	norms, termIndex, terms, postings []byte
	totalLength                       uint64
}

type posting struct{ row, tf uint32 }

// defaultKeywordChunk is how many memories buildKeyword analyzes and
// holds postings for at once.
const defaultKeywordChunk = 1 << 16

// buildKeyword analyzes docs, whose order is final, and encodes the
// keyword sections. Its working memory is bounded by chunk memories, not by
// the segment: each chunk's postings are sorted by term and spilled to a
// temporary file in dir (the system's if empty), and the files are merged
// term by term. Chunks are in row order, so a term's postings concatenate
// in order, and the output is the same bytes whatever the chunk size.
func buildKeyword(docs []Doc, chunk int, dir string) (keywordSections, error) {
	if chunk <= 0 {
		chunk = defaultKeywordChunk
	}
	var ks keywordSections
	ks.norms = make([]byte, len(docs))
	tw := &termWriter{ks: &ks}
	var runs []*os.File
	defer func() {
		for _, f := range runs {
			f.Close()
			os.Remove(f.Name())
		}
	}()
	for start := 0; start < len(docs); start += chunk {
		end := min(start+chunk, len(docs))
		post := map[string][]posting{}
		for i, a := range analyzeAll(docs[start:end]) {
			row := start + i
			n := text.EncodeLength(a.Length)
			ks.norms[row] = n
			// Sum the lengths scorers will use, so the average agrees.
			ks.totalLength += uint64(text.DecodeLength(n))
			for term, tf := range a.TF {
				post[term] = append(post[term], posting{uint32(row), tf})
			}
		}
		terms := make([]string, 0, len(post))
		for t := range post {
			terms = append(terms, t)
		}
		slices.Sort(terms)
		if start == 0 && end == len(docs) {
			// One chunk: no need to spill.
			for _, t := range terms {
				tw.add(t, post[t])
			}
			return tw.finish(), nil
		}
		f, err := os.CreateTemp(dir, "engram-postings-*")
		if err != nil {
			return ks, err
		}
		runs = append(runs, f)
		if err := writeRun(f, terms, post); err != nil {
			return ks, err
		}
	}
	if err := mergeRuns(runs, tw); err != nil {
		return ks, err
	}
	return tw.finish(), nil
}

// analyzeAll analyzes docs on every core.
func analyzeAll(docs []Doc) []text.Doc {
	out := make([]text.Doc, len(docs))
	workers := min(runtime.GOMAXPROCS(0), max(1, len(docs)/64))
	var wg sync.WaitGroup
	for w := range workers {
		wg.Go(func() {
			for i := w; i < len(docs); i += workers {
				out[i] = text.AnalyzeMemory(docs[i].Text, docs[i].Tags, docs[i].Source)
			}
		})
	}
	wg.Wait()
	return out
}

// A run is one chunk's postings, sorted by term: per term, uvarint length,
// the term, uvarint count, then (row, tf) uvarint pairs.
func writeRun(f *os.File, terms []string, post map[string][]posting) error {
	w := bufio.NewWriterSize(f, 1<<20)
	var buf []byte
	for _, t := range terms {
		ps := post[t]
		buf = binary.AppendUvarint(buf[:0], uint64(len(t)))
		buf = append(buf, t...)
		buf = binary.AppendUvarint(buf, uint64(len(ps)))
		for _, p := range ps {
			buf = binary.AppendUvarint(buf, uint64(p.row))
			buf = binary.AppendUvarint(buf, uint64(p.tf))
		}
		if _, err := w.Write(buf); err != nil {
			return err
		}
	}
	if err := w.Flush(); err != nil {
		return err
	}
	_, err := f.Seek(0, io.SeekStart)
	return err
}

type runReader struct {
	idx  int
	r    *bufio.Reader
	term string
	n    int
}

// next reads the next term's header; it reports false at the end.
func (rr *runReader) next() (bool, error) {
	l, err := binary.ReadUvarint(rr.r)
	if err == io.EOF {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	b := make([]byte, l)
	if _, err := io.ReadFull(rr.r, b); err != nil {
		return false, err
	}
	n, err := binary.ReadUvarint(rr.r)
	if err != nil {
		return false, err
	}
	rr.term, rr.n = string(b), int(n)
	return true, nil
}

func (rr *runReader) postings(dst []posting) ([]posting, error) {
	for range rr.n {
		row, err := binary.ReadUvarint(rr.r)
		if err != nil {
			return dst, err
		}
		tf, err := binary.ReadUvarint(rr.r)
		if err != nil {
			return dst, err
		}
		dst = append(dst, posting{uint32(row), uint32(tf)})
	}
	return dst, nil
}

type runHeap []*runReader

func (h runHeap) Len() int { return len(h) }
func (h runHeap) Less(a, b int) bool {
	if h[a].term != h[b].term {
		return h[a].term < h[b].term
	}
	return h[a].idx < h[b].idx
}
func (h runHeap) Swap(a, b int) { h[a], h[b] = h[b], h[a] }
func (h *runHeap) Push(x any)   { *h = append(*h, x.(*runReader)) }
func (h *runHeap) Pop() any {
	old := *h
	x := old[len(old)-1]
	*h = old[:len(old)-1]
	return x
}

// mergeRuns merges sorted runs; a term in several runs gets their postings
// in run order, which is row order.
func mergeRuns(runs []*os.File, tw *termWriter) error {
	h := &runHeap{}
	for i, f := range runs {
		rr := &runReader{idx: i, r: bufio.NewReaderSize(f, 1<<20)}
		ok, err := rr.next()
		if err != nil {
			return err
		}
		if ok {
			heap.Push(h, rr)
		}
	}
	var ps []posting
	for h.Len() > 0 {
		term := (*h)[0].term
		ps = ps[:0]
		for h.Len() > 0 && (*h)[0].term == term {
			rr := heap.Pop(h).(*runReader)
			var err error
			if ps, err = rr.postings(ps); err != nil {
				return err
			}
			ok, err := rr.next()
			if err != nil {
				return err
			}
			if ok {
				heap.Push(h, rr)
			}
		}
		tw.add(term, ps)
	}
	return nil
}

// termWriter encodes terms, in order, into the dictionary and postings.
type termWriter struct {
	ks                       *keywordSections
	idx, block               []byte
	prev, first              string
	nterms, nblocks, inBlock int
	blockStart               int
}

func (tw *termWriter) flush() {
	if tw.inBlock == 0 {
		return
	}
	b := binary.AppendUvarint(nil, uint64(tw.inBlock))
	b = append(b, tw.block...)
	tw.idx = binary.AppendUvarint(tw.idx, uint64(len(tw.first)))
	tw.idx = append(tw.idx, tw.first...)
	tw.idx = binary.AppendUvarint(tw.idx, uint64(tw.blockStart))
	tw.idx = binary.AppendUvarint(tw.idx, uint64(len(b)))
	tw.ks.terms = append(tw.ks.terms, b...)
	tw.blockStart = len(tw.ks.terms)
	tw.nblocks++
	tw.block, tw.inBlock, tw.prev = tw.block[:0], 0, ""
}

func (tw *termWriter) add(t string, ps []posting) {
	if tw.inBlock == termBlock {
		tw.flush()
	}
	if tw.inBlock == 0 {
		tw.first = t
	}
	shared := commonPrefix(tw.prev, t)
	tw.block = binary.AppendUvarint(tw.block, uint64(shared))
	tw.block = binary.AppendUvarint(tw.block, uint64(len(t)-shared))
	tw.block = append(tw.block, t[shared:]...)
	var total uint64
	for _, p := range ps {
		total += uint64(p.tf)
	}
	tw.block = binary.AppendUvarint(tw.block, uint64(len(ps)))
	tw.block = binary.AppendUvarint(tw.block, total)
	if len(ps) == 1 {
		tw.block = binary.AppendUvarint(tw.block, uint64(ps[0].row))
	} else {
		off := len(tw.ks.postings)
		tw.ks.postings = encodePostings(tw.ks.postings, ps, tw.ks.norms)
		tw.block = binary.AppendUvarint(tw.block, uint64(off))
		tw.block = binary.AppendUvarint(tw.block, uint64(len(tw.ks.postings)-off))
	}
	tw.prev = t
	tw.inBlock++
	tw.nterms++
}

func (tw *termWriter) finish() keywordSections {
	tw.flush()
	ti := binary.AppendUvarint(nil, uint64(tw.nterms))
	ti = binary.AppendUvarint(ti, uint64(tw.nblocks))
	tw.ks.termIndex = append(ti, tw.idx...)
	return *tw.ks
}

func commonPrefix(a, b string) int {
	n := min(len(a), len(b))
	for i := range n {
		if a[i] != b[i] {
			return i
		}
	}
	return n
}

func encodePostings(dst []byte, ps []posting, norms []byte) []byte {
	nb := (len(ps) + postingBlock - 1) / postingBlock
	skipAt := len(dst)
	dst = append(dst, make([]byte, nb*skipEntry)...)
	dataStart := len(dst)
	prev := int64(-1)
	var deltas, tfs [postingBlock]uint32
	for b := range nb {
		blk := ps[b*postingBlock : min((b+1)*postingBlock, len(ps))]
		maxTF, minNorm := uint32(0), byte(255)
		for i, p := range blk {
			deltas[i] = uint32(int64(p.row) - prev - 1)
			tfs[i] = p.tf - 1
			prev = int64(p.row)
			maxTF = max(maxTF, p.tf)
			minNorm = min(minNorm, norms[p.row])
		}
		if len(blk) == postingBlock {
			dst = appendPacked(dst, deltas[:])
			dst = appendPacked(dst, tfs[:])
		} else {
			for i := range blk {
				dst = binary.AppendUvarint(dst, uint64(deltas[i]))
				dst = binary.AppendUvarint(dst, uint64(tfs[i])+1)
			}
		}
		e := dst[skipAt+b*skipEntry:]
		binary.LittleEndian.PutUint32(e, blk[len(blk)-1].row)
		binary.LittleEndian.PutUint32(e[4:], uint32(len(dst)-dataStart))
		e[8] = byte(min(maxTF, 255))
		e[9] = minNorm
	}
	return dst
}

// appendPacked appends a width byte and vals bit-packed at that width.
func appendPacked(dst []byte, vals []uint32) []byte {
	var or uint32
	for _, v := range vals {
		or |= v
	}
	w := uint(bits.Len32(or))
	dst = append(dst, byte(w))
	var acc uint64
	var n uint
	for _, v := range vals {
		acc |= uint64(v) << n
		n += w
		for n >= 8 {
			dst = append(dst, byte(acc))
			acc >>= 8
			n -= 8
		}
	}
	if n > 0 {
		dst = append(dst, byte(acc))
	}
	return dst
}

// unpack reads a width byte and len(out) values; it returns bytes used.
func unpack(src []byte, out []uint32) (int, error) {
	if len(src) < 1 {
		return 0, errPostings
	}
	w := uint(src[0])
	if w > 32 {
		return 0, errPostings
	}
	need := 1 + (len(out)*int(w)+7)/8
	if len(src) < need {
		return 0, errPostings
	}
	if w == 0 {
		clear(out)
		return need, nil
	}
	mask := uint64(1)<<w - 1
	var acc uint64
	var n uint
	p := 1
	for i := range out {
		for n < w {
			acc |= uint64(src[p]) << n
			p++
			n += 8
		}
		out[i] = uint32(acc & mask)
		acc >>= w
		n -= w
	}
	return need, nil
}

var errPostings = fmt.Errorf("%w: postings", ErrFormat)

// ---- reading ----

// Keyword is the part of a segment's keyword index a node keeps in RAM:
// every memory's length byte and the dictionary's block index.
type Keyword struct {
	Count       int
	Analyzer    int
	TotalLength uint64
	// Terms is the number of distinct terms.
	Terms  int
	norms  []byte
	firsts []string
	offs   []uint64
	lens   []uint64
}

// Norm is a memory's length code (text.DecodeLength gives the length).
func (k *Keyword) Norm(row int) byte { return k.norms[row] }

// RAMBytes estimates the memory the index holds.
func (k *Keyword) RAMBytes() int64 {
	n := int64(len(k.norms)) + int64(len(k.offs))*16
	for _, f := range k.firsts {
		n += int64(len(f)) + 16
	}
	return n
}

// LoadKeyword reads the norms and term index (one range read). It returns
// nil for a segment without a keyword index.
func (r *Reader) LoadKeyword(ctx context.Context) (*Keyword, error) {
	if !r.HasKeyword() {
		return nil, nil
	}
	a, b := r.h.sections[secNorms], r.h.sections[secTermIndex]
	buf, err := r.rr.ReadRange(ctx, int64(a.off), int64(b.off+b.len-a.off))
	if err != nil {
		return nil, err
	}
	norms, tidx := buf[:a.len], buf[a.len:]
	if crc32.Checksum(norms, crcTable) != a.crc || crc32.Checksum(tidx, crcTable) != b.crc {
		return nil, fmt.Errorf("%w: keyword index checksum mismatch", ErrFormat)
	}
	k := &Keyword{Count: int(r.h.count), Analyzer: int(r.h.analyzer), TotalLength: r.h.totalLength, norms: norms}
	d := varReader{b: tidx}
	k.Terms = int(d.uvarint())
	nb := int(d.uvarint())
	if d.err != nil || nb > len(tidx) {
		return nil, fmt.Errorf("%w: term index", ErrFormat)
	}
	termsLen := r.h.sections[secTerms].len
	for range nb {
		first := string(d.bytes(int(d.uvarint())))
		off, n := d.uvarint(), d.uvarint()
		if d.err != nil || off+n > termsLen || off+n < off {
			return nil, fmt.Errorf("%w: term index", ErrFormat)
		}
		if l := len(k.firsts); l > 0 && first <= k.firsts[l-1] {
			return nil, fmt.Errorf("%w: term index order", ErrFormat)
		}
		k.firsts, k.offs, k.lens = append(k.firsts, first), append(k.offs, off), append(k.lens, n)
	}
	if !d.done() {
		return nil, fmt.Errorf("%w: term index", ErrFormat)
	}
	return k, nil
}

// TermInfo is a term's dictionary entry in one segment.
type TermInfo struct {
	// DF is how many memories in the segment contain the term; 0 means
	// none (the term isn't in the segment).
	DF      uint32
	TotalTF uint64
	// row is the memory, when DF is 1 (stored in the dictionary).
	row      uint32
	off, len uint64
}

// LookupTerms finds terms in the dictionary, reading each dictionary
// block they fall in once.
func (r *Reader) LookupTerms(ctx context.Context, k *Keyword, terms []string) ([]TermInfo, error) {
	out := make([]TermInfo, len(terms))
	byBlock := map[int][]int{}
	for i, t := range terms {
		b := sort.Search(len(k.firsts), func(j int) bool { return k.firsts[j] > t }) - 1
		if b >= 0 {
			byBlock[b] = append(byBlock[b], i)
		}
	}
	sec := r.h.sections[secTerms]
	for b, want := range byBlock {
		buf, err := r.rr.ReadRange(ctx, int64(sec.off+k.offs[b]), int64(k.lens[b]))
		if err != nil {
			return nil, err
		}
		err = parseTermBlock(buf, func(term string, ti TermInfo) bool {
			for _, i := range want {
				if terms[i] == term {
					out[i] = ti
				}
			}
			return true
		})
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// parseTermBlock calls fn for each term in a dictionary block, in order,
// until fn returns false.
func parseTermBlock(buf []byte, fn func(string, TermInfo) bool) error {
	d := varReader{b: buf}
	n := int(d.uvarint())
	var prev []byte
	for range n {
		shared, suffix := int(d.uvarint()), int(d.uvarint())
		if d.err != nil || shared > len(prev) {
			return fmt.Errorf("%w: term block", ErrFormat)
		}
		term := append(prev[:shared:shared], d.bytes(suffix)...)
		ti := TermInfo{DF: uint32(d.uvarint()), TotalTF: d.uvarint()}
		if ti.DF == 1 {
			ti.row = uint32(d.uvarint())
		} else {
			ti.off, ti.len = d.uvarint(), d.uvarint()
		}
		if d.err != nil || ti.DF == 0 {
			return fmt.Errorf("%w: term block", ErrFormat)
		}
		if !fn(string(term), ti) {
			return nil
		}
		prev = term
	}
	if !d.done() {
		return fmt.Errorf("%w: term block", ErrFormat)
	}
	return nil
}

// Postings are one term's rows and frequencies in a segment, with the
// per-block bounds pruning uses.
type Postings struct {
	DF      int
	last    []uint32
	end     []uint32
	maxTF   []uint8
	minNorm []uint8
	data    []byte
	// inline is set for a term in one memory (row, tf).
	inline    bool
	irow, itf uint32
}

// Postings reads a term's postings (one range read, none for a term in a
// single memory). t must be found (DF > 0).
func (r *Reader) Postings(ctx context.Context, k *Keyword, t TermInfo) (*Postings, error) {
	if t.DF == 0 {
		return nil, errors.New("segment: postings of a missing term")
	}
	if t.DF == 1 {
		if int(t.row) >= k.Count {
			return nil, errPostings
		}
		tf := uint32(min(t.TotalTF, uint64(^uint32(0))))
		return &Postings{
			DF: 1, inline: true, irow: t.row, itf: tf,
			last: []uint32{t.row}, maxTF: []uint8{uint8(min(tf, 255))}, minNorm: []uint8{k.norms[t.row]},
		}, nil
	}
	sec := r.h.sections[secPostings]
	if t.off+t.len > sec.len || t.off+t.len < t.off {
		return nil, errPostings
	}
	buf, err := r.rr.ReadRange(ctx, int64(sec.off+t.off), int64(t.len))
	if err != nil {
		return nil, err
	}
	return parsePostings(buf, int(t.DF), k.Count)
}

func parsePostings(buf []byte, df, count int) (*Postings, error) {
	nb := (df + postingBlock - 1) / postingBlock
	if len(buf) < nb*skipEntry {
		return nil, errPostings
	}
	p := &Postings{DF: df, data: buf[nb*skipEntry:]}
	prevLast, prevEnd := int64(-1), uint32(0)
	for b := range nb {
		e := buf[b*skipEntry:]
		last, end := binary.LittleEndian.Uint32(e), binary.LittleEndian.Uint32(e[4:])
		if int64(last) <= prevLast || int(last) >= count || end <= prevEnd || int(end) > len(p.data) {
			return nil, errPostings
		}
		p.last = append(p.last, last)
		p.end = append(p.end, end)
		p.maxTF = append(p.maxTF, e[8])
		p.minNorm = append(p.minNorm, e[9])
		prevLast, prevEnd = int64(last), end
	}
	if int(prevEnd) != len(p.data) {
		return nil, errPostings
	}
	return p, nil
}

// Blocks is the number of posting blocks.
func (p *Postings) Blocks() int { return len(p.last) }

// BlockLast is a block's last row.
func (p *Postings) BlockLast(b int) uint32 { return p.last[b] }

// BlockFirstBound is the smallest row a block can start at: one past the
// previous block's last row.
func (p *Postings) BlockFirstBound(b int) uint32 {
	if b == 0 {
		return 0
	}
	return p.last[b-1] + 1
}

// BlockMax is a block's largest tf (255 meaning 255 or more) and smallest
// length code.
func (p *Postings) BlockMax(b int) (maxTF uint8, minNorm uint8) { return p.maxTF[b], p.minNorm[b] }

// decode fills rows and tfs with block b and returns its size.
func (p *Postings) decode(b int, rows, tfs *[postingBlock]uint32) (int, error) {
	if p.inline {
		rows[0], tfs[0] = p.irow, p.itf
		return 1, nil
	}
	start := uint32(0)
	if b > 0 {
		start = p.end[b-1]
	}
	src := p.data[start:p.end[b]]
	n := postingBlock
	if b == len(p.last)-1 && p.DF%postingBlock != 0 {
		n = p.DF % postingBlock
	}
	prev := int64(-1)
	if b > 0 {
		prev = int64(p.last[b-1])
	}
	if n == postingBlock {
		used, err := unpack(src, rows[:])
		if err != nil {
			return 0, err
		}
		used2, err := unpack(src[used:], tfs[:])
		if err != nil {
			return 0, err
		}
		if used+used2 != len(src) {
			return 0, errPostings
		}
		for i := range n {
			prev += int64(rows[i]) + 1
			rows[i] = uint32(prev)
			tfs[i]++
		}
	} else {
		d := varReader{b: src}
		for i := range n {
			prev += int64(d.uvarint()) + 1
			rows[i], tfs[i] = uint32(prev), uint32(d.uvarint())
		}
		if d.err != nil || !d.done() {
			return 0, errPostings
		}
	}
	if int64(rows[n-1]) != int64(p.last[b]) {
		return 0, errPostings
	}
	return n, nil
}

// Iter walks postings in row order.
type Iter struct {
	p    *Postings
	blk  int
	rows [postingBlock]uint32
	tfs  [postingBlock]uint32
	n, i int
	// Row is the current row, NoRow when exhausted; TF its frequency.
	Row, TF uint32
	err     error
}

// Iter returns an iterator at the first posting.
func (p *Postings) Iter() *Iter {
	it := &Iter{p: p, blk: -1}
	it.load(0)
	return it
}

// Err reports a corrupt block met while iterating.
func (it *Iter) Err() error { return it.err }

func (it *Iter) load(b int) {
	if b >= it.p.Blocks() {
		it.Row, it.TF = NoRow, 0
		return
	}
	n, err := it.p.decode(b, &it.rows, &it.tfs)
	if err != nil {
		it.err, it.Row = err, NoRow
		return
	}
	it.blk, it.n, it.i = b, n, 0
	it.Row, it.TF = it.rows[0], it.tfs[0]
}

// Next moves to the next posting.
func (it *Iter) Next() {
	if it.Row == NoRow {
		return
	}
	it.i++
	if it.i < it.n {
		it.Row, it.TF = it.rows[it.i], it.tfs[it.i]
		return
	}
	it.load(it.blk + 1)
}

// Advance moves to the first posting at or after row, skipping whole
// blocks without decoding them.
func (it *Iter) Advance(row uint32) {
	if it.Row == NoRow || it.Row >= row {
		return
	}
	if it.p.last[it.blk] < row {
		b := it.blk + sort.Search(it.p.Blocks()-it.blk, func(j int) bool { return it.p.last[it.blk+j] >= row })
		it.load(b)
		if it.Row == NoRow {
			return
		}
	}
	for it.Row < row {
		it.Next()
	}
}

// varReader reads uvarints and byte strings, recording the first error.
type varReader struct {
	b   []byte
	p   int
	err error
}

func (d *varReader) uvarint() uint64 {
	if d.err != nil {
		return 0
	}
	v, n := binary.Uvarint(d.b[d.p:])
	if n <= 0 {
		d.err = ErrFormat
		return 0
	}
	d.p += n
	return v
}

func (d *varReader) bytes(n int) []byte {
	if d.err != nil || n < 0 || d.p+n > len(d.b) {
		d.err = ErrFormat
		return nil
	}
	out := d.b[d.p : d.p+n]
	d.p += n
	return out
}

func (d *varReader) done() bool { return d.err == nil && d.p == len(d.b) }

// verifyKeyword checks every dictionary block and posting list: order,
// counts, row ranges, and that each block's stored bounds hold.
func (r *Reader) verifyKeyword(ctx context.Context) error {
	k, err := r.LoadKeyword(ctx)
	if err != nil {
		return err
	}
	termsSec, err := r.section(ctx, secTerms)
	if err != nil {
		return err
	}
	postSec, err := r.section(ctx, secPostings)
	if err != nil {
		return err
	}
	var total uint64
	for row := range k.Count {
		total += uint64(text.DecodeLength(k.norms[row]))
	}
	if total != k.TotalLength {
		return fmt.Errorf("%w: total length", ErrFormat)
	}
	nterms := 0
	var prevTerm string
	for b := range k.firsts {
		first := true
		err := parseTermBlock(termsSec[k.offs[b]:k.offs[b]+k.lens[b]], func(term string, ti TermInfo) bool {
			if first && term != k.firsts[b] || nterms > 0 && term <= prevTerm {
				err = fmt.Errorf("%w: term order", ErrFormat)
				return false
			}
			first, prevTerm = false, term
			nterms++
			var p *Postings
			if ti.DF == 1 {
				p, err = r.Postings(ctx, k, ti)
			} else if ti.off+ti.len > uint64(len(postSec)) {
				err = errPostings
			} else {
				p, err = parsePostings(postSec[ti.off:ti.off+ti.len], int(ti.DF), k.Count)
			}
			if err != nil {
				return false
			}
			err = checkPostings(p, ti, k)
			return err == nil
		})
		if err != nil {
			return err
		}
	}
	if nterms != k.Terms {
		return fmt.Errorf("%w: term count", ErrFormat)
	}
	return nil
}

func checkPostings(p *Postings, ti TermInfo, k *Keyword) error {
	var rows, tfs [postingBlock]uint32
	n := 0
	var sum uint64
	for b := range p.Blocks() {
		m, err := p.decode(b, &rows, &tfs)
		if err != nil {
			return err
		}
		maxTF, minNorm := p.BlockMax(b)
		for i := range m {
			if tfs[i] == 0 || int(rows[i]) >= k.Count || uint32(maxTF) < min(tfs[i], 255) || minNorm > k.norms[rows[i]] {
				return errPostings
			}
			sum += uint64(tfs[i])
		}
		n += m
	}
	if n != int(ti.DF) || sum != ti.TotalTF {
		return errPostings
	}
	return nil
}
