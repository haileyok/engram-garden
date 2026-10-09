package segment

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"
	"math"
	"os"
	"slices"
	"sort"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/haileyok/engram-garden/internal/vec"
)

// RangeReader reads byte ranges of a segment file, from local disk or
// object storage.
type RangeReader interface {
	ReadRange(ctx context.Context, off, n int64) ([]byte, error)
	Size() int64
}

// BytesReader serves ranges from memory.
type BytesReader []byte

func (b BytesReader) Size() int64 { return int64(len(b)) }
func (b BytesReader) ReadRange(_ context.Context, off, n int64) ([]byte, error) {
	if off < 0 || n < 0 || off+n > int64(len(b)) {
		return nil, fmt.Errorf("range %d+%d outside %d bytes", off, n, len(b))
	}
	return b[off : off+n], nil
}

// FileReader serves ranges from a local file.
type FileReader struct {
	F    *os.File
	size int64
}

// OpenFile opens a local segment file for range reads.
func OpenFile(path string) (*FileReader, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	return &FileReader{F: f, size: st.Size()}, nil
}

func (f *FileReader) Size() int64  { return f.size }
func (f *FileReader) Close() error { return f.F.Close() }
func (f *FileReader) ReadRange(_ context.Context, off, n int64) ([]byte, error) {
	b := make([]byte, n)
	if _, err := f.F.ReadAt(b, off); err != nil && !(err == io.EOF && n == 0) {
		return nil, err
	}
	return b, nil
}

// Reader reads one segment.
type Reader struct {
	rr RangeReader
	h  header
}

// Open reads and checks a segment's header.
func Open(ctx context.Context, rr RangeReader) (*Reader, error) {
	if rr.Size() < int64(headerSize(1)+footerSize(1)) {
		return nil, fmt.Errorf("%w: file too short", ErrFormat)
	}
	// One read covers the header of any version.
	b, err := rr.ReadRange(ctx, 0, min(int64(maxHeaderSize), rr.Size()))
	if err != nil {
		return nil, err
	}
	h, err := parseHeader(b, rr.Size())
	if err != nil {
		return nil, err
	}
	return &Reader{rr: rr, h: h}, nil
}

func (r *Reader) Count() int { return int(r.h.count) }
func (r *Reader) Dims() int  { return int(r.h.dims) }

// Version is the file's format version.
func (r *Reader) Version() int { return int(r.h.version) }

// HasKeyword reports whether the segment has a keyword index (version 2).
func (r *Reader) HasKeyword() bool { return r.h.version >= 2 }

// Analyzer is the text analyzer version the keyword index was built with,
// or 0 without one.
func (r *Reader) Analyzer() int { return int(r.h.analyzer) }

// TotalLength is the sum of every memory's length in source tokens, for
// space-wide BM25 statistics. It's 0 without a keyword index.
func (r *Reader) TotalLength() uint64 { return r.h.totalLength }

func (r *Reader) section(ctx context.Context, s int) ([]byte, error) {
	sec := r.h.sections[s]
	b, err := r.rr.ReadRange(ctx, int64(sec.off), int64(sec.len))
	if err != nil {
		return nil, err
	}
	if crc32.Checksum(b, crcTable) != sec.crc {
		return nil, fmt.Errorf("%w: %s section checksum mismatch", ErrFormat, sectionNames[s])
	}
	return b, nil
}

// Verify reads the whole file and checks every section's checksum and the
// footer.
func (r *Reader) Verify(ctx context.Context) error {
	for s := range sectionsIn(r.h.version) {
		if _, err := r.section(ctx, s); err != nil {
			return err
		}
	}
	fs, hs := footerSize(r.h.version), headerSize(r.h.version)
	f, err := r.rr.ReadRange(ctx, r.rr.Size()-int64(fs), int64(fs))
	if err != nil {
		return err
	}
	if !bytes.Equal(f[:hs], r.h.marshal()) || string(f[hs:]) != endMagic {
		return fmt.Errorf("%w: footer does not match header", ErrFormat)
	}
	if _, err = r.LoadIndex(ctx); err != nil {
		return err
	}
	if r.HasKeyword() {
		return r.verifyKeyword(ctx)
	}
	return nil
}

