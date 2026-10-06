# AGENTS.md

Guidance for working on Engram Garden.

## Layout

- `cmd/engram-appview`: the indexer and search service.
- `cmd/engram-mcp`: per-agent MCP server (stdio).
- `internal/spaceclient`: acts as one account in spaces. It handles the
  delegation token to credential exchange, cached credentials and signed requests.
- `internal/indexer`: syncs member repos. It applies oplog entries to a set
  hash, checks the result against the member's signed commit, and falls back
  to a verified `getRepo` export.
- `internal/store`: Postgres and pgvector. The vector size is fixed when a
  database is first opened.
- `internal/appview`: XRPC handlers, notification receivers, `did:web`
  document, background sync and registration loop.
- `internal/mcpserver`: MCP tools.
- `internal/spacetest`: in-memory Spaces network for tests.
- Spaces primitives (tokens, HTTP signatures, commits, CARs) come from
  `github.com/haileyok/cocoon/space`. Fix protocol bugs there, not here.

## Tests

```bash
export ENGRAM_TEST_DATABASE_URL="postgres://postgres@127.0.0.1:5432/engram_test?sslmode=disable"
go test -race ./...
```

- Postgres-backed tests skip when `ENGRAM_TEST_DATABASE_URL` is unset. The
  database needs the `vector` extension installed. Each test gets its own
  schema (`storetest.New`), so tests run in parallel.
- Use `spacetest.New` for anything that talks to a PDS or a space authority,
  instead of mocking HTTP by hand. It has knobs for corrupt commits and
  missing values.
- Use `embed.Hashing` in tests. Its similarity is word overlap, so write test
  queries that share words with the expected memories.
- Every top-level test calls `t.Parallel()`.

## Conventions

- Keep `gofmt` and `go vet ./...` clean.
- Write the test first, and confirm it fails for the expected reason.
- Never index data that hasn't been verified against the author's signed
  commit.
