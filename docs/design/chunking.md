# Embedding long memories in chunks

Status: measured, not built. This records the experiment and what it says;
whether to change the record format is open.

## The question

Engram embeds a memory as one vector of its whole text (up to
`lex.MaxEmbedChars`). The `coding-agents` space averages 1,450 characters:
a third of its memories are over 1,500 and one in ten is over 3,000. A single
vector of a long, many-topic memory is a blur, so a query about one detail
in it may not find it. [Diderot](https://github.com/bluesky-social/diderot)
cuts bodies over 1,500 characters into windows of about 1,000 with 200 of
overlap, embeds each, and keeps the best window per event. Does that help
here, next to a keyword leg Diderot doesn't have?

## Method

`engram-eval chunks` (`cmd/engram-eval/chunks.go`) ranks the same queries with
the whole-memory index and with variants whose memories have several vectors
(a memory ranks by its best vector; `eval.NewIndexRows`). The keyword side and
the fusion (convex, alpha 0.5, the setting the appview ships) are unchanged.
Everything the variants surface that nobody graded is graded by a model, so
all systems are scored against the same pooled judgments.

- **Corpus:** the real `coding-agents` space, 1,996 memories (the corpus
  never leaves the developer's machine; see AGENTS.md).
- **Queries:** 470 known-item queries from the usual categories, plus a new
  `deep` category: a question about one detail a memory states only in its
  last third. Three times as many `deep` queries as the others, so the long
  memories are well represented. (`all` below is therefore weighted toward
  long memories more than real traffic would be.)
- **Judge:** the judge used to read only 1,500 characters of a memory, which
  would grade a match on a late passage as not useful and bias the experiment
  against chunking. `-judge-runes 6000` makes it read the whole memory.
- **Variants:** `c1000` (windows of 1,000 characters, 200 overlap, only for
  memories over 1,500: Diderot's rule), `c500` (500/100, over 800), each with
  and without the whole-text vector kept as well, and with a **chunk-count
  penalty**: a memory's similarity is its best vector's minus
  `penalty * ln(vectors)`.

## Results (nDCG@10, hybrid, change from one vector per memory)

| | whole (baseline) | c1000 | c500 | c1000, penalty 0.02 | c500, penalty 0.02 |
|---|---|---|---|---|---|
| all queries (440) | 0.712 | +0.004 | +0.003 | **+0.019** | **+0.022** |
| target ≤ 1,000 chars (163) | 0.726 | −0.021 | −0.023 | +0.005 | +0.007 |
| target 1,500–3,000 (142) | 0.716 | +0.020 | +0.027 | +0.033 | +0.044 |
| target > 3,000 (96) | 0.663 | +0.032 | +0.024 | +0.029 | +0.022 |
| `deep` queries (120) | 0.661 | +0.030 | +0.041 | +0.044 | **+0.060** |

(The intervals for the penalized variants on all queries are about ±0.01;
the unpenalized ones' include zero. Penalty 0.04 gives about the same overall
gain, with the best Recall@10, 0.615 against 0.592.)

- **Vector alone** gains a lot from chunks: +0.10 overall and +0.27 to +0.30
  on `deep` queries. That is the effect Diderot relies on, but the appview
  doesn't rank by vector alone.
- **In hybrid ranking the gain is small**, because the keyword leg already
  finds most late details by their words. Without a correction it nets to
  nothing: the +0.02 to +0.03 on long memories is paid for by −0.02 on short
  ones (and −0.04 to −0.07 on vague `common` queries, −0.03 to −0.05 on
  `wordform`), since a long memory has more vectors and so more chances to
  match by luck and crowds out a short one that matches as well.
- **The penalty removes that cost.** With 0.02 to 0.04 per ln(vectors), no
  category or length bucket is worse than the baseline, and the long ones
  keep their gain. Keeping the whole-text vector as well changes nothing.
- 0.02 and 0.04 were the only values tried, and the same queries chose and
  scored them, so +0.02 is a little optimistic.

## What it would cost

- **Records:** a memory carries one embedding today. Chunked, a record would
  carry several (c1000 makes 2.0 vectors per memory here, c500 makes 3.7), so
  records get 2 to 4 times larger, the agents embed more text per `remember`
  (batched), and every client (Go, Python, pi) changes.
- **Index:** segments need several rows per memory, with the 1-bit scan, the
  int8 re-rank and the manifest's memory counts working on rows; the
  penalty would be a constant in the index or the space's config.
- Net: roughly +0.02 nDCG@10 overall and +0.04 to +0.06 on the long memories
  where it matters, for a format change and 2 to 4 times the vectors.

## Cheaper things to try first

- A short noun-first summary line per memory, embedded with the text (or on
  its own as a second vector): one extra vector instead of several, and it
  would also give the keyword leg a field to weight.
- Embedding tags or the project as a prefix instead of a suffix.

Both fit `internal/eval/embed.go` and the same `chunks`-style comparison.

## Running it

```bash
go build -o /tmp/engram-eval ./cmd/engram-eval
engram-eval fetch -dir ~/.cache/engram-eval/coding-agents -space coding-agents
# queries: random memories, then more from the long ones (deep queries)
engram-eval gen -dir ... -n 300 -gen-runes 12000
engram-eval gen -dir ... -n 250 -noanswer 0 -minlen 2000 -gen-runes 12000
# a sample (3x as many deep queries) to keep the judging affordable
engram-eval chunks -dir ... -depth 10 -judge-runes 6000 -per-category 40
```

`ENGRAM_EVAL_LLM_EXTRA='{"reasoning_effort":"low"}'` is merged into each
request, which speeds up a reasoning model a lot; keep concurrency low
(`-concurrency 6`) against a rate-limited gateway.
