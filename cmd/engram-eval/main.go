// Command engram-eval measures keyword, vector and hybrid search quality on
// a corpus of memories, on a developer's machine. See "Evaluation" in
// docs/design/keyword-search.md.
//
// A working directory (-dir, outside the repository) holds everything:
//
//	corpus.jsonl     the memories, one JSON object per line
//	model.json       how they're embedded
//	queries.jsonl    generated (and logged) queries
//	judgments.jsonl  relevance grades
//	report.md        the latest report
//
// Steps: fetch (from a space) or import (a JSONL file), then gen, judge and
// report, or run for all three. sample and agreement check the model's
// grades against hand grades.
//
// Queries and grades come from an OpenAI-compatible chat model, configured
// with ENGRAM_EVAL_LLM_URL, ENGRAM_EVAL_LLM_MODEL and optionally
// ENGRAM_EVAL_LLM_KEY and ENGRAM_EVAL_LLM_HEADERS ("Name: value; …").
package main

import (
	"bufio"
	"context"
	"encoding/csv"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/haileyok/engram-garden/internal/agent"
	"github.com/haileyok/engram-garden/internal/eval"
	"github.com/haileyok/engram-garden/internal/oauthfile"
)

const usage = `usage: engram-eval <command> -dir DIR [flags]

commands:
  fetch      read a space's memories through the engram CLI's sign-in (-space)
  import     copy a JSONL corpus (-in) and set its model (-model, -dims, ...)
  gen        generate queries with the chat model (-n memories, -noanswer rounds, -log file)
  judge      grade the pooled results of every system
  report     tune fusion on the tuning split and report the held-out split
  run        gen, judge and report
  sample     write N model-graded pairs to grade by hand (handgrades.csv)
  agreement  compare hand grades with the model's (Cohen's kappa) and keep them
`

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})))
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1], os.Args[2:]); err != nil {
		fmt.Fprintln(os.Stderr, "engram-eval:", err)
		os.Exit(1)
	}
}

type opts struct {
	dir                      string
	space, in                string
	model, url, docPfx, qPfx string
	dims                     int
	n, noAnswer, depth, conc int
	log                      string
	seed                     uint64
}

func run(ctx context.Context, cmd string, args []string) error {
	var o opts
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	fs.StringVar(&o.dir, "dir", "", "working directory (required; keep it outside the repository)")
	fs.StringVar(&o.space, "space", "", "fetch: the space, by name or URI")
	fs.StringVar(&o.in, "in", "", "import: a JSONL file of {id, text, tags, source, createdAt}")
	fs.StringVar(&o.model, "model", "", "import: embedding model name")
	fs.IntVar(&o.dims, "dims", 0, "import: embedding dimensions")
	fs.StringVar(&o.url, "embed-url", "http://localhost:11434/v1", "OpenAI-compatible embeddings base URL")
	fs.StringVar(&o.docPfx, "doc-prefix", "", "import: document prefix")
	fs.StringVar(&o.qPfx, "query-prefix", "", "import: query prefix")
	fs.IntVar(&o.n, "n", 150, "gen: memories to write queries for; sample: pairs to hand-grade")
	fs.IntVar(&o.noAnswer, "noanswer", 3, "gen: rounds of 10 no-answer queries")
	fs.StringVar(&o.log, "log", "", "gen: an engram-mcp query log (ENGRAM_QUERY_LOG) to add as logged queries")
	fs.IntVar(&o.depth, "depth", 20, "judge: how deep to pool each system's results")
	fs.IntVar(&o.conc, "concurrency", 8, "chat model calls at once")
	fs.Uint64Var(&o.seed, "seed", 1, "sampling seed")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if o.dir == "" {
		return errors.New("-dir is required")
	}
	switch cmd {
	case "fetch":
		return fetch(ctx, o)
	case "import":
		return importCorpus(o)
	case "gen":
		return gen(ctx, o)
	case "judge":
		return judge(ctx, o)
	case "report":
		return report(ctx, o)
	case "run":
		for _, f := range []func(context.Context, opts) error{gen, judge, report} {
			if err := f(ctx, o); err != nil {
				return err
			}
		}
		return nil
	case "sample":
		return sample(o)
	case "agreement":
		return agreement(o)
	}
	return fmt.Errorf("unknown command %q\n%s", cmd, usage)
}

