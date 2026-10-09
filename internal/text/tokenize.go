// Package text turns memories and queries into the terms keyword search
// indexes and scores, and holds the scoring rules both sides share. See
// docs/design/keyword-search.md.
//
// Changing what any function here returns for the same input changes the
// index, so it needs a new Version and rewritten segments.
package text

import (
	"unicode"
	"unicode/utf8"

	"github.com/rivo/uniseg"
)

// Version identifies the analyzer. Segments record the version they were
// built with, and a space never scores across versions.
const Version = 1

const (
	// maxDocParts bounds the parts indexed for one token.
	maxDocParts = 64
	// opaqueMinLen is the shortest piece treated as an opaque identifier.
	opaqueMinLen = 16
)

// Field is the part of a memory a token came from.
type Field uint8

const (
	FieldText Field = iota
	FieldTag
	FieldSource
)

func (f Field) String() string {
	switch f {
	case FieldText:
		return "text"
	case FieldTag:
		return "tags"
	case FieldSource:
		return "source"
	}
	return "unknown"
}

// Kind is what sort of source token a token is.
type Kind uint8

const (
	// Word is a single word: an exact term and, for English words, a stem.
	Word Kind = iota
	// Compound is an identifier made of parts, like engram_space_uri or
	// ModelMismatch: an exact term for the whole, plus its parts.
	Compound
	// Opaque is a hash, CID or similar: only the whole token is a term.
	Opaque
	// CJK is a character bigram (or a lone character) from Chinese,
	// Japanese or Korean text.
	CJK
)

func (k Kind) String() string {
	switch k {
	case Word:
		return "word"
	case Compound:
		return "compound"
	case Opaque:
		return "opaque"
	case CJK:
		return "cjk"
	}
	return "unknown"
}

// Span locates a token or part in the original, unnormalized field.
type Span struct {
	Field Field
	// Tag is the tag's index when Field is FieldTag.
	Tag int
	// Start and End are UTF-8 byte offsets within the field (or tag).
	Start, End int
}

// Part is one piece of a compound token.
type Part struct {
	Term string
	// Stem is the part's stem term ("~…"), or empty if it isn't stemmed.
	Stem string
	Span Span
}

// Token is one source token and the terms it emits.
type Token struct {
	Kind  Kind
	Span  Span
	Exact string
	// Stem is the stem term ("~…") of a Word, or empty.
	Stem  string
	Parts []Part
}

// AppendTerms appends the token's distinct terms to dst: its exact form,
// stem, parts and their stems. Each source token adds 1 to the frequency of
// each of these, once.
func (t *Token) AppendTerms(dst []string) []string {
	start := len(dst)
	add := func(s string) {
		if s == "" {
			return
		}
		for _, have := range dst[start:] {
			if have == s {
				return
			}
		}
		dst = append(dst, s)
	}
	add(t.Exact)
	add(t.Stem)
	for _, p := range t.Parts {
		add(p.Term)
		add(p.Stem)
	}
	return dst
}

// joiner reports whether r joins words into one compound token when it
// appears between them with no whitespace.
func joiner(r rune) bool {
	switch r {
	case '_', '-', '.', '/', ':', '@', '#':
		return true
	}
	return false
}

func isCJK(r rune) bool {
	return unicode.In(r, unicode.Han, unicode.Hiragana, unicode.Katakana, unicode.Hangul)
}

type segKind uint8

const (
	segOther segKind = iota
	segWord
	segJoiner
	segCJK
)

type seg struct {
	start, end int
	kind       segKind
}

// segments splits s at Unicode word boundaries (UAX #29) and classifies
// each piece.
func segments(s string) []seg {
	var out []seg
	state := -1
	off := 0
	rest := s
	for len(rest) > 0 {
		var w string
		w, rest, state = uniseg.FirstWordInString(rest, state)
		out = append(out, seg{start: off, end: off + len(w), kind: classify(w)})
		off += len(w)
	}
	return out
}

