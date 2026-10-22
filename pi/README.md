# pi-engram

Engram Garden memory built into [pi](https://pi.dev), instead of an MCP
server the model has to remember to use.

- **Tools.** `recall`, `remember`, `get_memory`, `list_memories` and
  `forget` are ordinary pi tools, and a `memory` section of the system
  prompt names your spaces and says what memory is for. engram-mcp's other
  tools (`list_spaces`, `create_space`, members, models, indexing) are
  there for codemode scripts as `engram_<name>`.
- **Auto-recall.** Each prompt is searched for before the model sees it (a
  short follow-up, with the exchange it follows). The best dozen results
  above similarity 600 go to a small model (`model`, below), which keeps
  only those that bear on the prompt and the sentences of each that do;
  those are added to the conversation in a `<memories>` block, once per
  branch. Often none are, and nothing is added. If the model is slow or
  fails, only results that are clearly close (at least 650, within 80 of
  the best) are added. A short memory is shown whole; a long one, the part
  that matters. This takes two to three seconds a prompt (at most about
  eight, when the search and the model both run out of time), and the
  first one also waits for engram-mcp to start (about three).
- **Auto-capture.** When the agent goes idle with enough new conversation
  (about 40k characters), before compaction, and when the session ends, the
  stretch not read yet goes to the model with one question: what here would
  a future session be noticeably worse off without? The bar is high
  (decisions, preferences and corrections, costly gotchas, milestones; not
  what the code shows or work still in progress), at most three memories a
  stretch, and usually none. Thin memories and anything that looks like a
  credential are dropped. The rest is remembered with the tag `auto` and
  the project's name.
- **Consolidation.** Before any memory is stored, captured or remembered by
  the agent, it's compared with the most similar ones (similarity at least
  720). A near-copy (860 or more) is skipped; otherwise the model decides:
  skip it (already said), store it as new, or merge it with your own older
  memories it updates into one, forgetting those. A merge keeps what's still
  true and drops what the update made wrong (an old "not deployed yet"); it
  only happens within one space, and an old memory is forgotten only if the
  merged text keeps its identifiers and at least half its length. Forgotten
  memories are written in full to `~/.local/state/pi-engram/forgotten.jsonl`
  first, so a bad merge can be undone. Other people's memories are never
  forgotten. A session entry records how far each branch has been read, so
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

  "model": "router/deepseek-v4.1-flash:api",

  "autoRecall": true,
  "recallLimit": 6,
  "recallCandidates": 12,
  "recallCandidateSimilarity": 600,
  "recallFilter": true,
  "recallFilterTimeoutMs": 4000,
  "recallMinSimilarity": 650,
  "recallMaxGap": 80,
  "recallTimeoutMs": 4000,
  "recallStartTimeoutMs": 10000,
  "recallMinPromptChars": 12,
  "recallMaxChars": 700,

  "autoCapture": true,
  "captureSpace": "coding-agents",
  "captureTags": ["auto"],
  "captureMinChars": 40000,
  "captureExitMinChars": 3000,
  "captureExitTimeoutMs": 20000,
  "captureMax": 3,
  "captureDedupeSimilarity": 860,

  "consolidate": true,
  "consolidateMinSimilarity": 720,
  "consolidateTimeoutMs": 20000
}
```

`model` is the model for picking, capturing and consolidating memories; it
defaults to the session's model, and a small fast one fits (`captureModel`
is its old name). `captureSpace` defaults to the default space. `captureMax` applies to each 80k characters of conversation
read. `env` goes to engram-mcp, so `ENGRAM_*` settings work there
(an agent's own config dir, for example). `PI_ENGRAM_COMMAND`,
`PI_ENGRAM_AUTO_RECALL`, `PI_ENGRAM_AUTO_CAPTURE` and `PI_ENGRAM_MODEL`
override the files.

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
