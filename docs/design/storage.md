# Storage and scaling design

Status: implemented, except the parts marked as later. This replaced the
Postgres and pgvector store. The code is in `internal/spacestore` (the
index), `internal/segment`, `internal/vec`, `internal/blob`,
`internal/routing` and `internal/lex`. Where the implementation settled a
detail differently from the first draft, this document describes what was
built.

## Goals

1. **Each space uses its own resources.** A space's data, cost and limits are
   its own, so costs can be attributed per space and limits bound how much
   one busy space can affect others.
2. **Scale to a very large number of spaces and agents**, horizontally
   (more nodes) and vertically (bigger nodes).
3. **Keep hosting cheap.** Cost should track how many spaces are *in use*,
   not how many exist.
4. **Make export easy.** A space's index can be exported and imported by
   another appview without re-embedding.

Non-goals for now: searching across spaces, embedding on the appview (agents
embed their own memories and queries; see [Embeddings](#embeddings)), and
verifying that a memory's vector matches its text (see
[Vector integrity](#vector-integrity-deferred)).

## Why not a shared vector index

The previous store put every space's vectors in one Postgres table with one
HNSW index. That fit this workload badly:

- **The workload:** many spaces, each small (hundreds to a few thousand
  memories), most of them idle at any moment. Every search is confined to
  one space.
- A shared HNSW index has to stay in RAM to be fast, so we pay memory for
  idle spaces.
- Filtering a shared index down to one space degrades as the index grows.
  The old code widened `hnsw.ef_search` to compensate.
- Spaces share CPU and cache, so one busy space slows everyone.
- The index only holds data copied from members' PDSes and can always be
  rebuilt from them, so a replicated database buys durability we don't
  need.

## Overview

Each space's index is a small set of immutable files in object storage
(Wasabi, through its S3 API). Exactly one appview node owns a space at a
time. The owner keeps the space's hot parts in RAM and local SSD, and serves
both its searches and its sync.

```
            routing: hash(space URI) → owning node
 search /   ┌────────────────────────────────────────────────────────┐
 notify ───►│ node A              node B              node C         │
            │ RAM: manifests,     RAM: ...            ...            │
            │   1-bit vectors,                                       │
            │   write buffers                                        │
            │ SSD: recently used                                     │
            │   segments                                             │
            └────────┬───────────────────────────────────────────────┘
                     │ range reads, new segments, manifests
                     ▼
            Wasabi: spaces/<space key>/{manifest, segments}
```

The index needs no database. What can't be rebuilt from the members' PDSes
(which authorities granted the appview access, their OAuth sessions, which
spaces are registered) is a small Postgres database, described in
[indexing-access.md](indexing-access.md#where-grants-are-kept). Which node
owns a space isn't stored at all; see
[Routing](#routing-and-ownership).

## Embeddings

**Agents embed their own memories and queries.** The appview never runs a
model. It syncs memory records (verified against their authors' signed
commits, as before), then quantizes, stores and searches the vectors they
carry.

Why:

- Embedding is the most expensive step in the system. A careful published
  benchmark
  ([Big Iron, 2026](https://www.bigiron.cc/guides/embedding-throughput-across-cpu-igpu-and-dgpu-in-a-homelab))
  measured nomic-embed-text through Ollama on 512-token chunks at ~1.75
  documents/s on a laptop CPU (i7-12700H) vs ~59/s on a laptop RTX 3060.
  Embedding for every space would mean running GPU machines.
- Spread across agents, the same work is trivial. One agent writes a few
  memories an hour and embeds one short query per recall. Memories
  (~125 tokens) and queries (~20 tokens) are far shorter than those chunks,
  so even a laptop CPU keeps up. This is goal 1, each space using its own
  resources, applied to compute.
- The vector lives in the memory record, in the author's own repo and under
  their signed commit. Any appview can index a space from the records alone,
  without re-embedding, which also makes export nearly free.

### The space's model

Every vector in a space must come from the same model, used the same way,
or the vectors can't be compared. The space authority declares it in a
`garden.engram.config` record (record key `self`) in its own repo in the
space:

```json
{
  "$type": "garden.engram.config",
  "model": "nomic-embed-text",
  "modelDigest": "sha256:…",
  "dims": 768,
  "documentPrefix": "search_document: ",
  "queryPrefix": "search_query: ",
  "createdAt": "…"
}
```

- `modelDigest` pins the exact model build. A model tag such as Ollama's
  `nomic-embed-text` can point to different weights over time.
- nomic-embed-text expects `search_document: ` before stored text and
  `search_query: ` before queries, and retrieval quality drops without them.
  The prefixes are part of the declaration so every agent embeds the same
  way. Other models can leave them empty.
- `engram-mcp` reads the config, embeds through any OpenAI-compatible
  `/embeddings` endpoint (Ollama at `http://localhost:11434/v1` by default),
  and refuses to write if its model doesn't match the declared one.
- The appview copies the declared model into the space's manifest.

### Memory records carry their vector

`garden.engram.memory` gains a required `embedding` field:

```json
"embedding": {
  "model": "nomic-embed-text",
  "modelDigest": "sha256:…",
  "dims": 768,
  "encoding": "f16le",
  "vector": {"$bytes": "<1536 bytes, unpadded base64>"}
}
```

The text that gets embedded is the space's `documentPrefix`, the memory's
text (cut at 24,000 characters), and, when it has tags, a blank line and
`Tags: a, b`.

- `f16le` is little-endian IEEE half precision: 2 bytes per dimension,
  1.5 KB at 768 dimensions. That's close enough to full precision for
  search, and a third of the size of the same vector as JSON numbers.
- The appview indexes a memory only if its `model`, `modelDigest` and
  `dims` match the space's declared model. Memories that don't match are
  skipped, counted per author, and reported (in the log and in
  `garden.engram.getSpaceStatus`), so a misconfigured agent is easy to
  spot.
- During a model change, a memory also carries `nextEmbedding`, the same
  shape for the next model (see [Changing models](#changing-models)). The
  appview reads both fields and uses whichever matches each index. A second
  field kept the lexicon simpler than turning `embedding` into an array.

### Searching with a vector

`garden.engram.searchMemories` takes the query's vector instead of its
text: a `vector` parameter, base64url-encoded `f16le`, plus `model` and
`modelDigest`, which must match the space's model. The text query `q`
becomes optional. It isn't used for ranking yet, but it's kept for keyword
search later (see [Open questions](#open-questions)).

### Changing models

Vectors from different models can't be compared, so every search uses
exactly one model, the *active* one.

1. The authority updates `garden.engram.config` with the new model under a
   `next` key, alongside the current one (`engram model --next`).
2. Agents re-embed their memories and rewrite them with `nextEmbedding`.
   `engram-mcp` does this in the background for its own memories, at
   startup and every 15 minutes, and new memories carry both from then on.
3. The appview builds a second, *building* index from the new-model
   vectors, while searches keep using the active index. Seeing the new
   `next`, it forgets every repo's sync position, so the next space sync
   reads every repo again in full and picks up vectors it skipped before.
   Records it already holds unchanged keep their memory ids.
4. When the building index covers enough of the space
   (`garden.engram.getSpaceStatus` reports how many memories have the new
   vector), the authority promotes `next` to the current model
   (`engram model --promote`). The appview makes the building index active
   in one manifest update. `engram-mcp` notices on its next search (the
   appview answers `ModelMismatch`), re-reads the config and retries.

The appview honors the config only from the authority's repo, and syncs the
authority's repo first so the model is known before any memory. A
notification for a space whose config it hasn't seen yet triggers a full
space sync.

The catch: agents that never come back never re-embed, so their memories
drop out of search under the new model. That's the main cost of embedding
on the client. The optional server-side embedding below can fill the gap.

### Server-side embedding (later, optional)

An appview operator may later enable embedding on the appview: for memories
without a usable vector, for agents that can't run a model, and for
re-embedding during a model change. It isn't part of this design's first
version. The storage format doesn't change if it's added: an
appview-embedded vector is indexed exactly like a supplied one.

### Vector integrity (deferred)

A member could write a vector that doesn't match its text, either garbage
or a vector tuned to appear in every search. In a members-only space that's
a member misbehaving, and it's attributable: the vector is in the author's
signed repo, so anyone can recompute the embedding and prove the mismatch.
For now, the space authority handles it by removing the member.

A possible defense, if needed later: the first time a memory appears in
search results, the appview embeds its text and compares (memories are
immutable per CID, so each is checked once); it checks authors with a clean
record less often; and it caps results per author. That requires
server-side embedding, so it waits for that.

## Quantization

Each memory's 768-dimension float vector is stored twice, more compactly:

| Form | Size | Used for |
|---|---|---|
| **1-bit**: the sign of each dimension | 96 bytes | Scanning every memory in a search |
| **1-byte**: each dimension scaled to int8, plus a float32 scale per vector | 772 bytes | Re-ranking the top candidates |

Vectors are normalized before quantizing. The supplied half-precision vector
isn't kept in the index: the 1-byte form is close enough for re-ranking, and
the original is always available in the memory record on the author's PDS.

### Measured scan speed

On an 8-vCPU AMD EPYC 7R13, in plain Go with no assembly, scanning 100,000
vectors:

| Form | One core | 8 cores |
|---|---|---|
| float32, 768 dimensions | 2.2M vectors/s | not measured |
| 1-byte, 768 dimensions | 1.4M vectors/s | 4.6M vectors/s |
| 1-bit, 768 dimensions | 68M vectors/s | 313M vectors/s |

For comparison, verifying a P-256 signature takes 88 µs and a K-256
signature 107 µs.

So the search scans 1-bit vectors and re-ranks the top ~200 with the 1-byte
vectors. For typical spaces, the scan costs less than verifying the request's
signatures. These figures come from a small development box. Proper numbers
will come from a dedicated benchmark machine.

The 1-bit scan is already fast because `math/bits.OnesCount64` compiles to
the CPU's popcount instruction. The 1-byte re-rank can use SIMD through Go
1.26's experimental `simd/archsimd` package (amd64, built with
`GOEXPERIMENT=simd`). Its API isn't stable (Go 1.27 changed it and added an
experimental portable `simd` package), so the SIMD version lives behind a
build tag, with the plain-Go version as the fallback and the reference in
tests.

Measured with the implementation (`internal/vec` benchmarks, one core of
the development box): the 1-bit scan runs at 43M vectors/s, and the 1-byte
re-rank at 1.7M vectors/s in plain Go and 4.3M vectors/s with
`GOEXPERIMENT=simd` (AVX2 and FMA), 2.6 times faster. The SIMD version
only uses full 16-byte loads; the API's partial-slice load was 16 times
slower than plain Go.

## Object layout

Everything for a space lives under one prefix. The space key is the
lowercase hex SHA-256 of the space URI, which keeps keys uniform and free of
URI characters.

```
spaces/<space key>/
  manifest-<token>-<generation>.json
  seg-<segment id>.seg
```

### Manifest

The manifest is the space's root. A space's state is exactly what its
newest manifest lists.

```json
{
  "format": 1,
  "space": "at://did:plc:…/space/garden.engram.space/memory",
  "token": 7,
  "generation": 42,
  "active": {
    "model": "nomic-embed-text",
    "modelDigest": "sha256:…",
    "dims": 768,
    "segments": [
      {"id": "…", "count": 1000, "bytes": 2600000, "minCreatedAt": "…", "maxCreatedAt": "…",
       "createdAt": "…", "clustered": false}
    ]
  },
  "building": null,
  "deleted": "<base64 roaring bitmap of deleted memory ids>",
  "nextMemoryId": 51234,
  "repos": {
    "did:plc:alice": {"rev": "3mxa…", "setHash": "<base64>", "spaceRev": "3mxa…"}
  },
  "config": {"model": "nomic-embed-text", "modelDigest": "sha256:…", "dims": 768,
             "documentPrefix": "search_document: ", "queryPrefix": "search_query: "},
  "skipped": {"did:plc:bob": ["3mxb…"]},
  "spaceDeleted": false,
  "lastMergeAt": "…",
  "lastGcAt": "…",
  "updatedAt": "…"
}
```

- `token` is the owner's fencing token (see [Fencing](#fencing)), and
  `generation` increases by one with every update by that owner. Each
  update is a new object, `manifest-<token>-<generation>.json`, both numbers
  zero-padded so keys sort numerically. Never overwrite. **The current
  manifest is the one with the highest token, then the highest generation.**
  Older ones are deleted after the 90-day minimum (see
  [Wasabi constraints](#wasabi-constraints)).
- `repos` carries each member repo's sync position: the last oplog rev, the
  set hash state, and the latest known spaceRev. This is what the
  old `repos` Postgres table held. With it in the manifest, any node can
  resume a space from Wasabi alone.
- Memory ids are dense integers assigned in arrival order. The `deleted`
  bitmap marks ids whose record was deleted or replaced. An update gives the
  memory a new id and marks the old one deleted. During a model change a
  memory has the same id in both indexes, so one bitmap serves both. A
  merge removes the ids it dropped from the bitmap.
- `config` is the authority's declared model as last seen, and `skipped`
  lists, per author, the memories whose vectors don't match it.
  `spaceDeleted` remembers a space its authority deleted. `lastMergeAt` and
  `lastGcAt` pace maintenance.
- Segment ids are time-ordered: nanoseconds since the epoch in hex, then
  random bytes.

### Segment file

A segment is immutable and holds a batch of memories. Its sections are
contiguous, so each can be fetched with one range read:

```
[header]       magic "EGSEG", format version, count, dims, and per section:
               offset, length, CRC-32C
[metadata]     per memory, 40 bytes: memory id, author, record key, tag list
               offset and count, cluster, text+source size, createdAt,
               64-bit hash of the record CID
[strings]      authors, record keys and tags for this segment, deduplicated,
               plus the tag lists
[1-bit]        count × 96 bytes
[1-byte]       count × (768 + 4) bytes
[docs]         text, source, CID, indexedAt: zstd-compressed in ~64 KB blocks
[doc index]    per block: first row, offset and length
[clusters]     only in clustered segments: centers and each cluster's rows
[footer]       a copy of the header, then "EGSEGEND"
```

- Search filters (author, tags, created-after) are evaluated from the
  metadata section during the scan, before scoring.
- The CID hash and size in the metadata let a node tell whether a record
  changed, and count a space's bytes against its limit, without reading any
  documents. A cold load reads only the header, metadata, strings, doc index
  and 1-bit sections.
- URIs aren't stored: they're built from the space, author and record key.
- `docs` is compressed in blocks, so fetching a few results reads a few
  blocks, not the whole section.
- The format is versioned. Readers reject versions they don't know rather
  than guessing.

### Size estimates

Measured with a synthetic segment (`BenchmarkWrite100k`): 100,000 memories
of about 500 characters each come to **104 MB, of which 9.6 MB is 1-bit
vectors**, 77 MB is 1-byte vectors and 4 MB is metadata. The synthetic text
uses a tiny vocabulary, so its 11 MB of compressed docs is optimistic; real
text compresses to perhaps a quarter to a half of its size, adding
15–25 MB. Call it **~120–130 MB per 100k memories**, about half the first
estimate.

A search on a cold space needs the metadata, the 1-bit section, the 1-byte
vectors of the top candidates and the docs of the results: about 14 MB, or
12%, of a 100k space, because metadata and 1-bit vectors dominate. In a
test space of 2,000 memories it read 25%, since 200 candidates are a tenth
of such a small space.

## Write path

1. **Sync** (unchanged): a write notification or poll makes the indexer pull
   and verify the member's changes.
2. **Buffer:** verified creates, updates and deletes go into the space's
   in-memory write buffer. New memories are searchable immediately from the
   buffer.
3. **Flush:** when the buffer reaches 1,000 memories or an hour old, it
   becomes a new segment (one per index during a model change). A config
   change and an export flush right away, and shutdown flushes every space.
   The node uploads the segment, then a new manifest that lists it and carries the updated deletions and repo sync positions,
   then clears the flushed changes from the buffer.
   - Flushes for a space run one at a time. Each new manifest is derived
     from the owner's in-memory copy of the previous one, so no flush can
     drop another's segments or deletions.
   - **The manifest is the commit point.** Repo sync positions only advance
     in the same manifest that publishes the segments and deletions for
     those changes. A crash between uploading a segment and its manifest
     leaves an unreferenced segment, which garbage collection removes, and
     the old positions, so the changes are synced again.
4. **Merge:** see below.

There's no write-ahead log. The buffer only holds changes that are already
durable on the members' PDSes, and since vectors come in the records,
pulling a change again costs a request, not an embedding. Notifications are
accepted before syncing, as before.

- If a node crashes or restarts, its replacement loads the current manifest
  and the indexer re-syncs each repo from the sync position recorded there,
  using `listRepoOps`. Changes that were only in the lost buffer, at most
  about an hour's worth, are pulled again.
- If a PDS can't serve the oplog from that position, or the result doesn't
  verify against the signed commit, the indexer falls back to a full
  verified `getRepo` export of that repo, as before.
- Until a repo catches up, its newest memories are missing from search.
  They aren't lost.

### Merging segments

Small segments accumulate. A merge rewrites several segments into one,
dropping deleted memories, and publishes a manifest without the inputs. The
inputs themselves are left for garbage collection: deleting them before the
90-day minimum saves nothing, and a search that started before the merge may
still be reading them.

- Merge when a space has more than 8 segments, or when deleted memories
  exceed 25% of its segments. Keeping the segment count small also bounds
  how many range reads a cold search needs.
- Run merges at most once a day per space, and prefer segments older than
  90 days as inputs (see below). Merged segments are rebuilt from the
  inputs' 1-byte vectors, which re-quantize to the same values.
- Garbage collection runs on the same daily schedule. It deletes segments
  no current manifest references and manifests older than the current one,
  but only once they're past the 90-day minimum. A segment uploaded by a
  flush that crashed before its manifest is collected the same way.

## Wasabi constraints

Wasabi's pay-as-you-go terms shape the design:

| Term | Consequence |
|---|---|
| **90-day minimum storage.** An object deleted early is still billed for the remaining days. | Never rewrite whole files on change. New data goes to new segments. Deletions are recorded in the manifest, not by rewriting segments. Merges are infrequent and favor old segments. Each manifest generation is small, so keeping old generations for 90 days costs little. |
| **Free egress only up to the active storage volume per month.** | Cold loads count as egress. Local SSD caching and loading only the 1-bit section keep egress well under stored volume. |
| **1 TB minimum monthly charge** ($7.99 at the time of writing). | Negligible. |
| No per-request fees. | Many small range reads are fine. |

**Conditional writes work on Wasabi, as tested.** Wasabi's object-operations
documentation doesn't mention `If-None-Match` or `If-Match` on PutObject, and
an independent survey of S3-compatible providers' conditional writes
([zeropg storage backends notes](https://github.com/reisepass/zeropg/blob/main/docs/STORAGE-BACKENDS.md))
lists Wasabi as unconfirmed. The conformance test below, run on 2026-10-08
against `s3.us-west-2.wasabisys.com`, found that a PutObject with
`If-None-Match: *` on an existing key is rejected with HTTP 412
`PreconditionFailed` and leaves the object unchanged. The startup probe
therefore turns conditional writes on there. That was one region and
single-part writes (manifests are small), and Wasabi doesn't document the
behavior, so the probe stays. Re-run the test if Wasabi changes.

The survey also warns that some providers silently ignore the header,
returning 200 and overwriting. Oracle's S3 layer is documented to do this.
So the design doesn't depend on conditional writes (see
[Fencing](#fencing)), and doesn't trust them without proof. At startup, the
store probes the bucket: it writes a probe key, writes it again with
`If-None-Match: *`, and uses conditional writes only if the second write is
rejected with 412 or 409. A conformance test runs the same probe, plus the
store contract and a multipart upload, against each supported backend; see
the README's Development section for running it on a real bucket.

Storage cost at scale: a billion memories at ~2.5 KB each is ~2.5 TB, about
$20/month.

## Read path

1. Route the request to the space's owner. Concurrent requests for a space
   that isn't loaded share one load.
2. Scan the 1-bit vectors of every segment plus the write buffer. Skip
   deleted memories and apply filters. Keep the top 200 by Hamming distance.
3. Re-rank those with the 1-byte vectors and keep the top *k*.
4. Fetch the docs blocks for the results and return them.

### Cache tiers

| Tier | Holds | Evicted |
|---|---|---|
| RAM | Manifests, every segment's metadata and 1-bit sections, write buffers | Least recently used space |
| Local SSD | Full segment files for recently used spaces | Least recently used segment |
| Wasabi | Everything | Never (it's the source of truth for the index) |

A 1-byte section or docs block not on SSD is range-read from Wasabi. After a
space loads, its whole segment files are pulled onto SSD in the background,
and later reads come from there. Individual ranges aren't cached
separately; the whole-file pull covers them within seconds. Segments a node
writes itself go straight onto its SSD.

### Cold start

When a search arrives for a space with nothing in RAM or on SSD:

1. Fetch the newest manifest (a few KB).
2. Range-read each segment's metadata and 1-bit sections, up to 16 segments
   at once, then scan. (Scanning chunks as they arrive would save a little
   more time; it's a later refinement.)
3. Range-read the 1-byte vectors of the top candidates and the docs blocks
   of the results.
4. Respond. In the background, pull the rest of the space onto SSD.

For a 1 GB space (~400k memories), that's about 38 MB of 1-bit vectors, plus
200 candidates × 772 bytes ≈ 154 KB of 1-byte vectors, plus the docs blocks
of the results. The candidates are scattered across segments, so their reads
are grouped by segment, adjacent ranges are merged, and the groups are
fetched in parallel. Merging keeps a space to about 8 segments, so that's at
most a few dozen requests, and Wasabi doesn't charge per request. The
estimate is **0.5–1.5 s** for the first query, to be measured on the
benchmark machine.

- **Warm early.** Loading starts on the first sign a space is about to be
  used: an agent's MCP session starts (`engram-mcp` calls
  `garden.engram.warmSpace`), or a write notification arrives. Credential
  issuance happens at the authority, which doesn't tell the appview, so it
  isn't a signal.
- **Deadline.** The 1-bit scan always covers every segment and the write
  buffer. A partial scan could miss the best matches, so it is never
  returned. If re-ranking reads haven't finished by the deadline (default
  3 s), the search returns the complete 1-bit ranking, marked
  `approximate: true`, and the MCP tool says results may be less precise. If
  the 1-bit sections themselves can't be read by a hard limit (default
  10 s), the search fails with a retryable error, and the load carries on in
  the background.
- **Memory budget.** Each node has a RAM budget for 1-bit sections. A space
  whose 1-bit sections exceed its share (initially 1 GB) keeps them on SSD
  and streams them through the scan instead of pinning them in RAM.
- **Very large spaces.** A segment of 250k memories or more (in practice,
  a merge of a very large space) also stores a table of about √n cluster
  centers, trained by spherical k-means on a sample, with its rows grouped
  by nearest center. A search scans only the nearest max(4, k/8) clusters
  of such a segment. In tests the probed clusters held the true nearest
  neighbor for at least 95 of 100 queries; the recall/speed trade-off at
  real scale is for the benchmark machine.

## Routing and ownership

- **Assignment:** rendezvous hashing of the space key over the live node
  list. Every node computes the same owner, and adding or removing a node
  only moves the spaces whose owner changes.
- **Membership:** to begin with, a static node list in configuration.
  Later, nodes register with leases in a small coordination store.
- **Forwarding:** any node accepts a request. If it isn't the owner, it
  forwards the request to the owner over internal HTTP. Write notifications
  are forwarded the same way, so sync and search for a space always run on
  the same node.
- **Global state:** indexed spaces come from configuration plus the spaces
  whose authorities granted the appview access (see
  [indexing-access.md](indexing-access.md)), which are registered in the
  control-plane database. Every node rereads the registrations on each tick
  and when asked about a space it doesn't know. Who owns each space isn't
  stored anywhere: every node computes it from the node list. Each owner
  renews its spaces' notification registrations every 12 hours or so
  regardless. One node, the rendezvous owner of the key `coordinator`, does
  the jobs that one node should do for all, such as clearing abandoned
  sign-ins. (An earlier version also kept a registry of spaces and owners
  as objects in the bucket. Nothing read it, so it's gone.)
- Each node only syncs, registers for and flushes the spaces it owns. A
  forwarded request that arrives at a node that doesn't own the space (the
  nodes disagree, mid-change) gets a retryable `NotOwner` error rather than
  being forwarded again.

### Fencing

Two nodes may briefly both believe they own a space, for example during a
rolling deploy or a partition. Object storage can't stop the old owner from
uploading, so the design makes its uploads irrelevant instead:

- **Fencing tokens.** Taking ownership of a space requires a lease from a
  small coordination store, which hands out a strictly increasing token per
  space. The control-plane database could be that store, but nothing uses
  it for this yet: until then the token is the node list's configuration
  epoch, which increases on every membership change.
- **Readers ignore stale owners.** Manifest keys start with the token, and
  the current manifest is the one with the highest token. Whatever a stale
  owner uploads carries a lower token, so it's never chosen. Its orphaned
  segments are deleted by garbage collection once no current manifest
  references them and they're past the 90-day minimum.
- **Handover.** A new owner takes the lease, loads the current manifest,
  writes its first manifest under its own token (copying the state), and
  only then serves the space. Changes the old owner indexed but never
  published, or published under its lower token, are pulled again from the
  PDSes from the manifest's sync positions. The index is rebuildable, so
  losing them costs some requests, not data.
- **Before each flush**, an owner re-checks that its lease is current, and
  stops writing if it has lost the lease.
- During a handover, a request may briefly reach the old owner and see
  slightly stale results. That's acceptable for search.
- If the startup probe shows the bucket honors conditional writes,
  `If-None-Match: *` on each manifest key also stops two writers that hold
  the same token from racing for the same generation.

## Export and import

- **Export:** `garden.engram.exportSpace` streams a tar of the newest
  manifest and its segments. It requires a space credential, like the other
  read endpoints.
- **Import:** `engram-appview import <file>` validates the manifest and
  segment checksums and uploads them under the importing appview's own
  storage. Indexing then resumes from the manifest's repo sync positions.
- The export format is the storage format, so it's documented and versioned
  with it.
- An appview without an export can always build the index from the members'
  PDSes, since the vectors travel in the records. An export saves pulling
  every repo and gives the new appview a ready index straight away.

## Per-space limits

Limits per space (memories, bytes, searches per second, writes per day) are
enforced by the space's owner, after forwarding, so there's exactly one
place counting each space:

- Counts are live: the loaded index plus the write buffer. A new owner
  starts from the manifest's segments. Rate counters reset on handover,
  which is acceptable.
- Exceeding a limit returns a clear error (`RateLimitExceeded` for
  searches). Memories past a space's memory, byte or daily-write limit
  aren't indexed, and their repo's sync position doesn't advance, so
  they're pulled again on each sync and indexed once the limit is raised or
  memories are deleted.
- Per-space concurrency caps stop one space from monopolizing a node's CPU
  or Wasabi bandwidth: by default 8 concurrent searches per space, and 2
  notified syncs per space out of 32 per node. Notifications beyond a
  space's cap don't queue: they leave a flag, and a running sync for that
  space follows up with a full space sync, so a flood of notifications
  costs no more than the caps allow. Spaces on the same node still share
  hardware, so a busy space can add some latency for others, but limits
  bound how much.

## Build order

The plan was one PR per step. All of it shipped together, with this
document:

1. **Client-side embedding:** the `embedding` and `nextEmbedding` fields on
   `garden.engram.memory`, the `garden.engram.config` record, vector
   parameters on `garden.engram.searchMemories`; `engram-mcp` embeds
   through an OpenAI-compatible endpoint (Ollama by default) and checks the
   model digest; `engram model --set` declares the model; the indexer takes
   vectors from records; the appview's embedder is gone.
2. **Vectors and segments** (`internal/vec`, `internal/segment`): half
   precision, both quantizations, scan and re-rank, the segment format, and
   benchmarks.
3. **Object storage** (`internal/blob`): local directory and S3 (range
   reads), tested against an in-process fake S3, and the conditional-write
   probe.
4. **Per-space store** (`internal/spacestore`): write buffer, flush,
   deletions, merging, garbage collection, manifests, cache tiers, shared
   cold loads, warming, deadlines, limits and fencing.
5. **Indexer and appview** on the per-space store. Postgres is removed as
   the index. No data migration is needed: the index rebuilds from the
   PDSes. (Grants and sessions, which can't be rebuilt, later got a small
   database of their own: [indexing-access.md](indexing-access.md).)
6. **Routing** (`internal/routing`): rendezvous hashing and forwarding.
7. **Export and import.**
8. **Cluster index for very large spaces.**

Afterwards: the web app (`engram-web`), with live updates that poll the
appview while someone is watching a space.

## Open questions

- Benchmark-machine numbers: re-rank throughput with `simd/archsimd`, and
  cold-load latency from Wasabi.
- Embedding latency on typical agent hardware (laptop CPU through Ollama)
  for memory-length and query-length text, to confirm client-side embedding
  stays unnoticeable.
- Text search alongside vector search, for exact names and identifiers.
  Probably a small per-segment inverted index, designed after the first
  version.
