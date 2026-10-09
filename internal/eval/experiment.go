package eval

import (
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"

	"github.com/haileyok/engram-garden/internal/text"
)

// Experiment ranks a set of queries over an index with several systems.
type Experiment struct {
	Ix      *Index
	Queries []Query
	// QVecs are the queries' normalized vectors (nil entries skip vector
	// search for that query).
	QVecs [][]float32
	// Cand is how many candidates each side keeps (200, as the appview).
	Cand int

	parsed []text.Query
	docOf  map[string]int
}

// NewExperiment prepares queries for ranking.
func NewExperiment(ix *Index, qs []Query, qvecs [][]float32) *Experiment {
	e := &Experiment{Ix: ix, Queries: qs, QVecs: qvecs, Cand: 200, docOf: map[string]int{}}
	for i, m := range ix.Mems {
		e.docOf[m.ID] = i
	}
	for _, q := range qs {
		e.parsed = append(e.parsed, text.ParseQuery(q.Text))
	}
	return e
}

// System is a ranking method.
type System struct {
	Name string
	Rank func(e *Experiment, qi int) []Hit
}

// VectorSystem ranks by vector only, as the appview does today.
var VectorSystem = System{"vector", func(e *Experiment, qi int) []Hit { return e.Ix.Vector(e.QVecs[qi], e.Cand) }}

// KeywordSystem ranks by keyword score only.
var KeywordSystem = System{"keyword", func(e *Experiment, qi int) []Hit { return e.Ix.Keyword(&e.parsed[qi], e.Cand) }}

// HybridSystem fuses both.
func HybridSystem(f Fusion) System {
	name := "hybrid-rrf"
	if f.Kind == "convex" {
		name = fmt.Sprintf("hybrid-convex-%.2f", f.Alpha)
	}
	return System{name, func(e *Experiment, qi int) []Hit {
		return e.Ix.Hybrid(e.QVecs[qi], &e.parsed[qi], e.Cand, f)
	}}
}

// Ranked holds each system's ranking of each query, as memory indexes.
type Ranked map[string][][]int

// Run ranks every query with every system, keeping the top depth.
func (e *Experiment) Run(systems []System, depth int) Ranked {
	out := Ranked{}
	for _, s := range systems {
		rs := make([][]int, len(e.Queries))
		for qi := range e.Queries {
			rs[qi] = Docs(s.Rank(e, qi), depth)
		}
		out[s.Name] = rs
	}
	return out
}

// Pool lists, per query, the memories any system ranked in its top depth,
// which are the ones to grade.
func (e *Experiment) Pool(r Ranked, depth int) [][]int {
	out := make([][]int, len(e.Queries))
	names := make([]string, 0, len(r))
	for name := range r {
		names = append(names, name)
	}
	sort.Strings(names)
	for qi := range e.Queries {
		seen := map[int]bool{}
		for _, name := range names {
			for _, d := range r[name][qi][:min(depth, len(r[name][qi]))] {
				if !seen[d] {
					seen[d] = true
					out[qi] = append(out[qi], d)
				}
			}
		}
	}
	return out
}

// Grades collects judgments per query. A known-item query's target is
// always grade 2; human grades override model grades.
func (e *Experiment) Grades(js []Judgment) []Grades {
	byQuery := map[string]int{}
	for i, q := range e.Queries {
		byQuery[q.ID] = i
	}
	out := make([]Grades, len(e.Queries))
	for i := range out {
		out[i] = Grades{}
	}
	human := map[[2]string]bool{}
	for _, j := range js {
		qi, ok := byQuery[j.Query]
		d, ok2 := e.docOf[j.Memory]
		if !ok || !ok2 {
			continue
		}
		key := [2]string{j.Query, j.Memory}
		if human[key] {
			continue
		}
		if j.By == "human" {
			human[key] = true
		}
		out[qi][d] = j.Grade
	}
	for qi, q := range e.Queries {
		if d, ok := e.docOf[q.Target]; ok {
			out[qi][d] = 2
		}
	}
	return out
}

