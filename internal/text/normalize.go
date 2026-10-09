package text

import (
	"crypto/sha256"
	"encoding/hex"
	"hash/maphash"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/kljensen/snowball/english"
	"golang.org/x/text/cases"
	"golang.org/x/text/runes"
	"golang.org/x/text/transform"
	"golang.org/x/text/unicode/norm"
)

const (
	// maxTermBytes is the longest term stored as is. Longer terms keep
	// boundPrefix bytes and a hash of the whole.
	maxTermBytes = 64
	boundPrefix  = 48
	boundHashHex = 12
)

// normalize folds s for matching: compatibility forms, case and accents.
//
// The order matters: NFKC first, because compatibility forms can produce
// uppercase (Ⅻ → XII); then case folding (ß → ss); then NFKD and removing
// nonspacing marks, which strips accents in both precomposed and decomposed
// input and the dot that folding İ leaves; then NFC to recompose what NFKD
// split but isn't an accent (Hangul syllables).
func normalize(s string) string {
	if isASCII(s) {
		return asciiLower(s)
	}
	s = norm.NFKC.String(s)
	s = cases.Fold().String(s) // a Caser isn't safe to share
	s = norm.NFKD.String(s)
	s, _, _ = transform.String(runes.Remove(runes.In(unicode.Mn)), s)
	s = norm.NFC.String(s)
	return strings.Map(func(r rune) rune {
		switch r {
		case '\u2018', '\u2019', '\u02bc':
			return '\''
		}
		return r
	}, s)
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= utf8.RuneSelf {
			return false
		}
	}
	return true
}

func asciiLower(s string) string {
	for i := 0; i < len(s); i++ {
		if c := s[i]; c >= 'A' && c <= 'Z' {
			b := []byte(s)
			for j := i; j < len(b); j++ {
				if c := b[j]; c >= 'A' && c <= 'Z' {
					b[j] = c + 'a' - 'A'
				}
			}
			return string(b)
		}
	}
	return s
}

// bound keeps a term at most maxTermBytes long: a longer one becomes its
// first boundPrefix bytes (cut at a character boundary), "#", and the start
// of the SHA-256 of the whole term, so long URLs and paths that share a
// prefix stay distinct and an exact lookup still finds them.
func bound(term string) string {
	if len(term) <= maxTermBytes {
		return term
	}
	cut := boundPrefix
	for cut > 0 && !utf8.RuneStart(term[cut]) {
		cut--
	}
	h := sha256.Sum256([]byte(term))
	return term[:cut] + "#" + hex.EncodeToString(h[:])[:boundHashHex]
}

// StemPrefix marks stem terms, so they never collide with exact ones.
const StemPrefix = "~"

// stem returns the stem term of a normalized word, or "" if it isn't an
// English word: only ASCII letters and apostrophes are stemmed. Snowball's
// first step strips possessives.
func stem(term string) string {
	letters := false
	for i := 0; i < len(term); i++ {
		c := term[i]
		switch {
		case c >= 'a' && c <= 'z':
			letters = true
		case c == '\'':
		default:
			return ""
		}
	}
	if !letters {
		return ""
	}
	return stems.get(term)
}

// stems caches stem terms by word. The stemmer dominates analysis time and
// word frequencies are heavily skewed, so most lookups hit. The cache only
// changes speed: a miss computes the same stem.
var stems = func() *stemCache {
	c := &stemCache{seed: maphash.MakeSeed()}
	for i := range c.shards {
		c.shards[i].m = map[string]string{}
	}
	return c
}()

const (
	stemShards     = 64
	stemShardLimit = 1024 // words per shard; a full shard starts over
)

type stemCache struct {
	seed   maphash.Seed
	shards [stemShards]struct {
		mu sync.Mutex
		m  map[string]string
	}
}

func (c *stemCache) get(word string) string {
	sh := &c.shards[maphash.String(c.seed, word)%stemShards]
	sh.mu.Lock()
	s, ok := sh.m[word]
	sh.mu.Unlock()
	if ok {
		return s
	}
	s = bound(StemPrefix + english.Stem(word, true))
	sh.mu.Lock()
	if len(sh.m) >= stemShardLimit {
		clear(sh.m)
	}
	sh.m[strings.Clone(word)] = s
	sh.mu.Unlock()
	return s
}
