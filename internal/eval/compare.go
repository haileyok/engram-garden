package eval

import (
	"fmt"
	"strings"
)

// Slice is a subset of queries, for a report table.
type Slice struct {
	Name string
	In   func(q Query, targetRunes int) bool
}

// LengthSlices split known-item queries by the length of the memory they
// were written for.
func LengthSlices() []Slice {
	b := func(name string, lo, hi int) Slice {
		return Slice{name, func(q Query, n int) bool { return q.Target != "" && n > lo && n <= hi }}
	}
	return []Slice{b("target ≤ 1000 chars", 0, 1000), b("target 1000–1500", 1000, 1500), b("target 1500–3000", 1500, 3000), b("target > 3000", 3000, 1<<30)}
}

// CategorySlices are the generated categories.
func CategorySlices() []Slice {
	out := []Slice{{"all (no no-answer)", func(q Query, _ int) bool { return q.Category != CatNoAnswer }}}
	for _, c := range Categories {
		if c == CatNoAnswer || c == CatLogged {
			continue
		}
		out = append(out, Slice{"category " + c, func(q Query, _ int) bool { return q.Category == c }})
	}
	return out
}

// Compare writes, for each slice, every system's mean metrics at 10 and its
// difference from its baseline's nDCG@10 with a 95% bootstrap interval.
// baseline maps a system to the one it is compared with (itself or absent
// means it is a baseline).
func (e *Experiment) Compare(r Ranked, grades []Grades, order []string, baseline map[string]string, slices []Slice) string {
	var b strings.Builder
	scores := map[string]Scores{}
	for name, rs := range r {
		scores[name] = Score(rs, grades, 10)
	}
	for _, sl := range slices {
		in := make([]bool, len(e.Queries))
		n := 0
		for qi, q := range e.Queries {
			if !Answerable(grades[qi]) {
				continue
			}
			target := 0
			if d, ok := e.docOf[q.Target]; ok {
				target = len([]rune(e.Ix.Mems[d].Text))
			}
			if sl.In(q, target) {
				in[qi] = true
				n++
			}
		}
		if n == 0 {
			continue
		}
		fmt.Fprintf(&b, "### %s (%d queries)\n\n| system | nDCG@10 | Δ nDCG@10 [95%% CI] | MRR@10 | Recall@10 |\n|---|---|---|---|---|\n", sl.Name, n)
		for _, name := range order {
			s := scores[name]
			nd, _ := Mean(subset(s.NDCG, in))
			mrr, _ := Mean(subset(s.MRR, in))
			rec, _ := Mean(subset(s.Recall, in))
			delta := "—"
			if base, ok := baseline[name]; ok && base != name {
				d, lo, hi, _ := BootstrapDiff(subset(s.NDCG, in), subset(scores[base].NDCG, in), 2000, 1)
				delta = fmt.Sprintf("%+.3f [%+.3f, %+.3f]", d, lo, hi)
			}
			fmt.Fprintf(&b, "| %s | %.3f | %s | %.3f | %.3f |\n", name, nd, delta, mrr, rec)
		}
		b.WriteString("\n")
	}
	return b.String()
}