// Judged reports whether every pooled memory of a query has a grade.
func Judged(pool []int, g Grades) []int {
	var missing []int
	for _, d := range pool {
		if _, ok := g[d]; !ok {
			missing = append(missing, d)
		}
	}
	return missing
}

// Scores are per-query metric values for one system.
type Scores struct {
	NDCG, MRR, Recall []float64
}

// Score computes metrics at k for every query.
func Score(ranked [][]int, grades []Grades, k int) Scores {
	var s Scores
	for qi := range ranked {
		s.NDCG = append(s.NDCG, NDCGAt(ranked[qi], grades[qi], k))
		s.MRR = append(s.MRR, MRRAt(ranked[qi], grades[qi], k))
		s.Recall = append(s.Recall, RecallAt(ranked[qi], grades[qi], k))
	}
	return s
}

func subset(xs []float64, keep []bool) []float64 {
	var out []float64
	for i, x := range xs {
		if keep[i] {
			out = append(out, x)
		} else {
			out = append(out, math.NaN())
		}
	}
	return out
}

// Answerable reports whether a query has any relevant memory.
func Answerable(g Grades) bool {
	for _, v := range g {
		if v > 0 {
			return true
		}
	}
	return false
}

// Answered reports whether a query has a memory that's what the searcher
// wants (grade 2). Graders call loosely related memories "partly useful",
// so a no-answer query only counts as answerable with a grade 2.
func Answered(g Grades) bool {
	for _, v := range g {
		if v == 2 {
			return true
		}
	}
	return false
}

// TuneResult is the outcome of choosing a fusion on the tuning set.
type TuneResult struct {
	Fusion Fusion
	Name   string
	// NDCG is each candidate fusion's mean nDCG@10 on the tuning set.
	NDCG map[string]float64
}

// Tune picks the fusion with the best mean nDCG@10 over answerable tuning
// queries. Ties go to rank fusion, the simpler choice.
func (e *Experiment) Tune(grades []Grades, alphas []float64) TuneResult {
	keep := make([]bool, len(e.Queries))
	var idx []int
	for qi, q := range e.Queries {
		if Tuning(q) && Answerable(grades[qi]) {
			keep[qi] = true
			idx = append(idx, qi)
		}
	}
	res := TuneResult{NDCG: map[string]float64{}}
	best := math.Inf(-1)
	try := func(f Fusion) {
		s := HybridSystem(f)
		var vals []float64
		for _, qi := range idx {
			vals = append(vals, NDCGAt(Docs(s.Rank(e, qi), 10), grades[qi], 10))
		}
		m, _ := Mean(vals)
		res.NDCG[s.Name] = m
		if m > best+1e-9 {
			best, res.Fusion, res.Name = m, f, s.Name
		}
	}
	try(Fusion{Kind: "rrf", K: 60})
	for _, a := range alphas {
		try(Fusion{Kind: "convex", Alpha: a})
	}
	return res
}

// Report writes the held-out results as Markdown.
func (e *Experiment) Report(r Ranked, grades []Grades, systems []string, baseline string, tune TuneResult) string {
	var b strings.Builder
	held := make([]bool, len(e.Queries))
	nHeld := 0
	for qi, q := range e.Queries {
		held[qi] = !Tuning(q)
		if held[qi] {
			nHeld++
		}
	}
	fmt.Fprintf(&b, "## Held-out results\n\n%d queries held out of %d. Metrics at 10; Δ is against %s with a 95%% bootstrap interval.\n\n", nHeld, len(e.Queries), baseline)
	if len(tune.NDCG) > 0 {
		names := make([]string, 0, len(tune.NDCG))
		for n := range tune.NDCG {
			names = append(names, n)
		}
		sort.Strings(names)
		b.WriteString("Fusion chosen on the tuning set (mean nDCG@10): ")
		for i, n := range names {
			if i > 0 {
				b.WriteString(", ")
			}
			fmt.Fprintf(&b, "%s %.3f", n, tune.NDCG[n])
		}
		fmt.Fprintf(&b, ". Chosen: **%s**.\n\n", tune.Name)
	}
	cats := append([]string{"all"}, Categories...)
	base := Score(r[baseline], grades, 10)
	for _, cat := range cats {
		in := make([]bool, len(e.Queries))
		n := 0
		for qi, q := range e.Queries {
			if held[qi] && q.Category != CatNoAnswer && (cat == "all" || q.Category == cat) {
				in[qi] = true
				n++
			}
		}
		if n == 0 || cat == CatNoAnswer {
			continue
		}
		fmt.Fprintf(&b, "### %s (%d queries)\n\n| system | nDCG@10 | Δ nDCG@10 [95%% CI] | MRR@10 | Recall@10 |\n|---|---|---|---|---|\n", cat, n)
		for _, name := range systems {
			s := Score(r[name], grades, 10)
			nd, _ := Mean(subset(s.NDCG, in))
			mrr, _ := Mean(subset(s.MRR, in))
			rec, _ := Mean(subset(s.Recall, in))
			delta := "—"
			if name != baseline {
				d, lo, hi, _ := BootstrapDiff(subset(s.NDCG, in), subset(base.NDCG, in), 2000, 1)
				delta = fmt.Sprintf("%+.3f [%+.3f, %+.3f]", d, lo, hi)
			}
			fmt.Fprintf(&b, "| %s | %.3f | %s | %.3f | %.3f |\n", name, nd, delta, mrr, rec)
		}
		b.WriteString("\n")
	}
	return b.String()
}

