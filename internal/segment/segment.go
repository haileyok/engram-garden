// Package segment reads and writes segment files: immutable batches of
// memories with their quantized vectors, laid out so each section can be
// fetched with one range read.
//
// Layout (all integers little-endian):
//
//	[header]      magic, version, count, dims, then a table of sections:
//	              offset, length and CRC-32C of each
//	[metadata]    count fixed-width rows: memory id, author, record key,
//	              tags, cluster, createdAt
//	[strings]     authors, record keys and tags, deduplicated
//	[1-bit]       count × BitBytes(dims)
//	[1-byte]      count × Int8Bytes(dims)
//	[docs]        text, source and CID, zstd-compressed in ~64 KB blocks
//	[doc index]   per block: first row, offset and length
//	[clusters]    optional: cluster centers and each cluster's row range
//	[footer]      a copy of the header, then the end magic
//
// When a segment is clustered, its rows are grouped by cluster, so each
// cluster's rows are contiguous in every section.
package segment

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
)

// Version is the only format version this package reads and writes.
const Version = 1

const (
	magic    = "EGSEG\x00\x00\x00"
	endMagic = "EGSEGEND"
)

// Sections, in file order.
const (
	secMeta = iota
	secStrings
	secBits
	secInt8
	secDocs
	secDocIndex
	secClusters
	numSections
)

var sectionNames = [numSections]string{"metadata", "strings", "1-bit", "1-byte", "docs", "doc index", "clusters"}

const (
	sectionEntry = 24 // offset u64, length u64, crc u32, reserved u32
	// HeaderSize is the fixed size of the header (and of its copy in the
	// footer).
	HeaderSize = 8 + 4*4 + numSections*sectionEntry
	footerSize = HeaderSize + len(endMagic)
	metaRow    = 32
	noCluster  = 0xffff
	// MaxClusters bounds the clusters in one segment (cluster numbers are
	// 16-bit, with one value meaning "none").
	MaxClusters = 4096
)

var crcTable = crc32.MakeTable(crc32.Castagnoli)

type section struct {
	off, len uint64
	crc      uint32
}

type header struct {
	count, dims uint32
	sections    [numSections]section
}

func (h *header) marshal() []byte {
	b := make([]byte, 0, HeaderSize)
	b = append(b, magic...)
	b = binary.LittleEndian.AppendUint32(b, Version)
	b = binary.LittleEndian.AppendUint32(b, h.count)
	b = binary.LittleEndian.AppendUint32(b, h.dims)
	b = binary.LittleEndian.AppendUint32(b, 0) // flags, reserved
	for _, s := range h.sections {
		b = binary.LittleEndian.AppendUint64(b, s.off)
		b = binary.LittleEndian.AppendUint64(b, s.len)
		b = binary.LittleEndian.AppendUint32(b, s.crc)
		b = binary.LittleEndian.AppendUint32(b, 0)
	}
	return b
}

// ErrFormat reports a file that isn't a readable segment.
var ErrFormat = errors.New("not a valid segment")

func parseHeader(b []byte, size int64) (header, error) {
	var h header
	if len(b) < HeaderSize || string(b[:8]) != magic {
		return h, fmt.Errorf("%w: bad magic", ErrFormat)
	}
	if v := binary.LittleEndian.Uint32(b[8:]); v != Version {
		return h, fmt.Errorf("%w: unsupported version %d", ErrFormat, v)
	}
	h.count = binary.LittleEndian.Uint32(b[12:])
	h.dims = binary.LittleEndian.Uint32(b[16:])
	if h.dims == 0 || h.dims > 16000 {
		return h, fmt.Errorf("%w: bad dimensions %d", ErrFormat, h.dims)
	}
	p := 24
	end := uint64(HeaderSize)
	for i := range h.sections {
		s := section{
			off: binary.LittleEndian.Uint64(b[p:]),
			len: binary.LittleEndian.Uint64(b[p+8:]),
			crc: binary.LittleEndian.Uint32(b[p+16:]),
		}
		p += sectionEntry
		// Sections are contiguous and in order.
		if s.off != end || s.off+s.len < s.off {
			return h, fmt.Errorf("%w: %s section out of place", ErrFormat, sectionNames[i])
		}
		end = s.off + s.len
		h.sections[i] = s
	}
	if size >= 0 && end+uint64(footerSize) != uint64(size) {
		return h, fmt.Errorf("%w: sections end at %d, file is %d bytes", ErrFormat, end, size)
	}
	return h, nil
}
