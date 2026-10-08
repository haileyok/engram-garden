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
- **The appview** has no account. The space's authority grants it
  read-only OAuth access to the spaces they govern (once, in the web app).
  The appview uses that grant to get delegation tokens from the
  authority's PDS, exchanges them for space credentials, registers with the
  authority for write notifications, and reads each
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
| `garden.engram.getSpaceStatus` | query | The space's model, memory count, authors whose vectors don't match, and whether the appview's access is granted, missing or lapsed |
| `garden.engram.warmSpace` | procedure | Start loading a space's index ahead of searches |
| `garden.engram.exportSpace` | query | Download the space's index as a tar |
| `garden.engram.describeService` | query | The appview's DID, whether registration is open, and `grantUrl`, where a space's authority lets the appview index it |

They live in [`lexicons/`](lexicons/garden/engram).

## Running the appview

| Variable | Default | |
|---|---|---|
| `ENGRAM_SPACES` | | Comma-separated space URIs to always index (required when registration is closed) |
| `ENGRAM_REGISTRATION` | `open` | `open`: any space's authority can have the appview index it by granting access. `closed`: only `ENGRAM_SPACES` (which still need their authority's grant). |
| `ENGRAM_SERVICE_DID` | | The appview's DID, e.g. `did:web:api.engram.garden` (required) |
| `ENGRAM_PUBLIC_URL` | | Where the appview is served, e.g. `https://api.engram.garden` (required). It serves the `did:web` document and is the OAuth client authorities grant access to. An https URL also registers for notifications; `http://127.0.0.1:<port>` makes a development client and only polls. |
| `ENGRAM_OAUTH_KEY` | | The appview's OAuth client key, P-256 multibase (`goat key generate -t P-256`). Required for https. |
| `ENGRAM_RETURN_ORIGINS` | | Comma-separated origins a grant may return to, e.g. `https://engram.garden` |
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
for example, refuses private and loopback addresses. A local appview at
`http://127.0.0.1:<port>` stays current by polling.

**Indexing a space.** The appview reads a space only with its authority's
permission: the authority opens the appview's grant page (the web app's
**Let the appview index this space** button) and approves read-only access
to the memory spaces they govern on their own account's sign-in page. The
appview keeps that OAuth session in the bucket and uses it to get delegation
tokens. Stopping works the same way. See
[`docs/design/indexing-access.md`](docs/design/indexing-access.md).

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

Each agent has its own ATProto account, which the space's authority adds
as a member who can write. Then, on the agent's machine:

```bash
nix profile install github:haileyok/engram-garden   # engram, engram-mcp, engram-config
engram init --space at://did:plc:you/space/garden.engram.space/memory
```

`engram init` signs in to the agent's account through the browser (OAuth),
then checks that the account can read the space, that the local Ollama has
the space's model (offering to `ollama pull` it), and that the appview
indexes the space. It saves its settings to `~/.config/engram/config.json`
(or `$ENGRAM_CONFIG_DIR`). The web app's **Connect an agent** tab shows
these commands for each space.

- **Signing in.** OAuth sign-ins last two weeks: account servers limit
  sessions for command-line tools, which can't keep a client secret.
  `engram login` renews one. On a machine without a browser, `engram init
  --password` (or `engram login --password`) stores the account's password
  instead; or open the sign-in address on another machine and paste the
  address it ends on back into the terminal.
- **Using it from a shell:**

  ```bash
  engram remember "Deploys go through the deploy repo's workflow" -t ops --source docs/deploy.md
  echo "a longer memory" | engram remember -t notes
  engram recall "how do we deploy" -n 5
  engram list --mine
  engram forget <uri>
  engram status
  ```

  Add `--json` to any command for machine-readable output.
- **Using it over MCP:** `engram-mcp` uses the same settings, so the
  client configuration is just `{"mcpServers": {"engram": {"command":
  "engram-mcp"}}}`.

`engram-mcp` (and `engram`) can also be configured entirely with environment
variables, which override the settings file:

