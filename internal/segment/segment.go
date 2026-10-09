// Package segment reads and writes segment files: immutable batches of
// memories with their quantized vectors, laid out so each section can be
// fetched with one range read.
//
// Layout (all integers little-endian):
//
//	[header]      magic, version, count, dims, then a table of sections:
//	              offset, length and CRC-32C of each; version 2 adds the
//	              analyzer version and the total length of all memories
//	[metadata]    count fixed-width rows: memory id, author, record key,
//	              tags, cluster, document size, createdAt, CID hash
//	[strings]     authors, record keys and tags, deduplicated
//	[1-bit]       count × BitBytes(dims)
//	[1-byte]      count × Int8Bytes(dims)
//	[docs]        text, source and CID, zstd-compressed in ~64 KB blocks
//	[doc index]   per block: first row, offset and length
//	[clusters]    optional: cluster centers and each cluster's row range
//	[norms]       version 2: count × 1 byte, each memory's length
//	[term index]  version 2: the first term of each dictionary block
//	[terms]       version 2: the term dictionary, in blocks of 128 terms
//	[postings]    version 2: each term's rows and frequencies
//	[footer]      a copy of the header, then the end magic
//
// Version 1 files are read as before; they have no keyword sections. The
// keyword sections are described in keyword.go and
// docs/design/keyword-search.md.
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

// Version is the newest format this package writes. It reads versions 1
// and 2; WriteOptions.Keyword chooses which one Write produces.
const Version = 2

const (
	magic    = "EGSEG\x00\x00\x00"
	endMagic = "EGSEGEND"
)

// Sections, in file order. Version 1 has the first numSectionsV1.
const (
	secMeta = iota
	secStrings
	secBits
	secInt8
	secDocs
	secDocIndex
	secClusters
	secNorms
	secTermIndex
	secTerms
	secPostings
	numSections
)

const numSectionsV1 = secNorms

var sectionNames = [numSections]string{"metadata", "strings", "1-bit", "1-byte", "docs", "doc index", "clusters",
	"norms", "term index", "terms", "postings"}

const (
	sectionEntry = 24 // offset u64, length u64, crc u32, reserved u32
	// headerFixed is magic, version, count, dims and flags.
	headerFixed = 8 + 4*4
	// headerV2Extra is the analyzer version (u32), reserved (u32) and the
	// total length of all memories in source tokens (u64).
	headerV2Extra = 16
	// metaRow: id u32, author u32, rkey u32, tags offset u32, tag count
	// u16, cluster u16, doc size u32, createdAt i64, CID hash u64.
	metaRow   = 40
	noCluster = 0xffff
	// MaxClusters bounds the clusters in one segment (cluster numbers are
	// 16-bit, with one value meaning "none").
	MaxClusters = 4096
)

// sectionsIn is how many sections a version has.
func sectionsIn(version uint32) int {
	if version == 1 {
		return numSectionsV1
	}
	return numSections
}

// headerSize is the size of a version's header (and of its copy in the
// footer).
func headerSize(version uint32) int {
	n := headerFixed + sectionsIn(version)*sectionEntry
	if version >= 2 {
		n += headerV2Extra
	}
	return n
}

func footerSize(version uint32) int { return headerSize(version) + len(endMagic) }

// maxHeaderSize is the largest header of any version this package reads.
var maxHeaderSize = headerSize(Version)

var crcTable = crc32.MakeTable(crc32.Castagnoli)

type section struct {
	off, len uint64
	crc      uint32
}

type header struct {
	version     uint32
	count, dims uint32
	sections    [numSections]section
	// analyzer and totalLength are version 2's keyword statistics.
	analyzer    uint32
	totalLength uint64
}

func (h *header) marshal() []byte {
	b := make([]byte, 0, headerSize(h.version))
	b = append(b, magic...)
	b = binary.LittleEndian.AppendUint32(b, h.version)
	b = binary.LittleEndian.AppendUint32(b, h.count)
	b = binary.LittleEndian.AppendUint32(b, h.dims)
	b = binary.LittleEndian.AppendUint32(b, 0) // flags, reserved
	for _, s := range h.sections[:sectionsIn(h.version)] {
		b = binary.LittleEndian.AppendUint64(b, s.off)
		b = binary.LittleEndian.AppendUint64(b, s.len)
		b = binary.LittleEndian.AppendUint32(b, s.crc)
		b = binary.LittleEndian.AppendUint32(b, 0)
	}
	if h.version >= 2 {
		b = binary.LittleEndian.AppendUint32(b, h.analyzer)
		b = binary.LittleEndian.AppendUint32(b, 0)
		b = binary.LittleEndian.AppendUint64(b, h.totalLength)
	}
	return b
}

// ErrFormat reports a file that isn't a readable segment.
var ErrFormat = errors.New("not a valid segment")

// parseHeader parses a header from the start of a file of the given size
// (-1 when unknown). b may be longer than the header.
func parseHeader(b []byte, size int64) (header, error) {
	var h header
	if len(b) < headerFixed || string(b[:8]) != magic {
		return h, fmt.Errorf("%w: bad magic", ErrFormat)
	}
	h.version = binary.LittleEndian.Uint32(b[8:])
	if h.version != 1 && h.version != 2 {
		return h, fmt.Errorf("%w: unsupported version %d", ErrFormat, h.version)
	}
	hs := headerSize(h.version)
	if len(b) < hs {
		return h, fmt.Errorf("%w: header too short", ErrFormat)
	}
	h.count = binary.LittleEndian.Uint32(b[12:])
	h.dims = binary.LittleEndian.Uint32(b[16:])
	if h.dims == 0 || h.dims > 16000 {
		return h, fmt.Errorf("%w: bad dimensions %d", ErrFormat, h.dims)
	}
	p := headerFixed
	end := uint64(hs)
	for i := range sectionsIn(h.version) {
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
	if h.version >= 2 {
		h.analyzer = binary.LittleEndian.Uint32(b[p:])
		h.totalLength = binary.LittleEndian.Uint64(b[p+8:])
		if h.sections[secNorms].len != uint64(h.count) {
			return h, fmt.Errorf("%w: norms section size", ErrFormat)
		}
	} else {
		// Version 1 files end their sections at the clusters; the keyword
		// sections are empty and sit there.
		for i := numSectionsV1; i < numSections; i++ {
			h.sections[i] = section{off: end}
		}
	}
	if size >= 0 && end+uint64(len(endMagic)+hs) != uint64(size) {
		return h, fmt.Errorf("%w: sections end at %d, file is %d bytes", ErrFormat, end, size)
	}
	return h, nil
}
