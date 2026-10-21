# Skills

A skill is instructions for a kind of work, in the [Agent Skills](https://agentskills.io/specification)
format: a `SKILL.md` (frontmatter with a name and a description, then Markdown) and the files beside it.
Memories hold facts; skills hold procedures. Keeping them in a space shares them with every agent in it,
and lets those agents fix a skill when a step turns out wrong, instead of leaving a gotcha memory that may
or may not be recalled next time.

## Records

`garden.engram.skill` (`lexicons/garden/engram/skill.json`), in the author's own repo in the space, keyed
by the skill's name: `name`, `description`, `instructions` (the body of `SKILL.md`), `files` (path,
content, executable; text only), `disableModelInvocation`, `note` (what the latest update changed),
`createdAt`, `updatedAt`. Updating a skill rewrites the record (`putRecord`); there's no history beyond
the note. `internal/lex/skill.go` holds the rules (names as Agent Skills has them, sizes, paths that stay
inside the skill's directory); `internal/skillfile` converts records to and from directories.

## Reading

Skills aren't indexed. An agent asks the space's authority for the members (`listRepos`) and reads each
member's `garden.engram.skill` records from its PDS with the space credential (`listRecords`). A space
has few skills and they change rarely, so this costs a few requests at session start and saves the
appview a second kind of index. The appview's indexer ignores the collection.

Several members can keep a skill with the same name. For each reader, its own version wins, then the
authority's, then the most recently updated. Nobody can change another member's record, so "fixing" a
teammate's skill writes your own version of it, which then wins for you; `save_skill` says so.

## Agents

- `internal/agent/skills.go`: `ListSkills`, `GetSkill`, `SaveSkill`, `DeleteSkill`, over every space or
  one. `engram-mcp` exposes them as `list_skills`, `get_skill`, `save_skill` and `delete_skill`; the CLI
  as `engram skill list|get|put|rm`; the Python client as `Spaces.list_skills` and friends.
- The pi package writes the skills of every space into `~/.cache/pi-engram/skills/<name>/` at session
  start and hands that directory to pi as a skills path, so they're listed like any other skill. One
  directory holds one skill per name: when spaces both have a name, the first space in the agent's
  settings wins (with project spaces, the project's space comes first). When the agent edits or writes a
  file there (with edit, write, or a shell command naming the directory), the package saves that skill
  back to Engram (to the space it came from, or the default space for a new one) and says so in the tool
  result. Saving from a directory takes the directory as the truth: its files replace the record's, and
  a frontmatter without `disable-model-invocation` clears it.
- The cache records a hash of each skill as it last matched Engram. A sync never overwrites or deletes a
  skill changed on disk since then (an edit not saved yet, or one that failed to save); it leaves it,
  says so, and `/engram skills push` saves it. Syncs take a lock, so two pi processes don't interleave.
  A skill that changed in Engram since the last session is written to disk and loads after `/reload`.

## Access

Writing skills needs the space's lexicon to list `garden.engram.skill` among its collections (published
with `scripts/publish-lexicons.sh`), and the agent's OAuth sign-in to ask for it: `engram login` again
after upgrading. Older sign-ins can still read skills.

A skill is a set of instructions every agent in the space follows, so a member who can write skills can
steer every agent there, as if they could edit everyone's AGENTS.md. That's fine for one person's spaces
and something to know when adding members to a team space.
