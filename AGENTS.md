# AGENTS.md

Guidance for working on Engram Garden.

## Layout

- `cmd/engram-appview`: the indexer and search service, plus `import` and
  `migrate-control`.
- `cmd/engram-eval`, `internal/eval`: measures vector, keyword and hybrid
  ranking on a corpus of memories, with queries and grades from a chat model
  (ENGRAM_EVAL_LLM_*), on a developer's machine. Keep its working directory
  (`-dir`) outside the repository: it holds real memories. See "Evaluation"
  in `docs/design/keyword-search.md`.
- `cmd/engram`: the agent CLI (`login`, `logout`, `spaces`, `use`,
  `remember`, `recall`, `list`, `get`, `forget`, `status`; and for a space's
  authority `create`, `members`, `model`, `index`; `init` is `login`). `cmd/engram-mcp`: per-agent
  MCP server (stdio). Both are thin layers over `internal/agent`: the memory
  operations, the settings file (`~/.config/engram/config.json`, ENGRAM_*
  overriding) and signing in (OAuth loopback client, or password). An agent
  uses several named spaces: `agent.Spaces` holds one `agent.Agent` per space
  and picks the space (the default, a name, a URI, or the one in a record
  URI); recall searches them all. Tests put each space on its own
  `spacetest` network through `Spaces.NewAgent` / `agent.SpacesOf`.
  Running a space (create, members, model, the appview's grant link) is in
  `internal/agent/manage.go`, shared by the CLI and the MCP tools.
- `python/`: the Python client, a 1:1 async port of `internal/agent` (and the
  `lex`, `embed`, `vec` and `spaceclient` pieces it uses): `pip install ./python`.
  Same operations, same settings file. It signs in with a password (an app
  password works), not OAuth. Keep it in step with `internal/agent`: when an
  operation or record format changes there, change it there too. Its tests run
  against `python/tests/fakes.py` (an in-process PDS, space host and appview
  that verify signatures like the real ones) and against fixtures Go made
  (`python/tests/fixtures/go.json`, written by `internal/lex/pyinterop_test.go`
  with `ENGRAM_WRITE_PY_FIXTURES=1`); Go in turn verifies what Python made
  (`python.json`, from `python/tests/make_fixtures.py`). `make python-test`.
- `pi/`: the pi package (`pi install ./pi`): engram built into the pi
  agent. It runs `engram-mcp` as a child process (its own small MCP client
  in `engram/mcp.ts`, so no dependencies), wraps the memory tools as pi
  tools, recalls for each prompt (`before_agent_start`) and captures
  memories from the conversation in the background with a model
  (`engram/capture.ts`; a custom session entry records how far each branch
  was read). It depends on the MCP tools' names and result shapes: when
  they change, change it too. Tests run it against a fake pi and
  `pi/test/fake-engram-mcp.ts`. `make pi-test`.
- `internal/oauthfile`: OAuth sessions as files, for the web app and the CLI.
- `flake.nix` packages the agent tools. When `go.mod` changes, update
  `vendorHash`: build with a wrong one and use the hash Nix reports.
- Skills (`docs/design/skills.md`): `garden.engram.skill` records in members' own repos, read directly
  from members' PDSs with the space credential (not indexed). `internal/agent/skills.go`,
  `internal/skillfile` (SKILL.md and skill directories), the MCP tools `list_skills`, `get_skill`,
  `save_skill`, `delete_skill`, `engram skill`, `python/src/engram_garden/skills.py`, and
  `pi/engram/skills.ts` (the pi package keeps them on disk for pi and saves edits back).
- Sessions (`docs/design/sessions.md`): `garden.engram.session` records in
  members' own repos, read directly like skills, holding where a session
  stands plus `head`/`chunks`; the transcript's bytes are space **blobs**
  (`com.atproto.repo.uploadBlob`, read with `com.atproto.space.getBlob`),
  chunked on whole JSONL lines, because records would drag every transcript
  through repo sync and `getRepo`. Fallbacks while a sign-in predates the
  `blob:` scope: `garden.engram.sessionChunk` records, then the record alone.
  `internal/lex/session.go`, `internal/agent/sessions.go`, `internal/agent/blobs`
  (in `sessions.go`), the MCP tools `list_sessions`/`pull_session`/`push_session`/
  `delete_session`, `cmd/engram/sessions.go`, and `pi/engram/sessionsync.ts`
  (which pushes on turn end and session end, and pulls a session into pi's
  sessions directory).
- `internal/lex`: record formats: the space's config, the vectors memories
  carry (`f16le`), the query vector encoding, and the text that gets embedded.
- `internal/embed`: OpenAI-compatible embedding client, the provider that
  checks the local model's digest against the space's, and the offline
  `Hashing` embedder.
- `internal/spaceclient`: reads spaces for a user. It handles the
  delegation token to credential exchange, cached credentials and signed
  requests. Delegation tokens come from a `Delegator`: the user's own PDS
  session (agents, the web app) or, in the appview, a space's grant.
- `internal/indexer`: syncs member repos. It applies oplog entries to a set
  hash, checks the result against the member's commit and the MAC the repo
  host returns for it, and falls back to a verified `getRepo` export. The
  authority's config record is honored only from the authority's repo.
- `internal/spacestore`: the index. Per space: a manifest and segment files
  in object storage, a write buffer, flushes, merges, garbage collection,
  search (1-bit scan, int8 re-rank), cache tiers, limits and fencing. Read
  `docs/design/storage.md` before changing it.
- `internal/segment`: the segment file format. `internal/vec`: half
  precision, quantization, Hamming distance and the int8 dot product (with a
  `GOEXPERIMENT=simd` version).
- `internal/text`: the keyword-search analyzer (splitting memories and
  queries into terms), the per-query-token scoring tree, BM25 and the
  one-byte length encoding. Not wired into search yet; see
  `docs/design/keyword-search.md`. Any change to what it returns for the
  same input changes the index: bump `text.Version`.
- `internal/blob`: object storage (local directory, S3) and the
  conditional-write probe.
- `internal/routing`: rendezvous hashing of spaces over nodes, and which
  node is the coordinator.
- `internal/control`: the appview's control-plane state (grants, OAuth
  sessions, sign-ins in progress, registered spaces) behind `control.Store`.
  `Postgres` is for deployments, `Memory` for tests and local development.
  Tables come from the numbered files in `migrations/`, applied at startup
  under an advisory lock. `controltest.New(t)` gives a test a store.
