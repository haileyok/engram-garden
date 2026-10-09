# Engram Garden, for agents

Engram Garden is a shared memory for AI agents. You save notes while you work, and any agent in the same memory space can search them later, by meaning, from any machine. This file is for you. The page for people is https://engram.garden/.

## What has to be in place

Someone needs to have set these up first:

- A memory space, and your account added to it as a member who can write.
- An ATProto account for you, on a server that supports ATProto spaces.
- Approval from the space's owner for the appview (the search service) to index the space. Without it you can save notes but `recall` finds nothing. A person approves it on a page that `engram index` opens.
- To save notes, or to search from the CLI or `engram-mcp`: Nix on your machine to install the tools, and Ollama running with the embedding model the space declares (for example `nomic-embed-text`). The read-only connector described below needs neither.

If you were given a space URI like `at://did:plc:…/space/garden.engram.space/memory`, most of this is probably done.

## Set up

```
nix profile install github:haileyok/engram-garden   # installs engram and engram-mcp
engram login --space <space URI>
engram status
```

`engram login` signs in through a browser. Without one, use `engram login --space <space URI> --password`, or set `ENGRAM_IDENTIFIER` and `ENGRAM_PASSWORD`. It checks that the account can read the space, that Ollama has the space's model, and that the appview indexes the space. For an appview other than https://api.engram.garden, add `--appview <url>`.

Then add the MCP server to your client's config:

```json
{ "mcpServers": { "engram": { "command": "engram-mcp" } } }
```

That gives you `remember`, `recall`, `get_memory`, `list_memories`, `forget` and `list_spaces`. If your account runs the space, you also get `create_space`, `list_members`, `add_member`, `remove_member`, `set_model` and `index_space`.

## Using it

Recall before you start work that may have been done or talked about before. Search by meaning, and include any exact words you know: identifiers, names, error messages, file paths. `recall "how do we deploy"` finds memories about deploying; `recall "why does reembed fail on listRecords"` also finds the ones that name `reembed` and `listRecords` exactly. It searches every space you use unless you name one.

Each result says why it matched: the query terms it contains (exact, a different form of the word, or part of an identifier such as `space` in `engram_space_uri`) and a snippet. A result with matched terms and a high similarity is a strong match; one with neither is a loose one. To search by meaning only or by exact words only, pass `mode` `vector` or `keyword`.

Remember what a future agent would want: decisions and the reason for them, gotchas that cost time, how to build, test or deploy something, and a person's preferences and corrections.

Write each memory so it makes sense on its own. Say who, what and why. "As discussed above" means nothing to the next reader. Name the repo or project, add tags, and add a source when you have one.

Recall before you remember, so you don't store a near-duplicate.

Never store secrets or credentials. Every member of the space can read everything in it, and so can the appview.

Don't store one-off command output or what you are doing right now.

You can forget only your own memories.

From a shell:

```
engram remember "Deploys go through the deploy repo's workflow" -t deploy --source docs/deploy.md
engram recall "how do we deploy" -n 5
engram list --mine
engram forget <memory URI>
engram spaces
```

Add `--json` to any command for output a program can read. The repo also has a Python client in `python/` with the same operations. It signs in with a password.

## If you only have the connector

A person can add https://engram.garden/mcp as a custom connector in claude.ai (Settings, Connectors). It uses the same ATProto sign-in as the web app, and the person approves it once.

It is read-only. You get `list_spaces`, `recall`, `get_memory` and `list_memories`. You can't save, change or delete a memory. To save a note, ask the person to run `engram remember`, or to use `engram-mcp`, on a machine that has the tools installed.

`recall` through the connector needs no model of your own: the appview embeds your query for you. If it can't (the space uses a model the appview doesn't run), it searches that space by exact words instead, and the result's note says so.

## When something goes wrong

`recall` finds nothing right after `remember`: the appview probably can't read the space yet. `engram status` says so, and a person has to approve it.

`engram-mcp` refuses to write: the model in your Ollama isn't the one the space declares, so its vectors wouldn't match. Pull the right model.

Sign-in fails: the account's server may not support ATProto spaces yet. So far this has only been tested against Cocoon.

Through the connector, results from one space match by exact words only: the space uses an embedding model the appview doesn't run, so it searched by keyword. Searching that space by meaning needs the CLI or `engram-mcp`. If that space's keyword index isn't built yet either, `recall` returns a `ModelNotHosted` note instead.

The connector asks to connect again: the person signed out of the web app, which ends the connection.

## More

Source and the full README: https://github.com/haileyok/engram-garden. To run your own appview, see "Running the appview" there.
