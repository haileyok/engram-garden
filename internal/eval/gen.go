package eval

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"sync"

	"github.com/haileyok/engram-garden/internal/text"
)

// Query categories. A single average hides regressions, so every metric is
// reported per category.
const (
	CatParaphrase = "paraphrase"
	CatIdentifier = "identifier"
	CatPart       = "part"
	CatName       = "name"
	CatWordForm   = "wordform"
	CatCommon     = "common"
	CatTagSource  = "tagsource"
	CatMixed      = "mixed"
	CatNoAnswer   = "noanswer"
	CatLogged     = "logged"
)

// Categories lists the generated categories in report order.
var Categories = []string{CatParaphrase, CatIdentifier, CatPart, CatName, CatWordForm, CatCommon, CatTagSource, CatMixed, CatNoAnswer, CatLogged}

// Query is one evaluation query.
type Query struct {
	ID       string `json:"id"`
	Category string `json:"category"`
	Text     string `json:"text"`
	// Target is the memory a known-item query was written for.
	Target string `json:"target,omitempty"`
}

// QueryID derives a stable ID.
func QueryID(category, text, target string) string {
	h := sha256.Sum256([]byte(category + "\x00" + text + "\x00" + target))
	return hex.EncodeToString(h[:6])
}

// Tuning reports whether a query is in the tuning set (about 40%); the
// rest is held out for the decision.
func Tuning(q Query) bool {
	h := sha256.Sum256([]byte("split\x00" + q.ID))
	return h[0]%10 < 4
}

// features are what a memory offers each category.
type features struct {
	identifiers []string // compounds and opaque tokens, verbatim
	compounds   []string // identifiers with parts
	rare        []string // words in at most two memories
	tagSource   []string // words only in tags or source
}

func (ix *Index) features(m Memory) features {
	var f features
	inText := map[string]bool{}
	add := func(list *[]string, s string) {
		if !slices.Contains(*list, s) && len(*list) < 8 {
			*list = append(*list, s)
		}
	}
	text.Tokenize(m.Text, text.FieldText, 0, func(t *text.Token) {
		inText[t.Exact] = true
		raw := m.Text[t.Span.Start:t.Span.End]
		switch t.Kind {
		case text.Compound:
			// Hyphenated words (self-experiment) aren't identifiers.
			if len(raw) <= 64 && !strings.Contains(raw, "://") && identifierLike(raw) {
				add(&f.identifiers, raw)
				if len(t.Parts) >= 2 {
					add(&f.compounds, raw)
				}
			}
		case text.Opaque:
			add(&f.identifiers, raw)
		case text.Word:
			if len(raw) >= 4 && t.Stem != "" && ix.DF(t.Exact) <= 2 {
				add(&f.rare, raw)
			}
		}
	})
	other := func(s string) {
		text.Tokenize(s, text.FieldText, 0, func(t *text.Token) {
			if t.Kind == text.Word && len(t.Exact) >= 3 && !inText[t.Exact] {
				add(&f.tagSource, s[t.Span.Start:t.Span.End])
			}
		})
	}
	for _, tag := range m.Tags {
		other(tag)
	}
	other(m.Source)
	return f
}

const genSystem = `You write search queries that an AI agent would type into its memory search tool to find a specific stored memory. Agents search with short queries or one-sentence questions. Answer with a JSON object only.`

func genPrompt(m Memory, f features) (string, []string) {
	var b strings.Builder
	fmt.Fprintf(&b, "Memory:\n<<<\n%s\n>>>\n", truncateRunes(m.Text, 4000))
	if len(m.Tags) > 0 {
		fmt.Fprintf(&b, "Tags: %s\n", strings.Join(m.Tags, ", "))
	}
	if m.Source != "" {
		fmt.Fprintf(&b, "Source: %s\n", m.Source)
	}
	b.WriteString("\nWrite one query for each key below, each aimed at finding this memory among thousands of others. Use an empty string if a key is impossible.\n")
	keys := []string{CatParaphrase, CatWordForm, CatCommon}
	fmt.Fprintf(&b, "- %q: a natural question or phrase asking for this memory's information in different words. Don't copy its distinctive words or identifiers.\n", CatParaphrase)
	fmt.Fprintf(&b, "- %q: a query that uses a different grammatical form of a key word in the memory (\"migrating\" where it says \"migration\"), in otherwise different words.\n", CatWordForm)
	fmt.Fprintf(&b, "- %q: a vague query made mostly of common words (like \"the thing we fixed before the deploy\") that still points at this memory.\n", CatCommon)
	if len(f.identifiers) > 0 {
		keys = append(keys, CatIdentifier, CatMixed)
		fmt.Fprintf(&b, "- %q: a short lookup containing exactly one of these identifiers, copied verbatim: %s\n", CatIdentifier, quoteList(f.identifiers))
		fmt.Fprintf(&b, "- %q: a natural question that includes exactly one of these identifiers verbatim: %s\n", CatMixed, quoteList(f.identifiers))
	}
	if len(f.compounds) > 0 {
		keys = append(keys, CatPart)
		fmt.Fprintf(&b, "- %q: a query naming the idea behind one of these identifiers in plain separate words (\"space uri\" for engram_space_uri), without writing the identifier itself: %s\n", CatPart, quoteList(f.compounds))
	}
	if len(f.rare) > 0 {
		keys = append(keys, CatName)
		fmt.Fprintf(&b, "- %q: a short query using one of these rare names or terms verbatim: %s\n", CatName, quoteList(f.rare))
	}
	if len(f.tagSource) > 0 {
		keys = append(keys, CatTagSource)
		fmt.Fprintf(&b, "- %q: a query using one of these words, which appear only in the memory's tags or source: %s\n", CatTagSource, quoteList(f.tagSource))
	}
	fmt.Fprintf(&b, "\nAnswer as {%s}.", jsonKeys(keys))
	return b.String(), keys
}

