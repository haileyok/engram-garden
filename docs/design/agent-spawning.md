# Agents creating agents

Status: in progress. The PDS side is a Cocoon change (key-based sign-in);
the rest is `internal/coord`, `internal/agent` and `engram agent spawn`.

## The problem

Every agent has its own DID, and agents need to create sub-agents of their
own, so creating an agent has to be one step an agent can take by itself:
no shell on the PDS, no browser. At the same time an agent may only ever
act through a scoped, DPoP-bound OAuth session: no passwords, no app
passwords, no legacy sessions. And the operator must keep the controls
that stop any agent, however many generations down.

## The design

**A child agent signs in with a key of its own, through a standard OAuth
grant.** The child's runtime generates a keypair. Its account is created
with the public key registered as a sign-in key and no password. To sign
in, the runtime sends the PDS's token endpoint a short JWT signed with
that key (the JWT bearer grant, RFC 7523) and gets an ordinary OAuth
session: scoped, DPoP-bound, refreshable, listed and revocable like any
other. Nobody else ever holds the private key, the parent included.

```
parent agent                         PDS (Cocoon)
  │ 1. child key generated (private part stays with the child's runtime)
  │ 2. com.atproto.server.createAccount
  │      handle, email (placeholder), inviteCode,
  │      recoveryKey = the operator's did:key, authKey = child's did:key
  │                                    ──► account, PLC genesis (recovery key first)
  │ 3. POST /oauth/token  grant_type=jwt-bearer, assertion signed by the child key,
  │      DPoP proof, scope            ──► scoped OAuth session for the child
  │ 4. the child writes its declaration (operator = parent);
  │    the parent writes its operated record (agent = child)
```

The PDS knows nothing about parents and children. The chain lives in the
records: the child's `garden.engram.agents.declaration` names the parent,
the parent's `garden.engram.agents.operated` names the child, and so on up
to the operator, a person. `VerifyOperator` walks it.

## The PDS contract

### Creating an account with a sign-in key

`com.atproto.server.createAccount` takes one more optional input, `authKey`:
a did:key (K-256 or P-256). With `authKey` set:

- `password` may be omitted, and if it is the account has no password:
  password sign-in (`createSession`) always fails for it.
- No legacy session is created: `accessJwt` and `refreshJwt` in the output
  are empty strings. The account signs in with the key.
- The key is stored as an active sign-in key for the account.

Everything else is as before: invite codes when the PDS requires them
(their use count is the spawn budget), `recoveryKey` first in the PLC
genesis, the account events.

### Signing in with the key

`POST /oauth/token`, form-encoded, with a `DPoP` proof header:

| Field | Value |
|---|---|
| `grant_type` | `urn:ietf:params:oauth:grant-type:jwt-bearer` |
| `client_id` | the OAuth client. A loopback client (`http://localhost?...`) may use this grant; another client must list it in its metadata's `grant_types`. |
| `assertion` | the JWT below |
| `scope` | space-separated scopes, within the client's |

The assertion is a JWT signed by one of the account's active sign-in keys
(`alg` `ES256K` for K-256, `ES256` for P-256), with claims:

| Claim | Value |
|---|---|
| `iss`, `sub` | the account's DID (both) |
| `aud` | the PDS's OAuth issuer, `https://<hostname>` |
| `iat` | now (at most 60 s in the future) |
| `exp` | at most 5 minutes after `iat`, not past |
| `jti` | unique; a second use of the same `jti` is refused |

The response is the same as the authorization code grant's (`access_token`,
`token_type` `DPoP`, `refresh_token`, `scope`, `expires_in`, `sub`), with
the same DPoP nonce handling (`use_dpop_nonce`). The session refreshes with
the ordinary `refresh_token` grant, same client and DPoP key. Refused: an
unknown or revoked key, a bad signature or claims, a reused `jti`, a
deactivated or taken-down account (`invalid_grant`); scopes outside the
client's (`invalid_scope`).

### Revoking a key

Revoking a sign-in key (account dashboard, or `cocoon auth-keys revoke`)
stops it signing in and revokes the sessions issued through it.

## Stopping agents

Unchanged: the operator's offline key is first in every agent's PLC
rotation keys, the admin can deactivate or take down any account, and
OAuth sessions are revoked as usual. Revoking a sign-in key keeps a
stolen key file from signing in again. Nothing here gives a parent power
over a child's account.

## Verifying a chain

`VerifyOperator` follows operators upward, checking each declaration has
a matching operated record, until it reaches a DID that has no
declaration (a person). It also checks that the root's recovery key is the
first rotation key of each agent below it, so an agent created without it
fails verification. It gives up past a depth limit or on a cycle.

## Open

- A child that moves to another device copies its key file or gets a new
  key: adding a key from a signed-in session isn't designed yet.
- The child's key file is a long-lived credential on its device: scoped
  and revocable, but whoever reads it can sign in until it's revoked.
