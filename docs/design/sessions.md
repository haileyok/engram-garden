# Sessions

Status: in progress. Every agent session, and every subagent, keeps its
transcript in an archive space, written by the session's own identity. The
appview indexes the transcripts so agents can search what earlier sessions
did, and a session can be resumed on another machine.

## What it's for

Memories hold what capture decided was worth keeping. Transcripts hold
everything else, and agents keep needing it:

- "Have we seen this error before, and what fixed it?"
- "Which session touched this file, or ran this command?"
- "Why did we decide X?" — the reasoning around a decision, not only the
  decision a memory kept.
- Where a memory came from, so "verify before trusting" is one hop.
- What an agent actually did, signed by that agent: agents have shells, and
  they read each other's messages.
- Picking a session up on another machine.

## The archive

Transcripts live in their own space type, `garden.engram.archive`, separate
from memory spaces and lounges:

- **Its members are session identities.** Each identity (see
  `lounge.md`) writes its own session record into its own repo, so the
  record is signed by the agent whose work it is, and ident.link ties that
  agent to its principal. A memory space couldn't take thousands of one-off
  authors (merging, skill precedence and member removal all depend on a
  stable author); an archive is built for them.
- **The principal governs it.** The principal is the authority, so they read
  everything and grant the appview access. `engram archive create` makes
  one; new identities join it as they join the lounge (`Provisioner.finish`
  records `Archived` beside `Joined`).
- **Every member reads every member's records.** A space credential doesn't
  say who holds it. That is what lets agents search each other's sessions,
  and it's also why other people's agents, which a lounge may let in, never
  join an archive.
- **Not in memory spaces.** Transcripts would drown memories in recall, and
  the archive's membership, retention and access differ.

The archive's authority writes a `garden.engram.config` record declaring the
embedding model, as in memory spaces; the appview embeds passages with it.

## The session record

`garden.engram.session`, keyed by the harness's session ID, in the
identity's repo. It is rewritten as the session goes on:

- **Where the work stands:** `title`, `summary` (the harness's newest
  compaction summary), `status` (`active`, `ended`), `kind` (`session`,
  `subagent`), `parent` (the DID of the agent that started a subagent),
  `repo`, `branch`, `gitHead`, `gitDirty`, `cwd`, `machine`, `harness`,
  `model`, `startedAt`, `updatedAt`.
- **Where the transcript is:** `head` (`entryId`, `entries`, `bytes`) says
  how far it is stored; `chunks` name the bytes in order, each a blob with
  its size and entry range.

`kind` and `parent` come from the identity file, not from the caller.

## The bytes: blobs

Transcripts are megabytes and grow by appending, so chunks go in the
identity's **blobstore**, not its repo:

- A blob becomes the space's data when a space record references it, and the
  PDS drops it once no record does: deleting the session record deletes the
  transcript. A blob referenced only by space records is served only to
  space readers (`com.atproto.space.getBlob`).
- Every space record rides the member's repo, so `space/listRepoOps` and
  `space/getRepo` would carry every transcript if the bytes were records.
- Chunks are complete transcript lines, at most 3 MiB
  (`agent.MaxChunkBytes`). **The last chunk grows**: a push merges new lines
  into it, uploads the merged blob and names it in place of the old one, until
  it reaches 3 MiB and a new chunk starts. So a session has about one chunk
  per 3 MiB however often it pushes, and the record stays small (a 500 MB
  session has ~170 chunks). The replaced blob is garbage once no record names
  it.
- **A blob is checked against its CID** whenever it's read, by agents and by
  the appview. The CID is the digest; the signed record names the CID. That
  is how the appview indexes only what the author's commit covers.

The transcript is uploaded as written. Secrets in it are readable by every
member of the archive; redaction happens when the appview builds the index
(below), so search results and passages never carry them.

## Pushing

`push_session` takes the record's fields, `fromBytes` (how much the caller
believes is stored), `replace` and `data`:

- A trailing line without its newline is held back, so `bytes` is always a
  whole-line boundary.
- If `fromBytes` isn't what the record says, the answer is `resync` with the
  stored size and nothing is written. The caller then sends the whole file:
  `replace` with the first piece, then appends.
- One push carries at most 8 MiB (`MaxPushBytes`); a harness sends more in
  pieces, so a resync of any size works.
- Pushes for one session are serialized in the agent (one writer per
  identity; the space has no compare-and-swap for records).

pi pushes after a turn when enough has built up (`sessionSyncSeconds`,
`sessionSyncMinBytes`), before compaction, and at the end of the session
with `status: ended`. It reads only the file's new tail.

## The index

The appview builds it; nobody writes passage records. That keeps vectors out
of repos and makes the index a projection: changing how transcripts are cut,
or what's redacted, means rebuilding it, not rewriting anyone's repo.
`transcript.Version` is bumped when the output for the same input changes.

The appview syncs archives like memory spaces (verified repo reads), reads
each session record, fetches the chunks it hasn't indexed, checks them
against their CIDs, and cuts them into **passages**:

| passage | text | why |
|---|---|---|
| `turn` | the user's prompt and the final reply | "how did we do X" |
| `step` | assistant text between tool calls, with one line per tool call after it (`bash: …`, `edit path`) | decisions, and what was run |
| `error` | a failed tool call: the call, the start and end of its output, and the assistant text after it (the fix) | "have we seen this error" |
| `summary` | compaction summaries, and the record's title and summary | the condensed story |

Left out: thinking (10× the assistant text; revisit with `engram-eval`),
successful tool output (mostly file contents git already has, and the riskiest
for secrets and prompt injection), `custom_message` entries (injected
memories and lounge messages: indexing them would echo memories back),
images, and Engram's own tool calls.

Each passage carries facets: the session's DID, parent, kind, repo, branch,
cwd, machine, model, time, and the files and commands it touched.

**Redaction** runs on passage text before embedding: known token formats
(GitHub, Slack, AWS, OpenAI-style keys, JWTs, PEM blocks, URLs with
credentials, `KEY=value` assignments of secret-looking names) and long
high-entropy strings become `[redacted:<kind>]`.

## Tools

- `search_sessions`: passages matching a query, hybrid (keyword and vector),
  filtered by repo, DID, kind, time. Each hit names its session
  (`at://<did>/…/garden.engram.session/<id>`) and entry.
- `read_session`: a window of a session's transcript around an entry,
  rendered as text, so an agent reads context without pulling the file.
- `list_sessions`, `pull_session`: the records, and a whole transcript to
  resume.

Hits are quoted transcript, not instructions: tools mark them as such.
Passages are not part of the automatic recall before each prompt.

## Subagents

A subagent (`pi -p --no-session`) has no session file. pi-engram in the
subagent builds the transcript from its events in memory, in pi's session
format, and pushes it under the subagent's own identity, with `kind:
subagent` and its parent's DID.

## Not done

- No backfill of sessions from before identities.
- Kept until deleted by hand. Deleting a record needs the identity's
  password, so claimed identity files are kept.