func classify(w string) segKind {
	r, _ := utf8.DecodeRuneInString(w)
	if isCJK(r) {
		return segCJK
	}
	allJoiners := true
	for _, r := range w {
		if unicode.IsLetter(r) || unicode.IsNumber(r) {
			return segWord
		}
		if !joiner(r) {
			allJoiners = false
		}
	}
	if allJoiners {
		return segJoiner
	}
	return segOther
}

// Tokenize splits one field into source tokens, calling emit for each in
// order. The Token passed to emit is reused; copy what you keep.
func Tokenize(s string, field Field, tag int, emit func(*Token)) {
	segs := segments(s)
	var tok Token
	for i := 0; i < len(segs); {
		switch segs[i].kind {
		case segCJK:
			j := i + 1
			for j < len(segs) && segs[j].kind == segCJK {
				j++
			}
			emitCJK(s, segs[i].start, segs[j-1].end, field, tag, &tok, emit)
			i = j
		case segWord:
			// A compound is word (joiners word)*: segments are contiguous,
			// so joiners between words mean there's no whitespace.
			last := i
			for j := i + 1; j < len(segs); {
				k := j
				for k < len(segs) && segs[k].kind == segJoiner {
					k++
				}
				if k == j || k >= len(segs) || segs[k].kind != segWord {
					break
				}
				last = k
				j = k + 1
			}
			emitWordish(s, segs[i].start, segs[last].end, field, tag, &tok, emit)
			i = last + 1
		default:
			i++
		}
	}
}

func emitCJK(s string, start, end int, field Field, tag int, tok *Token, emit func(*Token)) {
	type rpos struct{ off, size int }
	var rs []rpos
	for off := start; off < end; {
		_, size := utf8.DecodeRuneInString(s[off:end])
		rs = append(rs, rpos{off, size})
		off += size
	}
	if len(rs) == 1 {
		*tok = Token{Kind: CJK, Span: Span{field, tag, start, end}, Exact: bound(normalize(s[start:end]))}
		emit(tok)
		return
	}
	for i := 0; i+1 < len(rs); i++ {
		a, b := rs[i].off, rs[i+1].off+rs[i+1].size
		*tok = Token{Kind: CJK, Span: Span{field, tag, a, b}, Exact: bound(normalize(s[a:b]))}
		emit(tok)
	}
}

func emitWordish(s string, start, end int, field Field, tag int, tok *Token, emit func(*Token)) {
	// Trim joiners from the ends: "_foo_" and "memory.add." (UAX #29 keeps
	// some of these inside a word).
	for start < end {
		r, size := utf8.DecodeRuneInString(s[start:end])
		if !joiner(r) {
			break
		}
		start += size
	}
	for start < end {
		r, size := utf8.DecodeLastRuneInString(s[start:end])
		if !joiner(r) {
			break
		}
		end -= size
	}
	if start >= end {
		return
	}
	raw := s[start:end]
	exact := bound(normalize(raw))
	if exact == "" {
		return
	}
	*tok = Token{Span: Span{field, tag, start, end}, Exact: exact}
	if opaque(raw) {
		tok.Kind = Opaque
		emit(tok)
		return
	}

	// Split on joiners into components; opaque components stay whole,
	// the rest split at case and letter–digit boundaries.
	var parts []Part
	addPart := func(a, b int, isOpaque bool) {
		if len(parts) >= maxDocParts || utf8.RuneCountInString(s[a:b]) < 2 {
			return
		}
		term := bound(normalize(s[a:b]))
		if term == "" {
			return
		}
		p := Part{Term: term, Span: Span{field, tag, a, b}}
		if !isOpaque {
			p.Stem = stem(term)
		}
		parts = append(parts, p)
	}
	singleComponent := true
	for a := start; a < end; {
		b := a
		for b < end {
			r, size := utf8.DecodeRuneInString(s[b:end])
			if joiner(r) {
				break
			}
			b += size
		}
		if b < end {
			singleComponent = false
		}
		if b > a {
			if opaque(s[a:b]) {
				addPart(a, b, true)
			} else {
				subparts(s, a, b, func(x, y int) { addPart(x, y, false) })
			}
		}
		// Skip the joiner run.
		for b < end {
			r, size := utf8.DecodeRuneInString(s[b:end])
			if !joiner(r) {
				break
			}
			b += size
		}
		a = b
	}

	if singleComponent && (len(parts) == 0 || (len(parts) == 1 && parts[0].Span.Start == start && parts[0].Span.End == end)) {
		// No joiners and no internal boundaries: a plain word.
		tok.Kind = Word
		tok.Stem = stem(exact)
		emit(tok)
		return
	}
	tok.Kind = Compound
	tok.Parts = parts
	emit(tok)
}

