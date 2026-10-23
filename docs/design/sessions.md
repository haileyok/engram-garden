# Sessions

Where an agent's working session stands, and its transcript, kept in a space
so the session can be picked up later — on another machine, or by another
agent. Two records types, one idea: the record is the index and the handoff,
the bytes are the transcript.

## The manifest record

`garden.engram.session`, keyed by the harness's session ID (a UUID for pi).
It is rewritten as the session goes on, and it holds two kinds of thing:

- **Where the work stands**: `title`, `summary` (markdown; the harness's own
  compaction summary when it has one), `status`, `repo`, `branch`, `cwd`,
  `machine`, `harness`, `model`, `gitHead`, `gitDirty`, `startedAt`. This is
  what another person or agent reads to pick the work up, and it is small
  enough to list every session in a space cheaply.
- **Where the transcript is**: `head` (`entryId`, `entries`, `bytes`) says
  how far it is stored; `chunks` names the bytes in order, each with its size,
  entry range and SHA-256; `mode` says where they are.

It is not searched as a memory. Like skills, session records are read
directly from members' PDSes with the space credential, so the appview's
index never carries them and `recall` never returns them.

## The bytes: blobs, not records

Transcripts are megabytes and grow by appending, so chunks go in the member's
**blobstore**, not in the repo:

- Uploading is ordinary `com.atproto.repo.uploadBlob`. A blob becomes the
  space's data when a space record references it, which is also what keeps it
  alive: the PDS garbage-collects a blob once no record names it. Deleting the
  manifest therefore deletes the transcript.
- A blob referenced **only** by space records is excluded from public sync and
  served only to space readers through `com.atproto.space.getBlob`, so a
  transcript is as private as the space.
- Chunks are split on **complete transcript lines**, at most 3 MiB each
  (`internal/agent/sessions.go`, `MaxChunkBytes`), and named by content, so
  appending a session uploads only the new chunk.

This is the whole reason not to use records for the bytes: every space record
rides the member's repo, so `space/listRepoOps` and `space/getRepo` would
carry every transcript the indexer never reads, and any full re-export would
pull the lot. A year of one heavy user's sessions is a few GB; as blobs it
costs the repo nothing.

## Modes

`mode` says how the current transcript is stored. A push tries them in order
and stops at the first the sign-in allows:

| mode | where the bytes are | when |
|---|---|---|
| `blob` | `chunks[].blob`, the member's blobstore | the default |
| `record` | `chunks[].rkey`, `garden.engram.sessionChunk` records | the sign-in can't upload blobs yet |
| `metadata` | nowhere: the record alone | the sign-in can't write transcripts at all |

`record` exists for a sign-in that predates the `blob:*` scope:
`uploadBlob` needs `blob:<mime>` and `atproto` doesn't imply it, so an agent
that signed in before this feature must run `engram login` again. Until it
does, chunk records keep the transcript (at 400 KiB a record, inside the 1 MiB
a space record holds). `metadata` is the honest last resort: the handoff still
lands, and the answer's `note` says what to do. A push's result carries the
mode and the note, so a harness can tell the person once.

## Pushing

`push_session` takes the manifest fields, `fromBytes` (how much the caller
believes is stored) and `data` (the transcript from there):

- A trailing line without its newline is held back, so `bytes` is always a
  whole-line boundary the caller can resume from.
- If `fromBytes` isn't what the manifest says, the answer is `resync: true`
  with the stored size and nothing is written: the caller sends the whole file
  with `replace: true`, which discards the old chunks (and, in `record` mode,
  deletes the chunk records that are no longer named).
- Every chunk is checked against its SHA-256 on the way back, so a damaged
  blob is reported rather than silently appended.

`pull_session` returns the manifest and the transcript, ready to be written as
a session file. `list_sessions` returns manifests only, so listing costs no
blob traffic.

## Picking a session up

A harness writes the pulled transcript where its own session files live. For
pi that is `~/.pi/agent/sessions/--<encoded cwd>--/<timestamp>_<id>.jsonl`,
which `/resume` (its All scope) and `--session` both find; pi prompts to
continue in the current directory when the recorded `cwd` doesn't exist on
this machine, and the `gitHead` in the record says which commit the work was
at.

One session belongs to one machine at a time: two machines appending to the
same session would each push a different suffix. The manifest's `machine`,
`head` and `updatedAt` are what a client needs to notice that, and pulling
refuses to overwrite an existing file.

## CLIs

- `engram sessions list`, `pull <id> [--to file]`, `push <file> [--id id]`,
  `rm <id>`, each with `--space`.
- MCP: `list_sessions`, `pull_session`, `push_session`, `delete_session`; the
  pi extension wraps them as `engram_<name>` for codemode and pushes
  automatically (`autoSessionSync`, at most once every `sessionSyncSeconds`,
  and once more when the session ends).