// CandidateRecall reports, for held-out known-item queries, how often the
// target is among the vector candidates and among the hybrid union.
func (e *Experiment) CandidateRecall() (vector, union float64, n int) {
	var v, u int
	for qi, q := range e.Queries {
		d, ok := e.docOf[q.Target]
		if !ok || Tuning(q) {
			continue
		}
		n++
		vs := Docs(e.Ix.Vector(e.QVecs[qi], e.Cand), e.Cand)
		ks := Docs(e.Ix.Keyword(&e.parsed[qi], e.Cand), e.Cand)
		inV := slices.Contains(vs, d)
		if inV {
			v++
		}
		if inV || slices.Contains(ks, d) {
			u++
		}
	}
	if n == 0 {
		return math.NaN(), math.NaN(), 0
	}
	return float64(v) / float64(n), float64(u) / float64(n), n
}

// NoAnswerFalsePositives sets thresholds on the tuning set (the similarity
// and keyword score that 90% of relevant top results clear) and reports,
// on held-out queries with no relevant memory, how often the top result
// clears them: the similarity threshold for vector, either for hybrid.
func (e *Experiment) NoAnswerFalsePositives(grades []Grades, hybrid Fusion) (vectorFP, hybridFP float64, n int) {
	var sims, bms []float64
	for qi, q := range e.Queries {
		if !Tuning(q) || q.Category == CatNoAnswer {
			continue
		}
		if h := e.Ix.Vector(e.QVecs[qi], e.Cand); len(h) > 0 && grades[qi][h[0].Doc] > 0 {
			sims = append(sims, h[0].Sim)
		}
		if h := e.Ix.Keyword(&e.parsed[qi], e.Cand); len(h) > 0 && grades[qi][h[0].Doc] > 0 {
			bms = append(bms, h[0].BM25)
		}
	}
	if len(sims) == 0 || len(bms) == 0 {
		return math.NaN(), math.NaN(), 0
	}
	sort.Float64s(sims)
	sort.Float64s(bms)
	simT, bmT := sims[len(sims)/10], bms[len(bms)/10]
	var vfp, hfp int
	for qi, q := range e.Queries {
		if Tuning(q) || q.Category != CatNoAnswer || Answered(grades[qi]) {
			continue
		}
		n++
		if h := e.Ix.Vector(e.QVecs[qi], e.Cand); len(h) > 0 && h[0].Sim >= simT {
			vfp++
		}
		if h := e.Ix.Hybrid(e.QVecs[qi], &e.parsed[qi], e.Cand, hybrid); len(h) > 0 && (h[0].HasSim && h[0].Sim >= simT || h[0].BM25 >= bmT) {
			hfp++
		}
	}
	if n == 0 {
		return math.NaN(), math.NaN(), 0
	}
	return float64(vfp) / float64(n), float64(hfp) / float64(n), n
}
