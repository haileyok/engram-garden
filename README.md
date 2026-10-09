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
  [`docs/design/storage.md`](docs/design/storage.md). Keyword and hybrid
  search are proposed in
  [`docs/design/keyword-search.md`](docs/design/keyword-search.md).
- **Control-plane state.** What can't be rebuilt from the members' PDSes
  (which authorities granted the appview access, their OAuth sessions, the
  spaces registered) lives in a small Postgres database, shared by every
  node. The bucket holds only the index.

## Lexicons

| NSID | Kind | Purpose |
|---|---|---|
| `garden.engram.space` | space type | Declares a memory space. Its collections are `garden.engram.memory` and `garden.engram.config`. |
| `garden.engram.config` | record (`self`) | The space's embedding model: `model`, `modelDigest`, `dims`, optional `documentPrefix`, `queryPrefix`, and `next` during a model change |
| `garden.engram.memory` | record | `text`, optional `tags`, optional `source`, `createdAt`, `embedding`, and `nextEmbedding` during a model change |
| `garden.engram.searchMemories` | query | Search with `vector`, `model`, `modelDigest`, `q`, `limit`, `author`, `tags` and `since`. With `q` and a keyword index (`ENGRAM_KEYWORD_SEARCH`), results rank by meaning and exact words together and explain each `match`; `mode` picks `hybrid`, `vector` or `keyword` (no vector needed). With text search on, `q` alone is enough: the appview embeds it (see `ENGRAM_TEXT_SEARCH`); otherwise `q` alone searches by keyword. The response's `mode` says what ran. |
| `garden.engram.getMemory` | query | One memory by URI |
| `garden.engram.listMemories` | query | Newest first, paged, with `author` and `tags` filters |
| `garden.engram.getMemoryGraph` | query | The newest memories (up to 500) and links between the ones that mean similar things, for drawing the space as a graph |
| `garden.engram.getSpaceStatus` | query | The space's model, memory count, authors whose vectors don't match, and whether the appview's access is granted, missing or lapsed |
| `garden.engram.warmSpace` | procedure | Start loading a space's index ahead of searches |
| `garden.engram.exportSpace` | query | Download the space's index as a tar |
| `garden.engram.describeService` | query | The appview's DID, whether registration is open, and `grantUrl`, where a space's authority lets the appview index it |

They live in [`lexicons/`](lexicons/garden/engram).