// valid drops generated queries that don't do what their category needs.
func valid(cat, q string, f features) bool {
	lower := strings.ToLower(q)
	containsAny := func(list []string) bool {
		for _, s := range list {
			if strings.Contains(lower, strings.ToLower(s)) {
				return true
			}
		}
		return false
	}
	switch cat {
	case CatIdentifier, CatMixed:
		return containsAny(f.identifiers)
	case CatPart:
		return !containsAny(f.compounds)
	case CatName:
		return containsAny(f.rare)
	case CatTagSource:
		return containsAny(f.tagSource)
	}
	return true
}

// Generate writes known-item queries for a sample of n memories.
func Generate(ctx context.Context, l *LLM, ix *Index, n int, seed uint64) ([]Query, error) {
	order := rand.New(rand.NewPCG(seed, 1)).Perm(len(ix.Mems))
	order = order[:min(n, len(order))]
	var mu sync.Mutex
	var out []Query
	var firstErr error
	var wg sync.WaitGroup
	for _, i := range order {
		m := ix.Mems[i]
		f := ix.features(m)
		prompt, keys := genPrompt(m, f)
		wg.Go(func() {
			ans := map[string]string{}
			if err := l.JSON(ctx, genSystem, prompt, &ans); err != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = fmt.Errorf("memory %s: %w", m.ID, err)
				}
				mu.Unlock()
				return
			}
			mu.Lock()
			defer mu.Unlock()
			for _, k := range keys {
				q := strings.TrimSpace(ans[k])
				if q == "" || !valid(k, q, f) {
					continue
				}
				out = append(out, Query{ID: QueryID(k, q, m.ID), Category: k, Text: q, Target: m.ID})
			}
		})
	}
	wg.Wait()
	slices.SortFunc(out, func(a, b Query) int { return strings.Compare(a.ID, b.ID) })
	return out, firstErr
}

const noAnswerSystem = `You write search queries for an AI agent's memory search tool. Answer with a JSON object only.`

// GenerateNoAnswer writes queries in the corpus's domain that its memories
// shouldn't answer. Judging later drops any that turn out answerable.
func GenerateNoAnswer(ctx context.Context, l *LLM, ix *Index, rounds int, seed uint64) ([]Query, error) {
	rng := rand.New(rand.NewPCG(seed, 2))
	var out []Query
	for r := range rounds {
		var b strings.Builder
		b.WriteString("Here are excerpts from an agent's stored memories:\n")
		for _, i := range rng.Perm(len(ix.Mems))[:min(30, len(ix.Mems))] {
			fmt.Fprintf(&b, "- %s\n", strings.ReplaceAll(truncateRunes(ix.Mems[i].Text, 200), "\n", " "))
		}
		b.WriteString("\nWrite 10 queries the same agent might plausibly search for, in the same domain and style, about specific things these memories don't cover, so that no stored memory would answer them. Include some with identifiers and some in plain language. Answer as {\"queries\": [...]}.")
		var ans struct {
			Queries []string `json:"queries"`
		}
		if err := l.JSON(ctx, noAnswerSystem, b.String()+fmt.Sprintf("\n(round %d)", r), &ans); err != nil {
			return out, err
		}
		for _, q := range ans.Queries {
			if q = strings.TrimSpace(q); q != "" {
				out = append(out, Query{ID: QueryID(CatNoAnswer, q, ""), Category: CatNoAnswer, Text: q})
			}
		}
	}
	return out, nil
}

// identifierLike reports whether a compound looks like an identifier rather
// than a hyphenated word: it has a joiner other than a hyphen, a digit, or
// a capital after its first letter.
func identifierLike(s string) bool {
	for i, r := range s {
		switch {
		case strings.ContainsRune("_./:@#", r), r >= '0' && r <= '9':
			return true
		case i > 0 && r >= 'A' && r <= 'Z':
			return true
		}
	}
	return false
}

func quoteList(xs []string) string {
	q := make([]string, len(xs))
	for i, x := range xs {
		q[i] = fmt.Sprintf("%q", x)
	}
	return strings.Join(q, ", ")
}

func jsonKeys(keys []string) string {
	q := make([]string, len(keys))
	for i, k := range keys {
		q[i] = fmt.Sprintf("%q: \"...\"", k)
	}
	return strings.Join(q, ", ")
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
