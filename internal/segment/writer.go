package segment

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"slices"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/haileyok/engram-garden/internal/vec"
)

// Doc is one memory to write.
type Doc struct {
	ID        uint32
	Author    string
	Rkey      string
	CID       string
	Text      string
	Source    string
	Tags      []string
	CreatedAt time.Time
	IndexedAt time.Time
	// Vector is the unit-length vector. When it's nil, Bits and Int8 must
	// hold the quantized forms, as when merging segments.
	Vector []float32
	Bits   []byte
	Int8   []byte
}

// WriteOptions configure a segment.
type WriteOptions struct {
	Dims int
	// ClusterThreshold clusters the segment when it holds at least this many
	// memories. Zero never clusters.
	ClusterThreshold int
	// BlockSize is the uncompressed size of a docs block (default 64 KB).
	BlockSize int
}

// Info describes a written segment.
type Info struct {
	Count        int
	Bytes        int64
	MinCreatedAt time.Time
	MaxCreatedAt time.Time
	Clustered    bool
}

var encoder, _ = zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault))

// Write writes docs as a segment. Docs may be reordered (grouped by
// cluster); their IDs travel with them.
func Write(w io.Writer, docs []Doc, opt WriteOptions) (Info, error) {
	dims := opt.Dims
	if dims <= 0 {
		return Info{}, errors.New("segment: dims is required")
	}
	if len(docs) == 0 {
		return Info{}, errors.New("segment: no docs")
	}
	bitLen, int8Len := vec.BitBytes(dims), vec.Int8Bytes(dims)
	for i := range docs {
		d := &docs[i]
		if d.Vector != nil {
			if len(d.Vector) != dims {
				return Info{}, fmt.Errorf("segment: doc %d has %d dimensions, want %d", d.ID, len(d.Vector), dims)
			}
			d.Bits = vec.AppendBits(nil, d.Vector)
			d.Int8 = vec.AppendInt8(nil, d.Vector)
		}
		if len(d.Bits) != bitLen || len(d.Int8) != int8Len {
			return Info{}, fmt.Errorf("segment: doc %d has no usable vector", d.ID)
		}
	}

	var clusters *clusterTable
	if opt.ClusterThreshold > 0 && len(docs) >= opt.ClusterThreshold {
		clusters = cluster(docs, dims)
	}

	// Strings.
	strIdx := map[string]uint32{}
	var strs []string
	intern := func(s string) uint32 {
		if i, ok := strIdx[s]; ok {
			return i
		}
		i := uint32(len(strs))
		strIdx[s] = i
		strs = append(strs, s)
		return i
	}
	var tagRefs []uint32
	meta := make([]byte, 0, len(docs)*metaRow)
	bits := make([]byte, 0, len(docs)*bitLen)
	int8s := make([]byte, 0, len(docs)*int8Len)
	info := Info{Count: len(docs), Clustered: clusters != nil}
	for i, d := range docs {
		if len(d.Tags) > 0xffff {
			return Info{}, fmt.Errorf("segment: doc %d has too many tags", d.ID)
		}
		tagsOff := uint32(len(tagRefs))
		for _, t := range d.Tags {
			tagRefs = append(tagRefs, intern(t))
		}
		cl := uint16(noCluster)
		if clusters != nil {
			cl = clusters.of[i]
		}
		meta = binary.LittleEndian.AppendUint32(meta, d.ID)
		meta = binary.LittleEndian.AppendUint32(meta, intern(d.Author))
		meta = binary.LittleEndian.AppendUint32(meta, intern(d.Rkey))
		meta = binary.LittleEndian.AppendUint32(meta, tagsOff)
		meta = binary.LittleEndian.AppendUint16(meta, uint16(len(d.Tags)))
		meta = binary.LittleEndian.AppendUint16(meta, cl)
		meta = binary.LittleEndian.AppendUint32(meta, 0)
		meta = binary.LittleEndian.AppendUint64(meta, uint64(d.CreatedAt.UnixMicro()))
		bits = append(bits, d.Bits...)
		int8s = append(int8s, d.Int8...)
		if info.MinCreatedAt.IsZero() || d.CreatedAt.Before(info.MinCreatedAt) {
			info.MinCreatedAt = d.CreatedAt
		}
		if d.CreatedAt.After(info.MaxCreatedAt) {
			info.MaxCreatedAt = d.CreatedAt
		}
	}
	strSec := binary.LittleEndian.AppendUint32(nil, uint32(len(strs)))
	strSec = binary.LittleEndian.AppendUint32(strSec, uint32(len(tagRefs)))
	off := uint32(0)
	for _, s := range strs {
		strSec = binary.LittleEndian.AppendUint32(strSec, off)
		off += uint32(len(s))
	}
	strSec = binary.LittleEndian.AppendUint32(strSec, off)
	for _, r := range tagRefs {
		strSec = binary.LittleEndian.AppendUint32(strSec, r)
	}
	for _, s := range strs {
		strSec = append(strSec, s...)
	}

	// Docs, in compressed blocks.
	blockSize := opt.BlockSize
	if blockSize <= 0 {
		blockSize = 64 << 10
	}
	var docsSec, docIdx []byte
	var block []byte
	nblocks := uint32(0)
	blockStart := 0
	flush := func(next int) {
		if len(block) == 0 {
			return
		}
		comp := encoder.EncodeAll(block, nil)
		docIdx = binary.LittleEndian.AppendUint32(docIdx, uint32(blockStart))
		docIdx = binary.LittleEndian.AppendUint64(docIdx, uint64(len(docsSec)))
		docIdx = binary.LittleEndian.AppendUint32(docIdx, uint32(len(comp)))
		docsSec = append(docsSec, comp...)
		nblocks++
		block = block[:0]
		blockStart = next
	}
	for i, d := range docs {
		for _, s := range []string{d.Text, d.Source, d.CID} {
			block = binary.AppendUvarint(block, uint64(len(s)))
			block = append(block, s...)
		}
		block = binary.AppendVarint(block, d.IndexedAt.UnixMicro())
		if len(block) >= blockSize {
			flush(i + 1)
		}
	}
	flush(len(docs))
	docIdx = append(binary.LittleEndian.AppendUint32(nil, nblocks), docIdx...)

	var clSec []byte
	if clusters != nil {
		clSec = clusters.marshal()
	}

	secs := [numSections][]byte{meta, strSec, bits, int8s, docsSec, docIdx, clSec}
	h := header{count: uint32(len(docs)), dims: uint32(dims)}
	pos := uint64(HeaderSize)
	for i, s := range secs {
		h.sections[i] = section{off: pos, len: uint64(len(s)), crc: crc32.Checksum(s, crcTable)}
		pos += uint64(len(s))
	}
	hb := h.marshal()
	cw := &countWriter{w: w}
	parts := [][]byte{hb}
	parts = append(parts, secs[:]...)
	parts = append(parts, hb, []byte(endMagic))
	for _, p := range parts {
		if _, err := cw.Write(p); err != nil {
			return Info{}, err
		}
	}
	info.Bytes = cw.n
	return info, nil
}

type countWriter struct {
	w io.Writer
	n int64
}

func (c *countWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// Bytes writes a segment into memory.
func Bytes(docs []Doc, opt WriteOptions) ([]byte, Info, error) {
	var buf bytes.Buffer
	info, err := Write(&buf, docs, opt)
	return buf.Bytes(), info, err
}

// sortByCluster reorders docs so each cluster's docs are contiguous.
func sortByCluster(docs []Doc, of []uint16) {
	idx := make([]int, len(docs))
	for i := range idx {
		idx[i] = i
	}
	slices.SortStableFunc(idx, func(a, b int) int { return int(of[a]) - int(of[b]) })
	nd := make([]Doc, len(docs))
	no := make([]uint16, len(docs))
	for i, j := range idx {
		nd[i], no[i] = docs[j], of[j]
	}
	copy(docs, nd)
	copy(of, no)
}