The official Bluesky PDS looks up `garden.engram.space` when someone signs in with a
`space:` scope, and refuses with `invalid_scope: Unable to retrieve space declarations`
if it can't find it. Cocoon doesn't mind. The lexicons are found through the DNS TXT record
`_lexicon.engram.garden` (`did=<the publishing account's DID>`) and the
`com.atproto.lexicon.schema` records in that account's repo;
[`scripts/publish-lexicons.sh`](scripts/publish-lexicons.sh) writes the records and says how
to set the DNS record. A deployment on other NSIDs would publish its own.

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
| `ENGRAM_DATABASE_URL` | | `postgres://user:password@host/dbname` for the grants, OAuth sessions and registered spaces. Required with `ENGRAM_STORAGE=s3`. With `dir` and no URL they're kept in memory and lost on restart, which is fine for development. It holds a password, so keep it with the other secrets. |
| `ENGRAM_CACHE_DIR` | `engram-cache` | Local disk cache of segment files |
| `ENGRAM_CACHE_BYTES` | 100 GiB | Disk cache budget |
| `ENGRAM_RAM_BYTES` | 8 GiB | RAM budget for loaded spaces |
| `ENGRAM_LIMIT_MEMORIES` / `ENGRAM_LIMIT_BYTES` / `ENGRAM_LIMIT_SEARCHES_PER_SECOND` / `ENGRAM_LIMIT_WRITES_PER_DAY` | unlimited | Per-space limits |
| `ENGRAM_KEYWORD_SEARCH` | `false` | Write segments with a keyword index and rewrite older ones. Once all of a space's segments have one, its searches that include `q` rank by meaning and exact words together (hybrid search; see `docs/design/keyword-search.md`). Every node reads keyword segments either way; turn this on only after every node runs a release that does. |
| `ENGRAM_TEXT_SEARCH` | `false` | Embed the text of searches that come without a vector, for callers that have no model of their own (the web app, the Claude connector). Uses the model `ENGRAM_EMBED_URL` / `ENGRAM_EMBED_MODEL` / `ENGRAM_EMBED_MODEL_DIGEST` name, usually an Ollama next to the appview, and only serves spaces that declare that same model (same name and digest); others get `ModelNotHosted`. The appview embeds only after verifying a credential for an indexed space. |
| `ENGRAM_TEXT_SEARCH_PER_SECOND` / `ENGRAM_TEXT_SEARCH_BURST` | `2` / `20` | How many text searches one space authority may make (not per space, so many spaces don't multiply it) |
| `ENGRAM_TEXT_SEARCH_CONCURRENCY` | `4` | Embeddings running at once on a node; callers past it get `EmbedderBusy` (503) |
| `ENGRAM_TEXT_SEARCH_AUTHORITIES` | any | Comma-separated DIDs: only spaces these accounts govern may use text search |
| `ENGRAM_NODES` | single node | `id=url,id=url`: every appview node and its internal URL |
| `ENGRAM_NODE_ID` | | This node's id in `ENGRAM_NODES` |
| `ENGRAM_NODES_EPOCH` | | A number that increases whenever `ENGRAM_NODES` changes |
| `ENGRAM_METRICS_LISTEN` | off | Address for Prometheus metrics at `/metrics`, e.g. `:9464`. A separate listener; keep it off the public internet. See [Monitoring](docs/monitoring.md). |
| `ENGRAM_METRICS_PER_SPACE` | `false` | Also report gauges labeled with each loaded space's URI. Leave off where spaces are many. |

```bash
go run ./cmd/engram-appview
```

At startup the appview checks whether the bucket honors conditional writes,
and uses them only if it does. It also connects to the database and creates
or updates its tables, so give it a user that may do that. Several nodes
starting at once is fine.

The database holds what can't be rebuilt: a grant lost is a grant its
authority has to make again. Back it up (for example `pg_dump` on a
schedule), and treat its credentials like the bucket's: it holds the OAuth
sessions that let the appview read every space it indexes.

Space hosts deliver notifications only to public HTTPS endpoints. Cocoon,
for example, refuses private and loopback addresses. A local appview at
`http://127.0.0.1:<port>` stays current by polling.

**Indexing a space.** The appview reads a space only with its authority's
permission: the authority opens the appview's grant page (the web app's
**Let the appview index this space** button) and approves read-only access
to the memory spaces they govern on their own account's sign-in page. The
appview keeps that OAuth session in its database and uses it to get
delegation tokens. Stopping works the same way. See
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

**Moving from an appview that kept grants in the bucket.** Earlier versions
stored grants, OAuth sessions and registered spaces as objects in the
bucket. To move to the database, stop the old appview, set
`ENGRAM_DATABASE_URL` (and the same storage settings), and run once:

```bash
engram-appview migrate-control
```

then start the new appview. It copies what the bucket has and leaves what
the database already has alone, so running it again is safe. Stop the old
appview first: an OAuth refresh token works once, so a session refreshed
after it was copied would be out of date in the database. The old objects
(`grants/`, `oauth/`, `registered-spaces/`, `registry-*`) stay in the bucket
until you delete them.

**Monitoring.** Set `ENGRAM_METRICS_LISTEN` (and `ENGRAM_WEB_METRICS_LISTEN`
on the web app) for Prometheus metrics. Logs are JSON lines on stderr.
[`docs/monitoring.md`](docs/monitoring.md) lists the metrics, and
`deploy/monitoring/` has a Grafana dashboard, alert rules, and Prometheus and
Alloy configs.

## Declaring the space's model

The authority declares it once, with the model available in a local
Ollama (or with `engram create <name> --model …`, or in the web app):

```bash
ollama pull nomic-embed-text
engram model --space memory --set nomic-embed-text
```

It records the model's digest and dimensions, and nomic-embed-text's task
prefixes. To change models later:

1. `engram model --next <model>`: agents start writing vectors for both models,
   and rewrite their existing memories in the background.
2. Watch `garden.engram.getSpaceStatus` until enough memories have the new
   vector.
3. `engram model --promote`: searches switch to the new model
   (`engram model --cancel` abandons the move).

Memories from agents that never come back don't get the new vector, so they
drop out of search after the switch.

## Giving an agent memory tools