// Meta is one memory's metadata.
type Meta struct {
	ID        uint32
	Author    string
	Rkey      string
	Tags      []string
	CreatedAt time.Time
}

// Index is a segment's metadata, strings, doc index and clusters: what a
// node keeps in RAM to filter, list and locate memories.
type Index struct {
	Count   int
	Dims    int
	meta    []byte
	offsets []uint32
	tagRefs []uint32
	strData []byte
	blocks  []block
	// Clusters is nil for unclustered segments.
	Clusters *Clusters

	strIdx map[string]uint32
}

type block struct {
	firstRow uint32
	off      uint64
	len      uint32
}

// Clusters is a segment's cluster index.
type Clusters struct {
	Centers [][]float32
	// Ranges holds each cluster's first row and row count.
	Ranges [][2]uint32
}

// LoadIndex reads the metadata, strings, doc index and cluster sections.
func (r *Reader) LoadIndex(ctx context.Context) (*Index, error) {
	// The four sections aren't adjacent (vectors and docs sit between), so
	// read metadata+strings together and doc index+clusters together.
	a, b := r.h.sections[secMeta], r.h.sections[secDocIndex]
	first, err := r.rr.ReadRange(ctx, int64(a.off), int64(r.h.sections[secStrings].off+r.h.sections[secStrings].len-a.off))
	if err != nil {
		return nil, err
	}
	second, err := r.rr.ReadRange(ctx, int64(b.off), int64(r.h.sections[secClusters].off+r.h.sections[secClusters].len-b.off))
	if err != nil {
		return nil, err
	}
	cut := func(buf []byte, base uint64, s int) ([]byte, error) {
		sec := r.h.sections[s]
		p := buf[sec.off-base : sec.off-base+sec.len]
		if crc32.Checksum(p, crcTable) != sec.crc {
			return nil, fmt.Errorf("%w: %s section checksum mismatch", ErrFormat, sectionNames[s])
		}
		return p, nil
	}
	meta, err := cut(first, a.off, secMeta)
	if err != nil {
		return nil, err
	}
	strs, err := cut(first, a.off, secStrings)
	if err != nil {
		return nil, err
	}
	didx, err := cut(second, b.off, secDocIndex)
	if err != nil {
		return nil, err
	}
	cls, err := cut(second, b.off, secClusters)
	if err != nil {
		return nil, err
	}
	ix := &Index{Count: int(r.h.count), Dims: int(r.h.dims), meta: meta}
	if len(meta) != ix.Count*metaRow {
		return nil, fmt.Errorf("%w: metadata size", ErrFormat)
	}
	if len(strs) < 8 {
		return nil, fmt.Errorf("%w: strings section", ErrFormat)
	}
	nstr := int(binary.LittleEndian.Uint32(strs))
	ntag := int(binary.LittleEndian.Uint32(strs[4:]))
	p := 8
	if len(strs) < p+4*(nstr+1)+4*ntag {
		return nil, fmt.Errorf("%w: strings section", ErrFormat)
	}
	ix.offsets = make([]uint32, nstr+1)
	for i := range ix.offsets {
		ix.offsets[i] = binary.LittleEndian.Uint32(strs[p:])
		p += 4
	}
	ix.tagRefs = make([]uint32, ntag)
	for i := range ix.tagRefs {
		ix.tagRefs[i] = binary.LittleEndian.Uint32(strs[p:])
		p += 4
	}
	ix.strData = strs[p:]
	if int(ix.offsets[nstr]) != len(ix.strData) || !slices.IsSorted(ix.offsets) {
		return nil, fmt.Errorf("%w: string offsets", ErrFormat)
	}
	for _, ref := range ix.tagRefs {
		if int(ref) >= nstr {
			return nil, fmt.Errorf("%w: tag reference", ErrFormat)
		}
	}
	for row := range ix.Count {
		m := ix.meta[row*metaRow:]
		author, rkey := binary.LittleEndian.Uint32(m[4:]), binary.LittleEndian.Uint32(m[8:])
		toff, tn := binary.LittleEndian.Uint32(m[12:]), uint32(binary.LittleEndian.Uint16(m[16:]))
		if int(author) >= nstr || int(rkey) >= nstr || int(toff+tn) > ntag {
			return nil, fmt.Errorf("%w: metadata row %d", ErrFormat, row)
		}
	}
	if len(didx) < 4 {
		return nil, fmt.Errorf("%w: doc index", ErrFormat)
	}
	nb := int(binary.LittleEndian.Uint32(didx))
	if len(didx) != 4+16*nb {
		return nil, fmt.Errorf("%w: doc index size", ErrFormat)
	}
	for i := range nb {
		q := didx[4+16*i:]
		ix.blocks = append(ix.blocks, block{
			firstRow: binary.LittleEndian.Uint32(q),
			off:      binary.LittleEndian.Uint64(q[4:]),
			len:      binary.LittleEndian.Uint32(q[12:]),
		})
	}
	if len(cls) > 0 {
		c, err := parseClusters(cls, ix.Dims, ix.Count)
		if err != nil {
			return nil, err
		}
		ix.Clusters = c
	}
	return ix, nil
}

