package coord

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Citation states.
const (
	// StateMismatch: the target resolved, but doesn't contain the cited
	// words (or the citation doesn't fit its relation).
	StateMismatch = "mismatch"
	// StateStale: the target changed or was replaced after it was cited.
	StateStale = "stale"
	// StateUnresolvable: the target doesn't exist.
	StateUnresolvable = "unresolvable"
)

// CitationError is a citation that didn't check out.
type CitationError struct {
	Index    int
	Citation Citation
	State    string
	Reason   string
}

func (e *CitationError) Error() string {
	return fmt.Sprintf("citation %d (%s %s): %s: %s", e.Index, e.Citation.Relation, e.Citation.Target.URI, e.State, e.Reason)
}

// UncitedError is text that neither a citation nor a basis entry covers.
type UncitedError struct {
	Text  string
	Spans []Span
}

func (e *UncitedError) Error() string {
	parts := make([]string, len(e.Spans))
	for i, s := range e.Spans {
		parts[i] = fmt.Sprintf("%q", e.Text[s.ByteStart:s.ByteEnd])
	}
	return "uncited text: " + strings.Join(parts, ", ") + "; cite what it rests on, or mark it as your own with a basis entry"
}

// uncovered returns the parts of text no span covers, ignoring whitespace
// and punctuation between spans.
func uncovered(text string, cites []Citation, basis []Basis) []Span {
	covered := make([]bool, len(text))
	mark := func(s Span) {
		for i := s.ByteStart; i < s.ByteEnd; i++ {
			covered[i] = true
		}
	}
	for _, c := range cites {
		mark(c.Span)
	}
	for _, b := range basis {
		mark(b.Span)
	}
	var gaps []Span
	for i := 0; i < len(text); {
		if covered[i] {
			i++
			continue
		}
		j := i
		for j < len(text) && !covered[j] {
			j++
		}
		if s, ok := trimFiller(text, i, j); ok {
			gaps = append(gaps, s)
		}
		i = j
	}
	return gaps
}

// trimFiller trims whitespace and punctuation from both ends of a range,
// reporting whether anything is left.
func trimFiller(text string, start, end int) (Span, bool) {
	filler := func(r rune) bool { return unicode.IsSpace(r) || unicode.IsPunct(r) }
	for start < end {
		r, n := utf8.DecodeRuneInString(text[start:end])
		if !filler(r) {
			break
		}
		start += n
	}
	for end > start {
		r, n := utf8.DecodeLastRuneInString(text[start:end])
		if !filler(r) {
			break
		}
		end -= n
	}
	return Span{ByteStart: start, ByteEnd: end}, start < end
}

// resolveCitations checks each citation and rewrites citations of relayed
// facts to cite their original records.
func (w *Workspace) resolveCitations(ctx context.Context, text string, cites []Citation) ([]Citation, error) {
	out := make([]Citation, len(cites))
	for i, c := range cites {
		fail := func(state, reason string) error {
			return &CitationError{Index: i, Citation: c, State: state, Reason: reason}
		}
		rec, err := w.getForCitation(ctx, c.Target, fail)
		if err != nil {
			return nil, err
		}
		if rec.Collection == MessageCollection && factual(c.Relation) {
			var m Message
			if err := rec.Decode(&m); err != nil {
				return nil, fail(StateMismatch, "the cited message can't be read: "+err.Error())
			}
			through, ok, err := citeThrough(c, rec.Ref(), m)
			if err != nil {
				return nil, fail(StateMismatch, err.Error())
			}
			if ok {
				c = through
				if rec, err = w.getForCitation(ctx, c.Target, fail); err != nil {
					return nil, err
				}
			}
		}
		if reason := checkCitation(c, rec, text[c.Span.ByteStart:c.Span.ByteEnd]); reason != "" {
			return nil, fail(StateMismatch, reason)
		}
		out[i] = c
	}
	return out, nil
}

func (w *Workspace) getForCitation(ctx context.Context, ref StrongRef, fail func(string, string) error) (Record, error) {
	rec, err := w.Get(ctx, ref)
	var stale *StaleError
	switch {
	case errors.As(err, &stale):
		return rec, fail(StateStale, fmt.Sprintf("the record is at %s now; read it again and cite what it says now", stale.Current))
	case errors.Is(err, ErrNotFound):
		return rec, fail(StateUnresolvable, "no such record")
	case err != nil:
		return rec, err
	}
	return rec, nil
}

// factual reports whether a relation takes the target's words as fact.
func factual(rel string) bool {
	return rel == RelQuotes || rel == RelRestates || rel == RelDerivedFrom
}