func path(o opts, name string) string { return filepath.Join(o.dir, name) }

// ---- corpus ----

func fetch(ctx context.Context, o opts) error {
	if o.space == "" {
		return errors.New("-space is required")
	}
	dir, err := agent.ConfigDir()
	if err != nil {
		return err
	}
	s, err := agent.LoadSettings(filepath.Join(dir, "config.json"), os.Getenv)
	if err != nil {
		return err
	}
	store := &oauthfile.FileStore{Dir: filepath.Join(dir, "oauth")}
	sp, err := agent.Open(ctx, s, agent.Options{Store: store, LockDir: store.Dir, HTTP: &http.Client{Timeout: time.Minute}})
	if err != nil {
		return agent.Explain(err)
	}
	a, _, err := sp.Agent(o.space)
	if err != nil {
		return err
	}
	cfg, err := a.Config(ctx, true)
	if err != nil {
		return err
	}
	var mems []eval.Memory
	cursor := ""
	for {
		out, err := a.List(ctx, agent.ListIn{Limit: 100, Cursor: cursor})
		if err != nil {
			return err
		}
		for _, m := range out.Memories {
			mems = append(mems, eval.Memory{ID: m.URI, Text: m.Text, Tags: m.Tags, Source: m.Source, CreatedAt: m.CreatedAt})
		}
		if out.Cursor == "" || len(out.Memories) == 0 {
			break
		}
		cursor = out.Cursor
	}
	if err := eval.WriteJSONL(path(o, "corpus.jsonl"), mems); err != nil {
		return err
	}
	m := eval.Model{URL: o.url, Name: cfg.Model, Dims: cfg.Dims, DocumentPrefix: cfg.DocumentPrefix, QueryPrefix: cfg.QueryPrefix}
	fmt.Printf("fetched %d memories; model %s (%d dimensions)\n", len(mems), m.Name, m.Dims)
	return eval.WriteJSON(path(o, "model.json"), m)
}

func importCorpus(o opts) error {
	if o.in == "" || o.model == "" || o.dims == 0 {
		return errors.New("-in, -model and -dims are required")
	}
	mems, err := eval.ReadJSONL[eval.Memory](o.in)
	if err != nil {
		return err
	}
	for i, m := range mems {
		if m.ID == "" || strings.TrimSpace(m.Text) == "" {
			return fmt.Errorf("%s: line %d needs an id and text", o.in, i+1)
		}
	}
	if err := eval.WriteJSONL(path(o, "corpus.jsonl"), mems); err != nil {
		return err
	}
	fmt.Printf("imported %d memories\n", len(mems))
	return eval.WriteJSON(path(o, "model.json"), eval.Model{URL: o.url, Name: o.model, Dims: o.dims, DocumentPrefix: o.docPfx, QueryPrefix: o.qPfx})
}

// load reads the corpus and builds the index with document vectors.
func load(ctx context.Context, o opts) (*eval.Index, *eval.Embeddings, error) {
	mems, err := eval.ReadJSONL[eval.Memory](path(o, "corpus.jsonl"))
	if err != nil {
		return nil, nil, err
	}
	if len(mems) == 0 {
		return nil, nil, errors.New("no corpus: run fetch or import first")
	}
	var m eval.Model
	if _, err := eval.ReadJSON(path(o, "model.json"), &m); err != nil {
		return nil, nil, err
	}
	emb, err := eval.OpenEmbeddings(m, path(o, "embeddings.gob"))
	if err != nil {
		return nil, nil, err
	}
	vecs, err := emb.Documents(ctx, mems)
	if err != nil {
		return nil, nil, err
	}
	return eval.NewIndex(mems, vecs), emb, nil
}

func llm(o opts) (*eval.LLM, error) {
	return eval.LLMFromEnv(os.Getenv, path(o, "llm-cache"), o.conc)
}

// ---- queries ----