func parseClusters(b []byte, dims, count int) (*Clusters, error) {
	if len(b) < 4 {
		return nil, fmt.Errorf("%w: clusters section", ErrFormat)
	}
	k := int(binary.LittleEndian.Uint32(b))
	if k == 0 || k > MaxClusters || len(b) != 4+8*k+4*k*dims {
		return nil, fmt.Errorf("%w: clusters section size", ErrFormat)
	}
	c := &Clusters{Ranges: make([][2]uint32, k), Centers: make([][]float32, k)}
	p := 4
	for i := range k {
		c.Ranges[i] = [2]uint32{binary.LittleEndian.Uint32(b[p:]), binary.LittleEndian.Uint32(b[p+4:])}
		if int(c.Ranges[i][0])+int(c.Ranges[i][1]) > count {
			return nil, fmt.Errorf("%w: cluster range", ErrFormat)
		}
		p += 8
	}
	for i := range k {
		v := make([]float32, dims)
		for j := range v {
			v[j] = math.Float32frombits(binary.LittleEndian.Uint32(b[p:]))
			p += 4
		}
		c.Centers[i] = v
	}
	return c, nil
}

func (ix *Index) str(i uint32) string {
	return string(ix.strData[ix.offsets[i]:ix.offsets[i+1]])
}

// StringID returns a string's index in this segment, if present. Filters
// use it to compare integers instead of strings.
func (ix *Index) StringID(s string) (uint32, bool) {
	if ix.strIdx == nil {
		m := make(map[string]uint32, len(ix.offsets)-1)
		for i := range len(ix.offsets) - 1 {
			m[ix.str(uint32(i))] = uint32(i)
		}
		ix.strIdx = m
	}
	i, ok := ix.strIdx[s]
	return i, ok
}

// ID is a row's memory id.
func (ix *Index) ID(row int) uint32 {
	return binary.LittleEndian.Uint32(ix.meta[row*metaRow:])
}

// AuthorID is a row's author, as a string index.
func (ix *Index) AuthorID(row int) uint32 {
	return binary.LittleEndian.Uint32(ix.meta[row*metaRow+4:])
}

// CreatedAt is a row's creation time in Unix microseconds.
func (ix *Index) CreatedAtMicros(row int) int64 {
	return int64(binary.LittleEndian.Uint64(ix.meta[row*metaRow+24:]))
}

// CIDHash is a row's CID hash (see CIDHash).
func (ix *Index) CIDHash(row int) uint64 {
	return binary.LittleEndian.Uint64(ix.meta[row*metaRow+32:])
}

// DocSize is the length of a row's text plus source.
func (ix *Index) DocSize(row int) int64 {
	return int64(binary.LittleEndian.Uint32(ix.meta[row*metaRow+20:]))
}

// HasTags reports whether a row carries every tag (as string indexes).
func (ix *Index) HasTags(row int, tags []uint32) bool {
	m := ix.meta[row*metaRow:]
	off, n := binary.LittleEndian.Uint32(m[12:]), uint32(binary.LittleEndian.Uint16(m[16:]))
	refs := ix.tagRefs[off : off+n]
	for _, t := range tags {
		if !slices.Contains(refs, t) {
			return false
		}
	}
	return true
}

