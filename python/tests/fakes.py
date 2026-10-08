"""An in-process fake of what the client talks to: the account's PDS, the space authority's host and the
appview. It checks what the real services check (delegation tokens, the credential's bound key, HTTP message
signatures and their audience) so a client that passes here would be accepted there."""

from __future__ import annotations

import base64
import json
import math
import time
from dataclasses import dataclass, field
from typing import Any

import httpx

from engram_garden import lex, space, vec
from engram_garden.embed import HASHING_DIGEST, hash_embed

APPVIEW_URL = "https://api.engram.test"
APPVIEW_DID = "did:web:api.engram.test"
PDS_URL = "https://pds.test"
AUTHORITY_DID = "did:plc:authorityauthorityauthor"
AGENT_DID = "did:plc:agentagentagentagentagen"
OTHER_DID = "did:plc:otherotherotherotherothe"
SPACE = f"at://{AUTHORITY_DID}/space/garden.engram.space/memory"
MODEL = lex.ModelInfo("hashing-64", HASHING_DIGEST, 64)
PASSWORD = "hunter2"


def b64url(b: bytes) -> str:
    return base64.urlsafe_b64encode(b).decode().rstrip("=")


def fake_jwt(payload: dict[str, Any]) -> str:
    return ".".join([b64url(b'{"alg":"ES256"}'), b64url(json.dumps(payload).encode()), b64url(b"sig")])


def json_resp(status: int, body: Any) -> httpx.Response:
    return httpx.Response(status, json=body)


def xrpc_err(status: int, name: str, message: str = "") -> httpx.Response:
    return json_resp(status, {"error": name, "message": message})


@dataclass
class Account:
    did: str
    handle: str
    password: str = PASSWORD
    #: Spaces the account is a member of, with (read, write).
    member_of: dict[str, tuple[bool, bool]] = field(default_factory=dict)


