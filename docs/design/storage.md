# Storage and scaling design

Status: proposed. This replaces the Postgres and pgvector store in
`internal/store`.

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

The current store puts every space's vectors in one Postgres table with one
HNSW index. That fits this workload badly:

- **The workload:** many spaces, each small (hundreds to a few thousand
  memories), most of them idle at any moment. Every search is confined to
  one space.
- A shared HNSW index has to stay in RAM to be fast, so we pay memory for
  idle spaces.
- Filtering a shared index down to one space degrades as the index grows.
  The current code already widens `hnsw.ef_search` to compensate.
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

There's no database. The few pieces of global state (which spaces are
indexed, node membership) are small and covered under
[Routing](#routing-and-ownership).

## Embeddings

**Agents embed their own memories and queries.** The appview never runs a
model. It syncs memory records (verified against their authors' signed
commits, as today), then quantizes, stores and searches the vectors they
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
  "vector": {"$bytes": "<1536 bytes, base64>"}
}
```

- `f16le` is little-endian IEEE half precision: 2 bytes per dimension,
  1.5 KB at 768 dimensions. That's close enough to full precision for
  search, and a third of the size of the same vector as JSON numbers.
- The appview indexes a memory only if its `model`, `modelDigest` and
  `dims` match the space's declared model. Memories that don't match are
  skipped, counted per author, and reported, so a misconfigured agent is
  easy to spot.
- To keep the transition to a new model simple, `embedding` may later
  become an array, one entry per model (see
  [Changing models](#changing-models)).

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
   `next` key, alongside the current one.
2. Agents re-embed their memories and rewrite them with an embedding for
   each model. `engram-mcp` does this in the background for its own
   memories.
3. The appview builds a second, *building* index from the new-model
   vectors, while searches keep using the active index.
4. When the building index covers enough of the space, the authority
   promotes `next` to the current model. The appview makes the building
   index active in one manifest update.

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
    "dims": 768,
    "segments": [
      {"id": "01J…", "count": 1000, "bytes": 2600000, "minCreatedAt": "…", "maxCreatedAt": "…"}
    ]
  },
  "building": null,
  "deleted": "<base64 roaring bitmap of deleted memory ids>",
  "nextMemoryId": 51234,
  "repos": {
    "did:plc:alice": {"rev": "3mxa…", "setHash": "<base64>", "spaceRev": "3mxa…"}
  }
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
  `repos` Postgres table holds today. With it in the manifest, any node can
  resume a space from Wasabi alone.
- Memory ids are dense integers assigned in arrival order. The `deleted`
  bitmap marks ids whose record was deleted or replaced. An update gives the
  memory a new id and marks the old one deleted.

### Segment file

A segment is immutable and holds a batch of memories. Its sections are
contiguous, so each can be fetched with one range read:

```
[header]       magic "EGSEG", format version, count, dims, section offsets
[metadata]     per memory, fixed width: memory id, author index, createdAt,
               tag bitmap offset