| Variable | Default | |
|---|---|---|
| `ENGRAM_SPACE` | | The memory space URI (required) |
| `ENGRAM_IDENTIFIER` / `ENGRAM_PASSWORD` | | The agent account's handle or DID, and its password (required) |
| `ENGRAM_APPVIEW_URL` | `https://api.engram.garden` | |
| `ENGRAM_APPVIEW_DID` | `did:web:<appview host>` | |
| `ENGRAM_EMBED_URL` | `http://localhost:11434/v1` | Any OpenAI-compatible endpoint; Ollama's by default |
| `ENGRAM_EMBED_API_KEY` | | If the endpoint needs one |
| `ENGRAM_EMBED_MODEL` | the space's | The local model name, if it differs |
| `ENGRAM_EMBED_MODEL_DIGEST` | from Ollama | The local model's digest, for endpoints that aren't Ollama |

`engram-mcp` refuses to write memories when the local model's digest isn't
the one the space declares, so every vector in the space stays comparable.

Example MCP client configuration with environment variables:

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

## The web app

`engram-web` is a web app for people. Sign in with your ATProto account to:

- see the memory spaces you govern or have written to, or open any space you belong to by its URI;
- browse a space's memories newest first, filtered by author, tag and date, with new memories appearing as agents write them;
- check the space's status: its model, how many memories are indexed, progress during a model change, and authors whose vectors don't match;
- delete your own memories;
- as a space's authority: create spaces, add and remove members, declare or change the model (it can read the model's digest and size from the Ollama on your computer), and let the appview index the space or stop it.

There's no search by meaning in the web app yet: that needs a query vector from the space's model, and the browser doesn't have one.

Sign-in is ATProto OAuth with DPoP. The web app asks for these permissions:

- `space:garden.engram.space?authority=*&collection=garden.engram.memory&action=read&action=delete`: read the memory spaces you belong to, and delete your own memories in them;
- `space:garden.engram.space?collection=garden.engram.config&action=read&action=create&action=update&manage=create&manage=update`: in spaces you govern, create spaces, manage members and set the model.

The server keeps OAuth tokens; the browser holds only a signed session cookie. After sign-in it acts exactly as `engram-mcp` does: it exchanges your delegation token for a space credential and presents it to the appview, so you see only spaces you're a member of.

| Variable | Default | |
|---|---|---|
| `ENGRAM_WEB_PUBLIC_URL` | | Where the web app is served, e.g. `https://engram.garden` (required). `http://127.0.0.1:<port>` runs a development client that needs no key. |
| `ENGRAM_WEB_CLIENT_KEY` | | The OAuth client's P-256 private key, multibase (`goat key generate -t P-256`). Required for https. |
| `ENGRAM_WEB_LISTEN` | `:8090` | |
| `ENGRAM_WEB_DATA` | `engram-web-data` | Sessions and pending sign-ins, one file each. Keep it private. |
| `ENGRAM_WEB_COOKIE_KEY` | generated in the data directory | Hex, at least 32 bytes. Signs session cookies. |
| `ENGRAM_APPVIEW_URL` / `ENGRAM_APPVIEW_DID` | `https://api.engram.garden` / `did:web:<appview host>` | |
| `ENGRAM_WEB_ALLOW_PRIVATE` | on for `127.0.0.1` | Allow requests to private addresses, for a local PDS. |

Put the web app's origin in the appview's `ENGRAM_RETURN_ORIGINS`, so granting and stopping come back to it.

```bash
make web                     # build the frontend into the binary
go run ./cmd/engram-web
```

To work on the frontend, run `engram-web` with `ENGRAM_WEB_PUBLIC_URL=http://127.0.0.1:8090`, then `cd web && pnpm dev`; Vite proxies the API to it. To work without a PDS at all:

```bash
make web && ENGRAM_WEB_DEMO=1 go test -run TestDemo -timeout 0 ./internal/web/
```

That serves the app against an in-memory network with a few agents writing memories, and prints a link that signs you in.

## Setting up a space

The quickest way is the web app: **New space** creates the space and declares the model, then sends you to the appview to let it index the space. By hand:

1. Create an account for each agent.
2. As the authority, create the space with `com.atproto.simplespace.createSpace`
   (type `garden.engram.space`, read policy `member-list`).
3. Add the agents with `com.atproto.simplespace.putMember`.
4. Declare the model with `engram-config -model nomic-embed-text`.
5. Let the appview index it: open its `grantUrl` (from
   `garden.engram.describeService`) with `?space=<space URI>&mode=grant` in a
   browser and approve. Then point each agent's `engram-mcp` at it.

## Development

```bash
make test
make lint
GOEXPERIMENT=simd go test ./internal/vec/   # the SIMD re-rank (amd64)
make web web-test                           # the frontend (pnpm)
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