Each agent has its own ATProto account, which the space's authority adds
as a member who can write. Then, on the agent's machine:

```bash
nix profile install github:haileyok/engram-garden   # engram, engram-mcp
engram login --space at://did:plc:you/space/garden.engram.space/memory
```

`engram login` signs in to the agent's account through the browser (OAuth),
then checks that the account can read the space, that the local Ollama has
the space's model (offering to `ollama pull` it), and that the appview
indexes the space. It saves its settings to `~/.config/engram/config.json`
(or `$ENGRAM_CONFIG_DIR`). The web app's **Connect an agent** tab shows
these commands for each space.

- **Signing in.** OAuth sign-ins last two weeks: account servers limit
  sessions for command-line tools, which can't keep a client secret.
  `engram login` renews one. On a machine without a browser, `engram login
  --password` stores the account's password instead; or open the sign-in
  address on another machine and paste the address it ends on back into the
  terminal. `engram logout` ends the sign-in (`engram init` still works, as
  `login`).
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
- **Several spaces.** An agent can use any spaces its account belongs to,
  each with a short name (by default the last part of its URI):

  ```bash
  engram spaces add at://did:plc:team/space/garden.engram.space/runbooks
  engram spaces                  # the spaces, the default (*), and each one's embedding model
  engram use runbooks            # make it the default
  engram remember "…" --space notes
  engram recall "how do we deploy"            # searches every space
  engram recall "how do we deploy" --space runbooks
  engram spaces remove runbooks
  ```

  `remember` and `list` use the default space unless given `--space`;
  `recall` searches every space unless given one; `get` and `forget` take
  the space from the memory's URI. Each space declares its own embedding
  model, and `engram spaces` shows which (name, digest, dimensions, prefixes)
  and whether this machine can embed with it. `engram spaces` also lists
  other spaces the account belongs to that aren't set up yet.

  A space's memories are only searchable once its authority has let the
  appview read it (`engram index`, or the web app). Until then `remember`
  succeeds and `recall` finds nothing, so `remember`, `recall`,
  `engram spaces` and `engram status` all say when the appview can't read
  a space, and who has to approve.
- **Using it over MCP:** `engram-mcp` uses the same settings, so the
  client configuration is just `{"mcpServers": {"engram": {"command":
  "engram-mcp"}}}`.

`engram-mcp` (and `engram`) can also be configured entirely with environment
variables, which override the settings file:

| Variable | Default | |
|---|---|---|
| `ENGRAM_SPACES` | | The spaces, comma-separated, each a URI or `name=URI`; replaces the saved ones |
| `ENGRAM_SPACE` | | The default space, by name or URI (a URI not yet set up is added). One of these, or the settings file, must name a space. |
| `ENGRAM_IDENTIFIER` / `ENGRAM_PASSWORD` | | The agent account's handle or DID, and its password (instead of `engram login`'s sign-in) |
| `ENGRAM_PDS_HOST` | resolved | Skip resolving the account's PDS (password sign-in) |
| `ENGRAM_CONFIG_DIR` | `~/.config/engram` | Where the settings and OAuth sessions live; one per agent on a shared machine |
| `ENGRAM_APPVIEW_URL` | `https://api.engram.garden` | |
| `ENGRAM_APPVIEW_DID` | `did:web:<appview host>` | |
| `ENGRAM_EMBED_URL` | `http://localhost:11434/v1` | Any OpenAI-compatible endpoint; Ollama's by default |
| `ENGRAM_EMBED_API_KEY` | | If the endpoint needs one |
| `ENGRAM_EMBED_MODEL` | the space's | The local model name, if it differs |
| `ENGRAM_EMBED_MODEL_DIGEST` | from Ollama | The local model's digest, for endpoints that aren't Ollama |
| `ENGRAM_EMBED_PROVIDER` | `openai` | `hashing` embeds offline without meaning, for tests |
| `ENGRAM_QUERY_LOG` | | Off. A file to append each recall's query and returned memory URIs to, one JSON line each, readable only by you. For evaluating search with `engram-eval gen -log`; it never leaves this machine. |

`engram-mcp` refuses to write memories when the local model's digest isn't
the one the space declares, so every vector in the space stays comparable.

Example MCP client configuration with environment variables:

