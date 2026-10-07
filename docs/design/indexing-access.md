# How the appview gets read access to a space

Status: implemented. This replaces the appview's own ATProto account. The
code is in `internal/appview` (`grants.go`, `oauth.go`), `internal/spaceclient`
and the web app's space pages.

## The problem

To index a space, the appview reads every member's memory records in the
background, whether or not anyone is asking at that moment. A space is
private, so reading it takes a space credential from the space's authority,
and the authority issues one only in exchange for a delegation token: a
short-lived JWT, minted by a user's PDS, saying "this app acts for this user
in this space".

Until now the appview had an ATProto account of its own. It logged in with a
password, asked its own PDS for delegation tokens, and the authority added
that account to the space as a read-only member. That's the wrong shape:

- The appview is a service, not a person. It needed a password in its
  configuration, and an account on some PDS that someone had to create and
  keep alive.
- The Spaces proposal says applications act **for users**. A delegation token
  is "minted by the user's PDS" through OAuth, and an application serving
  several users of a space "may obtain its credential using any one user's
  session. When it loses all OAuth sessions for a space, it can no longer
  renew the credential and loses access." ([proposal 0016][p0016])
- Asked whether services that process space data ahead of users should use "a
  bot DID of their own", the proposal's author answered: "Nope, the intention
  is that you do so with a long-standing OAuth credential, which shouldn't
  require any user interaction (aside from the initial grant)." ([discussion,
  post 10][disc])