type runeClass uint8

const (
	rcOther runeClass = iota
	rcLower
	rcUpper
	rcDigit
)

func classOf(r rune) runeClass {
	switch {
	case unicode.IsUpper(r) || unicode.IsTitle(r):
		return rcUpper
	case unicode.IsLetter(r):
		return rcLower
	case unicode.IsNumber(r):
		return rcDigit
	}
	return rcOther
}

// subparts splits s[start:end] at lower→upper changes, at the end of an
// acronym (HTTPServer → HTTP, Server, but not URLs → URL, s) and at
// letter–digit boundaries, calling part for each piece.
func subparts(s string, start, end int, part func(a, b int)) {
	type rc struct {
		off int
		c   runeClass
	}
	var buf [32]rc // most components are short: no allocation
	rs := buf[:0]
	for off := start; off < end; {
		r, size := utf8.DecodeRuneInString(s[off:end])
		rs = append(rs, rc{off, classOf(r)})
		off += size
	}
	letter := func(c runeClass) bool { return c == rcLower || c == rcUpper }
	from := start
	for i := 1; i < len(rs); i++ {
		prev, cur := rs[i-1].c, rs[i].c
		split := false
		switch {
		case prev == rcLower && cur == rcUpper:
			split = true
		case prev == rcUpper && cur == rcUpper && i+1 < len(rs) && rs[i+1].c == rcLower:
			// HTTPServer: split before the S. But URLs and IDs end in a
			// plural s, not a new word.
			pluralS := i+2 == len(rs) && s[rs[i+1].off] == 's'
			split = !pluralS
		case letter(prev) && cur == rcDigit, prev == rcDigit && letter(cur):
			split = true
		}
		if split {
			part(from, rs[i].off)
			from = rs[i].off
		}
	}
	part(from, end)
}

// opaque reports whether s looks like an identifier with no words in it: a
// hash, CID, DID identifier or record key. Such pieces are indexed only
// whole.
//
// The test is an AT Protocol record key (see tid), or at least 16 ASCII
// letters and digits that either are all digits or switch between letters
// and digits at least three times. Long all-letter words never qualify, and
// real hashes and base32 identifiers switch constantly.
func opaque(s string) bool {
	if tid(s) {
		return true
	}
	if len(s) < opaqueMinLen {
		return false
	}
	digits, switches := 0, 0
	prevDigit := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		isDigit := c >= '0' && c <= '9'
		isLetter := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
		if !isDigit && !isLetter {
			return false
		}
		if isDigit {
			digits++
		}
		if i > 0 && isDigit != prevDigit {
			switches++
		}
		prevDigit = isDigit
	}
	return digits == len(s) || switches >= 3
}

// tid reports whether s has the shape of an AT Protocol timestamp
// identifier, the usual record key: 13 characters of the base32-sortable
// alphabet (2–7, a–z), the first one of 2–7 or a–j. It also requires a
// digit, so 13-letter words don't qualify.
func tid(s string) bool {
	if len(s) != 13 {
		return false
	}
	if c := s[0]; !(c >= '2' && c <= '7' || c >= 'a' && c <= 'j') {
		return false
	}
	digit := false
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c >= '2' && c <= '7':
			digit = true
		case c >= 'a' && c <= 'z':
		default:
			return false
		}
	}
	return digit
}
