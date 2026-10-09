# Keyword and hybrid search design

Status: proposed. Nothing here is built yet. It extends
[storage.md](storage.md), which answers its open question about text search
alongside vector search. Read that first: this design reuses its segments,
manifests, write buffer, cache tiers and deadlines, and only describes what
changes.

## Goals

1. **Find what vectors miss.** Exact identifiers (`engram_space_uri`,
   `did:plc:…`, `ErrModelMismatch`), names, error strings, file paths and
   rare words. Embeddings blur these; a coding agent's memories are full of
   them.
2. **Never be worse than vector search.** Hybrid ranking must match or beat
   vector-only ranking on every kind of query we measure, not just on
   average (see [Evaluation](#evaluation)).
3. **Show why each result matched**, so agents can tell a strong match from a
   loose one and people can debug bad results.
4. **Scale to millions of memories per space** with warm keyword cost small
   next to the vector scan, and no extra waiting on a cold search.
5. **Keep the storage design's properties:** immutable segments, range reads
   of only what a search needs, per-space resources, rebuildable from the
   members' PDSes, export without re-indexing.

Non-goals for now: typo tolerance, prefix and wildcard queries, phrase
queries backed by stored positions, languages other than English beyond a
basic fallback, and learned sparse retrieval. Each is noted under
[Later](#later) with how it would fit.

## Overview

The text query `q` is already a parameter of
`garden.engram.searchMemories`, and both clients already send it on every
recall (`internal/agent/agent.go`, `python/src/engram_garden/agent.py`). It
just isn't used. This design uses it:

```
            q ──► analyzer ──► query terms (weighted)
                                   │
 vector ──► 1-bit scan ──┐         ▼
            (top 200)    │   BM25 over postings
                         │   (top 200, dynamic pruning)
                         ▼         │
                  union of candidates (≤ 400)
                         │
                         ▼
             re-rank all with 1-byte vectors
             (every candidate gets an exact cosine)
                         │
                         ▼
             fuse vector rank + keyword rank
                         │
                         ▼
             top k ──► docs ──► match explanations
```

- Each segment gains an inverted index: a term dictionary and postings, plus
  one byte per memory for its length. The write buffer keeps a small
  in-memory inverted index.
- BM25 statistics are summed across segments and the buffer at query time,
  so scores are exact for the whole space, not per segment.
- Keyword candidates are re-ranked with the same 1-byte vectors as vector
  candidates. A memory found only by keyword still gets a real vector score,
  and a memory found only by vector still gets a keyword score if any query
  term occurs in it.
- Without `q`, search behaves exactly as it does today.

## Analyzer

The analyzer turns text into terms, and runs the same way on memories and
queries. It matters more to quality than the ranking function, and generic
full-text analyzers get agent memories wrong: they split `engram_space_uri`
into three words and lose the identifier, or keep it whole and lose the
words.

### What gets indexed

A memory's text, its tags and its source, in that order, as one stream. The
tags and source are short, so they don't dilute the text, and the source
(URL, file path, conversation reference) is often exactly what an agent
searches for. Field weighting is a [later](#later) refinement once the
evaluation can measure it.

### Steps

1. **Normalize:** Unicode NFKC, then case folding, then removing combining
   accents (`café` → `cafe`).
2. **Split** on Unicode word boundaries (UAX #29), except that a run of
   letters, digits and the joiners `_ - . / : @ #` with no space in it stays
   one *compound* token: `engram_space_uri`, `memory.add`,
   `internal/spacestore/search.go`, `did:plc:abc123`, `hailey.at`. Trailing
   punctuation is trimmed from the compound (`see memory.add.` →
   `memory.add`).
3. **Emit terms** for each token:

   | Kind | Example input | Terms |
   |---|---|---|
   | Word | `Running` | `running` (exact), `~run` (stem) |
   | Compound | `engram_space_uri` | `engram_space_uri` (exact), parts `engram`, `space`, `uri` with their stems |
   | camelCase | `ModelMismatch` | `modelmismatch` (exact), parts `model`, `mismatch` |
   | Opaque | `bafyreib2rxk3…`, a 40-hex hash | the whole token only |
   | CJK run | `記憶検索` | overlapping character bigrams |

   - Stems use the Snowball English stemmer, with English possessives
     stripped first. They're stored as separate terms (marked with `~`), so
     an exact form can outrank a stemmed match.
   - Parts come from splitting compounds on joiners, camelCase boundaries
     and letter–digit boundaries (`v2beta` → `v`, `2`, `beta`; one-character
     parts are dropped).
   - A token is *opaque* when it looks like an identifier with no words in
     it: 16 or more characters of hex or base32, a CID, or the identifier
     part of a DID. Splitting those yields noise, and they're the tokens
     that grow the vocabulary without bound.
   - Tokens longer than 64 bytes are cut to 64 bytes.
4. **No stopword list.** Common words get a low IDF, and dynamic pruning
   skips their postings cheaply (see [Query execution](#query-execution)).
   A stopword list would break queries like `"to do"` or `the Who`.

### Query terms carry weights

The query goes through the same steps. Each query term gets a weight, and a
memory's keyword score is the weighted sum of its BM25 scores for those
terms:

| Query term kind | Starting weight |
|---|---|
| Exact form of a query word or compound | 1.0 |
| Stem | 0.5 |
| Part of a compound in the query | 0.4 |

So `engram_space_uri` matches the identifier itself most strongly, and also
matches memories that only talk about the "space URI". The weights are
starting points for the evaluation to tune, not settled values.

### Versioning

The analyzer has a version, recorded in each segment. Changing how text is
split or stemmed changes which terms exist, so it's treated like a model
change: segments with an older analyzer version are rewritten (see
[Rollout](#rollout-and-compatibility)), never silently mixed. While a space
holds segments with two analyzer versions, the query is analyzed once per
version.

## Ranking

### BM25 with exact space-wide statistics

Each memory's keyword score is BM25 (k1 = 1.2, b = 0.75 to start) summed
over the weighted query terms. BM25's term frequency saturates, so repeating
a word stops helping quickly. That also bounds how much a member can stuff a
memory with keywords, which, like a [mismatched vector](storage.md#vector-integrity-deferred),
is attributable misbehavior in a members-only space.

IDF and average length come from the whole space, not each segment: every
segment stores its memory count, total length and each term's document
frequency, and the write buffer keeps the same. A search adds them up. That
costs one dictionary lookup per term per segment, which a search does
anyway, and it means merging segments doesn't change scores.

Deleted memories still count in these statistics until a merge drops them,
as in Lucene. Merges already run once deletions pass 25%, which bounds the
error.

### Hybrid candidates

1. The 1-bit vector scan keeps its top 200, as today.
2. BM25 over the postings keeps its top 200 (`Candidates` for both).
3. The union, at most 400 memories, is re-ranked with the 1-byte vectors.
   Reading 400 instead of 200 vectors adds about 154 KB to a cold search's
   re-rank reads, grouped by segment like today's.

Filters (author, tags, since) and deletions are applied during the postings
walk from the metadata already in RAM, exactly as the vector scan does.

### Fusion

Two rankings over the same candidates: by cosine (from the re-rank) and by
BM25 (for candidates with any matching term). They're combined with
**reciprocal rank fusion** to start:

```
score(m) = 1 / (60 + vector_rank(m)) + 1 / (60 + keyword_rank(m))
```

(the second term is zero when no query term occurs in `m`). Rank fusion
needs no calibration between cosine and BM25 scales, which vary by model and
by space, so it's a safe default
([Cormack et al., 2009](https://dl.acm.org/doi/10.1145/1571941.1572114)).

A tuned convex combination of normalized scores usually beats rank fusion
([Bruch et al., 2023](https://arxiv.org/abs/2210.11934)) and needs few
labeled queries to tune. The evaluation compares both; the default ships
whichever wins, and the fusion method is recorded in the response so
clients can tell.

When the re-rank misses its deadline, the vector rank comes from the 1-bit
distances, the result is marked `approximate`, and fusion is unchanged.

### Quoted phrases

`"exact phrase"` in `q` is a filter on the results, not a ranking signal:
the phrase's words are searched as ordinary terms, and the final candidates
are checked against their text, which the search already reads to return
them. Storing positions would roughly double the postings, for a feature
agents use rarely. If a phrase filter leaves fewer than *k* results, the
response says so rather than silently returning fewer.

## Segment format, version 2

Version 2 adds four sections after `doc index`, before `clusters`.
Everything else keeps its layout, so a version 2 reader reads both versions.

```
[header]       … version 2, plus analyzer version and BM25 stats
[metadata] … [doc index]          (unchanged)
[norms]        count × 1 byte: each memory's length in terms, quantized
[term index]   every 128th term of the dictionary, with its block's offset
[terms]        the dictionary, in blocks of 128 sorted terms
[postings]     each term's postings, in blocks of 128
[clusters]     (unchanged)
[footer]
```

- **Norms** use a lossy one-byte encoding of length, like Lucene's: exact
  for short memories, coarser for long ones. BM25 only needs relative
  length, and one byte per memory keeps a 5M-memory segment's norms at 5 MB.
- **Term index:** small enough to keep in RAM with the metadata (about 1 MB
  for 5M distinct terms). A lookup binary-searches it, then reads one
  dictionary block.
- **Terms** are front-coded within each block (each term stores only what
  differs from the previous one). Each entry holds the document frequency,
  the total term frequency, and the postings' offset and length.
  - A term in exactly one memory stores that memory's row inline, with no
    postings. Most terms in a space are like this (one-off identifiers,
    hashes, typos), so this removes most postings lookups and most of the
    postings section's entries.
- **Postings** for each term, in row order:
  - a skip table: for each block of 128 rows, the last row, the block's
    byte offset, and the block's score bound (largest term frequency and
    smallest norm in the block, one byte each);
  - the blocks themselves: row deltas and term frequencies, each bit-packed
    at the block's widest value;
  - the last partial block in variable-length integers.

  The score bound is stored as (max frequency, min norm) rather than a
  score, because BM25 increases with frequency and decreases with length
  for any IDF and average length. So the bound stays valid as space-wide
  statistics change.
- In a clustered segment, rows are grouped by cluster, and postings refer
  to those rows. Nothing else changes.
- Each section has its CRC in the header table, as now.

### Size

These are estimates, to be replaced by measurements on real text (see
[Evaluation](#evaluation); the existing benchmark's synthetic text has a
tiny vocabulary, which would make postings look far smaller than they are):

| Per 1M memories of ~500 characters | Estimate |
|---|---|
| Postings: ~110 distinct terms per memory (exact, stem, parts) at ~1.2 bytes each | ~130 MB |
| Dictionary: a few million distinct terms, front-coded | 25–40 MB |
| Norms | 1 MB |
| Term index (RAM) | ~1 MB |
| **Total** | **~160–170 MB, about 13% on top of today's ~1.2 GB** |

### Building and merging

- **Flush:** the buffer's inverted index is written straight out with the
  segment. At 1,000 memories that's negligible.
- **Merge:** postings are rebuilt from the inputs' text, which a merge
  already reads (`ReadAll`). Re-analyzing is simpler than remapping the
  inputs' postings and moves segments to the current analyzer as a side
  effect. Analyzing is a few seconds per million memories; merges run at
  most daily.

## Query execution

### Warm

For each segment, in parallel:

1. Look up each query term in the term index and dictionary.
2. Walk the postings with **dynamic pruning**: keep the current top 200,
   and skip any block whose score bound can't beat the 200th score
   ([block-max WAND, Ding and Suel, 2011](https://dl.acm.org/doi/10.1145/2009916.2010048),
   or block-max MaxScore; we'll pick by benchmark). Common terms like
   `the` cost little, because most of their blocks are skipped.
3. Offer the segment's top 200 to the space-wide heap, as the vector scan
   does.

The write buffer is scored by brute force: at most 1,000 memories.

For small segments (under 10,000 rows), walking every posting into a dense
score array is simpler and as fast. The cutoff is for the benchmark to set.

### Cold

A cold search already range-reads every segment's metadata and 1-bit
section, which for a large space is tens of MB. The keyword part rides
alongside:

1. The term index arrives with the metadata (one extra section in the same
   wave of reads).
2. One wave of dictionary-block reads, one per query term per segment,
   started as soon as the term index arrives.
3. One wave of postings reads for the matched terms.

Waves 2 and 3 are small reads and run while the 1-bit sections are still
downloading, so in the common case keyword search adds nothing to a cold
search's latency.

The exception is a very common query term in a very large segment: a term
in half of a 5M-memory segment has a few MB of postings. The first answer
reads its skip table, then only the blocks whose bounds can matter. If the
deadline arrives first, the search returns what it has, marked
`approximate`, exactly like a late re-rank. After the first load the whole
segment is on SSD and this doesn't arise.

### Performance targets

To be measured on the benchmark machine with real text at 1M and 5M
memories:

| | Target |
|---|---|
| Warm keyword part, 4-term query, 5M memories | p50 ≤ 5 ms, p99 ≤ 20 ms |
| Warm hybrid search vs vector-only | ≤ 25% slower at p50 |
| Cold hybrid search vs vector-only | no slower at p50 |
| Index size | ≤ 15% of segment size |
| Merge time, 1M memories | ≤ 2× today's |

For scale: the 1-bit scan alone covers 5M memories in about 16 ms on 8
cores at the measured 313M vectors/s, before clustering cuts it down.

## API

### Search parameters

`garden.engram.searchMemories` gains one optional parameter:

- `mode`: `hybrid` (the default when `q` is present), `vector` (today's
  behavior, and the default when `q` is absent), or `keyword`.

In `keyword` mode, `vector`, `model` and `modelDigest` become optional. A
lexicon can relax required parameters without breaking existing clients.
That gives agents an exact lookup that doesn't need an embedding model, and
lets the web app search without one.

### Why each result matched

Each search result gains an optional `match` object, defined in
`garden.engram.defs`:

```json
"match": {
  "score": 0.0318,
  "fusion": "rrf",
  "vector": { "rank": 4, "similarity": 712 },
  "keyword": {
    "rank": 1,
    "score": 12.4,
    "terms": [
      { "term": "engram_space_uri", "kind": "exact", "field": "text" },
      { "term": "space", "kind": "part", "field": "text" },
      { "term": "~config", "kind": "stem", "field": "tags" }
    ]
  },
  "snippet": "…stored the «engram_space_uri» in memory_config so the harness…"
}
```

- `vector` is present for every result, since every candidate is
  re-ranked. `keyword` is present when any query term occurs.
- `terms` and `snippet` are computed only for the returned results, by
  re-analyzing the text the search already fetched. The snippet is the
  window of up to 200 characters with the most matched terms, with matches
  marked. That costs microseconds per result.
- The existing `similarity` field stays, so current clients see no change.

### Clients

- `engram-mcp`'s recall tool shows the matched terms under each memory
  (`matched: engram_space_uri (exact), space`), and the snippet when a
  memory is long. Agents use that to judge relevance without reading every
  memory in full.
- The Go and Python clients expose `match` and `mode` in their result and
  recall types.
- The web app can highlight matched terms in search results.

## Evaluation

Quality goals are only meaningful if measured. Before changing the segment
format, a harness (`cmd/engram-eval`) builds the keyword index in memory
from a space's exported segments and runs queries against vector-only,
keyword-only and hybrid ranking. Because ranking can be tried without the
storage work, the analyzer, weights and fusion can be tuned first.

### Data

- **Real spaces**, read locally and never committed: an agent's memory
  space (about 1,200 memories) and the `coding-agents` space.
- **A public benchmark with known judgments**, such as BEIR's SciFact and
  FiQA, as a check that the ranking holds on text we didn't tune for.
- **A scale corpus** for performance only: real memories, plus synthetic
  ones generated from them by an LLM, to reach 1M and 5M with a realistic
  vocabulary.

### Queries and judgments

Queries come in categories, because a single average hides regressions:

| Category | Example |
|---|---|
| Paraphrase | "how does the agent store long-term memory" |
| Exact identifier | `engram_space_uri`, `ErrModelMismatch` |
| Name or rare term | "Wasabi", "cocoon" |
| Mixed | "why does reembed fail on listRecords" |

- **Known-item queries:** an LLM reads one memory and writes the query an
  agent would use to find it, in each category. The source memory is the
  relevant answer.
- **Pooled judgments:** the top 20 from each system are pooled and graded
  0–2 by an LLM, with a sample checked by hand.

### Metrics and the bar to ship

Recall@10, MRR@10 and nDCG@10, per category.

- Hybrid must be at least as good as vector-only in every category, within
  noise, and clearly better on exact identifiers and names.
- Ties go to the simpler choice (rank fusion over a tuned combination,
  fewer term kinds).

## Rollout and compatibility

- **Readers first.** A release that reads version 2 segments ships before
  any node writes them, so a rolling deploy never has an old node reading a
  new segment. (Today's reader rejects any version but 1.)
- **Then writers,** behind a node option (`ENGRAM_KEYWORD_SEARCH`), off by
  default until the evaluation's bar is met on the benchmark machine.
- **Version 1 segments** have no keyword index. Search treats their
  memories as having no keyword matches, so hybrid search on a mixed space
  still works, just with keyword recall missing for older memories. Each
  version 1 segment is rewritten as version 2 by the merge machinery,
  one-for-one, from its own text and vectors.
  - Rewriting early costs the rest of the old segment's 90-day minimum on
    Wasabi. Today's spaces are small, so that's negligible, but upgrades
    are still rate-limited per node like merges.
- **Export and import** carry version 2 segments as-is. An importing appview
  that only reads version 1 rejects the export with a clear error.
- **Manifest:** each segment entry records its format and analyzer version,
  so a node can see what needs rewriting without opening segments. The
  manifest `format` stays 1: the new fields are optional, and older nodes
  decode manifests with plain `json.Unmarshal`, which ignores them.

## Build order

1. **Analyzer** (`internal/text`): normalization, splitting, term kinds,
   stemming, and golden tests from real memories.
2. **Evaluation harness** with an in-memory index, and the first
   measurements: tune term weights and fusion here.
3. **Postings format** (`internal/segment`): version 2 sections, writer,
   reader, dynamic pruning, benchmarks at 1M and 5M with real text.
4. **Per-space store** (`internal/spacestore`): buffer index, space-wide
   statistics, hybrid candidates, fusion, phrase filter, cold-path waves.
5. **API and clients:** `mode`, `match`, `engram-mcp` output, the Go and
   Python clients, web highlighting.
6. **Rollout:** readers, then writers behind the option, then rewriting
   version 1 segments.

## Later

- **Field weights** (BM25F): weight tags and source differently from text,
  if the evaluation shows it helps. The `field` in each match term already
  carries what that needs.
- **Prefix queries** for long identifiers (`bafyrei*`, a DID prefix): the
  dictionary is sorted, so a prefix is a range scan.
- **Typo tolerance:** a Levenshtein automaton over the dictionary, if the
  dictionary moves to an FST.
- **Learned sparse retrieval** (SPLADE-style): like dense vectors, sparse
  vectors could be computed by agents and carried in memory records, and
  stored in these same postings with learned weights. It needs a model on
  the agent side, so it waits until there's a case for it.
- **Adaptive fusion:** lean on keyword ranking for short, identifier-like
  queries and on vectors for questions, if the per-category metrics show a
  consistent split.

## Open questions

- Rank fusion or a tuned combination: decided by the evaluation.
- Whether `hybrid` should become the default for clients that send `q`
  today, or only when they ask for it. Defaulting to it is the point, but
  it changes existing agents' results on deploy.
- Where real recall queries for the evaluation come from. The appview
  doesn't log query text, and shouldn't by default; an opt-in local log in
  `engram-mcp` is one option.
- Whether to drop the exact-form terms for words whose stem equals the word
  (most short words), answering those from the stem's postings. That would
  cut postings by perhaps a fifth at some cost in simplicity.