```json
{
  "mcpServers": {
    "engram": {
      "command": "engram-mcp",
      "env": {
        "ENGRAM_SPACES": "team=at://did:plc:you/space/garden.engram.space/memory,at://did:plc:you/space/garden.engram.space/notes",
        "ENGRAM_SPACE": "team",
        "ENGRAM_IDENTIFIER": "agent-1.example.com",
        "ENGRAM_PASSWORD": "…"
      }
    }
  }
}
```

The tools (`remember`, `recall` and `list_memories` take an optional
`space`, by name or URI):

- `list_spaces` lists the agent's spaces, the default, and the embedding
  model each requires; whether the appview can read each (`indexing`, with a
  `warning` when it can't); and other spaces the account belongs to.
- `remember` embeds and stores a memory (`text`, optional `tags` and `source`) in the default space or `space`. Its `note` says if the appview can't read the space, so the memory won't be searchable.
- `recall` embeds the query with each space's model and searches every space (or `space`); each result names its space. An empty result from a space the appview can't read says so in its `note`.
- `get_memory` fetches one memory by URI.
- `list_memories` lists a space's memories newest first.
- `forget` deletes one of the agent's own memories.
- For spaces the agent's account governs: `create_space` (optionally
  declaring the model), `list_members`, `add_member` (`readOnly` to let
  them only recall), `remove_member`, `set_model` (`declare`, `next`,
  `promote`, `cancel`), and `index_space`, which reports whether the
  appview may index the space and returns the link a person opens to
  approve.

The server's instructions list the spaces, so an agent knows them without
calling `list_spaces`.

## Using memories from Python

`python/` is a Python client with the same operations as the Go tools, for
agents written in Python (async, `pip install ./python`): `remember`,
`recall`, `get`, `list`, `forget`, spaces, members, models and indexing. It
embeds locally with the space's model, as `engram-mcp` does, and reads and
writes the same settings file. See [`python/README.md`](python/README.md).

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
| `ENGRAM_WEB_METRICS_LISTEN` | off | Address for Prometheus metrics at `/metrics`, e.g. `:9465`. See [Monitoring](docs/monitoring.md). |
| `ENGRAM_WEB_DATA` | `engram-web-data` | Sessions and pending sign-ins, one file each, when there's no `ENGRAM_DATABASE_URL`; the cookie key. Keep it private. |
| `ENGRAM_WEB_COOKIE_KEY` | generated in the data directory | Hex, at least 32 bytes. Signs session cookies. |
| `ENGRAM_APPVIEW_URL` / `ENGRAM_APPVIEW_DID` | `https://api.engram.garden` / `did:web:<appview host>` | |
| `ENGRAM_WEB_ALLOW_PRIVATE` | on for `127.0.0.1` | Allow requests to private addresses, for a local PDS. |
| `ENGRAM_WEB_MCP` | `false` | Serve the connector for Claude and other MCP clients at `/mcp` (see below). Needs `ENGRAM_TEXT_SEARCH` on the appview and `ENGRAM_DATABASE_URL` here. |
| `ENGRAM_DATABASE_URL` | | `postgres://` URL of the control-plane database, the one the appview uses. When set, the web app's OAuth sessions and sign-ins in progress live there (in `web_sessions` and `web_requests`, apart from the appview's own sessions) instead of in files, so several web nodes share them and a restart signs nobody out. On the first start with it set, sessions still saved as files in `ENGRAM_WEB_DATA/oauth` are copied in (never replacing one the database has), and that directory is renamed to `oauth.imported-<time>`, kept as a backup. Required with `ENGRAM_WEB_MCP`. Without it, a development client keeps sessions as files and the connector's state in memory. |

Put the web app's origin in the appview's `ENGRAM_RETURN_ORIGINS`, so granting and stopping come back to it.

### Connecting Claude

