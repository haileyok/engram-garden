package text

// Query limits. Tokens are taken in query order until a limit would be
// exceeded; the rest are dropped and Query.Truncated is set.
const (
	MaxQueryTokens = 32
	MaxQueryTerms  = 128
	// MaxQueryParts is how many parts of one compound a query uses.
	MaxQueryParts = 16
)

// Query is an analyzed query: one group per distinct query token, over a
// shared list of distinct terms.
type Query struct {
	Groups []Group
	// Terms lists every distinct term the groups refer to. Scoring takes a
	// slice of per-term values in this order.
	Terms []string
	// Truncated is set when tokens were dropped to stay within the limits.
	Truncated bool
}

// Group is one query token and its alternatives. Its contribution to a
// score is the best alternative, not their sum, so one occurrence can't
// score twice:
//
//	word:     max(Exact·s(w), Stem·s(~w))
//	compound: max(Exact·s(x), Parts·Σ_p max(s(p), PartStem·s(~p)))
//	opaque, CJK: s(t)
type Group struct {
	Kind Kind
	// Text is the token's normalized form, for explanations.
	Text string
	// Exact, Stem and the parts' fields index Query.Terms; -1 means none.
	Exact int
	Stem  int
	Parts []PartTerms
}

// PartTerms are a compound part's term and stem, as indexes into
// Query.Terms (Stem is -1 if the part isn't stemmed).
type PartTerms struct {
	Term, Stem int
}

// ParseQuery analyzes a query. Tokens are compared after normalization, so
// "Space" and "space" are one token, and a compound's parts are distinct
// (space_space has one part).
func ParseQuery(q string) Query {
	var out Query
	index := map[string]int{}
	seen := map[string]bool{}
	newTerms := func(terms []string) int {
		n := 0
		for i, t := range terms {
			if _, ok := index[t]; ok {
				continue
			}
			dup := false
			for _, u := range terms[:i] {
				if u == t {
					dup = true
					break
				}
			}
			if !dup {
				n++
			}
		}
		return n
	}
	term := func(t string) int {
		if t == "" {
			return -1
		}
		if i, ok := index[t]; ok {
			return i
		}
		index[t] = len(out.Terms)
		out.Terms = append(out.Terms, t)
		return len(out.Terms) - 1
	}
	var terms []string
	Tokenize(q, FieldText, 0, func(t *Token) {
		if out.Truncated || seen[t.Exact] {
			return
		}
		// Distinct parts, at most MaxQueryParts.
		var parts []Part
		for _, p := range t.Parts {
			dup := false
			for _, have := range parts {
				if have.Term == p.Term {
					dup = true
					break
				}
			}
			if !dup && len(parts) < MaxQueryParts {
				parts = append(parts, p)
			}
		}
		terms = append(terms[:0], t.Exact)
		if t.Stem != "" {
			terms = append(terms, t.Stem)
		}
		for _, p := range parts {
			terms = append(terms, p.Term)
			if p.Stem != "" {
				terms = append(terms, p.Stem)
			}
		}
		if len(out.Groups) >= MaxQueryTokens || len(out.Terms)+newTerms(terms) > MaxQueryTerms {
			out.Truncated = true
			return
		}
		seen[t.Exact] = true
		g := Group{Kind: t.Kind, Text: t.Exact, Exact: term(t.Exact), Stem: term(t.Stem)}
		for _, p := range parts {
			g.Parts = append(g.Parts, PartTerms{Term: term(p.Term), Stem: term(p.Stem)})
		}
		out.Groups = append(out.Groups, g)
	})
	return out
}

// Weights are the alternatives' weights in a group's score.
type Weights struct {
	Exact, Stem, Parts, PartStem float64
}

// DefaultWeights are the starting weights, to be tuned by evaluation.
var DefaultWeights = Weights{Exact: 1.0, Stem: 0.5, Parts: 0.4, PartStem: 0.5}

// Score evaluates the query for one memory, given each term's value in
// Query.Terms order: its BM25 score for the memory, or zero if it doesn't
// occur.
//
// Every value enters through max and sum with non-negative weights, so
// evaluating Score on per-term upper bounds gives an upper bound of the
// score. The pruned walk, the dense walk, the write buffer and single
// candidates all score through this one function.
func (q *Query) Score(w Weights, vals []float64) float64 {
	var total float64
	for i := range q.Groups {
		total += q.Groups[i].Score(w, vals)
	}
	return total
}

// Score is one group's contribution.
func (g *Group) Score(w Weights, vals []float64) float64 {
	best := w.Exact * vals[g.Exact]
	if g.Stem >= 0 {
		best = max(best, w.Stem*vals[g.Stem])
	}
	if len(g.Parts) > 0 {
		var sum float64
		for _, p := range g.Parts {
			v := vals[p.Term]
			if p.Stem >= 0 {
				v = max(v, w.PartStem*vals[p.Stem])
			}
			sum += v
		}
		best = max(best, w.Parts*sum)
	}
	return best
}

// MatchKind is how a memory's term matched a query token.
type MatchKind uint8

const (
	MatchExact MatchKind = iota
	MatchStem
	MatchPart
	MatchPartStem
)

func (k MatchKind) String() string {
	switch k {
	case MatchExact:
		return "exact"
	case MatchStem:
		return "stem"
	case MatchPart:
		return "part"
	case MatchPartStem:
		return "partStem"
	}
	return "unknown"
}

// TermMatch is one term that contributed to a score.
type TermMatch struct {
	// Term indexes Query.Terms.
	Term int
	Kind MatchKind
}

// Contribution is one query token's share of a score and the terms that
// produced it: the winning alternative only, so an explanation reproduces
// the score.
type Contribution struct {
	Group int
	Score float64
	Terms []TermMatch
}

// Explain breaks a score down by query token, for tokens that contributed.
// Ties go to the exact form.
func (q *Query) Explain(w Weights, vals []float64) []Contribution {
	var out []Contribution
	for gi := range q.Groups {
		g := &q.Groups[gi]
		best := w.Exact * vals[g.Exact]
		terms := []TermMatch{{g.Exact, MatchExact}}
		if g.Stem >= 0 {
			if v := w.Stem * vals[g.Stem]; v > best {
				best, terms = v, []TermMatch{{g.Stem, MatchStem}}
			}
		}
		if len(g.Parts) > 0 {
			var sum float64
			var pt []TermMatch
			for _, p := range g.Parts {
				v, m := vals[p.Term], TermMatch{p.Term, MatchPart}
				if p.Stem >= 0 {
					if s := w.PartStem * vals[p.Stem]; s > v {
						v, m = s, TermMatch{p.Stem, MatchPartStem}
					}
				}
				if v > 0 {
					sum += v
					pt = append(pt, m)
				}
			}
			if v := w.Parts * sum; v > best {
				best, terms = v, pt
			}
		}
		if best > 0 {
			out = append(out, Contribution{Group: gi, Score: best, Terms: terms})
		}
	}
	return out
}
