package eval

import (
	"math"
	"math/rand/v2"
	"sort"
)

// Grades maps a memory (by index) to its judged relevance: 0 not relevant,
// 1 partly, 2 what the searcher wants. Unjudged memories count as 0.
type Grades map[int]int

// RecallAt is the share of relevant (grade ≥ 1) memories in the top k.
// It's NaN when nothing is relevant.
func RecallAt(ranked []int, g Grades, k int) float64 {
	rel := 0
	for _, v := range g {
		if v > 0 {
			rel++
		}
	}
	if rel == 0 {
		return math.NaN()
	}
	found := 0
	for _, d := range ranked[:min(k, len(ranked))] {
		if g[d] > 0 {
			found++
		}
	}
	return float64(found) / float64(rel)
}

// MRRAt is 1/rank of the first relevant memory in the top k, else 0. It's
// NaN when nothing is relevant.
func MRRAt(ranked []int, g Grades, k int) float64 {
	any := false
	for _, v := range g {
		if v > 0 {
			any = true
			break
		}
	}
	if !any {
		return math.NaN()
	}
	for i, d := range ranked[:min(k, len(ranked))] {
		if g[d] > 0 {
			return 1 / float64(i+1)
		}
	}
	return 0
}

// NDCGAt is normalized discounted cumulative gain at k, with gain 2^g − 1.
// It's NaN when nothing is relevant.
func NDCGAt(ranked []int, g Grades, k int) float64 {
	var ideal []int
	for _, v := range g {
		if v > 0 {
			ideal = append(ideal, v)
		}
	}
	if len(ideal) == 0 {
		return math.NaN()
	}
	sort.Sort(sort.Reverse(sort.IntSlice(ideal)))
	dcg := func(grades []int) float64 {
		var s float64
		for i, v := range grades[:min(k, len(grades))] {
			s += (math.Pow(2, float64(v)) - 1) / math.Log2(float64(i+2))
		}
		return s
	}
	got := make([]int, 0, k)
	for _, d := range ranked[:min(k, len(ranked))] {
		got = append(got, g[d])
	}
	return dcg(got) / dcg(ideal)
}

// Mean averages the values that aren't NaN, and counts them.
func Mean(xs []float64) (float64, int) {
	var s float64
	n := 0
	for _, x := range xs {
		if !math.IsNaN(x) {
			s += x
			n++
		}
	}
	if n == 0 {
		return math.NaN(), 0
	}
	return s / float64(n), n
}

// BootstrapDiff estimates mean(a − b) over paired values (pairs with a NaN
// are skipped) and its 95% confidence interval from resampling.
func BootstrapDiff(a, b []float64, iters int, seed uint64) (mean, lo, hi float64, n int) {
	var d []float64
	for i := range a {
		if !math.IsNaN(a[i]) && !math.IsNaN(b[i]) {
			d = append(d, a[i]-b[i])
		}
	}
	if len(d) == 0 {
		return math.NaN(), math.NaN(), math.NaN(), 0
	}
	mean, _ = Mean(d)
	rng := rand.New(rand.NewPCG(seed, 0x9e3779b97f4a7c15))
	means := make([]float64, iters)
	for it := range means {
		var s float64
		for range d {
			s += d[rng.IntN(len(d))]
		}
		means[it] = s / float64(len(d))
	}
	sort.Float64s(means)
	return mean, means[int(0.025*float64(iters))], means[int(0.975*float64(iters))-1], len(d)
}

// CohensKappa measures agreement between two graders beyond chance.
func CohensKappa(a, b []int) float64 {
	if len(a) == 0 || len(a) != len(b) {
		return math.NaN()
	}
	cats := map[int]bool{}
	for i := range a {
		cats[a[i]], cats[b[i]] = true, true
	}
	n := float64(len(a))
	var agree float64
	pa, pb := map[int]float64{}, map[int]float64{}
	for i := range a {
		if a[i] == b[i] {
			agree++
		}
		pa[a[i]]++
		pb[b[i]]++
	}
	po := agree / n
	var pe float64
	for c := range cats {
		pe += (pa[c] / n) * (pb[c] / n)
	}
	if pe == 1 {
		return 1
	}
	return (po - pe) / (1 - pe)
}