func gen(ctx context.Context, o opts) error {
	ix, _, err := load(ctx, o)
	if err != nil {
		return err
	}
	l, err := llm(o)
	if err != nil {
		return err
	}
	have, err := eval.ReadJSONL[eval.Query](path(o, "queries.jsonl"))
	if err != nil {
		return err
	}
	known, err := eval.Generate(ctx, l, ix, o.n, o.seed)
	if err != nil {
		return err
	}
	none, err := eval.GenerateNoAnswer(ctx, l, ix, o.noAnswer, o.seed)
	if err != nil {
		return err
	}
	logged, err := readQueryLog(o.log)
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	var all []eval.Query
	for _, q := range slices.Concat(have, known, none, logged) {
		if !seen[q.ID] {
			seen[q.ID] = true
			all = append(all, q)
		}
	}
	counts := map[string]int{}
	for _, q := range all {
		counts[q.Category]++
	}
	fmt.Printf("%d queries: %v\n", len(all), counts)
	return eval.WriteJSONL(path(o, "queries.jsonl"), all)
}

// readQueryLog reads engram-mcp's opt-in query log.
func readQueryLog(p string) ([]eval.Query, error) {
	if p == "" {
		return nil, nil
	}
	type entry struct {
		Query string `json:"query"`
	}
	es, err := eval.ReadJSONL[entry](p)
	if err != nil {
		return nil, err
	}
	var out []eval.Query
	for _, e := range es {
		if q := strings.TrimSpace(e.Query); q != "" {
			out = append(out, eval.Query{ID: eval.QueryID(eval.CatLogged, q, ""), Category: eval.CatLogged, Text: q})
		}
	}
	return out, nil
}

// ---- judging and reporting ----

var alphas = []float64{0.2, 0.3, 0.4, 0.5, 0.6, 0.7, 0.8}

func systems() []eval.System {
	s := []eval.System{eval.VectorSystem, eval.KeywordSystem, eval.HybridSystem(eval.Fusion{Kind: "rrf", K: 60})}
	for _, a := range alphas {
		s = append(s, eval.HybridSystem(eval.Fusion{Kind: "convex", Alpha: a}))
	}
	return s
}

func experiment(ctx context.Context, o opts) (*eval.Experiment, []eval.Judgment, error) {
	ix, emb, err := load(ctx, o)
	if err != nil {
		return nil, nil, err
	}
	qs, err := eval.ReadJSONL[eval.Query](path(o, "queries.jsonl"))
	if err != nil {
		return nil, nil, err
	}
	if len(qs) == 0 {
		return nil, nil, errors.New("no queries: run gen first")
	}
	qv, err := emb.Queries(ctx, qs)
	if err != nil {
		return nil, nil, err
	}
	js, err := eval.ReadJSONL[eval.Judgment](path(o, "judgments.jsonl"))
	return eval.NewExperiment(ix, qs, qv), js, err
}

func judge(ctx context.Context, o opts) error {
	e, js, err := experiment(ctx, o)
	if err != nil {
		return err
	}
	l, err := llm(o)
	if err != nil {
		return err
	}
	r := e.Run(systems(), o.depth)
	pool := e.Pool(r, o.depth)
	grades := e.Grades(js)
	var mu sync.Mutex
	var wg sync.WaitGroup
	var firstErr error
	done := 0
	for qi, q := range e.Queries {
		missing := eval.Judged(pool[qi], grades[qi])
		if len(missing) == 0 {
			continue
		}
		wg.Go(func() {
			mems := make([]eval.Memory, len(missing))
			for i, d := range missing {
				mems[i] = e.Ix.Mems[d]
			}
			g, err := eval.Judge(ctx, l, q.Text, mems)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if firstErr == nil {
					firstErr = fmt.Errorf("query %s: %w", q.ID, err)
				}
				return
			}
			for i, m := range mems {
				js = append(js, eval.Judgment{Query: q.ID, Memory: m.ID, Grade: g[i], By: "model"})
			}
			if done++; done%50 == 0 {
				fmt.Fprintf(os.Stderr, "judged %d queries\n", done)
			}
		})
	}
	wg.Wait()
	if err := eval.WriteJSONL(path(o, "judgments.jsonl"), js); err != nil {
		return err
	}
	fmt.Printf("%d judgments\n", len(js))
	return firstErr
}