[strings]      authors, tags and record keys for this segment, deduplicated
[1-bit]        count × 96 bytes
[1-byte]       count × (768 + 4) bytes
[docs]         text, source, URI, CID: zstd-compressed in ~64 KB blocks
[doc index]    per block: first memory id and byte offset
[footer]       section checksums, header copy
```

- Search filters (author, tags, created-after) are evaluated from the
  metadata section during the scan, before scoring.
- `docs` is compressed in blocks, so fetching a few results reads a few
  blocks, not the whole section.
- The format is versioned. Readers reject versions they don't know rather
  than guessing.

### Size estimates

For 100,000 memories of about 500 characters each, including metadata and
compressed text: **~250 MB in total, of which ~10 MB is 1-bit vectors.** A
search on a cold space needs the 1-bit section, the 1-byte vectors of the top
candidates and the docs of the results: about 4% of the space. A synthetic
100k-memory space will replace these estimates once the format exists.

## Write path

1. **Sync** (unchanged): a write notification or poll makes the indexer pull
   and verify the member's changes.
2. **Buffer:** verified creates, updates and deletes go into the space's
   in-memory write buffer. New memories are searchable immediately from the
   buffer.
3. **Flush:** when the buffer reaches 1,000 memories or an hour old, it
   becomes a new segment. The node uploads the segment, then a new manifest
   that lists it and carries the updated deletions and repo sync positions,
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
accepted before syncing, as today.

- If a node crashes or restarts, its replacement loads the current manifest
  and the indexer re-syncs each repo from the sync position recorded there,
  using `listRepoOps`. Changes that were only in the lost buffer, at most
  about an hour's worth, are pulled again.
- If a PDS can't serve the oplog from that position, or the result doesn't
  verify against the signed commit, the indexer falls back to a full
  verified `getRepo` export of that repo, as it does today.
- Until a repo catches up, its newest memories are missing from search.
  They aren't lost.

### Merging segments

Small segments accumulate. A merge rewrites several segments into one,
dropping deleted memories, then deletes the inputs.

- Merge when a space has more than 8 segments, or when deleted memories
  exceed 25% of its segments. Keeping the segment count small also bounds
  how many range reads a cold search needs.
- Run merges at most once a day per space, and prefer segments older than
  90 days as inputs (see below).

## Wasabi constraints

Wasabi's pay-as-you-go terms shape the design:

| Term | Consequence |
|---|---|
| **90-day minimum storage.** An object deleted early is still billed for the remaining days. | Never rewrite whole files on change. New data goes to new segments. Deletions are recorded in the manifest, not by rewriting segments. Merges are infrequent and favor old segments. Each manifest generation is small, so keeping old generations for 90 days costs little. |
| **Free egress only up to the active storage volume per month.** | Cold loads count as egress. Local SSD caching and loading only the 1-bit section keep egress well under stored volume. |
| **1 TB minimum monthly charge** ($7.99 at the time of writing). | Negligible. |
| No per-request fees. | Many small range reads are fine. |

**Conditional writes are unconfirmed on Wasabi.** Wasabi's object-operations
documentation doesn't mention `If-None-Match` or `If-Match` on PutObject, and
doesn't list conditional writes among its unsupported operations. An
independent survey of S3-compatible providers' conditional writes
([zeropg storage backends notes](https://github.com/reisepass/zeropg/blob/main/docs/STORAGE-BACKENDS.md))
lists Wasabi as unconfirmed. It also warns that some providers silently
ignore the header, returning 200 and overwriting. Oracle's S3 layer is
documented to do this.

So the design doesn't depend on conditional writes (see
[Fencing](#fencing)), and doesn't trust them without proof. At startup, the
store probes the bucket: it writes a probe key, writes it again with
`If-None-Match: *`, and uses conditional writes only if the second write is
rejected with 412 or 409. A CI conformance test runs the same probe against
each supported backend.

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

A 1-byte section or docs block not on SSD is range-read from Wasabi and then
cached.

### Cold start

When a search arrives for a space with nothing in RAM or on SSD:

1. Fetch the newest manifest (a few KB).
2. Range-read each segment's metadata and 1-bit sections in parallel,
   scanning each chunk as it arrives.
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
  used: an agent's MCP session starts (new endpoint
  `garden.engram.warmSpace`), a write notification arrives, or a credential
  is issued for it.
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
- **Very large spaces.** Past a threshold (initially 250k memories), a
  segment also stores a small table of cluster centers and groups its
  memories by nearest center. A search scans only the clusters nearest the
  query. This is not in the first version.

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
- **Global state:** the list of indexed spaces and their notification
  registrations is small, and also rebuildable. Indexed spaces come from
  configuration, and registrations are renewed daily by each space's owner
  regardless. The list lives in a single object
  (`registry-<token>-<generation>.json`, ordered like manifests) maintained
  by one coordinator node. If it's lost, owners re-register their spaces on
  the next renewal.

### Fencing

Two nodes may briefly both believe they own a space, for example during a
rolling deploy or a partition. Object storage can't stop the old owner from
uploading, so the design makes its uploads irrelevant instead:

- **Fencing tokens.** Taking ownership of a space requires a lease from a
  small coordination store, which hands out a strictly increasing token per
  space. Before the coordination store exists, the token is the node list's
  configuration epoch, which increases on every membership change.
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

- Counts are live: the manifest's totals plus the write buffer. A new owner
  starts from the manifest's totals. Rate counters reset on handover, which
  is acceptable.
- Exceeding a limit returns a clear error. Memories past a space's limit
  aren't indexed until the limit is raised or memories are deleted.
- Per-space concurrency caps and a fair queue for sync work stop one space
  from monopolizing a node's CPU or Wasabi bandwidth. Spaces on the same
  node still share hardware, so a busy space can add some latency for
  others, but limits bound how much.

## Build order

Each step is one PR with tests.

1. This document.
2. **Client-side embedding**, on the current Postgres store, since it's
   independent of storage:
   - lexicons: the `embedding` field on `garden.engram.memory`, the
     `garden.engram.config` record, and a vector parameter on
     `garden.engram.searchMemories`;
   - `engram-mcp` reads the space's config and embeds memories and queries
     through an OpenAI-compatible endpoint (Ollama by default);
   - the indexer takes vectors from records, skipping and reporting ones
     that don't match the space's model, and the appview's embedder is
     removed.
3. **Segment package:** writer, reader, both quantizations, scan and
   re-rank, plus benchmarks to run on the benchmark machine.
4. **Per-space store:** write buffer, flush, deletions, merging and
   manifests. Behind a storage interface with a local-directory
   implementation and an S3 implementation (range reads), tested against a
   fake S3, plus the conditional-write probe.
5. **Cache tiers:** shared cold loads, early warming, approximate results on
   deadline.
6. **Switch the indexer and appview** to the per-space store and remove
   Postgres. No data migration is needed: the index rebuilds from the
   PDSes.
7. **Routing:** rendezvous hashing, forwarding and fencing.
8. **Export and import.**
9. **Cluster index for very large spaces.**

Afterwards: the web UI and live notifications.

## Open questions

- Does Wasabi honor `If-None-Match` on PutObject? Its documentation doesn't
  say, so this is answered by the startup probe against a real bucket.
- Benchmark-machine numbers: re-rank throughput with `simd/archsimd`, and
  cold-load latency from Wasabi.
- Embedding latency on typical agent hardware (laptop CPU through Ollama)
  for memory-length and query-length text, to confirm client-side embedding
  stays unnoticeable.
- Text search alongside vector search, for exact names and identifiers.
  Probably a small per-segment inverted index, designed after the first
  version.
