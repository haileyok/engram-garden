package eval

import (
	"context"
	"fmt"
	"strings"
)

// Judgment is one graded (query, memory) pair.
type Judgment struct {
	Query  string `json:"query"`
	Memory string `json:"memory"`
	Grade  int    `json:"grade"`
	// By is "model", "known" (the memory a known-item query was written
	// for) or "human".
	By string `json:"by"`
}

const judgeSystem = `You judge search results for an AI agent's memory search tool. For each memory, grade how well it serves the query:
2 = it is what the searcher is looking for, or directly answers the query;
1 = related and partly useful;
0 = not useful for this query.
Judge each memory on its own. Answer with a JSON object only.`

// judgeBatch is how many memories one call grades.
const judgeBatch = 20

// Judge grades memories for a query, in order.
func Judge(ctx context.Context, l *LLM, query string, mems []Memory) ([]int, error) {
	var out []int
	for start := 0; start < len(mems); start += judgeBatch {
		batch := mems[start:min(start+judgeBatch, len(mems))]
		var b strings.Builder
		fmt.Fprintf(&b, "Query: %s\n\n", query)
		for i, m := range batch {
			fmt.Fprintf(&b, "Memory %d:\n<<<\n%s\n", i+1, truncateRunes(m.Text, 1500))
			if len(m.Tags) > 0 {
				fmt.Fprintf(&b, "Tags: %s\n", strings.Join(m.Tags, ", "))
			}
			if m.Source != "" {
				fmt.Fprintf(&b, "Source: %s\n", m.Source)
			}
			b.WriteString(">>>\n\n")
		}
		fmt.Fprintf(&b, "Answer as {\"grades\": [...]} with exactly %d integers, one per memory in order.", len(batch))
		var ans struct {
			Grades []int `json:"grades"`
		}
		if err := l.JSON(ctx, judgeSystem, b.String(), &ans); err != nil {
			return nil, err
		}
		if len(ans.Grades) != len(batch) {
			return nil, fmt.Errorf("judge returned %d grades for %d memories", len(ans.Grades), len(batch))
		}
		for _, g := range ans.Grades {
			out = append(out, min(max(g, 0), 2))
		}
	}
	return out, nil
}