// citeThrough rewrites a factual citation of a message to cite what that
// message cited for the reused words, listing the message in Via. It
// leaves the citation alone (ok false) when the words are the message's
// own inference, or the message is a question or proposal: citing another
// agent's opinion as opinion is fine.
func citeThrough(c Citation, msgRef StrongRef, m Message) (Citation, bool, error) {
	if m.Kind == "question" || m.Kind == "proposal" {
		return c, false, nil
	}
	if c.Selector == nil || c.Selector.Type != TextQuoteType {
		return c, false, errors.New("cite another agent's message with a text quote, so the reused words can be traced to their source")
	}
	start, ok := findQuote(m.Text, c.Selector)
	if !ok {
		return c, false, errors.New("the quoted words aren't in the cited message")
	}
	end := start + len(c.Selector.Exact)
	within := func(s Span) bool { return s.ByteStart <= start && end <= s.ByteEnd }
	for _, mc := range m.Citations {
		if !within(mc.Span) {
			continue
		}
		if !factual(mc.Relation) {
			return c, false, fmt.Errorf("the cited message %s those words; cite %s directly with that relation instead", mc.Relation, mc.Target.URI)
		}
		rel := weaker(c.Relation, mc.Relation)
		sel := mc.Selector
		if rel == RelQuotes {
			// Both quote, so the reused words are in the original too.
			sel = TextQuote(c.Selector.Exact)
		}
		via := append([]StrongRef{msgRef}, mc.Via...)
		return Citation{Span: c.Span, Relation: rel, Target: mc.Target, Selector: sel, Via: via}, true, nil
	}
	for _, b := range m.Basis {
		if within(b.Span) {
			return c, false, nil
		}
	}
	return c, false, errors.New("the quoted words span several of the message's citations, or none; cite each part separately")
}

// weaker is the relation of a fact reused through a relay: a quote of a
// restatement is a restatement, and anything computed stays computed.
func weaker(a, b string) string {
	switch {
	case a == RelDerivedFrom || b == RelDerivedFrom:
		return RelDerivedFrom
	case a == RelRestates || b == RelRestates:
		return RelRestates
	}
	return RelQuotes
}

// checkCitation checks a citation against the record it cites, returning
// why it doesn't hold, or "".
func checkCitation(c Citation, rec Record, spanText string) string {
	var v struct {
		Text        string `json:"text"`
		Title       string `json:"title"`
		Description string `json:"description"`
		Content     any    `json:"content"`
	}
	if err := rec.Decode(&v); err != nil {
		return "the cited record can't be read: " + err.Error()
	}
	text := v.Text
	if text == "" {
		text = strings.TrimSpace(v.Title + "\n\n" + v.Description)
	}
	switch c.Relation {
	case RelCorrects:
		if rec.Collection != MessageCollection {
			return "corrects must cite the message being corrected"
		}
	case RelQuotes:
		if c.Selector == nil || c.Selector.Type != TextQuoteType {
			return "a quote needs a text quote selector"
		}
		if spanText != c.Selector.Exact {
			return fmt.Sprintf("the span %q doesn't reproduce the quoted words %q", spanText, c.Selector.Exact)
		}
	case RelRestates, RelContradicts:
		if c.Selector == nil {
			return c.Relation + " needs a selector saying which words it rests on"
		}
	}
	if c.Selector == nil {
		return ""
	}
	s := c.Selector
	switch s.Type {
	case TextQuoteType:
		if _, ok := findQuote(text, s); !ok {
			return fmt.Sprintf("%q isn't in the cited record", s.Exact)
		}
	case ByteRangeType:
		if s.ByteStart == nil || s.ByteEnd == nil || *s.ByteStart < 0 || *s.ByteStart >= *s.ByteEnd {
			return "a byte range needs 0 <= byteStart < byteEnd"
		}
		// Ranges beyond the text point into the content blob, which isn't
		// checked here.
		if *s.ByteEnd > len(text) && v.Content == nil {
			return "the byte range is past the end of the cited record"
		}
	case JSONPointerType:
		if s.Pointer != "" && !strings.HasPrefix(s.Pointer, "/") {
			return "a JSON pointer starts with /"
		}
	default:
		return "unknown selector type " + s.Type
	}
	return ""
}

// findQuote finds the quote's exact words in text, using prefix and suffix
// to pick among repeats. It returns the first match's byte offset.
func findQuote(text string, s *Selector) (int, bool) {
	if s.Exact == "" {
		return 0, false
	}
	for off := 0; off <= len(text); {
		i := strings.Index(text[off:], s.Exact)
		if i < 0 {
			return 0, false
		}
		i += off
		if strings.HasSuffix(text[:i], s.Prefix) && strings.HasPrefix(text[i+len(s.Exact):], s.Suffix) {
			return i, true
		}
		off = i + 1
	}
	return 0, false
}