// Meta decodes a row's metadata.
func (ix *Index) Meta(row int) Meta {
	m := ix.meta[row*metaRow:]
	off, n := binary.LittleEndian.Uint32(m[12:]), uint32(binary.LittleEndian.Uint16(m[16:]))
	out := Meta{
		ID:        binary.LittleEndian.Uint32(m),
		Author:    ix.str(binary.LittleEndian.Uint32(m[4:])),
		Rkey:      ix.str(binary.LittleEndian.Uint32(m[8:])),
		CreatedAt: time.UnixMicro(int64(binary.LittleEndian.Uint64(m[24:]))).UTC(),
	}
	for _, ref := range ix.tagRefs[off : off+n] {
		out.Tags = append(out.Tags, ix.str(ref))
	}
	return out
}

// ProbeRanges returns the row ranges of the nprobe clusters nearest the
// query, or the whole segment when it isn't clustered.
func (ix *Index) ProbeRanges(query []float32, nprobe int) [][2]int {
	if ix.Clusters == nil || nprobe >= len(ix.Clusters.Centers) {
		return [][2]int{{0, ix.Count}}
	}
	type sc struct {
		c int
		s float32
	}
	scores := make([]sc, len(ix.Clusters.Centers))
	for i, c := range ix.Clusters.Centers {
		scores[i] = sc{i, vec.Dot(query, c)}
	}
	sort.Slice(scores, func(a, b int) bool { return scores[a].s > scores[b].s })
	var out [][2]int
	for _, s := range scores[:max(1, nprobe)] {
		r := ix.Clusters.Ranges[s.c]
		if r[1] > 0 {
			out = append(out, [2]int{int(r[0]), int(r[0] + r[1])})
		}
	}
	return out
}

// RAMBytes estimates the index's memory footprint.
func (ix *Index) RAMBytes() int64 {
	n := int64(len(ix.meta) + len(ix.strData) + 4*len(ix.offsets) + 4*len(ix.tagRefs) + 16*len(ix.blocks))
	if ix.Clusters != nil {
		n += int64(len(ix.Clusters.Centers) * (ix.Dims*4 + 8))
	}
	return n
}

// LoadBits reads the whole 1-bit section.
func (r *Reader) LoadBits(ctx context.Context) ([]byte, error) {
	return r.section(ctx, secBits)
}

// BitsRange reads the 1-bit vectors of rows [from, to), for streaming scans
// of spaces too large to keep in RAM.
func (r *Reader) BitsRange(ctx context.Context, from, to int) ([]byte, error) {
	bl := int64(vec.BitBytes(r.Dims()))
	sec := r.h.sections[secBits]
	return r.rr.ReadRange(ctx, int64(sec.off)+int64(from)*bl, int64(to-from)*bl)
}

// ReadInt8 reads the int8 vectors of the given rows, grouping adjacent rows
// into one read. The result is in the order of rows.
func (r *Reader) ReadInt8(ctx context.Context, rows []int) ([][]byte, error) {
	il := int64(vec.Int8Bytes(r.Dims()))
	sec := r.h.sections[secInt8]
	order := make([]int, len(rows))
	for i := range order {
		order[i] = i
	}
	sort.Slice(order, func(a, b int) bool { return rows[order[a]] < rows[order[b]] })
	out := make([][]byte, len(rows))
	for i := 0; i < len(order); {
		j := i + 1
		for j < len(order) && rows[order[j]] <= rows[order[j-1]]+1 {
			j++
		}
		first, last := rows[order[i]], rows[order[j-1]]
		if first < 0 || last >= r.Count() {
			return nil, fmt.Errorf("row %d out of range", last)
		}
		b, err := r.rr.ReadRange(ctx, int64(sec.off)+int64(first)*il, int64(last-first+1)*il)
		if err != nil {
			return nil, err
		}
		for _, k := range order[i:j] {
			p := int64(rows[k]-first) * il
			out[k] = b[p : p+il]
		}
		i = j
	}
	return out, nil
}

// Body is a memory's stored document.
type Body struct {
	Text      string
	Source    string
	CID       string
	IndexedAt time.Time
}