With `ENGRAM_WEB_MCP=true`, claude.ai (and any other client of remote MCP servers) can search your spaces. In claude.ai, Settings, Connectors, add a custom connector with the URL `https://engram.garden/mcp` (your own web app's origin plus `/mcp`). Claude registers itself, sends you to a page here to sign in (the same ATProto sign-in as the web app) and approve it, and from then on can call `list_spaces`, `recall`, `get_memory` and `list_memories`. It can't write, change or delete anything.

How it works: the web app is also an OAuth authorization server for MCP clients (RFC 9728 and 8414 metadata at `/.well-known/`, dynamic client registration at `/mcp-oauth/register`, PKCE with S256 only, short-lived access tokens and single-use refresh tokens; reusing a spent refresh token ends the grant). A token stands for one account's approval of one app, and the tools run as that account through the web app session the approval was made in, so Claude reads what you can read and nothing else. Because of that, signing out of the web app ends the connection and Claude asks you to connect again. Registered apps, approvals and approvals waiting for tokens live in the control-plane database (migration `002_mcp.sql`, behind `control.Store`), so several web nodes share them and they survive restarts; codes and refresh tokens are stored only as hashes. `GET /api/connectors` lists an account's connections and `POST /api/connectors/revoke` ends one.

Claude's servers can't run your local model, so `recall` sends the query text to the appview, which embeds it. That needs `ENGRAM_TEXT_SEARCH=true` on the appview with an Ollama (or other endpoint) running the space's model; see the appview settings above. Spaces that use a model the appview doesn't run return a `ModelNotHosted` note instead of results.

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

The quickest way is the web app: **New space** creates the space and declares the model, then sends you to the appview to let it index the space. From a terminal, signed in as the space's account:

```bash
engram create memory --model nomic-embed-text
engram members add agent-1.example.com
engram index            # opens the appview's page; approve, and it waits until it can index
```

An agent with `engram-mcp` can do the same with the `create_space`,
`add_member`, `set_model` and `index_space` tools (`index_space` returns
the link for a person to approve). By hand:

1. Create an account for each agent.
2. As the authority, create the space with `com.atproto.simplespace.createSpace`
   (type `garden.engram.space`, read policy `member-list`).
3. Add the agents with `com.atproto.simplespace.putMember`.
4. Declare the model with `engram model --set nomic-embed-text`.
5. Let the appview index it: open its `grantUrl` (from
   `garden.engram.describeService`) with `?space=<space URI>&mode=grant` in a
   browser and approve. Then point each agent's `engram-mcp` at it.

## Development

```bash
make test
make lint
GOEXPERIMENT=simd go test ./internal/vec/   # the SIMD re-rank (amd64)
make web web-test                           # the frontend (pnpm)
make python-lint python-test                # the Python client (uv)
```

Tests run against an in-memory Spaces network (`internal/spacetest`). It
builds real tokens, credentials, signed commits and repo CARs with Cocoon's
`space` package, so the appview's verification runs end to end without
any external services. Storage tests use a local directory and an
in-process S3, and the control-plane store is in memory.

To run those against a real Postgres, with Docker running:

```bash
make test-postgres
```

It starts a throwaway Postgres, runs the control-plane store's own tests,
then every suite that builds an appview with its store in Postgres, and
removes the container. To use a database you already have, set
`ENGRAM_TEST_POSTGRES_URL=postgres://…` (the target then doesn't start one).
Each test works in its own schema and drops it afterwards.

To check a real bucket such as Wasabi, put `ENGRAM_TEST_S3_ENDPOINT` (with
`_REGION`, `_BUCKET`, `_ACCESS_KEY`, `_SECRET_KEY`) in
`~/.config/engram-garden/real-bucket.env`, one `KEY="value"` per line and
mode 600, then run:

```bash
make test-real-bucket   # or REAL_BUCKET_ENV=/other/path make test-real-bucket
```

Nothing reads that file except this target. The tests themselves read the
variables from the environment, and skip when `ENGRAM_TEST_S3_ENDPOINT` is
unset.

`internal/blob` runs the store contract, a multipart upload with ranged
reads across the part boundary, the conditional-write probe, and logs what
the provider answers to a conditional overwrite. `internal/spacestore` runs
a space's whole life: flushes, a cold load from the bucket with an empty
disk cache, a merge, garbage collection, and two writers racing on the same
manifest. Each run writes under its own `conformance/…/` prefix and deletes
it afterwards. Wasabi bills deleted objects for a 90-day minimum, so use a
dedicated test bucket. A run writes about 18 MB, which is small next to
Wasabi's 1 TB monthly minimum.

Benchmarks for the benchmark machine:

```bash
go test -run x -bench . ./internal/vec/ ./internal/segment/
GOEXPERIMENT=simd go test -run x -bench Int8 ./internal/vec/
```