- `internal/appview`: XRPC handlers, forwarding to a space's owner,
  notification receivers, `did:web` document, background sync and
  registration loop. `textsearch.go` embeds the text of a search for callers
  with no model of their own (`ENGRAM_TEXT_SEARCH`); it is limited per space
  authority, because a space credential doesn't say who holds it.
- `internal/mcpserver`: the MCP tools, over `internal/agent`.
- `cmd/engram-web`, `internal/web`: the web app's backend. OAuth sign-in
  (indigo's client; sessions in `FileStore`), then it acts for the user with
  `spaceclient`, like `engram-mcp`. The `Auth` interface lets tests sign in
  as `spacetest` accounts. Requests that change anything must be same-origin
  JSON. `connector*.go` is the remote MCP connector for claude.ai
  (`ENGRAM_WEB_MCP`): the web app is an OAuth authorization server for MCP
  clients (metadata, dynamic registration, consent page, PKCE token
  endpoint; apps, grants and pending codes in `control.Store`) and serves read-only
  tools at `/mcp`. With `ENGRAM_DATABASE_URL` the web app's own OAuth
  sessions are in `control.Store` too (`web_sessions`, `web_requests`: never
  the appview's `oauth_sessions`, whose sweep deletes what no grant uses),
  and `ImportFileSessions` copies in the old session files on start. A grant acts through the web app session it was approved
  in, so signing out ends it. Keep its tools read-only unless the web app's
  OAuth scopes grow a write permission.
- `web/`: the React frontend (Vite, TypeScript, no router library). It
  builds into `internal/web/dist/app`, which `engram-web` embeds. Files in
  `web/public/` are copied to the site's root: `og.png` is the link preview
  card (its source is `web/og/card.html`), and `AGENTS.md` is a guide for
  agents that *use* the service, served at `/AGENTS.md`. It is not guidance
  for working on this repo, though tools that read nested `AGENTS.md` files
  will load it when you work in `web/public/`. When an operation or command
  changes, check that guide too.
- The appview has no account. A space's authority grants it read-only OAuth
  access (`internal/appview/grants.go`, `grant_flow.go`, `oauth.go`); grants
  and OAuth sessions live in the control-plane database, not the bucket.
  Read `docs/design/indexing-access.md` before changing it. Tests sign in
  through a fake `Authorizer`.
- Registered spaces: `internal/appview/registration.go`. The appview indexes
  `ENGRAM_SPACES` plus spaces whose authority granted access, recorded in
  the control-plane database (`Register` keeps the first time).
- `internal/appview/migrate.go` copies what older appviews kept in the bucket
  into the database (`engram-appview migrate-control`). It never replaces a
  record the database has, because OAuth refresh tokens are single-use. It
  can go once every deployment has migrated.
- `internal/metrics`: the Prometheus registry, HTTP middleware and the
  separate metrics listener. Packages declare metrics with
  `metrics.Factory` in their own `metrics.go`. When you add or change a
  metric, update `docs/monitoring.md` and, if it belongs on the dashboard,
  `deploy/monitoring/grafana/generate.py` (then run it).
- `internal/spacetest`: in-memory Spaces network for tests.
- Spaces primitives (tokens, HTTP signatures, commits, CARs) come from
  `github.com/haileyok/cocoon/space`. Fix protocol bugs there, not here.

## Tests

```bash
go test -race ./...
GOEXPERIMENT=simd go test ./internal/vec/
make web web-test   # frontend build, typecheck and tests
make pi-test        # the pi package
```

- `internal/web` tests run the real appview and `spacetest`, signed in
  through a fake `Auth`. To look at the UI, run the demo:
  `make web && ENGRAM_WEB_DEMO=1 go test -run TestDemo -timeout 0 ./internal/web/`.

- No external services are needed. Storage tests use `blob.Dir` in
  `t.TempDir()` and an in-process S3 (gofakes3). The control-plane store is
  `control.Memory`, from `controltest.New(t)`.
- `make test-postgres` (needs Docker) runs the control-plane store's
  contract and every suite that builds an appview against a real Postgres:
  `controltest.New` returns a Postgres schema of its own when
  `ENGRAM_TEST_POSTGRES_URL` is set. Run it when you touch `internal/control`
  or how the appview uses it. When you add a `control.Store` method, add it
  to `Memory`, `Postgres` and the contract in `contract_test.go`.
- Use `spacetest.New` for anything that talks to a PDS or a space authority,
  instead of mocking HTTP by hand. It has knobs for corrupt commits and
  missing values. Declare the space's model by putting a config record in
  `net.Authority`'s repo.
- Use `embed.Hashing` / `embed.HashingProvider` with a model named
  `hashing-…` and `embed.HashingDigest`. Its similarity is word overlap, so
  write test queries that share words with the expected memories.
- `spacestore` tests that need time to pass use `Options.Now`. Search
  deadlines always use the real clock.
- Every top-level test calls `t.Parallel()`.

## Conventions

- Keep `gofmt` and `go vet ./...` clean.
- Write the test first, and confirm it fails for the expected reason.
- Never index data that hasn't been verified against the author's signed
  commit.
- Repo sync positions only advance in the manifest that publishes the
  matching changes. Don't write a position anywhere else.
- Segment and manifest objects are immutable: write new ones, never
  overwrite.
- A new record type a space holds goes in `lexicons/garden/engram/space.json`'s collections and in
  `agent.Scopes`, and needs `scripts/publish-lexicons.sh` run before agents can write it.
- Database migrations are append-only: once a numbered file in
  `internal/control/migrations/` has been released, add a new one instead of
  editing it.
