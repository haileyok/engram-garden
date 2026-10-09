package eval

import (
	"fmt"
	"math"
	"runtime"
	"sort"
	"sync"

	"github.com/haileyok/engram-garden/internal/text"
)

// Config is one setting of everything hybrid ranking can tune.
type Config struct {
	Weights text.Weights `json:"weights"`
	BM25    text.BM25    `json:"bm25"`
	Fusion  Fusion       `json:"fusion"`
}

// Name describes a config compactly.
func (c Config) Name() string {
	f := "rrf"
	if c.Fusion.Kind == "convex" {
		f = fmt.Sprintf("a%.2f", c.Fusion.Alpha)
	}
	return fmt.Sprintf("hybrid[stem %.2f parts %.2f partStem %.2f k1 %.2f b %.2f %s]",
		c.Weights.Stem, c.Weights.Parts, c.Weights.PartStem, c.BM25.K1, c.BM25.B, f)
}

// System ranks with this config.
func (c Config) System(name string) System {
	if name == "" {
		name = c.Name()
	}
	return System{name, func(e *Experiment, qi int) []Hit {
		return e.Ix.With(c.Weights, c.BM25).Hybrid(e.QVecs[qi], &e.parsed[qi], e.Cand, c.Fusion)
	}}
}

// Grid is the set of configs a tuning run tries.
type Grid struct {
	Stem, Parts, PartStem, K1, B, Alpha []float64
	// RRF adds rank fusion for each keyword setting.
	RRF bool
}

// DefaultGrid spans the design's starting values.
var DefaultGrid = Grid{
	Stem:     []float64{0.5, 0.7, 0.85, 1.0},
	Parts:    []float64{0.3, 0.4, 0.6},
	PartStem: []float64{0.5, 0.8},
	K1:       []float64{0.9, 1.2, 1.6},
	B:        []float64{0.4, 0.75},
	Alpha:    []float64{0.4, 0.5, 0.6},
	RRF:      true,
}

// Configs expands the grid.
func (g Grid) Configs() []Config {
	var out []Config
	for _, st := range g.Stem {
		for _, pa := range g.Parts {
			for _, ps := range g.PartStem {
				for _, k1 := range g.K1 {
					for _, b := range g.B {
						w := text.Weights{Exact: 1, Stem: st, Parts: pa, PartStem: ps}
						p := text.BM25{K1: k1, B: b}
						for _, a := range g.Alpha {
							out = append(out, Config{w, p, Fusion{Kind: "convex", Alpha: a}})
						}
						if g.RRF {
							out = append(out, Config{w, p, Fusion{Kind: "rrf", K: 60}})
						}
					}
				}
			}
		}
	}
	return out
}

// Scored is a config's tuning-set result.
type Scored struct {
	Config Config
	// Macro is mean nDCG@10 averaged over categories, so no category's
	// size decides the choice.
	Macro float64
	// PerCategory is mean nDCG@10 per category.
	PerCategory map[string]float64
}

// TuningQueries are the answerable queries in the tuning split.
func (e *Experiment) TuningQueries(grades []Grades) []int {
	var idx []int
	for qi, q := range e.Queries {
		if Tuning(q) && q.Category != CatNoAnswer && Answerable(grades[qi]) {
			idx = append(idx, qi)
		}
	}
	return idx
}

// Evaluate scores configs on the tuning split, best macro average first.
func (e *Experiment) Evaluate(configs []Config, grades []Grades) []Scored {
	idx := e.TuningQueries(grades)
	out := make([]Scored, len(configs))
	var wg sync.WaitGroup
	work := make(chan int)
	for range runtime.GOMAXPROCS(0) {
		wg.Go(func() {
			for ci := range work {
				s := configs[ci].System("")
				byCat := map[string][]float64{}
				for _, qi := range idx {
					cat := e.Queries[qi].Category
					byCat[cat] = append(byCat[cat], NDCGAt(Docs(s.Rank(e, qi), 10), grades[qi], 10))
				}
				sc := Scored{Config: configs[ci], PerCategory: map[string]float64{}}
				var sum float64
				for cat, v := range byCat {
					m, _ := Mean(v)
					sc.PerCategory[cat] = m
					sum += m
				}
				if len(byCat) > 0 {
					sc.Macro = sum / float64(len(byCat))
				} else {
					sc.Macro = math.NaN()
				}
				out[ci] = sc
			}
		})
	}
	for ci := range configs {
		work <- ci
	}
	close(work)
	wg.Wait()
	sort.SliceStable(out, func(a, b int) bool { return out[a].Macro > out[b].Macro })
	return out
}
