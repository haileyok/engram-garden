# Storage and scaling design

Status: proposed. This replaces the Postgres and pgvector store in
`internal/store`.

## Goals

1. **Each space uses its own resources.** A space's data, cost and limits are
   its own, so one busy space can't slow down another and costs can be
   attributed per space.
2. **Scale to a very large number of spaces and agents**, horizontally
   (more nodes) and vertically (bigger nodes).
3. **Keep hosting cheap.** Cost should track how many spaces are *in use*,
   not how many exist.
4. **Make export easy.** A space's index can be exported and imported by
   another appview without re-embedding.

Non-goals for now: searching across spaces, hosting embedding models (the
appview calls an embeddings endpoint), and agent-supplied vectors (see
[Embeddings](#embeddings)).

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
            │   segments, WAL                                        │
            └────────┬───────────────────────────────────────────────┘
                     │ range reads, new segments, manifests
                     ▼
            Wasabi: spaces/<space key>/{manifest, segments}
```

There's no database. The few pieces of global state (which spaces are
indexed, node membership) are small and covered under
[Routing](#routing-and-ownership).

## Embeddings

- The appview embeds memories and queries itself, through the
  OpenAI-compatible `/embeddings` API. That covers Ollama
  (`http://localhost:11434/v1`), which is the primary target.
- **768 dimensions.** `nomic-embed-text` produces 768 natively.
- **Task prefixes.** nomic-embed-text expects `search_document: ` before
  stored text and `search_query: ` before queries, and retrieval quality
  drops without them. Prefixes become configuration
  (`ENGRAM_EMBED_DOC_PREFIX`, `ENGRAM_EMBED_QUERY_PREFIX`) so other models
  can leave them empty.
- **One active model per space.** Vectors from different models can't be
  compared, so every search uses exactly one model. To change models, the
  manifest gains a second, *building* index (its own model, dimensions and
  segments). The owner re-embeds every live memory into it in the
  background, and new writes go to both indexes. Searches keep using the
  *active* index until the building index covers every memory. Then one
  manifest update makes it active and lists the old segments for deletion.
  Rolling back means dropping the building index.
- **Agent-supplied vectors (later).** `garden.engram.memory` will gain an
  optional field carrying a vector and the model that produced it. When it
  matches the space's model, the appview can store it instead of embedding.
  This isn't part of this work. The record format stays unchanged until
  then.

## Quantization

Each memory's 768-dimension float vector is stored twice, more compactly:

| Form | Size | Used for |
|---|---|---|
| **1-bit**: the sign of each dimension | 96 bytes | Scanning every memory in a search |
| **1-byte**: each dimension scaled to int8, plus a float32 scale per vector | 772 bytes | Re-ranking the top candidates |

Vectors are normalized before quantizing. The full float vector isn't kept:
the 1-byte form is close enough for re-ranking, and re-embedding from text
recovers the original.

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
will come from a dedicated benchmark machine, including SIMD for the 1-byte
re-rank.

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
   in-memory write buffer, and are appended to a write-ahead log (WAL) on
   local SSD before being acknowledged. Newly embedded memories are
   searchable immediately from the buffer.
3. **Flush:** when the buffer reaches 1,000 memories or an hour old, it
   becomes a new segment. The node uploads the segment, then a new manifest
   that lists it and carries the updated deletions and repo sync positions,
   then truncates the WAL.
4. **Merge:** see below.

Nothing is acknowledged to anyone on the strength of the WAL. Notifications
are accepted before syncing, as today. Durability rests on the members'
PDSes:

- If a node crashes, its replacement loads the current manifest and the
  indexer re-syncs each repo from the sync position recorded there, using
  `listRepoOps`. Changes that were only in the lost buffer are pulled and
  embedded again. Embedding doesn't have to reproduce identical vectors,
  only equivalent ones.
- If a PDS can't serve the oplog from that position, or the result doesn't
  verify against the signed commit, the indexer falls back to a full
  verified `getRepo` export of that repo, as it does today.
- Until a repo catches up, its newest memories are missing from search.
  They aren't lost.

The WAL only saves re-embedding after a restart on the same node.

### Merging segments

Small segments accumulate. A merge rewrites several segments into one,
dropping deleted memories, then deletes the inputs.

- Merge when a space has more than 8 segments, or when deleted memories
  exceed 25% of its segments.
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

Unverified: whether Wasabi honors S3 conditional writes (`If-None-Match` on
PutObject). Wasabi's documentation pages don't clearly say. The design
doesn't depend on it (see [Fencing](#fencing)). If it is supported, it adds
a cheap safety check.

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
| Local SSD | Full segment files for recently used spaces, WAL | Least recently used segment |
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
fetched in parallel. That's at most a few dozen requests, and Wasabi doesn't
charge per request. The estimate is **0.5–1.5 s** for the first query, to be
measured on the benchmark machine.

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
  registrations is small. It lives in a single object
  (`registry-<generation>.json`) maintained by one coordinator node.

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
  losing them costs re-embedding, not data.
- **Before each flush**, an owner re-checks that its lease is current, and
  stops writing if it has lost the lease.
- During a handover, a request may briefly reach the old owner and see
  slightly stale results. That's acceptable for search.
- If Wasabi supports conditional writes, `If-None-Match: *` on each manifest
  key also stops two writers that hold the same token from racing for the
  same generation.

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
  PDSes. An export only saves re-embedding.

## Per-space limits

Limits per space (memories, bytes, searches per second, embedding volume per
day) are enforced by the owning node from the manifest's counts. Exceeding
one returns a clear error rather than slowing down other spaces.

## Build order

Each step is one PR with tests.

1. This document.
2. **Segment package:** writer, reader, both quantizations, scan and
   re-rank, plus benchmarks to run on the benchmark machine.
3. **Per-space store:** write buffer, WAL, flush, deletions, merging and
   manifests. Behind a storage interface with a local-directory
   implementation and an S3 implementation (range reads), tested against a
   fake S3.
4. **Cache tiers:** shared cold loads, early warming, approximate results on
   deadline.
5. **Switch the indexer and appview** to the per-space store and remove
   Postgres. No data migration is needed: the index rebuilds from the
   PDSes.
6. **Routing:** rendezvous hashing, forwarding and fencing.
7. **Export and import.**
8. **Cluster index for very large spaces.**

Afterwards: the web UI and live notifications.

## Open questions

- Does Wasabi honor `If-None-Match` on PutObject? To test against a real
  bucket.
- Benchmark-machine numbers: scan throughput with SIMD, cold-load latency
  from Wasabi, and Ollama embedding throughput (CPU vs GPU).
- Text search alongside vector search, for exact names and identifiers.
  Probably a small per-segment inverted index, designed after the first
  version.