func report(ctx context.Context, o opts) error {
	e, js, err := experiment(ctx, o)
	if err != nil {
		return err
	}
	grades := e.Grades(js)
	tune := e.Tune(grades, alphas)
	all := systems()
	r := e.Run(all, 10)
	names := []string{"vector", "keyword", "hybrid-rrf"}
	if tune.Name != "hybrid-rrf" {
		names = append(names, tune.Name)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# engram-eval report\n\n%d memories, %d queries, %d judgments.\n\n", len(e.Ix.Mems), len(e.Queries), len(js))
	answerableNoAnswer := 0
	for qi, q := range e.Queries {
		if q.Category == eval.CatNoAnswer && eval.Answered(grades[qi]) {
			answerableNoAnswer++
		}
	}
	b.WriteString(e.Report(r, grades, names, "vector", tune))
	v, u, n := e.CandidateRecall()
	fmt.Fprintf(&b, "## Candidate recall\n\nHeld-out known-item queries (%d): the target is among the 200 vector candidates %.1f%% of the time, and among the hybrid union %.1f%%.\n\n", n, 100*v, 100*u)
	vfp, hfp, nn := e.NoAnswerFalsePositives(grades, tune.Fusion)
	fmt.Fprintf(&b, "## No-answer queries\n\n%d held-out queries with no relevant memory (%d generated as no-answer turned out answerable and count as ordinary queries). Top result looks strong: vector %.1f%%, hybrid (%s) %.1f%%.\n", nn, answerableNoAnswer, 100*vfp, tune.Name, 100*hfp)
	if err := os.WriteFile(path(o, "report.md"), []byte(b.String()), 0o600); err != nil {
		return err
	}
	fmt.Print(b.String())
	return nil
}

// ---- hand grades ----

func sample(o opts) error {
	ctx := context.Background()
	e, js, err := experiment(ctx, o)
	if err != nil {
		return err
	}
	byID := map[string]eval.Query{}
	for _, q := range e.Queries {
		byID[q.ID] = q
	}
	mems := map[string]eval.Memory{}
	for _, m := range e.Ix.Mems {
		mems[m.ID] = m
	}
	var model []eval.Judgment
	for _, j := range js {
		if j.By == "model" {
			model = append(model, j)
		}
	}
	rng := rand.New(rand.NewPCG(o.seed, 3))
	rng.Shuffle(len(model), func(i, j int) { model[i], model[j] = model[j], model[i] })
	model = model[:min(o.n, len(model))]
	f, err := os.Create(path(o, "handgrades.csv"))
	if err != nil {
		return err
	}
	w := csv.NewWriter(f)
	_ = w.Write([]string{"query_id", "memory_id", "query", "memory", "grade (0 not useful, 1 partly, 2 what they want)"})
	for _, j := range model {
		_ = w.Write([]string{j.Query, j.Memory, byID[j.Query].Text, mems[j.Memory].Text, ""})
	}
	w.Flush()
	if err := f.Close(); err != nil {
		return err
	}
	fmt.Printf("wrote %d pairs to %s; fill in the last column, then run agreement\n", len(model), path(o, "handgrades.csv"))
	return w.Error()
}

func agreement(o opts) error {
	f, err := os.Open(path(o, "handgrades.csv"))
	if err != nil {
		return err
	}
	defer f.Close()
	rows, err := csv.NewReader(bufio.NewReader(f)).ReadAll()
	if err != nil {
		return err
	}
	js, err := eval.ReadJSONL[eval.Judgment](path(o, "judgments.jsonl"))
	if err != nil {
		return err
	}
	modelGrade := map[[2]string]int{}
	for _, j := range js {
		if j.By == "model" {
			modelGrade[[2]string{j.Query, j.Memory}] = j.Grade
		}
	}
	var hand, model []int
	for _, row := range rows[min(1, len(rows)):] {
		if len(row) < 5 || strings.TrimSpace(row[4]) == "" {
			continue
		}
		g, err := strconv.Atoi(strings.TrimSpace(row[4]))
		if err != nil || g < 0 || g > 2 {
			return fmt.Errorf("grade %q for %s/%s isn't 0, 1 or 2", row[4], row[0], row[1])
		}
		hand = append(hand, g)
		model = append(model, modelGrade[[2]string{row[0], row[1]}])
		js = append(js, eval.Judgment{Query: row[0], Memory: row[1], Grade: g, By: "human"})
	}
	if len(hand) == 0 {
		return errors.New("no hand grades filled in yet")
	}
	agree := 0
	for i := range hand {
		if hand[i] == model[i] {
			agree++
		}
	}
	fmt.Printf("%d hand grades: exact agreement %.0f%%, Cohen's kappa %.2f\n", len(hand), 100*float64(agree)/float64(len(hand)), eval.CohensKappa(hand, model))
	return eval.WriteJSONL(path(o, "judgments.jsonl"), js)
}
