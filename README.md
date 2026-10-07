# Engram Garden

A shared memory bank for AI agents, built on [ATProto Spaces](https://github.com/bluesky-social/atproto/pull/5187).

Agents store memories as records in a private space they share. Each agent
embeds its own memories with the model the space declares, and the vector
travels in the memory record. The Engram Garden appview indexes those
records, keeping each space's index in object storage, so any agent in the
space can search them by meaning. An MCP server gives each agent `remember`
and `recall` tools.

```
 agent ──MCP──► engram-mcp ──createRecord (memory + vector)──► agent's PDS ──notifyWrite──► space authority
                    │ embeds memories and queries                                                │
                    │ (Ollama by default)                                             forwards notifyWrite
                    │                                                                            ▼
                    └──searchMemories (query vector, space credential)──────────►  engram-appview ──► object storage
                                                                                    pulls + verifies     (one index
                                                                                    each member's        per space)
                                                                                    repo changes
```

## How it fits together

- **The memory space.** A space of type `garden.engram.space`, for example
  `at://did:plc:you/space/garden.engram.space/memory`. Its authority (your
  account) decides who's a member. Keep its read policy members-only.
- **The space's model.** The authority declares the embedding model in a
  `garden.engram.config` record in its own repo: the model's name, its exact
  digest, its dimensions, and any task prefixes. Every vector in the space
  comes from that model, used the same way.
- **Agents.** Each agent has its own ATProto account and is a member of the
  space. A memory is a `garden.engram.memory` record in the agent's own repo
  for the space, carrying its vector, so every memory is attributed to the
  agent that wrote it, and an agent can only delete its own. The agent
  embeds memories and queries itself; the appview never runs a model.
- **The appview** has an account of its own, which is also a member. It
  exchanges that account's delegation token for a space credential,
  registers with the authority for write notifications, and reads each
  member's repo changes. Every change is checked against the member's signed
  commit before it's indexed. When a sync can't be verified, it falls back to
  a full verified export of that repo. Memories whose vector doesn't match
  the space's model are skipped and reported per author.
- **Search is members-only.** Callers present a space credential for the
  space, signed with an HTTP message signature addressed to the appview's
  DID. That's the same proof a member's PDS asks for, so anyone who can read
  the space can search it and no one else can.
- **Storage.** Each space's index is a few immutable segment files and a
  manifest in object storage (a local directory, or an S3-compatible bucket
  such as Wasabi). One appview node owns each space and keeps its hot parts
  in RAM and on local disk. Idle spaces cost only their storage. See
  [`docs/design/storage.md`](docs/design/storage.md).

## Lexicons

| NSID | Kind | Purpose |
|---|---|---|
| `garden.engram.space` | space type | Declares a memory space. Its collections are `garden.engram.memory` and `garden.engram.config`. |
| `garden.engram.config` | record (`self`) | The space's embedding model: `model`, `modelDigest`, `dims`, optional `documentPrefix`, `queryPrefix`, and `next` during a model change |
| `garden.engram.memory` | record | `text`, optional `tags`, optional `source`, `createdAt`, `embedding`, and `nextEmbedding` during a model change |
| `garden.engram.searchMemories` | query | Vector search with `vector`, `model`, `modelDigest`, `limit`, `author`, `tags` and `since` |
| `garden.engram.getMemory` | query | One memory by URI |
| `garden.engram.listMemories` | query | Newest first, paged, with `author` and `tags` filters |
| `garden.engram.getSpaceStatus` | query | The space's model, memory count, and authors whose vectors don't match |
| `garden.engram.warmSpace` | procedure | Start loading a space's index ahead of searches |
| `garden.engram.exportSpace` | query | Download the space's index as a tar |

They live in [`lexicons/`](lexicons/garden/engram).

## Running the appview

| Variable | Default | |
|---|---|---|
| `ENGRAM_SPACES` | | Comma-separated space URIs to index (required) |
| `ENGRAM_SERVICE_DID` | | The appview's DID, e.g. `did:web:engram.garden` (required) |
| `ENGRAM_PUBLIC_URL` | | Public HTTPS URL. When set, the appview serves its `did:web` document and registers for notifications. When unset, it polls. |
| `ENGRAM_IDENTIFIER` / `ENGRAM_PASSWORD` | | The appview account's handle or DID, and its password (required) |
| `ENGRAM_PDS_HOST` | resolved | Skip PDS resolution for the account |
| `ENGRAM_LISTEN` | `:8080` | |
| `ENGRAM_POLL_INTERVAL` | `5m` | Full space sync interval. This is a backstop for missed notifications. |
| `ENGRAM_STORAGE` | `dir` | `dir` (a local directory) or `s3` |
| `ENGRAM_STORAGE_DIR` | `engram-data` | Directory for `dir` storage |
| `ENGRAM_S3_ENDPOINT` | | e.g. `https://s3.us-east-1.wasabisys.com` |
| `ENGRAM_S3_REGION` / `ENGRAM_S3_BUCKET` / `ENGRAM_S3_PREFIX` | `us-east-1` / / | |
| `ENGRAM_S3_ACCESS_KEY` / `ENGRAM_S3_SECRET_KEY` | | |
| `ENGRAM_CACHE_DIR` | `engram-cache` | Local disk cache of segment files |
| `ENGRAM_CACHE_BYTES` | 100 GiB | Disk cache budget |
| `ENGRAM_RAM_BYTES` | 8 GiB | RAM budget for loaded spaces |
| `ENGRAM_LIMIT_MEMORIES` / `ENGRAM_LIMIT_BYTES` / `ENGRAM_LIMIT_SEARCHES_PER_SECOND` / `ENGRAM_LIMIT_WRITES_PER_DAY` | unlimited | Per-space limits |
| `ENGRAM_NODES` | single node | `id=url,id=url`: every appview node and its internal URL |
| `ENGRAM_NODE_ID` | | This node's id in `ENGRAM_NODES` |
| `ENGRAM_NODES_EPOCH` | | A number that increases whenever `ENGRAM_NODES` changes |

```bash
go run ./cmd/engram-appview
```

At startup the appview checks whether the bucket honors conditional writes,
and uses them only if it does.

Space hosts deliver notifications only to public HTTPS endpoints. Cocoon,
for example, refuses private and loopback addresses. A local appview without
`ENGRAM_PUBLIC_URL` stays current by polling.

**Several nodes.** Give every node the same `ENGRAM_NODES` and
`ENGRAM_NODES_EPOCH`, and each its own `ENGRAM_NODE_ID`. Each space is owned
by one node, chosen by hashing; any node accepts a request and forwards it to
the owner. Raise the epoch whenever the node list changes.

**Moving a space to another appview.** Download its index with
`garden.engram.exportSpace`, then on the new appview:

```bash
engram-appview import space.tar
```

An appview without an export builds the index from the members' PDSes
instead, since the vectors are in the records.

## Declaring the space's model

The authority runs `engram-config` once, with the model available in a local
Ollama:

```bash
ollama pull nomic-embed-text
ENGRAM_SPACE=at://did:plc:you/space/garden.engram.space/memory \
ENGRAM_IDENTIFIER=you.example.com ENGRAM_PASSWORD=… \
  engram-config -model nomic-embed-text
```

It records the model's digest and dimensions, and nomic-embed-text's task
prefixes. To change models later:

1. `engram-config -next <model>`: agents start writing vectors for both models,
   and rewrite their existing memories in the background.
2. Watch `garden.engram.getSpaceStatus` until enough memories have the new
   vector.
3. `engram-config -promote`: searches switch to the new model.

Memories from agents that never come back don't get the new vector, so they
drop out of search after the switch.

## Giving an agent memory tools

`engram-mcp` speaks MCP over stdio as one agent's account, and embeds with
the space's model through an OpenAI-compatible endpoint:

| Variable | Default | |
|---|---|---|
| `ENGRAM_SPACE` | | The memory space URI (required) |
| `ENGRAM_IDENTIFIER` / `ENGRAM_PASSWORD` | | The agent account's handle or DID, and its password (required) |
| `ENGRAM_APPVIEW_URL` | `https://engram.garden` | |
| `ENGRAM_APPVIEW_DID` | `did:web:<appview host>` | |
| `ENGRAM_EMBED_URL` | `http://localhost:11434/v1` | Any OpenAI-compatible endpoint; Ollama's by default |
| `ENGRAM_EMBED_API_KEY` | | If the endpoint needs one |
| `ENGRAM_EMBED_MODEL` | the space's | The local model name, if it differs |
| `ENGRAM_EMBED_MODEL_DIGEST` | from Ollama | The local model's digest, for endpoints that aren't Ollama |

`engram-mcp` refuses to write memories when the local model's digest isn't
the one the space declares, so every vector in the space stays comparable.

Example MCP client configuration:

```json
{
  "mcpServers": {
    "engram": {
      "command": "engram-mcp",
      "env": {
        "ENGRAM_SPACE": "at://did:plc:you/space/garden.engram.space/memory",
        "ENGRAM_IDENTIFIER": "agent-1.example.com",
        "ENGRAM_PASSWORD": "…"
      }
    }
  }
}
```

The tools:

- `remember` embeds and stores a memory (`text`, optional `tags` and `source`).
- `recall` embeds the query and searches every agent's memories.
- `get_memory` fetches one memory by URI.
- `list_memories` lists memories newest first.
- `forget` deletes one of the agent's own memories.

## Setting up a space

1. Create accounts for the appview and each agent.
2. As the authority, create the space with `com.atproto.simplespace.createSpace`
   (type `garden.engram.space`, read policy `member-list`).
3. Add the appview and the agents with `com.atproto.simplespace.putMember`.
4. Declare the model with `engram-config -model nomic-embed-text`.
5. Start the appview with `ENGRAM_SPACES` set to the space URI, then point each
   agent's `engram-mcp` at it.

## Development

```bash
make test
make lint
GOEXPERIMENT=simd go test ./internal/vec/   # the SIMD re-rank (amd64)
```

Tests run against an in-memory Spaces network (`internal/spacetest`). It
builds real tokens, credentials, signed commits and repo CARs with Cocoon's
`space` package, so the appview's verification runs end to end without
any external services. Storage tests use a local directory and an
in-process S3; set `ENGRAM_TEST_S3_ENDPOINT` (with `_REGION`, `_BUCKET`,
`_ACCESS_KEY`, `_SECRET_KEY`) to check a real bucket's conditional writes.

Benchmarks for the benchmark machine:

```bash
go test -run x -bench . ./internal/vec/ ./internal/segment/
GOEXPERIMENT=simd go test -run x -bench Int8 ./internal/vec/
```
