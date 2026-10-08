# AGENTS.md

Guidance for working on Engram Garden.

## Layout

- `cmd/engram-appview`: the indexer and search service, plus `import` and
  `migrate-control`.
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
- `internal/oauthfile`: OAuth sessions as files, for the web app and the CLI.
- `flake.nix` packages the agent tools. When `go.mod` changes, update
  `vendorHash`: build with a wrong one and use the hash Nix reports.
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
  hash, checks the result against the member's signed commit, and falls back
  to a verified `getRepo` export. The authority's config record is honored
  only from the authority's repo.
- `internal/spacestore`: the index. Per space: a manifest and segment files
  in object storage, a write buffer, flushes, merges, garbage collection,
  search (1-bit scan, int8 re-rank), cache tiers, limits and fencing. Read
  `docs/design/storage.md` before changing it.
- `internal/segment`: the segment file format. `internal/vec`: half
  precision, quantization, Hamming distance and the int8 dot product (with a
  `GOEXPERIMENT=simd` version).
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
  registration loop.
- `internal/mcpserver`: the MCP tools, over `internal/agent`.
- `cmd/engram-web`, `internal/web`: the web app's backend. OAuth sign-in
  (indigo's client; sessions in `FileStore`), then it acts for the user with
  `spaceclient`, like `engram-mcp`. The `Auth` interface lets tests sign in
  as `spacetest` accounts. Requests that change anything must be same-origin
  JSON.
- `web/`: the React frontend (Vite, TypeScript, no router library). It
  builds into `internal/web/dist/app`, which `engram-web` embeds.
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
- Database migrations are append-only: once a numbered file in
  `internal/control/migrations/` has been released, add a new one instead of
  editing it.
