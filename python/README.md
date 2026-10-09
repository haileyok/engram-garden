# engram-garden (Python)

A Python client for [Engram Garden](../README.md): the same operations as the Go `engram` CLI and
`engram-mcp`, for agents written in Python. It is async (`httpx`), has no model or vector-math
dependencies, and reads and writes the same settings file as the Go tools.

```bash
pip install ./python        # from a checkout; Python 3.11+
```

## Using it

```python
import engram_garden as eg

settings = eg.load_settings()  # ~/.config/engram/config.json, with ENGRAM_* variables on top

async with await eg.open_spaces(settings) as spaces:
    await spaces.remember("Deploys go through the deploy repo's workflow", tags=["ops"], source="docs/deploy.md")
    found = await spaces.recall("how do we deploy", limit=5)  # searches every space
    # By meaning and exact words together; each result's .match says why it
    # matched. mode="vector" or mode="keyword" picks one.
    found = await spaces.recall("why does reembed fail on listRecords", mode="keyword")
    for m in found.memories:
        print(m.similarity, m.text, m.uri)

    page = await spaces.list(limit=25)  # newest first
    await spaces.forget(found.memories[0].uri)  # your own memories only
```

Or without a settings file:

```python
settings = eg.Settings(
    spaces=[eg.SpaceEntry(name="memory", uri="at://did:plc:.../space/garden.engram.space/memory")],
    account=eg.Account(handle="agent.example.com", sign_in=eg.SIGN_IN_PASSWORD, password="app-password"),
    embed=eg.Embed(url="http://localhost:11434/v1"),  # Ollama; this is the default
)
settings.apply_defaults()
```

Every method mirrors the Go library (`internal/agent`), in snake_case:

| Go (`agent.Spaces`) | Python (`Spaces`) |
|---|---|
| `Remember`, `Recall`, `Get`, `List`, `Forget` | `remember`, `recall`, `get`, `list`, `forget` |
| `ListSpaces` | `list_spaces` |
| `CreateSpace`, `ListMembers`, `AddMember`, `RemoveMember` | `create_space`, `list_members`, `add_member`, `remove_member` |
| `Model`, `SetModel` | `model`, `set_model` |
| `IndexState`, `IndexSpace` | `index_state`, `index_space` |
| `Run` | `run` |

`Spaces.agent(name_or_uri)` returns the per-space `Agent` (`config`, `indexing`, `warm`, `reembed`, `run`).

The embedding model is the one the space declares. The client embeds locally (Ollama by default),
checks the local model's digest against the space's, and refuses to write a vector from a different
model, exactly as the Go tools do.

### Differences from the Go library

- **Password sign-in only.** An app password works. A session saved by `engram login` (OAuth) can't be
  resumed from Python; sign-ins last as long as the account's tokens can be refreshed.
- `remember` takes an optional `created_at`, so a migration can keep a memory's original time.
- No MCP server or command line: use the Go `engram` and `engram-mcp` for those.

## Tests

```bash
cd python
uv venv && uv pip install -e ".[dev]"
uv run pytest
```

The tests run against an in-process fake PDS, space authority and appview (`tests/fakes.py`), and check
interoperability with the Go implementation using fixtures made by Go (`tests/fixtures/`).
