# pi-engram

Engram Garden memory built into [pi](https://pi.dev), instead of an MCP
server the model has to remember to use.

- **Tools.** `recall`, `remember`, `get_memory`, `list_memories` and
  `forget` are ordinary pi tools, and a `memory` section of the system
  prompt names your spaces and says what memory is for. engram-mcp's other
  tools (`list_spaces`, `create_space`, members, models, indexing) are
  there for codemode scripts as `engram_<name>`.
- **Auto-recall.** Each prompt is searched for before the model sees it.
  Memories clearly about it (similarity at least 650, and within 80 of the
  best match) are added to the conversation in a `<memories>` block, once
  per branch. Searching never holds a prompt up for more than four seconds, except that
  the first one waits for engram-mcp to start (about three seconds).
- **Auto-capture.** When the agent goes idle with enough new conversation
  (about 24k characters), before compaction, and when the session ends, the
  stretch not read yet goes to a model with one question: what here would a
  future session want? What comes back is checked: thin memories, anything
  that looks like a credential, and near-duplicates of stored memories are
  dropped. The rest is remembered with the tag `auto` and the project's
  name. A session entry records how far each branch has been read, so
  nothing is read twice. Only saved interactive sessions are captured, not
  `pi -p --no-session` runs such as subagents.
- **Skills.** The skills kept in your spaces (see
  `docs/design/skills.md`) are written to `~/.cache/pi-engram/skills` at
  session start and loaded by pi like any other skills. When the agent
  edits or writes a file there, the skill is saved back to Engram for every
  agent in its space; a new directory with a `SKILL.md` becomes a new
  shared skill. `save_skill` does the same as a tool. Skills changed
  elsewhere load after `/reload`.
- **`/engram`** shows the state (`status`), the spaces (`spaces`), searches
  (`recall <query>`), captures now (`capture`), lets you read and forget
  what was captured (`review`), lists the skills (`skills`, or
  `skills sync` to fetch them now), and restarts engram-mcp (`restart`). The
  footer shows the default space, and warns when a space isn't indexed.

The memory work stays in Go: the extension runs `engram-mcp` as a child
process and speaks MCP to it over stdio, with a small client of its own, so
the package has no dependencies.

## Install

Set up engram first (`engram login --space <space URI>`, then
`engram status`; see the main README). Then:

```bash
nix profile install github:haileyok/engram-garden
pi install ~/.nix-profile/share/pi-engram
```

or from a checkout, `pi install ./pi`. The extension finds `engram-mcp` on
`PATH`, beside the package in a Nix profile, or in `~/go/bin`.

If `engram` is in `~/.pi/agent/mcp.json`, remove it: this replaces it, and
the extension warns while both are there. The memory paragraphs in
`AGENTS.md` files can go too.

## Settings

`~/.pi/agent/engram.json`, overridden by a trusted project's
`.pi/engram.json`. Every field is optional:

```json
{
  "command": "engram-mcp",
  "args": [],
  "env": {},

  "autoRecall": true,
  "recallLimit": 5,
  "recallMinSimilarity": 650,
  "recallMaxGap": 80,
  "recallTimeoutMs": 4000,
  "recallStartTimeoutMs": 10000,
  "recallMinPromptChars": 12,
  "recallMaxChars": 700,

  "autoCapture": true,
  "captureModel": "router/deepseek-v4.1-flash:api",
  "captureSpace": "coding-agents",
  "captureTags": ["auto"],
  "captureMinChars": 24000,
  "captureExitMinChars": 3000,
  "captureExitTimeoutMs": 20000,
  "captureMax": 5,
  "captureDedupeSimilarity": 860
}
```

`captureModel` defaults to the session's model and `captureSpace` to the
default space. `captureMax` applies to each 80k characters of conversation
read. `env` goes to engram-mcp, so `ENGRAM_*` settings work there
(an agent's own config dir, for example). `PI_ENGRAM_COMMAND`,
`PI_ENGRAM_AUTO_RECALL`, `PI_ENGRAM_AUTO_CAPTURE` and
`PI_ENGRAM_CAPTURE_MODEL` override the files.

Captured memories come from a model reading everything the session saw,
including files and web pages, so text written to steer it can end up in a
memory. They carry the `auto` tag: look them over with `/engram review`,
find them later with `list_memories` and `tags: ["auto"]`, or turn capture
off for a project in its `.pi/engram.json`.

Similarities depend on the space's embedding model. The defaults are for
`nomic-embed-text`: unrelated prompts top out around 590, related ones
score 650 to 830.

## Development

```bash
cd pi
npm install        # TypeScript and pi's types, for checking only
npm run typecheck
npm test
```

The tests run the extension against a fake pi and `test/fake-engram-mcp.ts`,
a stand-in for engram-mcp with the same tools. To try changes in pi without
installing: `pi -e ./pi`.
