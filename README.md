# Engram Garden

A shared memory bank for AI agents, built on [ATProto Spaces](https://github.com/bluesky-social/atproto/pull/5187).

Agents store memories as records in a private space they share. The Engram
Garden appview indexes those records with embeddings (Postgres and pgvector)
so any agent in the space can search them by meaning. An MCP server gives
each agent `remember` and `recall` tools.

```
 agent ──MCP──► engram-mcp ──createRecord──► agent's PDS ──notifyWrite──► space authority
                    │                                                         │
                    │                                              forwards notifyWrite
                    │                                                         ▼
                    └──searchMemories (space credential)──────────►  engram-appview ──► Postgres + pgvector
                                                                      pulls + verifies each
                                                                      member's repo changes
```

## How it fits together

- **The memory space.** A space of type `garden.engram.space`, for example
  `at://did:plc:you/space/garden.engram.space/memory`. Its authority (your
  account) decides who's a member. Keep its read policy members-only.
- **Agents.** Each agent has its own ATProto account and is a member of the space.
  A memory is a `garden.engram.memory` record in the agent's own repo for
  the space, so every memory is attributed to the agent that wrote it, and
  an agent can only delete its own.
- **The appview** has an account of its own, which is also a member. It
  exchanges that account's delegation token for a space credential,
  registers with the authority for write notifications, and reads each
  member's repo changes. Every change is checked against the member's signed
  commit before it's indexed. When a sync can't be verified, it falls back to
  a full verified export of that repo.
- **Search is members-only.** Callers present a space credential for the
  space, signed with an HTTP message signature addressed to the appview's
  DID. That's the same proof a member's PDS asks for, so anyone who can read
  the space can search it and no one else can.

## Lexicons

| NSID | Kind | Purpose |
|---|---|---|
| `garden.engram.space` | space type | Declares a memory space. Its collection is `garden.engram.memory`. |
| `garden.engram.memory` | record | `text`, optional `tags`, optional `source`, `createdAt` |
| `garden.engram.searchMemories` | query | Semantic search with `q`, `limit`, `author`, `tags` and `since` |
| `garden.engram.getMemory` | query | One memory by URI |
| `garden.engram.listMemories` | query | Newest first, paged, with `author` and `tags` filters |

They live in [`lexicons/`](lexicons/garden/engram).

## Running the appview

Requirements: Postgres with the `vector` extension available, and an
OpenAI-compatible embeddings endpoint.

| Variable | Default | |
|---|---|---|
| `ENGRAM_DATABASE_URL` | | Postgres URL (required) |
| `ENGRAM_SPACES` | | Comma-separated space URIs to index (required) |
| `ENGRAM_SERVICE_DID` | | The appview's DID, e.g. `did:web:engram.garden` (required) |
| `ENGRAM_PUBLIC_URL` | | Public HTTPS URL. When set, the appview serves its `did:web` document and registers for notifications. When unset, it polls. |
| `ENGRAM_IDENTIFIER` / `ENGRAM_PASSWORD` | | The appview account's handle or DID, and its password (required) |
| `ENGRAM_PDS_HOST` | resolved | Skip PDS resolution for the account |
| `ENGRAM_LISTEN` | `:8080` | |
| `ENGRAM_POLL_INTERVAL` | `5m` | Full space sync interval. This is a backstop for missed notifications. |
| `ENGRAM_EMBED_PROVIDER` | `openai` | `openai` (any OpenAI-compatible API) or `hashing` (offline, word-overlap only; for development) |
| `ENGRAM_EMBED_URL` | `https://api.openai.com/v1` | Ollama's is `http://localhost:11434/v1` |
| `ENGRAM_EMBED_API_KEY` | `$OPENAI_API_KEY` | |
| `ENGRAM_EMBED_MODEL` | `text-embedding-3-small` | |
| `ENGRAM_EMBED_DIMS` | `1536` | Fixed per database. Changing it requires a fresh database. |

```bash
go run ./cmd/engram-appview
```

Space hosts deliver notifications only to public HTTPS endpoints. Cocoon,
for example, refuses private and loopback addresses. A local appview without
`ENGRAM_PUBLIC_URL` stays current by polling.

## Giving an agent memory tools

`engram-mcp` speaks MCP over stdio as one agent's account:

| Variable | Default | |
|---|---|---|
| `ENGRAM_SPACE` | | The memory space URI (required) |
| `ENGRAM_IDENTIFIER` / `ENGRAM_PASSWORD` | | The agent account's handle or DID, and its password (required) |
| `ENGRAM_APPVIEW_URL` | `https://engram.garden` | |
| `ENGRAM_APPVIEW_DID` | `did:web:<appview host>` | |

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

- `remember` stores a memory (`text`, optional `tags` and `source`).
- `recall` runs a semantic search across every agent's memories.
- `get_memory` fetches one memory by URI.
- `list_memories` lists memories newest first.
- `forget` deletes one of the agent's own memories.

## Setting up a space

1. Create accounts for the appview and each agent.
2. As the authority, create the space with `com.atproto.simplespace.createSpace`
   (type `garden.engram.space`, read policy `member-list`).
3. Add the appview and the agents with `com.atproto.simplespace.putMember`.
4. Start the appview with `ENGRAM_SPACES` set to the space URI, then point each
   agent's `engram-mcp` at it.

## Design

[`docs/design/storage.md`](docs/design/storage.md) describes the planned
storage and scaling design: per-space index files in object storage, owned
by one node at a time.

## Development

```bash
make test        # needs ENGRAM_TEST_DATABASE_URL for the Postgres-backed tests
make lint
```

Tests run against an in-memory Spaces network (`internal/spacetest`). It
builds real tokens, credentials, signed commits and repo CARs with Cocoon's
`space` package, so the appview's verification runs end to end without
any external services.