@dataclass
class World:
    """The fake network. Use .transport with httpx.AsyncClient(transport=...)."""

    config: dict[str, Any] | None = field(default_factory=lambda: lex.Config(MODEL).record())
    accounts: dict[str, Account] = field(default_factory=dict)
    #: (space, repo, collection) -> rkey -> record
    records: dict[tuple[str, str, str], dict[str, dict[str, Any]]] = field(default_factory=dict)
    access_state: str = "granted"
    #: Calls seen, as "METHOD host/nsid", for assertions.
    calls: list[str] = field(default_factory=list)
    #: Make the next N PDS calls fail with ExpiredToken.
    expire_tokens: int = 0
    #: Make the next N credentialed calls fail with a 401.
    reject_credentials: int = 0
    refreshes: int = 0
    exchanges: int = 0
    spaces_created: list[dict[str, Any]] = field(default_factory=list)
    members: dict[str, dict[str, tuple[bool, bool]]] = field(default_factory=dict)
    _tokens: dict[str, str] = field(default_factory=dict)  # access jwt -> did
    _refresh: dict[str, str] = field(default_factory=dict)  # refresh jwt -> did
    _delegations: dict[str, tuple[str, str]] = field(default_factory=dict)  # token -> (did, space)
    _creds: dict[str, tuple[str, str, str]] = field(default_factory=dict)  # jwt -> (did, space, bound key)
    _n: int = 0

    def __post_init__(self) -> None:
        self.accounts.setdefault(AGENT_DID, Account(AGENT_DID, "agent.test", member_of={SPACE: (True, True)}))
        self.accounts.setdefault(AUTHORITY_DID, Account(AUTHORITY_DID, "authority.test"))
        self.accounts.setdefault(OTHER_DID, Account(OTHER_DID, "other.test", member_of={SPACE: (True, True)}))
        self.members.setdefault(SPACE, {AGENT_DID: (True, True), OTHER_DID: (True, True)})

    @property
    def transport(self) -> httpx.MockTransport:
        return httpx.MockTransport(self.handle)

    # ---- helpers ----

    def put(self, space_uri: str, repo: str, collection: str, rkey: str, rec: dict[str, Any]) -> None:
        self.records.setdefault((space_uri, repo, collection), {})[rkey] = rec

    def memories(self, space_uri: str, repo: str | None = None) -> dict[str, dict[str, Any]]:
        """uri -> record for the memories in a space."""
        out = {}
        for (sp, r, coll), recs in self.records.items():
            if sp == space_uri and coll == lex.MEMORY_COLLECTION and (repo is None or r == repo):
                for rkey, rec in recs.items():
                    out[f"{sp}/{r}/{coll}/{rkey}"] = rec
        return out

    def add_other_memory(self, text: str, tags: list[str] | None = None, space_uri: str = SPACE) -> str:
        """A memory another agent wrote, embedded like a client would."""
        v = hash_vector(text + ("\n\nTags: " + ", ".join(tags) if tags else ""))
        rec: dict[str, Any] = {
            "$type": lex.MEMORY_COLLECTION,
            "text": text,
            "createdAt": "2026-01-01T00:00:00.000Z",
            "embedding": lex.embedding_record(MODEL, v),
        }
        if tags:
            rec["tags"] = tags
        self._n += 1
        rkey = f"3other{self._n:06d}"
        self.put(space_uri, OTHER_DID, lex.MEMORY_COLLECTION, rkey, rec)
        return f"{space_uri}/{OTHER_DID}/{lex.MEMORY_COLLECTION}/{rkey}"

    def _mint_credential(self, did: str, space_uri: str, bound_key: str) -> str:
        now = int(time.time())
        jwt = fake_jwt(
            {
                "iss": AUTHORITY_DID,
                "sub": did,
                "space": space_uri,
                "iat": now,
                "exp": now + 600,
                "cnf": {"kid": bound_key},
                "jti": str(self._n),
            }
        )
        self._n += 1
        self._creds[jwt] = (did, space_uri, bound_key)
        return jwt

    def _check_credentialed(self, req: httpx.Request, audience: str | None) -> tuple[str, str] | httpx.Response:
        """Verify a credentialed request as a space host or appview would."""
        if self.reject_credentials > 0:
            self.reject_credentials -= 1
            return xrpc_err(401, "AuthRequired", "bad credential")
        auth = req.headers.get("authorization", "")
        if not auth.startswith("Atproto-Space "):
            return xrpc_err(401, "AuthRequired", "no credential")
        cred = auth.removeprefix("Atproto-Space ")
        if cred not in self._creds:
            return xrpc_err(401, "AuthRequired", "unknown credential")
        did, space_uri, bound = self._creds[cred]
        try:
            space.verify_space_signature(dict(req.headers), bound)
        except ValueError as e:
            return xrpc_err(401, "BadSpaceSignature", str(e))
        if audience is None:  # a space host: its audience is the space's authority
            audience = space.Ref.parse(space_uri).authority
        if req.headers.get("atproto-space-audience") != audience:
            return xrpc_err(401, "BadSpaceSignature", "wrong audience")
        return did, space_uri

    # ---- routing ----

    def handle(self, req: httpx.Request) -> httpx.Response:
        host = f"{req.url.scheme}://{req.url.host}"
        path = req.url.path
        if path == "/.well-known/atproto-did":
            for a in self.accounts.values():
                if a.handle == req.url.host:
                    return httpx.Response(200, text=a.did)
            return httpx.Response(404, text="no such handle")
        self.calls.append(f"{req.method} {req.url.host}{path}")
        if host == "https://plc.directory":
            return self._plc(path.lstrip("/"))
        if host == PDS_URL:
            return self._pds(req)
        if host == APPVIEW_URL:
            return self._appview(req)
        return httpx.Response(404, text="unknown host " + host)

    def _plc(self, did: str) -> httpx.Response:
        if did not in self.accounts:
            return httpx.Response(404, text="not found")
        a = self.accounts[did]
        return json_resp(
            200,
            {
                "id": did,
                "alsoKnownAs": [f"at://{a.handle}"],
                "service": [{"id": "#atproto_pds", "type": "AtprotoPersonalDataServer", "serviceEndpoint": PDS_URL}],
            },
        )

    def _pds(self, req: httpx.Request) -> httpx.Response:
        nsid = req.url.path.removeprefix("/xrpc/")
        q = req.url.params
        body: dict[str, Any] = json.loads(req.content) if req.content else {}
        if nsid == "com.atproto.server.createSession":
            for a in self.accounts.values():
                if body.get("identifier") in (a.did, a.handle) and body.get("password") == a.password:
                    return json_resp(200, self._session(a.did, a.handle))
            return xrpc_err(401, "AuthenticationRequired", "Invalid identifier or password")
        if nsid == "com.atproto.server.refreshSession":
            rj = req.headers.get("authorization", "").removeprefix("Bearer ")
            if rj not in self._refresh:
                return xrpc_err(400, "ExpiredToken", "refresh token expired")
            self.refreshes += 1
            did = self._refresh.pop(rj)
            return json_resp(200, self._session(did, self.accounts[did].handle))
        # Everything else needs a session.
        if nsid == "com.atproto.space.getSpaceCredential":
            return self._exchange(req, body)
        tok = req.headers.get("authorization", "").removeprefix("Bearer ")
        # Credentialed reads at the authority's PDS (the space config).
        if req.headers.get("authorization", "").startswith("Atproto-Space "):
            return self._pds_credentialed(req, nsid, q)
        if self.expire_tokens > 0:
            self.expire_tokens -= 1
            return xrpc_err(400, "ExpiredToken", "token expired")
        did = self._tokens.get(tok)
        if did is None:
            return xrpc_err(401, "AuthenticationRequired", "bad token")
        return self._pds_session(did, nsid, q, body)

    def _session(self, did: str, handle: str) -> dict[str, Any]:
        self._n += 1
        access, refresh = f"access-{did}-{self._n}", f"refresh-{did}-{self._n}"
        self._tokens[access], self._refresh[refresh] = did, did
        return {"did": did, "handle": handle, "accessJwt": access, "refreshJwt": refresh}

    def _exchange(self, req: httpx.Request, body: dict[str, Any]) -> httpx.Response:
        self.exchanges += 1
        auth = req.headers.get("authorization", "")
        if not auth.startswith("Bearer "):
            return xrpc_err(401, "AuthRequired", "no delegation token")
        deleg = self._delegations.get(auth.removeprefix("Bearer "))
        if deleg is None or deleg[1] != body.get("space"):
            return xrpc_err(401, "InvalidToken", "bad delegation token")
        try:
            bound = space.verify_space_signature(dict(req.headers), "")
        except ValueError as e:
            return xrpc_err(401, "BadSpaceSignature", str(e))
        return json_resp(200, {"credential": self._mint_credential(deleg[0], deleg[1], bound)})

    def _pds_credentialed(self, req: httpx.Request, nsid: str, q: httpx.QueryParams) -> httpx.Response:
        checked = self._check_credentialed(req, None)
        if isinstance(checked, httpx.Response):
            return checked
        if nsid == "com.atproto.space.getRecord":
            key = (q["space"], q["repo"], q["collection"])
            if q["collection"] == lex.CONFIG_COLLECTION and self.config is not None:
                return json_resp(200, {"uri": "x", "value": self.config})
            rec = self.records.get(key, {}).get(q["rkey"])
            if rec is None:
                return xrpc_err(404, "RecordNotFound", "no such record")
            return json_resp(200, {"uri": "x", "value": rec})
        return xrpc_err(404, "MethodNotImplemented", nsid)

    def _pds_session(self, did: str, nsid: str, q: httpx.QueryParams, body: dict[str, Any]) -> httpx.Response:
        acct = self.accounts[did]
        if nsid == "com.atproto.space.getDelegationToken":
            sp = q["space"]
            if sp not in acct.member_of and not sp.startswith(f"at://{did}/"):
                return xrpc_err(403, "NotAMember", "not a member of that space")
            tok = f"deleg-{self._n}"
            self._n += 1
            self._delegations[tok] = (did, sp)
            return json_resp(200, {"token": tok})
        if nsid in ("com.atproto.space.createRecord", "com.atproto.space.putRecord"):
            if body["repo"] != did:
                return xrpc_err(403, "Forbidden", "can only write your own repo")
            _, write = acct.member_of.get(body["space"], (False, False))
            if body["collection"] == lex.MEMORY_COLLECTION and not write:
                return xrpc_err(403, "Forbidden", "not allowed to write")
            if nsid.endswith("createRecord"):
                self._n += 1
                rkey = f"3mem{self._n:08d}"
            else:
                rkey = body["rkey"]
            if body["collection"] == lex.CONFIG_COLLECTION:
                self.config = body["record"]
            else:
                self.put(body["space"], did, body["collection"], rkey, body["record"])
            uri = f"{body['space']}/{did}/{body['collection']}/{rkey}"
            return json_resp(200, {"uri": uri, "cid": "bafy-" + rkey})
        if nsid == "com.atproto.space.deleteRecord":
            if body["repo"] != did:
                return xrpc_err(403, "Forbidden", "can only delete your own repo")
            recs = self.records.get((body["space"], did, body["collection"]), {})
            recs.pop(body["rkey"], None)
            return json_resp(200, {})
        if nsid == "com.atproto.space.listRecords":
            recs = self.records.get((q["space"], q["repo"], q["collection"]), {})
            limit = int(q.get("limit", "50"))
            keys = sorted(recs)
            start = keys.index(q["cursor"]) + 1 if "cursor" in q else 0
            page = keys[start : start + limit]
            # Like cocoon: list items name a record by collection and rkey, with no uri.
            out: dict[str, Any] = {
                "records": [
                    {"collection": q["collection"], "rkey": k, "cid": "bafy-" + k, "value": recs[k]} for k in page
                ]
            }
            if start + limit < len(keys):
                out["cursor"] = page[-1]
            return json_resp(200, out)
        if nsid == "com.atproto.space.getRecord":
            if q["collection"] == lex.CONFIG_COLLECTION and self.config is not None:
                return json_resp(200, {"uri": "x", "value": self.config})
            return xrpc_err(404, "RecordNotFound", "no such record")
        if nsid == "com.atproto.space.listSpaces":
            return json_resp(200, {"spaces": [{"uri": u} for u in acct.member_of]})
        if nsid == "com.atproto.simplespace.createSpace":
            self.spaces_created.append(body)
            uri = f"at://{did}/space/{body['spaceType']}/{body['skey']}"
            self.members[uri] = {did: (True, True)}
            acct.member_of[uri] = (True, True)
            return json_resp(200, {"uri": uri})
        if nsid == "com.atproto.simplespace.listMembers":
            ms = self.members.get(q["space"], {})
            return json_resp(200, {"members": [{"did": d, "read": r, "write": w} for d, (r, w) in ms.items()]})
        if nsid == "com.atproto.simplespace.putMember":
            self.members.setdefault(body["space"], {})[body["did"]] = (body["read"], body["write"])
            return json_resp(200, {})
        if nsid == "com.atproto.simplespace.removeMember":
            self.members.get(body["space"], {}).pop(body["did"], None)
            return json_resp(200, {})
        return xrpc_err(404, "MethodNotImplemented", nsid)

    def _appview(self, req: httpx.Request) -> httpx.Response:
        nsid = req.url.path.removeprefix("/xrpc/")
        q = req.url.params
        if nsid == "garden.engram.describeService":
            return json_resp(
                200, {"did": APPVIEW_DID, "grantUrl": APPVIEW_URL + "/oauth/grant", "registration": "open"}
            )
        checked = self._check_credentialed(req, APPVIEW_DID)
        if isinstance(checked, httpx.Response):
            return checked
        _, space_uri = checked
        body = json.loads(req.content) if req.content else {}
        if (q.get("space") or body.get("space")) != space_uri:
            return xrpc_err(403, "Forbidden", "credential is for another space")
        if nsid == "garden.engram.getSpaceStatus":
            return json_resp(200, {"access": {"state": self.access_state}})
        if nsid == "garden.engram.warmSpace":
            return json_resp(200, {})
        if nsid == "garden.engram.getMemory":
            rec = self.memories(space_uri).get(q["uri"])
            if rec is None:
                return xrpc_err(400, "NotFound", "no such memory")
            return json_resp(200, {"memory": self._view(q["uri"], rec)})
        if nsid == "garden.engram.listMemories":
            items = self._filtered(space_uri, q)
            items.sort(key=lambda kv: kv[1].get("createdAt", ""), reverse=True)
            limit = int(q.get("limit", "25"))
            start = int(q.get("cursor", "0"))
            page = items[start : start + limit]
            out: dict[str, Any] = {"memories": [self._view(u, r) for u, r in page]}
            if start + limit < len(items):
                out["cursor"] = str(start + limit)
            return json_resp(200, out)
        if nsid == "garden.engram.searchMemories":
            cfg = lex.parse_config(self.config or {})
            if (q["model"], q["modelDigest"]) != (cfg.model, cfg.model_digest):
                return xrpc_err(400, "ModelMismatch", "query model isn't the space's model")
            qv = lex.decode_query_vector(q["vector"])
            scored = []
            for uri, rec in self._filtered(space_uri, q):
                for e in lex.parse_embeddings(rec):
                    if e.key() == cfg.model_info.key():
                        scored.append((sum(a * b for a, b in zip(qv, e.vector, strict=True)), uri, rec))
            scored.sort(key=lambda t: -t[0])
            limit = int(q.get("limit", "10"))
            return json_resp(
                200,
                {
                    "memories": [
                        {**self._view(u, r), "similarity": max(0, min(1000, round(s * 1000)))}
                        for s, u, r in scored[:limit]
                    ]
                },
            )
        return xrpc_err(404, "MethodNotImplemented", nsid)

    def _filtered(self, space_uri: str, q: httpx.QueryParams) -> list[tuple[str, dict[str, Any]]]:
        want_tags = q.get_list("tags")
        out = []
        for uri, rec in self.memories(space_uri).items():
            if (a := q.get("author")) and f"/{a}/" not in uri:
                continue
            if want_tags and not set(want_tags) <= set(rec.get("tags", [])):
                continue
            if (since := q.get("since")) and rec["createdAt"] < since:
                continue
            out.append((uri, rec))
        return out

    @staticmethod
    def _view(uri: str, rec: dict[str, Any]) -> dict[str, Any]:
        author = uri.split("/")[6]
        v = {
            "uri": uri,
            "cid": "bafy",
            "author": author,
            "text": rec["text"],
            "tags": rec.get("tags", []),
            "createdAt": rec["createdAt"],
            "indexedAt": rec["createdAt"],
        }
        if "source" in rec:
            v["source"] = rec["source"]
        return v


def hash_vector(text: str) -> list[float]:
    """What the hashing provider makes of a text, normalized as a client does."""
    v = hash_embed(text, MODEL.dims)
    assert vec.normalize(v) and math.isclose(sum(x * x for x in v), 1.0, rel_tol=1e-6)
    return v
