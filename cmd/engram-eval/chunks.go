package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/haileyok/engram-garden/internal/eval"
)

// chunkVariant is one way to embed memories in windows.
type chunkVariant struct {
	name    string
	params  eval.ChunkParams
	whole   bool
	penalty float64
}

// chunkVariants are the layouts the chunking experiment compares with
// embedding every memory whole: Diderot's own windows (cut above 1500
// characters, 1000 long, overlapping 200), shorter windows, and each with
// the whole-text vector kept too.
var chunkVariants = []chunkVariant{
	{"c1000", eval.ChunkParams{Threshold: 1500, Target: 1000, Overlap: 200}, false, 0},
	{"c1000+whole", eval.ChunkParams{Threshold: 1500, Target: 1000, Overlap: 200}, true, 0},
	{"c500", eval.ChunkParams{Threshold: 800, Target: 500, Overlap: 100}, false, 0},
	{"c500+whole", eval.ChunkParams{Threshold: 800, Target: 500, Overlap: 100}, true, 0},
	{"c1000 pen.02", eval.ChunkParams{Threshold: 1500, Target: 1000, Overlap: 200}, false, 0.02},
	{"c1000 pen.04", eval.ChunkParams{Threshold: 1500, Target: 1000, Overlap: 200}, false, 0.04},
	{"c500 pen.02", eval.ChunkParams{Threshold: 800, Target: 500, Overlap: 100}, false, 0.02},
	{"c500 pen.04", eval.ChunkParams{Threshold: 800, Target: 500, Overlap: 100}, false, 0.04},
}

// chunks embeds memories in windows, ranks with each layout next to the
// whole-memory baseline, grades what the layouts surface that nobody has
// graded, and reports.
func chunks(ctx context.Context, o opts) error {
	e, js, err := experiment(ctx, o)
	if err != nil {
		return err
	}
	_, emb, err := load(ctx, o)
	if err != nil {
		return err
	}
	fusions := []eval.Fusion{{Kind: "convex", Alpha: 0.5}}
	all := []eval.System{eval.VectorSystem}
	baseline := map[string]string{}
	order := []string{"vector"}
	hybridNames := []string{}
	for _, f := range fusions {
		s := eval.HybridSystem(f)
		all = append(all, s)
		hybridNames = append(hybridNames, s.Name)
	}
	for _, v := range chunkVariants {
		rows, err := emb.Chunked(ctx, e.Ix.Mems, v.params, v.whole)
		if err != nil {
			return err
		}
		total := 0
		for _, r := range rows {
			total += len(r)
		}
		fmt.Fprintf(os.Stderr, "%s: %d vectors for %d memories\n", v.name, total, len(rows))
		for _, s := range eval.VariantSystems(v.name, e.Ix.WithRows(rows).WithChunkPenalty(v.penalty), fusions) {
			all = append(all, s)
			if strings.HasSuffix(s.Name, " vector") {
				baseline[s.Name] = "vector"
			} else {
				baseline[s.Name] = hybridNames[indexOfHybrid(s.Name, hybridNames)]
			}
		}
	}
	js, err = judgeSystems(ctx, o, e, js, all)
	if err != nil {
		return err
	}
	grades := e.Grades(js)
	r := e.Run(all, 10)
	order = []string{"vector"}
	for _, v := range chunkVariants {
		order = append(order, v.name+" vector")
	}
	for _, h := range hybridNames {
		order = append(order, h)
		for _, v := range chunkVariants {
			order = append(order, v.name+" "+h)
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# chunking experiment\n\n%d memories, %d queries, %d judgments. Judge reads %d runes of each memory.\n\nBaseline: one vector per memory (what Engram does). Variants cut memories into windows (name: window size/overlap), ranked by their best window; \"+whole\" keeps the whole-text vector as well. The keyword side is the same everywhere.\n\n",
		len(e.Ix.Mems), len(e.Queries), len(js), eval.JudgeMaxRunes)
	b.WriteString("## By length of the memory the query was written for\n\n")
	b.WriteString(e.Compare(r, grades, order, baseline, eval.LengthSlices()))
	b.WriteString("## By category\n\n")
	b.WriteString(e.Compare(r, grades, order, baseline, eval.CategorySlices()))
	if err := os.WriteFile(path(o, "chunks-report.md"), []byte(b.String()), 0o600); err != nil {
		return err
	}
	fmt.Print(b.String())
	return nil
}

func indexOfHybrid(name string, hybrids []string) int {
	for i, h := range hybrids {
		if strings.HasSuffix(name, " "+h) {
			return i
		}
	}
	return 0
}
