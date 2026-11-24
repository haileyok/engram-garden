package eval

import (
	"fmt"
	"unicode"
)

// ChunkParams says how a long memory is cut into windows for embedding. It
// is the rule Diderot uses for its events: a body of at most Threshold runes
// stays whole, a longer one becomes windows of about Target runes that
// overlap by Overlap, each cut at whitespace when there's some within
// Lookback runes of the end.
type ChunkParams struct {
	Threshold int
	Target    int
	Overlap   int
	Lookback  int
}

// Name identifies the parameters in reports.
func (p ChunkParams) Name() string {
	return fmt.Sprintf("c%d/%d", p.Target, p.Overlap)
}

// Chunk splits text into windows. A text within Threshold is one chunk.
func Chunk(s string, p ChunkParams) []string {
	if s == "" {
		return nil
	}
	if p.Lookback == 0 {
		p.Lookback = 64
	}
	runes := []rune(s)
	if len(runes) <= p.Threshold {
		return []string{s}
	}
	var out []string
	start := 0
	for start < len(runes) {
		end := start + p.Target
		if end >= len(runes) {
			out = append(out, string(runes[start:]))
			break
		}
		end = boundary(runes, start, end, p.Lookback)
		out = append(out, string(runes[start:end]))
		next := end - p.Overlap
		if next <= start {
			next = start + 1
		}
		start = next
	}
	return out
}

// boundary moves end back to just after whitespace within lookback runes,
// never closer to start than half the window.
func boundary(r []rune, start, end, lookback int) int {
	lo := max(start+(end-start)/2, end-lookback)
	for i := end; i > lo; i-- {
		if unicode.IsSpace(r[i-1]) {
			return i
		}
	}
	return end
}