- No other Spaces app found logs an appview into its own account. The ones
  with a server side either make the server the space's authority
  (HappyView), keep the data in the server itself (Contrail, Arbiter), or ask
  a managing app (Bluesky's bulletin). None of those fit Engram Garden, where
  memories live in each agent's own repo.

We also considered giving the appview a `did:web` with a key of its own and
having it sign its own delegation tokens. Cocoon happens to accept that, but
the proposal requires the delegation token's key to be the user's `#atproto`
key, so other space hosts may refuse it.

## The design

**The space's authority grants the appview long-lived, read-only OAuth access
to the spaces they govern.** The appview keeps that session and uses it only
to ask the authority's PDS for delegation tokens, which it exchanges for space
credentials exactly as before. The appview has no account, no password, and
no place in the member list.

```
 person (the space's authority)
   │ 1. "Let the appview index this space" in the web app
   ▼
 appview /oauth/grant?space=…&return=…          (api.engram.garden)
   │ 2. OAuth: PAR, then the person's authorization server
   ▼
 authority's PDS: consent to "read the memory spaces you govern"
   │ 3. callback to the appview
   ▼
 appview: session DID == space authority?  credential works?
   │ 4. save the grant and the space's registration; start syncing
   ▼
 back to the web app (return URL)

 later, every ~8 minutes per space:
 appview ──getDelegationToken (OAuth, DPoP)──► authority's PDS
         ──getSpaceCredential────────────────► authority (space host)
         ──listRepos / listRepoOps / getRepo─► members' PDSes
```

### The grant

- **Who can grant: the space's authority only.** Every member can read the
  space, and the proposal allows any member's session, but having a space
  indexed by an outside service is the authority's call, as adding the
  appview as a member was before. The callback refuses a session whose DID
  isn't the space's authority.
- **What it asks for:** `atproto space:garden.engram.space?action=read`. With
  no `authority` parameter, a `space:` grant covers only spaces the user
  governs, and `read` is what `getDelegationToken` checks. The grant can't
  write, delete or manage anything, and it doesn't reach spaces the person
  is merely a member of.
- **One OAuth session per space.** Granting a second space signs in again. An
  OAuth session lets the person see and revoke each grant separately, and
  each space's session is used only by the node that owns the space, so two
  nodes never race to refresh the same tokens (a refresh token is replaced
  each time it's used).
- **Granting again replaces the grant.** The old session is revoked at the
  authorization server and deleted.

### Stopping

- **Stop indexing** in the web app runs the same OAuth flow with only the
  `atproto` scope, to prove the person is the space's authority. The appview
  then revokes the stored session, deletes the grant, and revokes the
  sign-in it just used. The space's existing index stays where it is, and
  isn't updated again unless someone grants again.
- Revoking the app's access at the PDS also works. The appview notices when
  its next token refresh fails and reports the grant as lapsed.

### The appview as an OAuth client

- The appview is a confidential OAuth client with DPoP, like the web app, but
  separate from it: client metadata at
  `<ENGRAM_PUBLIC_URL>/oauth/client-metadata.json`, its own P-256 key in
  `ENGRAM_OAUTH_KEY`, and its callback at `/oauth/callback`. For development,
  an `http://127.0.0.1:<port>` public URL makes a loopback client that needs
  no key.
- Like the web app's sign-in, each callback must come from the browser that
  started it: starting a grant sets a cookie holding a MAC of the OAuth
  state. The MAC key is derived from `ENGRAM_OAUTH_KEY`, so it's the same on
  every node and the callback can land on any of them. The node that takes
  the callback checks the grant works by getting a credential with it. If it
  owns the space it starts syncing at once; otherwise the owner picks the
  space up at its next poll.
- The return URL must be on an origin listed in `ENGRAM_RETURN_ORIGINS`
  (comma-separated, e.g. `https://engram.garden`); otherwise the appview shows
  its own short result page. This keeps `/oauth/grant` from being an open
  redirect.

### Where grants are kept

In the same bucket as the index:

| Key | Contents | Written |
|---|---|---|
| `grants/<space key>.json` | space, the authority's DID, OAuth session ID, when granted | on each grant; deleted on stop |
| `oauth/sessions/<hash of DID>/<hash of session ID>.json` | indigo's OAuth session: tokens, DPoP key, auth server URLs | on grant and on every token refresh |
| `oauth/requests/<hash of state>.json` | indigo's record of a sign-in in progress | at the start of a grant or stop; deleted at the callback; ignored after 10 minutes |
| `oauth/pending/<hash of state>.json` | the space, grant or stop, and return URL of a sign-in in progress | same |
| `registered-spaces/<space key>.json` | the space is indexed (unchanged, write-once) | on the first grant |

Grants and sessions are overwritten in place. The rule that objects are
written once applies to segments, manifests and registrations, not to these.

A stolen OAuth session allows reading every memory space its grantor
governs, until the grant is stopped or revoked. The bucket already holds the
text of every indexed memory, so it needed protecting anyway; encrypting
sessions at rest is a later improvement.

### Using a grant

`spaceclient` takes a **delegation source** instead of a PDS session:

- agents (`engram-mcp`) and the web app: their own session's
  `getDelegationToken`, as before;
- the appview: look up the space's grant, resume its OAuth session, and call
  `getDelegationToken` on the authority's PDS. indigo refreshes the access
  token when it expires and saves the new tokens through the bucket store.

A credential lasts 10 minutes and is renewed 90 seconds before it expires, so
each indexed space uses its grant about every 8½ minutes while its owner node
syncs it. That keeps Cocoon's refresh token, which expires after 3 months
unused, alive indefinitely. Cocoon ends a confidential client's session after
2 years regardless, so the authority will eventually need to grant again.

### Telling people about it

- `garden.engram.describeService` returns the service DID, whether
  registration is open, and `grantUrl`, the page that starts a grant. The
  `account` field is gone.
- `garden.engram.getSpaceStatus` gains `access`: `granted` (with who granted
  it and when), `missing` (no grant: the index isn't updated), or `lapsed`
  (the last attempt to use the grant failed because the authorization server
  refused it; grant again).
- `garden.engram.registerSpace` is removed. Granting registers the space.

### Configuration

| Variable | |
|---|---|
| `ENGRAM_IDENTIFIER` / `ENGRAM_PASSWORD` / `ENGRAM_PDS_HOST` | removed from the appview (agents still use them) |
| `ENGRAM_OAUTH_KEY` | the appview's OAuth client key, P-256 multibase. Required when `ENGRAM_PUBLIC_URL` is https. |
| `ENGRAM_RETURN_ORIGINS` | origins a grant may return to, e.g. `https://engram.garden` |
| `ENGRAM_PUBLIC_URL` | now required: it's also the OAuth client's base URL, and without grants the appview can't read anything. An `http://127.0.0.1:<port>` URL makes a development client and turns off notifications (polling only). |
| `ENGRAM_SPACES` | still indexed, but each still needs a grant from its authority |
| `ENGRAM_REGISTRATION=closed` | grants are accepted only for `ENGRAM_SPACES` |

`engram-mcp` and `engram-web` now default to `https://api.engram.garden`
(`did:web:api.engram.garden`), where the appview runs, with the web app at
`https://engram.garden`.

## What changes for people

- Creating a space in the web app: create it, declare the model, then **let
  the appview index it**, which goes to your PDS's consent screen and back.
  There's no appview member to add.
- Existing spaces need a grant from their authority before the appview can
  update them again. If the appview's old account is in the member list, the
  authority can remove it.

## Testing

- `spacetest`'s authority now always admits itself, as Cocoon does, so a
  grant from the authority yields credentials.
- Appview tests sign in through a fake authorizer that returns `spacetest`
  sessions, the way the web app's tests fake `Auth`. They cover: granting,
  refusing a session from someone other than the authority, refusing a
  callback from another browser, return-URL checks, stopping, a lapsed
  grant, closed registration, and syncing a space through its grant.
- The real OAuth client is covered as far as its metadata documents. The
  full flow needs a real PDS and is part of the end-to-end test on Cocoon.

## Later

- Let members keep a space indexed when the authority's grant lapses.
- Encrypt OAuth sessions at rest.
- Delete a space's index when indexing is stopped, rather than leaving it.
- Check other PDSes accept `space:` scopes; only Cocoon is verified.

[p0016]: https://github.com/bluesky-social/proposals/blob/main/0016-permissioned-data/README.md
[disc]: https://discourse.atmosphere.community/t/permissioned-data-proposal-discussion/946
