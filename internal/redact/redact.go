// Package redact removes secrets from text before it's indexed or shown:
// known token formats, credentials in URLs, values assigned to
// secret-looking names, private keys, and long random-looking strings. Each
// becomes [redacted:<kind>].
//
// It's a filter for transcripts, which are full of command output: it
// prefers removing a harmless random string to leaving a token. Hashes,
// CIDs, DIDs and UUIDs are kept, since they're what people search for.
// Any change to what it returns for the same input changes the session
// index: bump transcript.Version.
package redact

import (
	"math"
	"regexp"
	"strings"
)

type rule struct {
	kind string
	re   *regexp.Regexp
	// keep is how many leading submatches stay (a name and its "=").
	keep int
	// value means the last submatch is a value that may not be a secret
	// at all: a number, a boolean, or a reference to one elsewhere.
	value bool
}

// notSecret is a value that names or counts rather than holds a secret.
var notSecret = regexp.MustCompile(`^["']?(?:[0-9][0-9._]*|true|false|null|none|nil|undefined|required|optional|string|\[?redacted.*|\*+|x+|<[^>]*>|\$\{?[A-Za-z_][A-Za-z0-9_]*\}?|process\.env\.[A-Za-z0-9_]+|os\.Getenv\(.*|env\(.*|[A-Za-z_][A-Za-z0-9_]*\.[A-Za-z_][A-Za-z0-9_.]*)["']?$`)

var rules = []rule{
	{kind: "private-key", re: regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY( BLOCK)?-----[\s\S]*?(-----END [A-Z0-9 ]*PRIVATE KEY( BLOCK)?-----|$)`)},
	{kind: "github-token", re: regexp.MustCompile(`\b(gh[pousr]_[A-Za-z0-9]{30,}|github_pat_[A-Za-z0-9_]{40,})`)},
	{kind: "slack-token", re: regexp.MustCompile(`\bxox[abposre]-[A-Za-z0-9-]{10,}`)},
	{kind: "slack-webhook", re: regexp.MustCompile(`https://hooks\.slack\.com/services/[A-Za-z0-9/_-]+`)},
	{kind: "aws-key", re: regexp.MustCompile(`\b(AKIA|ASIA|AGPA|AROA|AIDA)[0-9A-Z]{16}\b`)},
	{kind: "api-key", re: regexp.MustCompile(`\bsk-(ant-|proj-|live-|test-)?[A-Za-z0-9_-]{20,}`)},
	{kind: "api-key", re: regexp.MustCompile(`\b(AIza[0-9A-Za-z_-]{35}|glpat-[0-9A-Za-z_-]{20,}|npm_[A-Za-z0-9]{36}|xai-[A-Za-z0-9]{40,}|hf_[A-Za-z0-9]{30,}|rk_live_[A-Za-z0-9]{20,})`)},
	{kind: "jwt", re: regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`)},
	// user:password@ in a URL; the host stays.
	{kind: "url-credentials", re: regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.-]*://)[^/\s:@'"]+:[^/\s@'"]+@`), keep: 1},
	{kind: "bearer", re: regexp.MustCompile(`(?i)(\b(?:bearer|basic|dpop)[ \t]+)[A-Za-z0-9][A-Za-z0-9._~+/=-]{15,}`), keep: 1},
	// NAME=value and NAME: value for secret-looking names, in shells,
	// env files, YAML and JSON. The name stays.
	{kind: "secret", re: regexp.MustCompile(`(?i)((?:"|')?[A-Za-z0-9_.-]*(?:secret|token|passw(?:or)?d|passwd|pwd|api[_-]?key|apikey|private[_-]?key|access[_-]?key|credentials?|auth[_-]?key|session[_-]?key|cookie)(?:"|')?\s*[=:]\s*)("[^"\n]{4,}"|'[^'\n]{4,}'|[^\s"',;}{\]]{4,})`), keep: 1, value: true},
}

// token is a run of characters a random secret is made of.
var token = regexp.MustCompile(`[A-Za-z0-9+/_-]{32,}={0,2}`)

var (
	hexOnly  = regexp.MustCompile(`^[0-9a-fA-F]+$`)
	b32Lower = regexp.MustCompile(`^[a-z2-7]+$`)
	uuidLike = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
)

// Text returns s with secrets replaced.
func Text(s string) string {
	for _, r := range rules {
		s = r.re.ReplaceAllStringFunc(s, func(m string) string {
			if r.keep == 0 {
				return "[redacted:" + r.kind + "]"
			}
			sub := r.re.FindStringSubmatch(m)
			kept := strings.Join(sub[1:r.keep+1], "")
			val := sub[len(sub)-1]
			if strings.HasPrefix(strings.Trim(val, `"'`), "[redacted:") {
				return m // already redacted by an earlier rule
			}
			if r.value && notSecret.MatchString(val) {
				return m
			}
			return kept + "[redacted:" + r.kind + "]"
		})
	}
	return token.ReplaceAllStringFunc(s, func(m string) string {
		if looksRandom(m) {
			return "[redacted:random]"
		}
		return m
	})
}

// looksRandom reports whether a long token looks like a key rather than a
// word, path, hash or identifier: mixed case and digits, and close to the
// most varied a string of its length can be.
func looksRandom(s string) bool {
	core := strings.Trim(s, "=-_/+")
	if len(core) < 32 || hexOnly.MatchString(core) || b32Lower.MatchString(core) || uuidLike.MatchString(core) {
		return false
	}
	if strings.HasPrefix(core, "redacted") {
		return false
	}
	var upper, lower, digit bool
	for _, c := range core {
		switch {
		case c >= 'A' && c <= 'Z':
			upper = true
		case c >= 'a' && c <= 'z':
			lower = true
		case c >= '0' && c <= '9':
			digit = true
		}
	}
	if !upper || !lower || !digit {
		return false
	}
	// Paths and identifiers made of words have separators every few
	// characters; keys don't.
	if strings.Count(core, "/") > 2 || strings.Count(core, "_") > 3 || strings.Count(core, "-") > 3 {
		return false
	}
	// Identifiers (TestHandler_OIDCRedirect, BucketServerSideEncryption)
	// are mostly words: runs of four or more lowercase letters. In random
	// text such runs are rare.
	if wordy(core) >= 0.45 || strings.HasPrefix(core, "toolu_") || strings.HasPrefix(core, "call_") {
		return false
	}
	return entropy(core) >= 4.2
}

// wordy is the share of s in runs of at least four lowercase letters.
func wordy(s string) float64 {
	in, run := 0, 0
	for i := 0; i <= len(s); i++ {
		if i < len(s) && s[i] >= 'a' && s[i] <= 'z' {
			run++
			continue
		}
		if run >= 4 {
			in += run
		}
		run = 0
	}
	return float64(in) / float64(len(s))
}

// entropy is the Shannon entropy of s, in bits per character.
func entropy(s string) float64 {
	var counts [256]int
	for i := 0; i < len(s); i++ {
		counts[s[i]]++
	}
	n := float64(len(s))
	var h float64
	for _, c := range counts {
		if c > 0 {
			p := float64(c) / n
			h -= p * math.Log2(p)
		}
	}
	return h
}
