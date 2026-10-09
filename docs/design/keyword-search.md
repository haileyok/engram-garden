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
4. **Scale to millions of memories per space**, with warm keyword cost small
   next to the vector scan and cold searches no slower than today's, both
   demonstrated by benchmarks rather than assumed.
5. **Keep the storage design's properties:** immutable segments, range reads
   of only what a search needs, per-space resources, rebuildable from the
   members' PDSes, export without re-indexing.

Non-goals for now: quoted phrases, typo tolerance, prefix and wildcard
queries, languages other than English beyond a basic fallback, and learned
sparse retrieval. Each is noted under [Later](#later) with how it would fit.

## Where things stand

`garden.engram.searchMemories` takes a query vector, plus an optional text
query `q`. Only `space` is a required parameter in the lexicon; the handler
enforces the rest:

- **Vector sent:** `q` is ignored for ranking. Both clients send `q` with
  their vector on every recall (`internal/agent/agent.go`,
  `python/src/engram_garden/agent.py`).
- **Only `q` sent:** a service configured to embed queries embeds `q`
  itself (`handleSearch` and `embedQuery` in `internal/appview/server.go`),
  or answers `ModelNotHosted` (it doesn't run the space's model),
  `TextSearchNotAllowed` (it doesn't embed for this authority) or
  `EmbedderBusy`. A service not configured to embed queries answers
  `InvalidRequest`, because the vector fields are missing.

Either way ranking is by vector only. This design adds keyword ranking, and
turns some of those errors into keyword results (see [Modes](#modes)).

## Overview

```
            q ──► analyzer ──► query tokens and their terms
                                   │
 vector ──► 1-bit scan ──┐         ▼
            (top 200)    │   BM25 over postings, with pruning
                         │   (top 200)
                         ▼         │
                  union of candidates (≤ 400)
                         │
          ┌──────────────┼───────────────────┐
          ▼              ▼                   ▼
   BM25 for vector-  1-bit distance for   re-rank all with
   only candidates   keyword-only ones    1-byte vectors
          └──────────────┼───────────────────┘
                         ▼
             fuse vector rank + keyword rank
                         │
                         ▼
             top k ──► docs ──► match explanations
```

- Each segment gains an inverted index: a term dictionary and postings,
  plus one byte per memory for its length. The write buffer keeps an
  in-memory inverted index, updated as memories arrive.
- BM25 statistics are summed across the active index's segments and the
  buffer at query time, so scores use the whole space's statistics, not one
  segment's.
- Every candidate gets both scores. Keyword-only candidates are re-ranked
  with the 1-byte vectors like the rest; vector-only candidates are scored
  against the query's terms directly. Neither side is guessed.
- Without `q`, or with `mode=vector`, search behaves exactly as it does
  today.

## Analyzer

The analyzer turns text into terms, and runs the same way on memories and
queries. It matters more to quality than the ranking function, and generic
full-text analyzers get agent memories wrong: they split `engram_space_uri`
into three words and lose the identifier, or keep it whole and lose the
words.

### What gets indexed

A memory's text, its tags and its source, as one stream, with each term
occurrence remembering which field it came from (for explanations, and for
field weights [later](#later)). Tags and source are short, so they don't
dilute the text, and the source (URL, file path, conversation reference) is
often exactly what an agent searches for.

### Steps

1. **Split the original text**, before any normalization, so case and
   punctuation are still there to find boundaries. Each token keeps its
   UTF-8 byte span in its field, for highlighting.
   - Split on Unicode word boundaries (UAX #29), except that a run of
     letters, digits and the joiners `_ - . / : @ #` with no whitespace
     stays one *compound* token: `engram_space_uri`, `memory.add`,
     `internal/spacestore/search.go`, `did:plc:abc123`, `hailey.at`.
     Leading and trailing joiners are trimmed (`see memory.add.` →
     `memory.add`).
   - Each run of CJK characters becomes one token per overlapping
     character bigram.
2. **Detect opaque pieces before splitting them.** A piece is *opaque* when
   it is an AT Protocol record key (13 characters of the base32-sortable
   alphabet, with a digit), or at least 16 ASCII letters and digits that are
   all digits or switch between letters and digits at least three times
   (hashes, CIDs, DID identifiers). Long all-letter words never qualify.
   Test the whole token first; if it's opaque, it emits only
   itself. Otherwise split it on joiners only, and test each of those
   components the same way. An opaque component emits only itself: it is
   never split at case or letter–digit boundaries, which would otherwise
   shred a hash like `3fa9c2e1…` into noise. So `did:plc:abc…` yields the
   compound, `did`, `plc` and the opaque identifier, nothing more.
3. **Find parts** of the remaining components, still on the original text:
   split on lower→upper case changes, at the end of an acronym
   (`HTTPServer` → `HTTP`, `Server`) and on letter–digit boundaries
   (`v2beta` → `v`, `2`, `beta`). One-character parts are dropped.
4. **Normalize each term** separately: Unicode NFKC (compatibility forms can
   produce uppercase: `Ⅻ` → `XII`), full case folding (`ß` → `ss`), NFKD
   and removing nonspacing marks (so precomposed and decomposed `café` both
   become `cafe`, and folding `İ` leaves no dot), then NFC (recomposing
   Hangul). Curly apostrophes become `'`.
5. **Bound length.** A term longer than 64 bytes becomes its first 48 bytes
   (cut at a UTF-8 character boundary), `#`, and the first 12 hex digits of
   the SHA-256 of the whole term. Long URLs and paths that share a prefix
   stay distinct, and the query side applies the same rule, so an exact
   lookup still works.
6. **Stem** words and non-opaque parts with the Snowball English stemmer,
   after stripping English possessives. Stems are separate terms, marked
   with a leading `~`.

### Terms per token

| Kind | Example input | Terms |
|---|---|---|
| Word | `Running` | `running` (exact), `~run` (stem) |
| Compound | `engram_space_uri` | `engram_space_uri` (exact); parts `engram`, `space`, `uri`; part stems `~engram`, `~space`, `~uri` |
| camelCase | `ModelMismatch` | `modelmismatch` (exact); parts `model`, `mismatch`; their stems |
| Opaque | `bafyreib2rxk3…` | the whole token only |
| CJK | `記憶検索` | `記憶`, `憶検`, `検索` |

A part and an ordinary word with the same spelling are the same term: the
`space` in `engram_space_uri` and a plain `space` share postings. That's
what lets a query for "space uri" find the identifier.

### Counting

- **Term frequency:** each source token adds 1 to the frequency of each
  *distinct* term it emits. `space_space` adds 1 to `space`, not 2.
- **Document length** is the number of source tokens (words, compounds,
  opaque tokens, CJK bigrams), not the number of terms emitted. Identifier-
  heavy text is then not penalized as "long" for emitting parts and stems,
  and length doesn't change when expansion rules change.
- **No stopword list.** Common words get a low IDF. A stopword list would
  break queries like `the Who`. Their cost is bounded by
  [query limits](#limits-and-budgets), not by pretending they're free.

### Versioning

The analyzer has a version, recorded in each segment's header. Changing how
text is split, normalized or stemmed changes which terms exist and their
statistics, so a space never scores across analyzer versions. See
[Analyzer and format changes](#analyzer-and-format-changes).

## Scoring

### BM25

For a term *t* in memory *d*, with *N* memories, *df(t)* memories containing
*t*, term frequency *tf*, document length *dl* and average length *avgdl*:

```
idf(t)  = ln(1 + (N − df(t) + 0.5) / (df(t) + 0.5))        always > 0
tfn(t,d) = tf · (k1 + 1) / (tf + k1 · (1 − b + b · dl / avgdl))  < k1 + 1
s(t,d)  = idf(t) · tfn(t,d)
```

with k1 = 1.2 and b = 0.75 to start. This is Lucene's BM25, whose IDF is
never negative. *dl* is always the decoded one-byte norm (see
[Norms](#segment-format-version-2)), both when scoring and when computing
bounds, so the two agree.

### Combining a query token's terms

Each query token contributes the best of its alternatives, not their sum,
so a token can't score twice for one occurrence:

```
word token w:      c(w) = max( 1.0 · s(w), 0.5 · s(~w) )
compound token x:  c(x) = max( 1.0 · s(x),
                               0.4 · Σ over distinct parts p of max( s(p), 0.5 · s(~p) ) )
opaque token o:    c(o) = s(o)
score(d) = Σ over distinct query tokens of c
```

- Query tokens are compared after normalization, so `Space` and `space`
  are the same token and count once. A compound's parts are also distinct
  after normalization: `space_space` has one part, `space`.
- The weights (1.0, 0.5, 0.4) are starting points for the evaluation to
  tune.
- This is a tree of max and sum over non-negative leaves, not a flat sum
  of term scores: a word whose exact form scores 1.0 and stem 0.8
  contributes 1.0, not 1.8. Every path that scores (the pruned walk, the
  dense walk for small segments, the buffer, and scoring a single
  candidate) evaluates the same tree, sharing one implementation, so they
  can't disagree.
- Each leaf rises with *tf* and falls with *dl*, so evaluating the same
  tree on per-block upper bounds gives a valid upper bound for every row
  those blocks cover (see [Query execution](#query-execution)).

The evaluation must include exact-identifier queries against memories that
contain only the identifier's rarer parts, to check that the exact form
wins where it should. Rare parts have a high IDF, so this isn't automatic.

### Space-wide statistics

*N*, total length and each term's *df* are summed over the segments of the
**active** index and the write buffer. The building index during a model
change is excluded: it holds the same memories again. The segment list,
buffer, deletions and statistics are snapshotted together at the start of a
search, as the vector path already snapshots segments and deletions.

These statistics lag deletions: a deleted memory still counts until a merge
drops it, as in Lucene. A merge that drops deletions changes *N*, *df* and
*avgdl*, and so changes scores slightly; a merge without deletions doesn't.
Lag is not tightly bounded: deletes can remove every occurrence of a term
long before deletions reach the 25% merge trigger. The evaluation measures
ranking on a delete-heavy space, and if drift matters, merges can be
triggered by the gap between live and counted memories as well.

### Abuse

BM25's term frequency saturates (*tfn* < k1 + 1), so repeating a word stops
helping quickly. Stuffing a memory with keywords is, like a
[mismatched vector](storage.md#vector-integrity-deferred), attributable
misbehavior in a members-only space.

## Ranking

### Candidates

1. The 1-bit vector scan keeps its top 200, as today.
2. BM25 over the postings keeps its top 200, with filters (author, tags,
   since) and deletions applied from the metadata in RAM during the walk, as
   the vector scan does.
3. **Complete the scores** for the union, at most 400 memories:
   - each vector-only candidate is scored for every query term, by seeking
     that term's postings to the candidate's row (skip table, then one
     block);
   - each keyword-only candidate gets its 1-bit distance, from the bits in
     RAM or a range read when the space streams its bits. It may sit in a
     cluster the vector scan didn't probe, so it has no distance yet.
4. Re-rank the union with the 1-byte vectors (a quantized cosine, as today).

### Fusion

Two rankings over the same candidates: by cosine and by keyword score (for
candidates with any matching term). They're combined with **reciprocal
rank fusion** to start:

```
fused(m) = 1 / (60 + vector_rank(m)) + 1 / (60 + keyword_rank(m))
```

The second term is zero when no query term occurs in *m*. Rank fusion needs
no calibration between cosine and BM25 scales, which vary by model and by
space, so it's a safe default
([Cormack et al., 2009](https://dl.acm.org/doi/10.1145/1571941.1572114)).
Fusing doesn't guarantee hybrid beats either ranking alone; the
[evaluation](#evaluation) is what checks that.

A tuned convex combination of normalized scores usually beats rank fusion
([Bruch et al., 2023](https://arxiv.org/abs/2210.11934)) and needs few
labeled queries to tune. The evaluation compares both; the default ships
whichever wins, and the response names the fusion used.

### When parts run late

Each candidate's two scores are each one of: **known**, **known to be
zero** (keyword only: no query term occurs), or **unknown** (its stage ran
out of time). Unknown is never treated as zero, and the two vector
estimators are never mixed in one ranking. The vector rank is chosen in
this order:

1. **The 1-byte re-rank finished for every candidate:** rank all by it.
   1-bit distances aren't used, even where they're missing.
2. **Otherwise,** if every candidate has a 1-bit distance, rank all by
   1-bit distance, as today, and discard the partial re-rank.
3. **Otherwise** (the re-rank is incomplete *and* some keyword-only
   candidates lack a 1-bit distance), rank by 1-bit distance the
   candidates that have one; the rest have an unknown vector score.

A candidate with an unknown score contributes nothing to that side of the
fusion (as a missing rank does in rank fusion), and its explanation says
the score is unknown rather than omitting it, so it can't be mistaken for a
non-match.

| What ran out of time | Reason reported |
|---|---|
| 1-byte re-rank (cases 2 and 3 above) | `vectorRerank` |
| 1-bit distances for some keyword-only candidates (case 3) | `vectorPartial` |
| Keyword scores for some vector-only candidates | `keywordPartial` |
| Postings walk (budget or deadline) | `keywordBudget` |
| Query over the size limit, some tokens dropped (not a timeout) | `queryTruncated` |

Any of these sets `approximate`, and the reasons are listed in a new
`approximateReasons` field. Several can apply at once; tests cover each
combination.

## Segment format version 2

Version 2 adds four sections **after** `clusters`. The header grows, so
absolute offsets shift, but the existing sections keep their order and
layout, and today's two-read `LoadIndex` (metadata and strings, then doc
index and clusters) still reads two adjacent ranges:

```
[header]       version 2, plus analyzer version and keyword statistics
[metadata] … [clusters]           (unchanged)
[norms]        count × 1 byte
[term index]   every 128th term of the dictionary, with its block's offset
[terms]        the dictionary, in blocks of 128 sorted terms
[postings]     each term's postings, in blocks of 128 rows
[footer]
```

Loading a segment adds one range read for norms and term index (adjacent),
which stay in RAM. Terms and postings are only range-read by searches.

- **Header** gains the analyzer version, memory count, total length (in
  source tokens) and section entries for the new sections. The reader
  dispatches on version for the header, section table and footer.
- **Norms:** each memory's length, encoded lossily in one byte (exact for
  short memories, coarser for long ones). The encoder rounds *down*, and
  the scorer only ever uses the decoded value, so bounds computed from norms
  are consistent with scores.
- **Term index:** for 5M distinct terms, about 40,000 entries, roughly 1 MB
  (to be measured with real term lengths).
- **Terms:** front-coded within each block (each term stores only what
  differs from the previous). Each entry holds *df*, total term frequency,
  and the postings' offset and length. A term in exactly one memory stores
  that memory's row inline instead of postings; its total term frequency is
  that memory's *tf*. Most terms in a space are like this (one-off
  identifiers, hashes, typos).
- **Postings,** in row order:
  - a skip table with, for each block of 128 rows: the last row, the
    block's byte offset, its largest *tf* (one byte, saturating: 255 means
    "255 or more", and the bound then uses *tfn*'s limit, k1 + 1), and its
    smallest norm byte;
  - the blocks: row deltas and exact term frequencies, each bit-packed at
    the block's widest value;
  - the last partial block in variable-length integers.
- In a clustered segment, rows are grouped by cluster and postings refer
  to those final row numbers. Nothing else changes.
- Each section has its CRC in the header table, as now.

### Size

Estimates, assuming 768-dimension vectors and ~500-character memories, to
be replaced by measurements on real text. The existing benchmark's
synthetic text has a tiny vocabulary, which would make postings look far
smaller than they are.

Measured with the analyzer on an agent's 1,443 real memories (about 900
characters each): 124 source tokens and **204 distinct terms per memory**,
about one per 4.4 bytes of text. Half the vocabulary occurs in only one
memory, and terms average 10.6 bytes. 28% of postings are a stem equal to
its word (`the` and `~the`); see [Open questions](#open-questions).

| Per 1M memories of ~500 characters | Estimate |
|---|---|
| Postings: ~115 distinct terms per memory at ~1.2 bytes each, plus skip tables | ~150 MB |
| Dictionary: a few million distinct terms, front-coded | 25–40 MB |
| Norms | 1 MB |
| Term index (RAM) | ~1 MB |
| **Total** | **~180–190 MB, about 15% on top of today's ~1.2 GB** |

Longer memories add postings in proportion to their text, but also add
compressed text to the segment, so the proportion moves less than the
absolute size.

The proportion depends on dimensions and text length; the target below is
for this workload, not a property of the format.

### Building

- **Flush:** the buffer's inverted index is written out with the segment.
- **Merge:** postings are rebuilt by analyzing the inputs' text, which a
  merge already reads (`ReadAll`). Re-analyzing is simpler than remapping
  postings and always produces the current analyzer version. Analysis runs
  at about 55–65 µs per real memory per core (14–16 MB/s, with stems
  cached), so a million memories cost about a minute of CPU, spread across
  cores.
- **Memory:** merges already hold every input's vectors and text. Postings
  are built from (term, row, tf) tuples sorted in chunks of bounded size,
  spilled to local SSD when a chunk fills, and merged, so the extra peak
  memory is bounded (a few hundred MB, configurable) rather than growing
  with the space. Large merges and rewrites run one at a time per node.
  Benchmarks record peak RSS, not just time.

## Query execution

### Limits and budgets

- **Query size:** at most 32 distinct query tokens and 128 terms after
  expansion. Tokens are taken in query order until either limit would be
  exceeded; the rest are dropped and the response says so
  (`queryTruncated` in `approximateReasons`). A single compound with more
  than 16 parts keeps the compound itself and its first 16 parts.
- **One work budget per search,** not per segment, so it doesn't grow
  with the number of segments. It counts postings blocks decoded, bytes
  read (dictionary blocks, skip tables, postings, cold prefetch) and
  buffer memories scored, across every stage:
  - prefetch during a cold load,
  - dictionary and skip-table lookups,
  - the postings walk, with each segment given a share in proportion to
    its rows and unused shares passed on,
  - scoring the buffer,
  - completing scores for vector-only candidates.

  Running out stops the stage it happens in: the walk reports
  `keywordBudget`, completion reports `keywordPartial`, and later stages
  are skipped.
- **Cancellation:** every stage checks the request's deadline and
  cancellation every few hundred blocks or memories.

The budget is set from benchmarks, so that well-formed queries never hit
it and adversarial ones can't monopolize a node.

### Warm

For each segment of the active index, in parallel:

1. Look up each query term in the term index and dictionary (one block,
   usually cached), and collect *df* for the space-wide statistics.
2. After all segments report *df* (a barrier; lookups are fast), walk the
   postings with **dynamic pruning**: keep the current top 200 and skip any
   range of rows whose score bound can't beat the 200th score
   ([block-max WAND, Ding and Suel, 2011](https://dl.acm.org/doi/10.1145/2009916.2010048),
   or block-max MaxScore; we'll pick by benchmark).
3. Offer the segment's top 200 to the space-wide heap.

Because the score is a max/sum tree rather than a flat sum, the walk works
on **query tokens**, not terms:

- Each query token is one iterator: a union over its terms' postings (the
  exact form, the stem, the parts and their stems) that yields each row any
  of them contains, and evaluates that token's part of the tree for it.
  Pruning algorithms see one list per query token.
- A token's bound over a range of rows is its tree evaluated on its terms'
  block bounds. A range ends where any of its terms' current blocks ends,
  so every bound used covers the whole range; this is the usual block-max
  rule, applied to unions.
- In MaxScore, query tokens, not terms, are split into essential and
  non-essential by their maximum bounds.
- Rows are scored by the same tree in every path (see
  [Combining a query token's terms](#combining-a-query-tokens-terms)).
  Randomized tests compare pruned and exhaustive top-k, and record blocks
  visited, for queries with overlapping exact and stem postings, parts
  shared between query tokens, and repeated parts.

Pruning helps most when a query mixes rare and common terms: the common
terms' blocks rarely beat the threshold set by the rare ones. It does
**not** make a query of only common words cheap: with one term, IDF scales
the bounds and the threshold alike. Those queries are the worst case and
are benchmarked explicitly; the work budget caps them.

For small segments (under ~10,000 rows), walking every posting into a dense
score array is simpler and as fast. The cutoff is for the benchmark to set.

The write buffer's inverted index is scored exhaustively. It's updated as
memories arrive, so its cost follows the number of matching memories, not
the buffer's size. (A flush starts at 1,000 memories, but that isn't a cap:
an initial sync or a slow upload can grow the buffer well past it.) A
common word can still match every buffered memory, so buffer scoring draws
on the search's work budget like everything else.

### Cold

Today a search waits for the whole space to load (each segment's metadata,
then its 1-bit section, for up to 16 segments at once) before searching. Keyword search adds a
dependency chain: term index, then dictionary blocks, then the statistics
barrier, then postings. To keep cold searches no slower:

- **Query-aware loading.** Today a load is shared through a
  `singleflight` group in `Node.space`: the first caller runs it, and
  later callers only see its final result. Nothing can join a load in
  progress. So each load gets a small **progress object**, registered for
  the space while the load runs:
  - It publishes each segment's index (metadata, norms, term index) as
    soon as it's read, before that segment's 1-bit section, and keeps the
    ones already published so a search that joins late catches up.
  - A search subscribes with its query terms. For each published segment
    it starts the dictionary reads for its terms, then the postings reads,
    while the 1-bit sections are still downloading. Scoring waits for the
    statistics barrier; fetching doesn't.
  - Prefetch belongs to the search, not the load: it's admitted by the
    space's rate and concurrency limits *before* it starts (today those are
    checked only after loading), draws on the search's work budget, is
    deduplicated across searches waiting on the same load, and is cancelled
    if its search is. Cancelling a search never cancels the shared load.
  - **Text-only searches** need the space's model before they can embed
    `q`, and today `embedQuery` gets it through `Store.Config`, which waits
    for the whole load. The load publishes the space's config as soon as
    it's read (it comes from the manifest), so embedding the query, keyword
    prefetch and the 1-bit download all overlap.

  Tests cover a search arriving first, a text-only search arriving first,
  several searches with different terms, a search joining late, and a
  waiting search being cancelled.
- **Parallel range reads.** `ReadInt8` today reads each adjacent run of
  rows in turn. With 400 scattered candidates in a large merged segment,
  that can be hundreds of sequential requests. Reads per segment become
  parallel (bounded) and coalesce rows separated by small gaps. The vector
  path benefits too.
- **Very common terms in very large segments.** A term in half of a
  5M-memory segment has a few MB of postings. The first search reads its
  skip table and only the blocks whose bounds can matter; the work budget
  and deadline apply as usual.
- **SSD caching is best effort.** After a load the node pulls whole
  segments onto SSD in the background, but that can lag or fail, so cold
  reads are designed for, not assumed away.

For scale, a 5M-memory space has 480 MB of 1-bit vectors at 768
dimensions, so its cold load already takes seconds. Whether a space's bits
are pinned in RAM or streamed depends on `SpaceRAMShare` (1 GiB by default,
counting both indexes during a model change), so the benchmarks cover both.
The keyword reads are small next to the bits. "No slower" is a target the
benchmarks must show, across one large merged segment, many small ones,
and a disabled or undersized SSD cache.

### Performance targets

On the benchmark machine with real text and 768 dimensions. "Warm" means
the space is loaded and its segments are on SSD. Query sizes count query
tokens before expansion.

| | Target |
|---|---|
| Warm keyword part, 4-token query, 5M memories | p50 ≤ 5 ms, p99 ≤ 20 ms |
| Warm keyword part, 1 common-word query, 5M memories | p99 ≤ 50 ms, or stopped by the budget |
| Warm hybrid search vs vector-only, same space | ≤ 25% slower at p50 |
| Cold hybrid search vs vector-only | no slower at p50 |
| Index size | ≤ 15% of segment size for this workload |
| Merge of 1M memories | ≤ 2× today's time, extra peak RSS within the configured budget |

For comparison, the 1-bit scan alone covers 5M memories in about 16 ms on 8
cores, extrapolated from the measured 313M vectors/s; a full search with
filters and the candidate heap will be slower, and is measured alongside.

## API

### Modes

`garden.engram.searchMemories` gains an optional `mode` parameter:

| Request | Default mode | What runs | Vector fields in results |
|---|---|---|---|
| `vector`, no `q` | `vector` | Today's search | yes |
| `vector` and `q` | `hybrid` | Both, fused | yes |
| `q` only, service embeds queries | `hybrid` | Server embeds `q`, then both | yes |
| `q` only, service doesn't embed | `keyword` (fallback) | Keyword only | no |
| `q`, `mode=keyword` | — | Keyword only, no embedding | no |
| `mode=vector` with `q` | — | Today's search: `q` is ignored for ranking if a vector is sent, and embedded by the service as today if not | yes |

- **Hybrid is the default** whenever there's a query text and a vector (sent
  or embedded by the service). Existing clients send both, so they get
  hybrid ranking on deploy with no change.
- **Falling back to keyword.** When only `q` is sent and the service can't
  embed it (it doesn't embed queries, doesn't run the space's model, or
  doesn't embed for this authority), the search runs in keyword mode
  instead of answering `InvalidRequest`, `ModelNotHosted` or
  `TextSearchNotAllowed`. Clients without a model, such as the Claude
  connector, get keyword results rather than an error. `EmbedderBusy` stays
  a retryable error: it's momentary, and keyword results would be a silent
  downgrade. If the space's keyword index isn't complete either, the
  original embedding error is returned, since nothing can run.
- The response gains `mode`, the mode that actually ran, so a client can
  tell a fallback from a hybrid search and say so (for example, "keyword
  matches only: this service can't embed queries for this space").
- `mode=keyword` or `mode=hybrid` without `q`, `mode=hybrid` or
  `mode=vector` without any way to get a vector, and an unknown mode are
  `InvalidRequest`. An explicit `mode=hybrid` or `mode=vector` never falls
  back.
- A `q` that's empty after analysis (only whitespace or punctuation)
  counts as no `q`: a vector search if there's a vector, `InvalidRequest`
  otherwise.
- **Keyword mode searches the active index.** It needs no query vector, but
  it searches the same memories as any other mode: those whose vector
  matches the space's active model. A memory without a usable vector isn't
  indexed at all, and a space with no declared model still answers
  `NoModel`. Making keyword search independent of vectors would need a text
  index with its own lifecycle; that's out of scope.
- If a space's keyword index isn't complete (see
  [Analyzer and format changes](#analyzer-and-format-changes)), `hybrid`
  runs as `vector` and `keyword` answers a new `KeywordIndexBuilding`
  error. The response's `keywordCoverage` field says which applied, so a
  partial keyword index is never presented as a complete result.

### Why each result matched

Each result gains an optional `match` object, defined in
`garden.engram.defs`. Lexicons have no floating-point type, so scores are
scaled integers, like the existing `similarity` (cosine × 1000):

```json
"match": {
  "fusion": "rrf",
  "vector": { "rank": 4, "similarity": 712 },
  "keyword": {
    "rank": 1,
    "score": 1240,
    "terms": [
      { "term": "engram_space_uri", "kind": "exact", "field": "text" },
      { "term": "space", "kind": "part", "field": "text" },
      { "term": "~config", "kind": "stem", "field": "tags" }
    ]
  },
  "snippet": {
    "field": "text",
    "text": "…stored the engram_space_uri in memory_config so the harness…",
    "highlights": [{ "byteStart": 13, "byteEnd": 29 }]
  }
}
```

- `keyword.score` is the BM25 score × 100. `kind` is how the memory's term
  matched (`exact`, `stem`, `part`). `terms` lists exactly the terms that
  contributed to the score, the winning alternative of each query token, so
  the explanation reproduces the score.
- `vector` is absent in keyword mode, and for a keyword-only candidate
  whose vector side ran late. `keyword` is absent when no query term
  occurs.
- Highlights are UTF-8 byte ranges within `snippet.text`, the same
  convention as Bluesky's rich-text facets, rather than inserted markers.
  The snippet comes from the field with the most matches, so a memory that
  matched only on a tag or its source shows that field.
- Terms and snippets are computed only for the returned results, by
  re-analyzing the text the search already fetched with the same analyzer
  version, keeping byte spans. That costs microseconds per result.
- The existing `similarity` field stays, so current clients see no change.

The response also gains `approximateReasons` (see
[When parts run late](#when-parts-run-late)) and `keywordCoverage`.

### Clients

- **Searching one space** (`Agent.Recall`, and `Spaces.Recall` with one
  space): keep the server's order. Today three places re-sort by
  `similarity`, which would throw away the hybrid ranking even for a single
  space: both `Spaces.Recall` implementations (Go
  `internal/agent/spaces.go`, Python `python/src/engram_garden/spaces.py`)
  and the Claude connector's recall (`internal/web/connector_tools.go`).
  The connector treats a missing similarity as lowest, so keyword-fallback
  results from several spaces would come out in space order.
- **Searching several spaces:** if every space answered in vector mode,
  sort by `similarity` as today. Otherwise merge by reciprocal rank fusion
  of each memory's position in its space's results. Raw BM25 scores from
  different spaces aren't comparable, and nor are cosines from different
  models. A space that failed is reported in the note, as today.
- `engram-mcp`'s recall tool shows the matched terms under each memory
  (`matched: engram_space_uri (exact), space`) and the snippet when a
  memory is long.
- **Notes about approximate results** come from `approximateReasons`.
  Today the Go client and the connector say "the index was still loading"
  whenever `approximate` is set, which would be wrong for a keyword budget
  or a truncated query.
- The Go and Python clients expose `mode`, `match`, `approximateReasons` and
  `keywordCoverage`. End-to-end tests cover default recall through
  `engram-mcp` and the Python client, for one space and several.
- The web app can highlight matched terms from the byte ranges.

### What tells agents and people about search

Hybrid becomes the default, so everything that describes recall as
semantic or vector-only changes in the same release that turns it on:

- **`engram recall`** (`cmd/engram/cli.go`): a `-mode` flag; output shows
  matched terms and, after a fallback, which mode ran.
- **`engram-mcp`'s `recall` tool** (`internal/mcpserver/server.go`) and the
  **Claude connector's** (`internal/web/connector_tools.go`): both
  descriptions say "Semantic search … describe what you're looking for in
  natural language". They become: search by meaning *and* exact words, so
  include the identifiers, names, error messages or paths you know; the
  output explains each match. They also gain the `mode` parameter.
- **The agent guide** (`web/public/AGENTS.md`): how to write recall
  queries (natural language plus exact terms), what the matched terms
  mean, and that the connector now falls back to keyword results instead
  of a `ModelNotHosted` note.
- **The repository's `AGENTS.md`, the README and `python/README.md`:**
  where they describe search, the analyzer and keyword index (for people
  changing the code), and the new parameters and fields.

## Analyzer and format changes

A space's keyword index is either complete in one analyzer version or not
used. Scores are never mixed across versions.

- **The target analyzer version** is derived, not stored, so it can't be
  lost or disagree across restarts: it's the highest of the node's
  configured analyzer version (the newest one its code ships, unless
  overridden) and every analyzer version found in the space's segment
  headers that the node supports. It never moves backwards: a node never
  rewrites segments to an older analyzer, so two releases can't fight. A
  release that targets a new analyzer version only ships after a release
  that can read it, the same rule as the segment format.
- Segments in the target version are *covered*; anything else (a version 1
  segment, or one from an older analyzer) isn't.
- **The buffer is always analyzed in the target version.** Buffered
  memories keep their text, so if a node's target changes (at load, or on
  finding a newer segment), it re-analyzes the buffer under the space's
  lock before anything else runs. The buffer is therefore always covered,
  and a space with only a buffer and no segments is correctly searchable.
- Keyword search runs for a space only when every segment of its active
  index is covered. Until then, `hybrid` falls back to `vector` and
  `keywordCoverage` reports the covered fraction.
- **Flushes, merges and rewrites capture the target when they start** and
  write that version. If the target moved while they ran, their output is
  simply uncovered, and the rewrite job picks it up.
- **Model changes:** the rewrite job covers the building index too, so a
  promotion usually finds it already covered. If not, keyword search
  pauses until it is. The buffer's keyword index depends only on text, so
  it carries over unchanged when promotion swaps the buffer's vectors.
- Tests cover a buffer-only space, a restart midway through an analyzer
  upgrade, a flush in flight when the target changes, and promotion with
  the building index partly covered.
- **Getting covered:** a *rewrite* job replaces an uncovered segment with a
  covered one with the same rows and vectors, publishing a manifest per
  replacement, under the same lease check and manifest rules as a merge.
  It's scheduled independently of the merge triggers (a space with few
  segments and no deletions would otherwise never merge), rate-limited per
  node, and also handles the building index during a model change.
- **Analyzer upgrades** work the same way: a release with a new analyzer
  version makes every older segment uncovered, keyword search pauses for
  that space until rewriting finishes, and coverage is reported meanwhile. Analyzer changes
  should be rare. If the pause becomes a problem, rewritten segments could
  carry postings for both versions during the switch, at the cost of
  temporarily larger segments.
- Rewriting early costs the rest of the old segment's 90-day minimum on
  Wasabi. That's small today, and the rate limit bounds it.
- **Manifest fields are hints.** The manifest records each segment's
  format and analyzer version so a node can plan rewrites without opening
  segments, but a segment's header is the truth, and the target is derived
  from headers and configuration. A node that finds the manifest fields
  missing, because an older node wrote the manifest and dropped them,
  reads the headers instead.
- **Releases:**
  1. Readers: understand version 2 segments, every analyzer version so
     far, and the new manifest fields. Nothing writes them yet.
  2. Writers, behind a node option (`ENGRAM_KEYWORD_SEARCH`), off by
     default until the evaluation's bar is met on the benchmark machine.
  3. Rewriting version 1 segments.

  Once writers are on, rolling back past the reader release isn't
  supported: older nodes can't open version 2 segments. The reader release
  is the rollback floor.
- **Export and import** carry version 2 segments as-is. An importing
  appview that only reads version 1 rejects the export with a clear error.

## Evaluation

Before changing the segment format, a harness (`cmd/engram-eval`) builds
the keyword index in memory from a space's segments and runs queries
against vector-only, keyword-only and hybrid ranking. Ranking can be tried
without the storage work, so the analyzer, weights and fusion are tuned
first.

### Data

- **Real spaces:** an agent's memory space (about 1,200 memories) and the
  `coding-agents` space. Never committed. Their contents go only to an LLM
  the spaces' owner has approved for generating queries, judging and
  synthesizing data.
- **A public benchmark with known judgments,** such as BEIR's SciFact and
  FiQA, as a check that the ranking holds on text we didn't tune for.
- **A scale corpus** for performance and for ranking at scale: real
  memories plus locally generated ones, at 1M and 5M, with realistic
  identifiers mixed in (DIDs, CIDs, paths, error names) at a skewed
  frequency distribution, and some near-duplicates, so exact-identifier
  queries face millions of distractors.

### Running it

The evaluation runs on a developer's machine, not on the appview:
`engram-eval` reads the spaces through the normal client, builds the
index in memory, embeds with the space's model, and calls an
OpenAI-compatible chat endpoint, configured by environment variables, for
generating queries and grading. Apart from those model calls, nothing it
reads or produces leaves the machine. Its outputs (queries, judgments,
reports) stay out of the repository except for aggregate numbers quoted in
PRs.

**Real recall queries** come from an opt-in local log in `engram-mcp`
(`ENGRAM_QUERY_LOG=<path>`): each recall's query text, space and the
returned memory ids, appended to a file on the agent's own machine. The
appview never logs query text. The eval reads that file as an extra query
set, judged the same way as the generated ones.

### Queries and judgments

Queries come in categories, because a single average hides regressions:

| Category | Example |
|---|---|
| Paraphrase | "how does the agent store long-term memory" |
| Exact identifier | `engram_space_uri`, `ErrModelMismatch` |
| Part of an identifier | "space uri" for `engram_space_uri` |
| Name or rare term | "Wasabi", "cocoon" |
| Word forms | "migrating" for a memory that says "migration" |
| Common words | "the thing we did before" |
| Tag or source only | a term that appears only in a memory's tags or URL |
| Mixed | "why does reembed fail on listRecords" |
| No answer | a query with no relevant memory |

- **Known-item queries:** a model reads one memory and writes the query an
  agent would use to find it, per category. The source memory is the
  relevant answer.
- **Pooled judgments:** the top 20 from each system are pooled and graded
  0–2 by a model. A sample of at least 100 is graded by hand, and model
  grades are only trusted if they agree well with the hand grades.
- Queries are split into a tuning set and a held-out set. Weights and
  fusion are tuned on the first; the second is used once, for the decision.

### Metrics and the bar to ship

Recall@10, MRR@10 and nDCG@10 per category, with 95% bootstrap confidence
intervals, plus **candidate recall**: how often the relevant memory is in
the 400-candidate union at all, compared with the 200 vector candidates.

On the held-out set:

- In every category, the lower bound of the confidence interval for
  (hybrid − vector) nDCG@10 is at least −0.01.
- For exact identifiers, parts of identifiers and names, hybrid is better
  than vector-only with the whole interval above zero.
- **"No answer" queries** use a false-positive rate. On the tuning set,
  choose a similarity threshold and a keyword-score threshold that 90% of
  relevant top results clear. On held-out "no answer" queries, a false
  positive is a top result that clears a threshold: the similarity
  threshold for vector-only, either threshold for hybrid. Hybrid's rate
  must not exceed vector-only's beyond the confidence interval, so keyword
  matches on common words don't make irrelevant results look strong. Fused
  scores are rank-based, so they're never used as confidence.
- Ties go to the simpler choice: rank fusion over a tuned combination,
  fewer term kinds.

## Build order

1. **Analyzer** (`internal/text`): splitting with byte spans, parts,
   opaque detection, normalization, length bounding, stemming, and golden
   tests from real memories, including camelCase, acronyms, accents in both
   Unicode forms, DIDs and long URLs.
2. **Evaluation harness** (`cmd/engram-eval`, run locally) with an
   in-memory index, and the opt-in query log in `engram-mcp`: tune weights
   and fusion, measure candidate recall.
3. **Postings format** (`internal/segment`): version 2 sections, writer
   with bounded memory, reader, dynamic pruning. Randomized tests check that
   pruned top-k equals exhaustive top-k, including *tf* over 255, every
   norm value, changed average lengths, ties and repeated query tokens.
   Benchmarks at 1M and 5M with real text, including the worst cases above.
4. **Per-space store** (`internal/spacestore`): buffer index, statistics
   snapshot, completed candidate scores, fusion, budgets, query-aware
   loading, parallel range reads, coverage and rewrites.
5. **API and clients:** `mode` (with the keyword fallback), `match`,
   `approximateReasons`, `keywordCoverage`, `KeywordIndexBuilding`;
   rank-preserving merges in both `Spaces.Recall`; the CLI, `engram-mcp`
   and connector tools; web highlighting.
6. **Documentation**, in the release that turns hybrid on: tool
   descriptions, `web/public/AGENTS.md`, the repository's `AGENTS.md`, the
   READMEs (see
   [What tells agents and people about search](#what-tells-agents-and-people-about-search)).
7. **Rollout:** reader release, writers behind the option, rewriting.

## Later

- **Quoted phrases.** Treating `"exact phrase"` as a filter over the top
  candidates can miss every true phrase match when its words also appear
  apart in many memories. Doing it properly needs either stored positions
  (roughly doubling postings) or progressively widening a conjunctive
  candidate set and checking text within a budget, with clear semantics for
  punctuation, fields and malformed quotes, and a response field for
  incomplete checks. Until then, quotes are ignored and phrase words are
  ordinary terms.
- **Field weights** (BM25F): weight tags and source differently from text,
  if the evaluation shows it helps. Field provenance is already kept.
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
- Whether to drop the exact-form terms for words whose stem equals the word
  (most short words), answering those from the stem's postings. On real
  memories that's 28% of postings. The cost is ranking: the exact form of
  such a word could no longer outrank its stemmed matches (`run` against
  `running`). The evaluation measures whether that matters.