var decoder, _ = zstd.NewReader(nil, zstd.WithDecoderConcurrency(0))

// ReadDocs reads the documents of the given rows, fetching each needed
// block once. The result is in the order of rows.
func (r *Reader) ReadDocs(ctx context.Context, ix *Index, rows []int) ([]Body, error) {
	sec := r.h.sections[secDocs]
	byBlock := map[int][]int{} // block -> positions in rows
	for i, row := range rows {
		if row < 0 || row >= ix.Count {
			return nil, fmt.Errorf("row %d out of range", row)
		}
		bi := sort.Search(len(ix.blocks), func(k int) bool { return int(ix.blocks[k].firstRow) > row }) - 1
		if bi < 0 {
			return nil, fmt.Errorf("%w: no block for row %d", ErrFormat, row)
		}
		byBlock[bi] = append(byBlock[bi], i)
	}
	out := make([]Body, len(rows))
	for bi, positions := range byBlock {
		bl := ix.blocks[bi]
		if bl.off+uint64(bl.len) > sec.len {
			return nil, fmt.Errorf("%w: doc block outside section", ErrFormat)
		}
		comp, err := r.rr.ReadRange(ctx, int64(sec.off+bl.off), int64(bl.len))
		if err != nil {
			return nil, err
		}
		raw, err := decoder.DecodeAll(comp, nil)
		if err != nil {
			return nil, fmt.Errorf("%w: doc block: %v", ErrFormat, err)
		}
		bodies, err := parseBlock(raw)
		if err != nil {
			return nil, err
		}
		for _, i := range positions {
			k := rows[i] - int(bl.firstRow)
			if k >= len(bodies) {
				return nil, fmt.Errorf("%w: row %d missing from its block", ErrFormat, rows[i])
			}
			out[i] = bodies[k]
		}
	}
	return out, nil
}

func parseBlock(b []byte) ([]Body, error) {
	var out []Body
	readStr := func() (string, bool) {
		n, k := binary.Uvarint(b)
		if k <= 0 || uint64(len(b)-k) < n {
			return "", false
		}
		s := string(b[k : k+int(n)])
		b = b[k+int(n):]
		return s, true
	}
	for len(b) > 0 {
		var d Body
		var ok bool
		if d.Text, ok = readStr(); !ok {
			return nil, fmt.Errorf("%w: doc block", ErrFormat)
		}
		if d.Source, ok = readStr(); !ok {
			return nil, fmt.Errorf("%w: doc block", ErrFormat)
		}
		if d.CID, ok = readStr(); !ok {
			return nil, fmt.Errorf("%w: doc block", ErrFormat)
		}
		t, k := binary.Varint(b)
		if k <= 0 {
			return nil, fmt.Errorf("%w: doc block", ErrFormat)
		}
		b = b[k:]
		d.IndexedAt = time.UnixMicro(t).UTC()
		out = append(out, d)
	}
	return out, nil
}

// ReadAll reads every row's vectors, metadata and document, as needed to
// merge segments.
func (r *Reader) ReadAll(ctx context.Context, ix *Index) ([]Doc, error) {
	bits, err := r.LoadBits(ctx)
	if err != nil {
		return nil, err
	}
	int8s, err := r.section(ctx, secInt8)
	if err != nil {
		return nil, err
	}
	rows := make([]int, ix.Count)
	for i := range rows {
		rows[i] = i
	}
	bodies, err := r.ReadDocs(ctx, ix, rows)
	if err != nil {
		return nil, err
	}
	bl, il := vec.BitBytes(r.Dims()), vec.Int8Bytes(r.Dims())
	out := make([]Doc, ix.Count)
	for i := range out {
		m := ix.Meta(i)
		out[i] = Doc{
			ID: m.ID, Author: m.Author, Rkey: m.Rkey, Tags: m.Tags, CreatedAt: m.CreatedAt,
			CID: bodies[i].CID, Text: bodies[i].Text, Source: bodies[i].Source, IndexedAt: bodies[i].IndexedAt,
			Bits: bits[i*bl : (i+1)*bl], Int8: int8s[i*il : (i+1)*il],
		}
	}
	return out, nil
}
